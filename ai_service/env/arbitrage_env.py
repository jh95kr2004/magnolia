import gymnasium as gym
from gymnasium import spaces
import numpy as np
import pandas as pd


class ArbitrageEnv(gym.Env):
    """
    OpenAI Gym Environment for Magnolia DRL Portfolio Manager.
    Simulates bidirectional target exposure with dynamic 2x2 matrix execution costs
    matching the Go backend heuristics (Digital Twin architecture).
    """

    def __init__(
        self,
        df: pd.DataFrame,
        initial_balance=10000.0,
        base_fee_rate=0.0005,
        foreign_fee_rate=0.004,
        slippage=0.0005,
        execution_threshold=0.10,
        randomize_fees=False,
        is_eval=False,
    ):
        super(ArbitrageEnv, self).__init__()

        self.df = df.reset_index(drop=True)

        self.df["sma_1h"] = (
            self.df["exchange_premium_spread"].rolling(60, min_periods=1).mean()
        )
        self.df["sma_24h"] = (
            self.df["exchange_premium_spread"].rolling(1440, min_periods=1).mean()
        )
        self.df["std_24h"] = (
            self.df["exchange_premium_spread"].rolling(1440, min_periods=1).std()
        )
        self.df["z_score"] = (
            self.df["exchange_premium_spread"] - self.df["sma_24h"]
        ) / (self.df["std_24h"] + 1e-8)

        # New Alpha Factors: Bollinger Band Width & MACD
        self.df["bb_upper"] = self.df["sma_24h"] + (2.0 * self.df["std_24h"])
        self.df["bb_lower"] = self.df["sma_24h"] - (2.0 * self.df["std_24h"])
        self.df["bb_width"] = (self.df["bb_upper"] - self.df["bb_lower"]) / (
            self.df["sma_24h"] + 1e-8
        )

        ema_12 = self.df["exchange_premium_spread"].ewm(span=12, adjust=False).mean()
        ema_26 = self.df["exchange_premium_spread"].ewm(span=26, adjust=False).mean()
        self.df["macd"] = ema_12 - ema_26

        self.df.fillna(0, inplace=True)

        self.initial_balance = initial_balance
        self.base_fee_rate_default = base_fee_rate
        self.foreign_fee_rate_default = foreign_fee_rate
        self.slippage_default = slippage
        self.execution_threshold = execution_threshold
        self.randomize_fees = randomize_fees

        self.is_eval = is_eval
        self.current_step = 0
        self.max_steps = len(self.df) - 1

        # State Machine constants
        self.STATE_NEUTRAL = 0
        self.STATE_HOLDING_LEG = 1
        self.STATE_TRANSFERRING = 2
        self.STATE_REPAYING = 3
        
        self.state = self.STATE_NEUTRAL
        self.transfer_remaining_steps = 0

        # Action Space: index 0 is exposure [-1.0, 1.0], index 1 is action_decision [-1.0, 1.0]
        self.action_space = spaces.Box(
            low=np.array([-1.0, -1.0], dtype=np.float32),
            high=np.array([1.0, 1.0], dtype=np.float32),
            dtype=np.float32
        )

        # State Space: 22 features (18 original + 4 One-hot State Machine features)
        self.observation_space = spaces.Box(
            low=-np.inf, high=np.inf, shape=(22,), dtype=np.float32
        )

    def reset(self, seed=None, options=None):
        super().reset(seed=seed)
        if self.is_eval:
            self.current_step = 0
            self.max_steps = len(self.df) - 1
        else:
            if len(self.df) > 1000:
                self.current_step = np.random.randint(0, len(self.df) - 1000)
                self.max_steps = self.current_step + 1000
            else:
                self.current_step = 0
                self.max_steps = len(self.df) - 1

        self.balance = self.initial_balance
        self.exposure = 0.0  # From -1.0 to 1.0
        self.entry_spread = 0.0
        self.state = self.STATE_NEUTRAL
        self.transfer_remaining_steps = 0
        # Backend-identical Foreign USD Capital Skew & Dual-Trigger Rebalance thresholds (GEMINI.md Section 8.3)
        self.foreign_usd_skew = 0.50
        self.balance_skew_threshold = 0.15
        self.rebalance_trigger_skew = 0.20
        self.rebalance_max_premium = 0.0005

        if self.randomize_fees:
            # Dynamic asymmetric fee generation to simulate any combination of exchanges
            self.current_base_fee_rate = np.random.uniform(0.0000, 0.015)
            self.current_foreign_fee_rate = np.random.uniform(0.0000, 0.015)
            self.current_slippage = np.random.uniform(0.0, 0.002)
        else:
            self.current_base_fee_rate = self.base_fee_rate_default
            self.current_foreign_fee_rate = self.foreign_fee_rate_default
            self.current_slippage = self.slippage_default

        return self._get_obs(), {}

    def _get_obs(self):
        row = self.df.iloc[self.current_step]
        
        # One-hot state variables
        state_neutral = 1.0 if self.state == self.STATE_NEUTRAL else 0.0
        state_holding = 1.0 if self.state == self.STATE_HOLDING_LEG else 0.0
        state_transferring = 1.0 if self.state == self.STATE_TRANSFERRING else 0.0
        state_repaying = 1.0 if self.state == self.STATE_REPAYING else 0.0

        obs = np.array(
            [
                row["exchange_premium_spread"],
                row["sma_1h"],
                row["sma_24h"],
                row["z_score"],
                row.get("predicted_spread", 0.0),  # Mean
                row.get("predicted_spread_max", 0.0),
                row.get("predicted_spread_min", 0.0),
                row["base_funding_rate"],
                row["foreign_funding_rate"],
                row["bb_width"],  # New Feature 1
                row["macd"],  # New Feature 2
                self.exposure,
                self.balance,
                self._get_unrealized_pnl(row["exchange_premium_spread"]),
                self.current_base_fee_rate,
                self.current_foreign_fee_rate,
                self.current_slippage,
                # Explicit Friction: Sum of base and foreign fees + overall slippage
                (self.current_base_fee_rate + self.current_foreign_fee_rate) + self.current_slippage,
                # One-hot state representations
                state_neutral,
                state_holding,
                state_transferring,
                state_repaying,
            ],
            dtype=np.float32,
        )
        return obs

    def _get_unrealized_pnl(self, current_spread):
        if self.exposure == 0:
            return 0.0
        # In Transfer Arbitrage (TRANSFERRING or REPAYING state with Short Base / Long Foreign),
        # the coin bought on Foreign is transferred to repay the Base coin loan 1:1, locking in entry_spread.
        if self.state in (self.STATE_TRANSFERRING, self.STATE_REPAYING) and self.exposure < 0:
            return abs(self.exposure) * self.initial_balance * max(0.0, self.entry_spread)
        # Bidirectional PnL equation matching physics of convergence trades:
        # If exposure > 0 (Long Base, Short Foreign), profit when spread RISES
        # If exposure < 0 (Short Base, Long Foreign), profit when spread FALLS
        return (
            self.exposure
            * self.initial_balance
            * (current_spread - self.entry_spread)
        )

    def step(self, action):
        # 1. Parse Action
        # Center interval [-0.33, +0.33] maps to 0 (TRADE/HOLD) so Gaussian mean 0.0 naturally trades/holds
        action_exposure = float(np.clip(action[0], -1.0, 1.0).item())
        action_dec = float(np.clip(action[1], -1.0, 1.0).item())
        if action_dec > 0.33:
            action_type = 1  # TRANSFER (Strategy C)
        elif action_dec < -0.33:
            action_type = 2  # REPAY / EXIT
        else:
            action_type = 0  # TRADE / HOLD

        row = self.df.iloc[self.current_step]
        current_spread = float(row["exchange_premium_spread"])
        base_funding = float(row["base_funding_rate"])
        foreign_funding = float(row["foreign_funding_rate"])

        # 2. Calculate Previous Equity
        unrealized_pnl_prev = self._get_unrealized_pnl(current_spread)
        total_equity_prev = self.balance + unrealized_pnl_prev

        # 3. Dynamic Cost Variables
        borrow_rate_pct = float(row.get("borrow_rate_pct", 0.0041))  # Default roughly 0.0041% hourly
        borrow_rate = borrow_rate_pct / 100.0

        prev_state = self.state
        step_fees_paid = 0.0
        repay_via_transfer = False
        rebalance_triggered = False

        # 3b. Dual-Trigger 100% Delta-Neutral Reverse Transfer Rebalancing (GEMINI.md Section 8.3)
        # Triggered when in NEUTRAL state, Foreign USD Skew <= 20% (RebalanceTriggerSkew), and spread <= RebalanceMaxPremium
        if (
            self.state == self.STATE_NEUTRAL
            and self.foreign_usd_skew <= self.rebalance_trigger_skew
            and current_spread <= self.rebalance_max_premium
        ):
            rebalance_amount = 0.50 - self.foreign_usd_skew
            rebalance_notional = rebalance_amount * self.initial_balance
            rebalance_fee = rebalance_notional * (self.current_base_fee_rate + self.current_foreign_fee_rate) + 2.50
            # Reverse premium bonus/cost when buying Spot Base at negative/near-zero spread and shorting Foreign
            rebalance_spread_pnl = rebalance_notional * (-current_spread)
            self.balance = self.balance - rebalance_fee + rebalance_spread_pnl
            step_fees_paid += rebalance_fee
            self.foreign_usd_skew = 0.50
            rebalance_triggered = True

        # 4. State Machine Transition Logic
        target_exposure = self.exposure

        if self.state == self.STATE_TRANSFERRING:
            # Undergoing transfer - lock exposure and ignore user actions
            target_exposure = self.exposure
            self.transfer_remaining_steps -= 1
            if self.transfer_remaining_steps <= 0:
                self.state = self.STATE_REPAYING  # Transfer completed, awaiting repayment

        elif self.state == self.STATE_NEUTRAL:
            # In Neutral state, there is no open position to transfer or repay; any exposure >= threshold is an entry
            if abs(action_exposure) >= self.execution_threshold:
                target_exposure = action_exposure
                self.state = self.STATE_HOLDING_LEG
            else:
                target_exposure = 0.0

        elif self.state == self.STATE_HOLDING_LEG:
            if action_type == 1:  # TRANSFER (Only valid when Short Base / Long Foreign, entry_spread >= 0.5%, and Foreign USD Skew >= 15%)
                if (
                    self.exposure < 0
                    and self.entry_spread >= 0.005
                    and self.foreign_usd_skew >= self.balance_skew_threshold
                ):
                    self.state = self.STATE_TRANSFERRING
                    self.transfer_remaining_steps = 8 if self.is_eval else np.random.randint(5, 15)
                    gas_fee = 2.50
                    self.balance -= gas_fee
                    step_fees_paid += gas_fee
                    # Moving coin from Foreign to Base consumes Foreign USD allocation
                    self.foreign_usd_skew = max(0.05, self.foreign_usd_skew - abs(self.exposure) * 0.15)
                    target_exposure = self.exposure
                elif abs(action_exposure) < self.execution_threshold:
                    self.state = self.STATE_NEUTRAL
                    target_exposure = 0.0
                else:
                    target_exposure = action_exposure
            elif action_type == 2 or abs(action_exposure) < self.execution_threshold:  # REPAY / Market Unwind
                target_exposure = 0.0
                self.state = self.STATE_NEUTRAL
            else:  # TRADE (Hold / Scale-In / Scale-Out)
                target_exposure = action_exposure

        elif self.state == self.STATE_REPAYING:
            if action_type == 2 or abs(action_exposure) < self.execution_threshold or current_spread <= 0.002:
                # Complete loan return using transferred coin
                target_exposure = 0.0
                self.state = self.STATE_NEUTRAL
                repay_via_transfer = True
            else:
                # Deferring repayment for funding rate yield optimization. Still holding position.
                target_exposure = self.exposure
                borrow_fee = abs(self.exposure) * self.initial_balance * borrow_rate * (1.0 / 60.0)
                self.balance -= borrow_fee
                step_fees_paid += borrow_fee

        # 5. Order Execution and Digital Twin Cost Calculation
        delta_exposure = target_exposure - self.exposure
        is_short_base = (target_exposure < 0) if target_exposure != 0 else (self.exposure < 0)

        base_spot_tx = self.current_base_fee_rate + self.current_slippage
        foreign_spot_tx = self.current_foreign_fee_rate + self.current_slippage

        base_future_tx = base_spot_tx * 0.5
        foreign_future_tx = foreign_spot_tx * 0.5

        base_funding_dec = base_funding
        foreign_funding_dec = foreign_funding

        if is_short_base:
            base_future_cost = base_future_tx - (base_funding_dec * 3.0)
            foreign_future_cost = foreign_future_tx + (foreign_funding_dec * 3.0)
            base_spot_cost = base_spot_tx + borrow_rate
            foreign_spot_cost = foreign_spot_tx
        else:
            base_future_cost = base_future_tx + (base_funding_dec * 3.0)
            foreign_future_cost = foreign_future_tx - (foreign_funding_dec * 3.0)
            base_spot_cost = base_spot_tx
            foreign_spot_cost = foreign_spot_tx + borrow_rate

        cheapest_base_exec = min(base_spot_cost, base_future_cost)
        cheapest_foreign_exec = min(foreign_spot_cost, foreign_future_cost)
        optimal_round_trip_fee = max(0.0005, cheapest_base_exec + cheapest_foreign_exec)

        vetoed = False
        # Only run Veto Manager when INCREASING exposure or flipping direction (never block exits or scale-outs!)
        is_opening_or_increasing = (
            target_exposure != 0.0
            and (
                abs(target_exposure) > abs(self.exposure)
                or np.sign(target_exposure) != np.sign(self.exposure)
            )
        )
        if is_opening_or_increasing and abs(delta_exposure) >= self.execution_threshold:
            trade_value = abs(delta_exposure) * self.initial_balance
            actual_slippage = self.current_slippage * (1.0 + (trade_value / 5000.0) ** 2)
            optimal_round_trip_fee = optimal_round_trip_fee - self.current_slippage + actual_slippage

            if target_exposure > 0:
                # Long Base / Short Foreign: profits when spread widens (or reverts up from negative reverse premium)
                forecast_widening = float(row.get("predicted_spread_max", current_spread)) - current_spread
                mean_reversion_up = float(row.get("sma_24h", 0.0)) - current_spread
                predicted_jump = max(forecast_widening, mean_reversion_up * 0.6)
            else:
                # Short Base / Long Foreign: profits from spread convergence OR instant transfer spread
                convergence_jump = current_spread - float(row.get("predicted_spread_min", current_spread))
                mean_reversion_down = current_spread - float(row.get("sma_24h", 0.0))
                transfer_spread = max(0.0, current_spread)
                predicted_jump = max(convergence_jump, mean_reversion_down * 0.6, transfer_spread)

            fee_percentage_wall = optimal_round_trip_fee
            if predicted_jump < fee_percentage_wall:
                # VETO trigger: Rollback trade execution and state transition
                target_exposure = self.exposure
                delta_exposure = 0.0
                self.state = prev_state
                vetoed = True

        trade_executed = False
        # Execute final trade deductions
        if abs(delta_exposure) >= self.execution_threshold:
            trade_executed = True
            trade_value = abs(delta_exposure) * self.initial_balance
            if repay_via_transfer:
                # Returning transferred coin to repay loan incurs no market sell/buy fee
                transaction_cost = 0.0
            elif abs(target_exposure) > abs(self.exposure):
                # Entry leg fee (half of round-trip)
                transaction_cost = trade_value * (optimal_round_trip_fee * 0.5)
            else:
                # Exit leg fee (half of round-trip)
                transaction_cost = trade_value * (optimal_round_trip_fee * 0.5)

            self.balance -= transaction_cost
            step_fees_paid += transaction_cost

            # Realized PnL Calculation
            if self.exposure != 0:
                if np.sign(target_exposure) != np.sign(self.exposure) and target_exposure != 0:
                    closed_exposure = self.exposure
                elif target_exposure == 0:
                    closed_exposure = self.exposure
                else:
                    if abs(target_exposure) < abs(self.exposure):
                        closed_exposure = self.exposure - target_exposure
                    else:
                        closed_exposure = 0.0

                if closed_exposure != 0:
                    if repay_via_transfer and closed_exposure < 0:
                        # Strategy C (Transfer Arbitrage): locked in entry_spread directly
                        realized_pnl = abs(closed_exposure) * self.initial_balance * max(0.0, self.entry_spread)
                    else:
                        # Strategy A/B/D/E (Convergence Arbitrage): realized spread difference
                        realized_pnl = closed_exposure * self.initial_balance * (current_spread - self.entry_spread)
                    self.balance += realized_pnl

            # Weighted dollar-cost average Entry Spread
            if self.exposure == 0 and target_exposure != 0:
                self.entry_spread = current_spread
            elif np.sign(target_exposure) != np.sign(self.exposure) and target_exposure != 0:
                self.entry_spread = current_spread
            elif abs(target_exposure) > abs(self.exposure):
                old_weight = abs(self.exposure)
                added_weight = abs(target_exposure) - abs(self.exposure)
                self.entry_spread = (self.entry_spread * old_weight + current_spread * added_weight) / abs(target_exposure)
            elif target_exposure == 0:
                self.entry_spread = 0.0

            self.exposure = target_exposure

        # 6. Progress Time & Accrue Funding Rate
        self.current_step += 1
        done = self.current_step >= self.max_steps

        # Apply continuous funding fee yield accruals
        accrual_factor = 1.0 / (8 * 60)
        if self.exposure > 0:
            funding_pnl = self.initial_balance * abs(self.exposure) * (foreign_funding_dec - base_funding_dec) * accrual_factor
        elif self.exposure < 0:
            funding_pnl = self.initial_balance * abs(self.exposure) * (base_funding_dec - foreign_funding_dec) * accrual_factor
        else:
            funding_pnl = 0.0
        self.balance += funding_pnl

        # Base Borrow interest fee hourly accrual applied per step
        if self.exposure < 0:
            borrow_fee = abs(self.exposure) * self.initial_balance * borrow_rate * (1.0 / 60.0)
            self.balance -= borrow_fee
            step_fees_paid += borrow_fee

        # 7. Reward & Step Output
        if done:
            next_spread = current_spread
        else:
            next_spread = float(self.df.iloc[self.current_step]["exchange_premium_spread"])

        unrealized_pnl_curr = self._get_unrealized_pnl(next_spread)
        total_equity_curr = self.balance + unrealized_pnl_curr

        reward = total_equity_curr - total_equity_prev

        if total_equity_curr <= 0:
            done = True
            reward -= 1000.0

        info = {
            "balance": float(self.balance),
            "exposure": float(self.exposure),
            "total_equity": float(total_equity_curr),
            "unrealized_pnl": float(unrealized_pnl_curr),
            "state": int(self.state),
            "action_type": int(action_type),
            "trade_executed": bool(trade_executed),
            "repay_via_transfer": bool(repay_via_transfer),
            "rebalance_triggered": bool(rebalance_triggered),
            "foreign_usd_skew": float(self.foreign_usd_skew),
            "vetoed": bool(vetoed),
            "step_fees_paid": float(step_fees_paid),
            "step_funding_pnl": float(funding_pnl),
        }

        return self._get_obs(), float(reward), done, False, info
