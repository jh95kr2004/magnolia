import gymnasium as gym
from gymnasium import spaces
import numpy as np
import pandas as pd


class ArbitrageEnv(gym.Env):
    """
    OpenAI Gym Environment for Magnolia DRL Portfolio Manager.
    Simulates bidirectional target exposure with dynamic 2x2 matrix execution costs
    matching the Go backend heuristics (Digital Twin architecture).
    Observation vector (506 dims): 26 scalar state/inflection features + 480 1-minute forecast trajectory deltas.
    """

    SCALAR_DIM = 26
    TRAJ_DIM = 480
    OBS_DIM = 506

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
        event_sample_ratio=0.50,
    ):
        super(ArbitrageEnv, self).__init__()

        raw_attrs = getattr(df, "attrs", {})
        self.df = df.reset_index(drop=True)

        if "pair_id" in self.df.columns and self.df["pair_id"].nunique() > 1:
            grp = self.df.groupby("pair_id", sort=False)["exchange_premium_spread"]
            self.df["sma_1h"] = grp.transform(lambda s: s.rolling(60, min_periods=1).mean())
            self.df["sma_24h"] = grp.transform(lambda s: s.rolling(1440, min_periods=1).mean())
            self.df["std_24h"] = grp.transform(lambda s: s.rolling(1440, min_periods=1).std()).fillna(0.0005)
            ema_12 = grp.transform(lambda s: s.ewm(span=12, adjust=False).mean())
            ema_26 = grp.transform(lambda s: s.ewm(span=26, adjust=False).mean())
        else:
            self.df["sma_1h"] = (
                self.df["exchange_premium_spread"].rolling(60, min_periods=1).mean()
            )
            self.df["sma_24h"] = (
                self.df["exchange_premium_spread"].rolling(1440, min_periods=1).mean()
            )
            self.df["std_24h"] = (
                self.df["exchange_premium_spread"].rolling(1440, min_periods=1).std()
            )
            ema_12 = self.df["exchange_premium_spread"].ewm(span=12, adjust=False).mean()
            ema_26 = self.df["exchange_premium_spread"].ewm(span=26, adjust=False).mean()

        self.df["z_score"] = (
            self.df["exchange_premium_spread"] - self.df["sma_24h"]
        ) / (self.df["std_24h"] + 1e-8)

        # New Alpha Factors: Bollinger Band Width & MACD
        self.df["bb_upper"] = self.df["sma_24h"] + (2.0 * self.df["std_24h"])
        self.df["bb_lower"] = self.df["sma_24h"] - (2.0 * self.df["std_24h"])
        self.df["bb_width"] = (self.df["bb_upper"] - self.df["bb_lower"]) / (
            self.df["sma_24h"].abs() + 1e-8
        )
        self.df["macd"] = ema_12 - ema_26

        if "predicted_spread" not in self.df.columns:
            self.df["predicted_spread"] = self.df["exchange_premium_spread"]
        if "predicted_spread_max" not in self.df.columns:
            self.df["predicted_spread_max"] = self.df["exchange_premium_spread"] + 0.005
        if "predicted_spread_min" not in self.df.columns:
            self.df["predicted_spread_min"] = self.df["exchange_premium_spread"] - 0.005
        if "time_to_max" not in self.df.columns:
            self.df["time_to_max"] = 0.25
        if "time_to_min" not in self.df.columns:
            self.df["time_to_min"] = 0.50
        if "first_wave_edge" not in self.df.columns:
            up_e = self.df["predicted_spread_max"] - self.df["exchange_premium_spread"]
            dn_e = self.df["exchange_premium_spread"] - self.df["predicted_spread_min"]
            self.df["first_wave_edge"] = np.where(up_e >= dn_e, up_e, -dn_e)
        if "first_wave_time" not in self.df.columns:
            self.df["first_wave_time"] = np.where(
                self.df["first_wave_edge"] >= 0, self.df["time_to_max"], self.df["time_to_min"]
            )

        self.df.fillna(0, inplace=True)

        # Pre-extract contiguous NumPy arrays for 50x faster step() and _get_obs() (no pandas .iloc overhead)
        self._col_spread = self.df["exchange_premium_spread"].values.astype(np.float32)
        self._col_sma_1h = self.df["sma_1h"].values.astype(np.float32)
        self._col_sma_24h = self.df["sma_24h"].values.astype(np.float32)
        self._col_z_score = self.df["z_score"].values.astype(np.float32)
        self._col_pred_mean = self.df["predicted_spread"].values.astype(np.float32)
        self._col_pred_max = self.df["predicted_spread_max"].values.astype(np.float32)
        self._col_pred_min = self.df["predicted_spread_min"].values.astype(np.float32)
        self._col_t_max = self.df["time_to_max"].values.astype(np.float32)
        self._col_t_min = self.df["time_to_min"].values.astype(np.float32)
        self._col_fw_edge = self.df["first_wave_edge"].values.astype(np.float32)
        self._col_fw_time = self.df["first_wave_time"].values.astype(np.float32)
        self._col_base_fund = self.df["base_funding_rate"].values.astype(np.float32)
        self._col_for_fund = self.df["foreign_funding_rate"].values.astype(np.float32)
        self._col_bb_width = self.df["bb_width"].values.astype(np.float32)
        self._col_macd = self.df["macd"].values.astype(np.float32)
        if "borrow_rate_pct" in self.df.columns:
            self._col_borrow = self.df["borrow_rate_pct"].values.astype(np.float32)
        else:
            self._col_borrow = np.full(len(self.df), 0.0041, dtype=np.float32)

        if "tether_real_premium_spread" in self.df.columns:
            self._col_real_spread = self.df["tether_real_premium_spread"].values.astype(np.float32)
        else:
            self._col_real_spread = self._col_spread.copy()

        self._col_event_listing = (
            self.df["event_new_listing"].values.astype(np.float32)
            if "event_new_listing" in self.df.columns
            else np.zeros(len(self.df), dtype=np.float32)
        )
        self._col_event_warning = (
            self.df["event_warning"].values.astype(np.float32)
            if "event_warning" in self.df.columns
            else np.zeros(len(self.df), dtype=np.float32)
        )
        self._col_event_delisting = (
            self.df["event_delisting"].values.astype(np.float32)
            if "event_delisting" in self.df.columns
            else np.zeros(len(self.df), dtype=np.float32)
        )
        self._col_event_suspension = (
            self.df["event_wallet_suspension"].values.astype(np.float32)
            if "event_wallet_suspension" in self.df.columns
            else np.zeros(len(self.df), dtype=np.float32)
        )
        self._col_vasp_tier = (
            self.df["vasp_tier"].values.astype(np.int32)
            if "vasp_tier" in self.df.columns
            else np.ones(len(self.df), dtype=np.int32)
        )
        self._col_slippage = (
            self.df["slippage"].values.astype(np.float32)
            if "slippage" in self.df.columns
            else np.full(len(self.df), slippage, dtype=np.float32)
        )
        self._col_transfer_delay = (
            self.df["transfer_delay_mins"].values.astype(np.int32)
            if "transfer_delay_mins" in self.df.columns
            else np.where(self._col_vasp_tier == 3, 14, 8).astype(np.int32)
        )
        self._col_gas_fee = (
            self.df["network_gas_fee_usd"].values.astype(np.float32)
            if "network_gas_fee_usd" in self.df.columns
            else np.where(self._col_vasp_tier == 3, 5.0, 2.5).astype(np.float32)
        )
        # Terminal delisting flag: strictly env-internal at final candle t == t_end, NEVER in _get_obs()!
        self._col_is_terminated = (
            self.df["is_terminated"].values.astype(np.float32)
            if "is_terminated" in self.df.columns
            else np.zeros(len(self.df), dtype=np.float32)
        )
        self._col_pair_id = (
            self.df["pair_id"].values
            if "pair_id" in self.df.columns
            else np.full(len(self.df), "DEFAULT")
        )
        self.event_sample_ratio = float(event_sample_ratio)
        ev_sum = (
            self._col_event_listing
            + self._col_event_warning
            + self._col_event_delisting
            + self._col_event_suspension
        )
        self._event_start_candidates = np.where(ev_sum > 0.05)[0]

        self._static_market_features = np.column_stack(
            [
                self._col_spread,
                self._col_sma_1h,
                self._col_sma_24h,
                self._col_z_score,
                self._col_pred_mean,
                self._col_pred_max,
                self._col_pred_min,
                self._col_t_max,
                self._col_t_min,
                self._col_fw_edge,
                self._col_fw_time,
                self._col_base_fund,
                self._col_for_fund,
                self._col_bb_width,
                self._col_macd,
            ]
        ).astype(np.float32)

        self._t_grid = np.linspace(
            1.0 / self.TRAJ_DIM, 1.0, self.TRAJ_DIM, dtype=np.float32
        )
        self._wave_sin = np.sin(np.pi * np.clip(self._t_grid * 2.0, 0.0, 1.0)).astype(
            np.float32
        )
        self.traj_deltas = None
        if "full_traj_deltas" in raw_attrs and len(self.df) <= 5000:
            ftd = raw_attrs["full_traj_deltas"]
            if len(ftd) == len(self.df):
                self.traj_deltas = ftd
            elif "_row_id" in self.df.columns:
                row_ids = self.df["_row_id"].values.astype(np.int64)
                self.traj_deltas = np.asarray(ftd[row_ids], dtype=np.float32)

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
        self.STATE_STAGED = 4  # STAGED_ASSASSINATION / PRE_STAGED_LISTING

        self.state = self.STATE_NEUTRAL
        self.transfer_remaining_steps = 0
        self.staged_hold_steps = 0
        self.max_staged_hold_steps = 720  # 12 hours max stage window before in-place unwind
        self.staged_entry_basis = 0.0
        self.staged_trigger_type = ""
        self.in_transit_hedge_mode = "BASE_BORROW"  # or "FOREIGN_HEDGE" (3-Stage Fallback)

        # Action Space: index 0 is exposure [-1.0, 1.0], index 1 is action_decision [-1.0, 1.0]
        self.action_space = spaces.Box(
            low=np.array([-1.0, -1.0], dtype=np.float32),
            high=np.array([1.0, 1.0], dtype=np.float32),
            dtype=np.float32
        )

        # State Space: 506 features (26 scalar state/inflection features + 480 1-minute forecast trajectory deltas)
        self.observation_space = spaces.Box(
            low=-np.inf, high=np.inf, shape=(self.OBS_DIM,), dtype=np.float32
        )

    def reset(self, seed=None, options=None):
        super().reset(seed=seed)
        if self.is_eval:
            self.current_step = 0
            self.max_steps = len(self.df) - 1
        else:
            if len(self.df) > 1000:
                if (
                    len(self._event_start_candidates) > 0
                    and np.random.rand() < self.event_sample_ratio
                ):
                    cand = int(np.random.choice(self._event_start_candidates))
                    self.current_step = max(0, min(cand - 10, len(self.df) - 1000))
                else:
                    self.current_step = np.random.randint(0, len(self.df) - 1000)
                self.max_steps = min(len(self.df) - 1, self.current_step + 1000)
            else:
                self.current_step = 0
                self.max_steps = len(self.df) - 1

        self.balance = self.initial_balance
        self.exposure = 0.0  # From -1.0 to 1.0
        self.entry_spread = 0.0
        self.state = self.STATE_NEUTRAL
        self.transfer_remaining_steps = 0
        self.staged_hold_steps = 0
        self.staged_entry_basis = 0.0
        self.staged_trigger_type = ""
        self.in_transit_hedge_mode = "BASE_BORROW"
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
            self.current_slippage = float(self._col_slippage[self.current_step])

        return self._get_obs(), {}

    def _get_obs(self):
        idx = self.current_step
        obs = np.empty(self.OBS_DIM, dtype=np.float32)
        obs[:15] = self._static_market_features[idx]
        obs[15] = self.exposure
        obs[16] = self.balance
        obs[17] = self._get_unrealized_pnl(float(self._col_spread[idx]))
        obs[18] = self.current_base_fee_rate
        obs[19] = self.current_foreign_fee_rate
        obs[20] = self.current_slippage
        obs[21] = (self.current_base_fee_rate + self.current_foreign_fee_rate) + self.current_slippage
        obs[22] = 1.0 if self.state == self.STATE_NEUTRAL else 0.0
        obs[23] = 1.0 if self.state in (self.STATE_HOLDING_LEG, self.STATE_STAGED) else 0.0
        obs[24] = 1.0 if self.state == self.STATE_TRANSFERRING else 0.0
        obs[25] = (
            1.0
            if self.state == self.STATE_REPAYING
            else (0.5 if self.state == self.STATE_STAGED else 0.0)
        )
        if self.traj_deltas is not None:
            obs[26:] = self.traj_deltas[idx]
        else:
            mean_d = float(self._col_pred_mean[idx] - self._col_spread[idx])
            fw_d = float(self._col_fw_edge[idx])
            obs[26:] = (0.5 * fw_d) * self._wave_sin + (0.5 * mean_d) * self._t_grid
        return obs

    def _get_unrealized_pnl(self, current_spread):
        if self.exposure == 0:
            return 0.0
        # In STAGED_ASSASSINATION (Spot on Base + 1x Short on Foreign), profit when current_spread rises above staged_entry_basis
        if self.state == self.STATE_STAGED:
            return abs(self.exposure) * self.initial_balance * (current_spread - self.staged_entry_basis)
        # In Transfer Arbitrage (TRANSFERRING or REPAYING state with Short Base / Long Foreign),
        # if hedged via Base coin loan, the coin bought on Foreign repays the Base loan 1:1, locking in entry_spread.
        # If hedged via Foreign Futures 1x Short (3-Stage Fallback), PnL tracks current_spread until sold on Base.
        if self.state in (self.STATE_TRANSFERRING, self.STATE_REPAYING) and self.exposure < 0:
            if self.in_transit_hedge_mode == "FOREIGN_HEDGE":
                return abs(self.exposure) * self.initial_balance * current_spread
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
        if action_dec > 0.66 and action_exposure > 0:
            action_type = 3  # STAGE_ASSASSINATION (Pre-position Spot on Base + 1x Short on Foreign)
        elif action_dec > 0.33:
            action_type = 1  # TRANSFER (Strategy C)
        elif action_dec < -0.33:
            action_type = 2  # REPAY / EXIT
        else:
            action_type = 0  # TRADE / HOLD

        idx = self.current_step
        current_spread = float(self._col_spread[idx])
        real_spread = float(self._col_real_spread[idx])
        base_funding = float(self._col_base_fund[idx])
        foreign_funding = float(self._col_for_fund[idx])
        ev_listing = float(self._col_event_listing[idx]) > 0.5
        ev_warning = float(self._col_event_warning[idx]) > 0.5
        ev_delisting = float(self._col_event_delisting[idx]) > 0.5
        ev_suspension = float(self._col_event_suspension[idx]) > 0.5
        vasp_tier = int(self._col_vasp_tier[idx])
        if not self.randomize_fees:
            self.current_slippage = float(self._col_slippage[idx])
        step_gas_fee = float(self._col_gas_fee[idx])
        step_transfer_delay = int(self._col_transfer_delay[idx])

        # 2. Calculate Previous Equity
        unrealized_pnl_prev = self._get_unrealized_pnl(current_spread)
        total_equity_prev = self.balance + unrealized_pnl_prev

        # 3. Dynamic Cost Variables
        borrow_rate_pct = float(self._col_borrow[idx])  # Default roughly 0.0041% hourly
        borrow_rate = borrow_rate_pct / 100.0

        prev_state = self.state
        step_fees_paid = 0.0
        repay_via_transfer = False
        rebalance_triggered = False
        assassination_sniped = False
        staged_timeout_unwind = False
        in_transit_defense_triggered = False
        delisting_veto_or_exit = False

        # 3b. Dual-Trigger 100% Delta-Neutral Reverse Transfer Rebalancing (GEMINI.md Section 8.3)
        # Triggered when in NEUTRAL state, Foreign USD Skew <= 20% (RebalanceTriggerSkew), and spread <= RebalanceMaxPremium
        if (
            self.state == self.STATE_NEUTRAL
            and self.foreign_usd_skew <= self.rebalance_trigger_skew
            and current_spread <= self.rebalance_max_premium
            and not ev_delisting
        ):
            rebalance_amount = 0.50 - self.foreign_usd_skew
            rebalance_notional = rebalance_amount * self.initial_balance
            rebalance_fee = rebalance_notional * (self.current_base_fee_rate + self.current_foreign_fee_rate) + step_gas_fee
            # Reverse premium bonus/cost when buying Spot Base at negative/near-zero spread and shorting Foreign
            rebalance_spread_pnl = rebalance_notional * (-current_spread)
            self.balance = self.balance - rebalance_fee + rebalance_spread_pnl
            step_fees_paid += rebalance_fee
            self.foreign_usd_skew = 0.50
            rebalance_triggered = True

        # 4. State Machine Transition Logic
        target_exposure = self.exposure
        round_trip_breakeven = (self.current_base_fee_rate + self.current_foreign_fee_rate) + self.current_slippage

        # Hard Safety Guardrail: DELISTING announcement blocks new entries and forces immediate exit
        if ev_delisting:
            delisting_veto_or_exit = True
            if self.state == self.STATE_NEUTRAL:
                target_exposure = 0.0
            elif self.state in (self.STATE_HOLDING_LEG, self.STATE_STAGED, self.STATE_REPAYING):
                target_exposure = 0.0
                self.state = self.STATE_NEUTRAL
                self.staged_hold_steps = 0
                action_type = 2

        elif self.state == self.STATE_TRANSFERRING:
            # Undergoing transfer - lock exposure and ignore user actions
            target_exposure = self.exposure
            self.transfer_remaining_steps -= 1
            if self.transfer_remaining_steps <= 0:
                # Check Post-Arrival Kimp Drop Defense (Ch 05 Section 5)
                if (
                    self.in_transit_hedge_mode == "FOREIGN_HEDGE"
                    and real_spread < round_trip_breakeven
                ):
                    # Do NOT market-sell at a loss upon arrival! Auto-transition to STAGED_ASSASSINATION
                    self.state = self.STATE_STAGED
                    self.staged_hold_steps = 0
                    self.staged_entry_basis = 0.0  # Foreign Spot + Foreign 1x Short locked 0.0 basis
                    self.staged_trigger_type = "IN_TRANSIT_KIMP_DROP_DEFENSE"
                    in_transit_defense_triggered = True
                else:
                    self.state = self.STATE_REPAYING  # Transfer completed, awaiting repayment/execution

        elif self.state == self.STATE_STAGED:
            self.staged_hold_steps += 1
            if self.staged_trigger_type == "IN_TRANSIT_KIMP_DROP_DEFENSE":
                target_exit_spread = max(self.staged_entry_basis + 0.015, round_trip_breakeven * 1.5, 0.018)
            else:
                target_exit_spread = max(self.staged_entry_basis + 0.032, round_trip_breakeven * 2.0, 0.035)
            if current_spread >= target_exit_spread or (
                action_type == 2 and current_spread > self.staged_entry_basis + round_trip_breakeven
            ):
                # Instant 0-latency assassination exit on Base (Base Spot Sell + Foreign Short Cover)
                target_exposure = 0.0
                self.state = self.STATE_NEUTRAL
                self.staged_hold_steps = 0
                assassination_sniped = True
            elif self.staged_hold_steps >= self.max_staged_hold_steps or action_type == 2:
                # Timeout or manual exit: simultaneous in-place unwind (Base Spot Sell + Foreign Short Cover)
                # Zero reverse blockchain gas fee!
                target_exposure = 0.0
                self.state = self.STATE_NEUTRAL
                self.staged_hold_steps = 0
                staged_timeout_unwind = True
            else:
                target_exposure = self.exposure

        elif self.state == self.STATE_NEUTRAL:
            # Check if staging an Assassination pre-position (Action 3 or causal Listing/Warning/Suspension catalyst)
            can_stage_catalyst = (
                action_type == 3
                or (
                    (ev_listing or ev_warning or ev_suspension)
                    and current_spread <= 0.008
                    and float(self._col_pred_max[idx]) - current_spread >= 0.020
                    and abs(action_exposure) >= self.execution_threshold
                )
            )
            if can_stage_catalyst and self.foreign_usd_skew >= self.balance_skew_threshold:
                target_exposure = abs(action_exposure) if abs(action_exposure) >= self.execution_threshold else 0.50
                self.state = self.STATE_STAGED
                self.staged_hold_steps = 0
                self.staged_entry_basis = current_spread
                self.staged_trigger_type = "LLM_EVENT_PRE_POSITION" if (ev_listing or ev_warning or ev_suspension) else "AI_SURGE_FORECAST"
                gas_fee = step_gas_fee
                self.balance -= gas_fee
                step_fees_paid += gas_fee
                self.foreign_usd_skew = max(0.05, self.foreign_usd_skew - abs(target_exposure) * 0.15)
            elif abs(action_exposure) >= self.execution_threshold:
                target_exposure = action_exposure
                self.state = self.STATE_HOLDING_LEG
            else:
                target_exposure = 0.0

        elif self.state == self.STATE_HOLDING_LEG:
            if action_type == 3 and self.foreign_usd_skew >= self.balance_skew_threshold:
                # Transition existing hedge into STAGED_ASSASSINATION by pre-depositing to Base
                target_exposure = abs(self.exposure) if abs(self.exposure) >= self.execution_threshold else 0.50
                self.state = self.STATE_STAGED
                self.staged_hold_steps = 0
                self.staged_entry_basis = current_spread
                self.staged_trigger_type = "LLM_EVENT_PRE_POSITION"
                gas_fee = step_gas_fee
                self.balance -= gas_fee
                step_fees_paid += gas_fee
                self.foreign_usd_skew = max(0.05, self.foreign_usd_skew - abs(target_exposure) * 0.15)
            elif action_type == 1:  # TRANSFER (Only valid when Short Base / Long Foreign, wallet not suspended, real_spread >= 0.5%, and Foreign USD Skew >= 15%)
                if (
                    self.exposure < 0
                    and not ev_suspension
                    and real_spread >= 0.005
                    and self.foreign_usd_skew >= self.balance_skew_threshold
                ):
                    self.state = self.STATE_TRANSFERRING
                    self.transfer_remaining_steps = step_transfer_delay if self.is_eval else max(2, int(step_transfer_delay * np.random.uniform(0.8, 1.25)))
                    gas_fee = step_gas_fee
                    self.balance -= gas_fee
                    step_fees_paid += gas_fee
                    # Moving coin from Foreign to Base consumes Foreign USD allocation
                    self.foreign_usd_skew = max(0.05, self.foreign_usd_skew - abs(self.exposure) * 0.15)
                    target_exposure = self.exposure
                elif abs(action_exposure) < self.execution_threshold:
                    self.state = self.STATE_NEUTRAL
                    target_exposure = 0.0
                else:
                    target_exposure = self.exposure if np.sign(action_exposure) == np.sign(self.exposure) else action_exposure
            elif action_type == 2 or abs(action_exposure) < self.execution_threshold:  # REPAY / Market Unwind
                target_exposure = 0.0
                self.state = self.STATE_NEUTRAL
            else:  # TRADE (Hold existing leg unless flipping direction to prevent fee churning)
                if self.exposure != 0.0 and np.sign(action_exposure) == np.sign(self.exposure):
                    target_exposure = self.exposure
                else:
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

        vetoed = bool(ev_delisting and prev_state == self.STATE_NEUTRAL and abs(action_exposure) >= self.execution_threshold)
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

            if target_exposure > 0 or self.state == self.STATE_STAGED:
                # Long Base / Short Foreign or Pre-Staged Assassination: profits when spread widens
                forecast_widening = float(self._col_pred_max[idx]) - current_spread
                first_wave_up = max(0.0, float(self._col_fw_edge[idx]))
                mean_reversion_up = float(self._col_sma_24h[idx]) - current_spread
                catalyst_bonus = 0.030 if (ev_listing or ev_warning or ev_suspension) else 0.0
                predicted_jump = max(forecast_widening, first_wave_up, mean_reversion_up * 0.6, catalyst_bonus)
            else:
                # Short Base / Long Foreign: profits from spread narrowing OR instant transfer spread
                convergence_jump = current_spread - float(self._col_pred_min[idx])
                first_wave_dn = max(0.0, -float(self._col_fw_edge[idx]))
                mean_reversion_down = current_spread - float(self._col_sma_24h[idx])
                if ev_warning or ev_suspension:
                    # During Gaduri Warning / Wallet Suspension, deposits are blocked and upward squeeze risk is high;
                    # only allow Short Base entry when Forecaster explicitly predicts a strong downward reversion wave
                    predicted_jump = first_wave_dn if float(self._col_fw_edge[idx]) < -0.005 else 0.0
                else:
                    transfer_spread = max(0.0, real_spread)
                    predicted_jump = max(convergence_jump, first_wave_dn, mean_reversion_down * 0.6, transfer_spread)

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
                    if prev_state == self.STATE_STAGED:
                        # STAGED_ASSASSINATION exit (sniped surge or in-place simultaneous unwind):
                        # captures (current_spread - staged_entry_basis) with ZERO reverse blockchain gas fee
                        realized_pnl = abs(closed_exposure) * self.initial_balance * (current_spread - self.staged_entry_basis)
                    elif repay_via_transfer and closed_exposure < 0:
                        # Strategy C (Transfer Arbitrage): locked in entry_spread directly (or current_spread if foreign hedged)
                        if self.in_transit_hedge_mode == "FOREIGN_HEDGE":
                            realized_pnl = abs(closed_exposure) * self.initial_balance * current_spread
                        else:
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

        # 6b. Terminal Delisting Settlement (strictly at t == t_end, zero look-ahead) & Episode Boundary Settlement
        if float(self._col_is_terminated[idx]) > 0.5 and self.exposure != 0.0:
            # Forced liquidation penalty if agent ignored delisting notice and held until market termination
            delist_haircut = abs(self.exposure) * self.initial_balance * 0.05
            self.balance += self._get_unrealized_pnl(current_spread) - delist_haircut
            step_fees_paid += delist_haircut
            self.exposure = 0.0
            self.entry_spread = 0.0
            self.state = self.STATE_NEUTRAL
            self.staged_hold_steps = 0
        elif not done and self._col_pair_id[self.current_step] != self._col_pair_id[idx]:
            # Cleanly settle open position at end of current (exchange_pair, coin) episode before next episode starts
            if self.exposure != 0.0:
                self.balance += self._get_unrealized_pnl(current_spread)
                self.exposure = 0.0
                self.entry_spread = 0.0
            self.state = self.STATE_NEUTRAL
            self.transfer_remaining_steps = 0
            self.staged_hold_steps = 0
            self.foreign_usd_skew = 0.50

        # Apply continuous funding fee yield accruals
        accrual_factor = 1.0 / (8 * 60)
        if self.exposure > 0 or self.state == self.STATE_STAGED:
            funding_pnl = self.initial_balance * abs(self.exposure) * (foreign_funding_dec - base_funding_dec) * accrual_factor
        elif self.exposure < 0:
            funding_pnl = self.initial_balance * abs(self.exposure) * (base_funding_dec - foreign_funding_dec) * accrual_factor
        else:
            funding_pnl = 0.0
        self.balance += funding_pnl

        # Base Borrow interest fee hourly accrual applied per step
        if self.exposure < 0 and self.state != self.STATE_STAGED:
            borrow_fee = abs(self.exposure) * self.initial_balance * borrow_rate * (1.0 / 60.0)
            self.balance -= borrow_fee
            step_fees_paid += borrow_fee

        # 7. Reward & Step Output
        if done or self._col_pair_id[self.current_step] != self._col_pair_id[idx]:
            next_spread = current_spread
        else:
            next_spread = float(self._col_spread[self.current_step])

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
            "assassination_sniped": bool(assassination_sniped),
            "staged_timeout_unwind": bool(staged_timeout_unwind),
            "in_transit_defense_triggered": bool(in_transit_defense_triggered),
            "delisting_veto_or_exit": bool(delisting_veto_or_exit),
            "staged_trigger_type": str(self.staged_trigger_type),
            "staged_hold_steps": int(self.staged_hold_steps),
            "foreign_usd_skew": float(self.foreign_usd_skew),
            "vetoed": bool(vetoed),
            "step_fees_paid": float(step_fees_paid),
            "step_funding_pnl": float(funding_pnl),
        }

        return self._get_obs(), float(reward), done, False, info
