import os
import numpy as np
import logging
import pandas as pd
from stable_baselines3 import PPO
from stable_baselines3.common.vec_env import DummyVecEnv, VecNormalize, VecFrameStack
from env.arbitrage_env import ArbitrageEnv

logger = logging.getLogger("RL_Agent")

class PortfolioRLAgent:
    """
    Wrapper for the trained PPO Portfolio Manager DRL Model.
    Provides inference capability for integration with Python Daemon.
    """

    def __init__(
        self, model_path: str = "model/weights/ppo_arbitrage_final.zip", device="cpu", n_stack: int = 15
    ):
        self.model_path = model_path
        self.model = None
        self.device = device
        self.n_stack = n_stack

        self.load_model()

    def load_model(self):
        if os.path.exists(self.model_path):
            try:
                self.model = PPO.load(self.model_path, device=self.device)

                vec_path = os.path.join(
                    os.path.dirname(self.model_path), "vec_normalize.pkl"
                )
                if os.path.exists(vec_path):
                    dummy_df = pd.DataFrame(
                        {
                            "exchange_premium_spread": [0],
                            "base_funding_rate": [0],
                            "foreign_funding_rate": [0],
                        }
                    )
                    env = ArbitrageEnv(df=dummy_df)
                    vec_env = DummyVecEnv([lambda: env])
                    vec_env_stacked = VecFrameStack(vec_env, n_stack=self.n_stack)
                    
                    self.vec_normalize = VecNormalize.load(vec_path, vec_env_stacked)
                    self.vec_normalize.training = False
                    self.vec_normalize.norm_reward = False
                else:
                    self.vec_normalize = None

                logger.info(
                    f"Successfully loaded DRL Portfolio Manager from {self.model_path}"
                )
            except Exception as e:
                logger.error(f"Failed to load RL model: {e}")
                self.model = None
        else:
            logger.warning(
                f"RL Agent model not found at {self.model_path}. Inference will fallback to basic math heuristics."
            )
            self.model = None

    def predict(self, stacked_obs: np.ndarray, use_forecaster: bool = True) -> tuple:
        """
        Takes the env state and returns (action_score_float, target_weight_float, action_type_int).
        State: `(1, 22 * n_stack)` stacked observation array
        Outputs:
            action_score (float): The AI's confidence and direction (0=Short, 5=Flat, 10=Long).
            target_weight (float): 0.0 ~ 1.0 (How much of the total portfolio to allocate to this pair).
            action_type (int): 0=TRADE, 1=TRANSFER, 2=REPAY.
        """
        if self.model is None:
            # Fallback macro heuristic (if weights are not trained yet)
            if len(stacked_obs.flatten()) > 0:
                flat_obs = stacked_obs.flatten()
                feature_dim = len(flat_obs) // self.n_stack
                latest = flat_obs[-feature_dim:]
                spread = float(latest[0])        # Decimal (e.g. 0.03 = 3%)
                pred_premium = float(latest[4])  # Decimal
                base_fee = float(latest[14]) if len(latest) > 14 else 0.0005
                foreign_fee = float(latest[15]) if len(latest) > 15 else 0.002
                slippage = float(latest[16]) if len(latest) > 16 else 0.0005
            else:
                spread, pred_premium = 0.0, 0.0
                base_fee, foreign_fee, slippage = 0.0005, 0.002, 0.0005

            friction_wall = base_fee + foreign_fee + slippage
            reversion_edge = (spread - pred_premium) if spread >= 0 else (pred_premium - spread)
            transfer_edge = max(0.0, spread)
            best_edge = max(reversion_edge, transfer_edge)

            if best_edge <= friction_wall * 1.2:
                return 5.0, 0.0, 0

            action_score = 2.0 if spread >= 0 else 8.0
            # At 5% (0.05) net edge, allocate up to 50% (0.50); at 1% (0.01) edge, allocate 20% (0.20)
            target_weight = min(0.50, max(0.0, best_edge / 0.05))
            return action_score, target_weight, 0

        # Extract raw features from the latest frame before normalization
        latest_frame = stacked_obs[0, -22:]
        spread = float(latest_frame[0])
        sma_24h = float(latest_frame[2])
        mean_pred = float(latest_frame[4])
        max_pred = float(latest_frame[5])
        min_pred = float(latest_frame[6])
        cur_exposure = float(latest_frame[11])
        unrealized_pnl = float(latest_frame[13])
        base_fee = float(latest_frame[14])
        foreign_fee = float(latest_frame[15])
        slippage = float(latest_frame[16])
        state_neutral = float(latest_frame[18])
        state_holding = float(latest_frame[19])
        state_transferring = float(latest_frame[20])
        state_repaying = float(latest_frame[21])

        # Normalize the ENTIRE vector
        if hasattr(self, "vec_normalize") and self.vec_normalize is not None:
            norm_obs = self.vec_normalize.normalize_obs(stacked_obs)
        else:
            norm_obs = stacked_obs

        action, _ = self.model.predict(norm_obs, deterministic=True)
        
        # Parse 2D continuous action outputs
        base_target_exposure = float(np.clip(action[0][0], -1.0, 1.0).item())
        action_decision = float(np.clip(action[0][1], -1.0, 1.0).item())

        # For Continuous models, the magnitude of the exposure directly represents confidence.
        target_weight = abs(base_target_exposure)

        # Action Score dictates direction and priority
        action_score = (base_target_exposure + 1.0) * 5.0 # 0=MegaShort, 5=Flat, 10=MegaLong

        # Map action_decision to action_type (Discrete 0=TRADE, 1=TRANSFER, 2=REPAY)
        # Matching ArbitrageEnv.step(): center [-0.33, +0.33] is TRADE/HOLD
        if action_decision > 0.33:
            action_type = 1  # TRANSFER
        elif action_decision < -0.33:
            action_type = 2  # REPAY / EXIT
        else:
            action_type = 0  # TRADE / HOLD

        friction_wall = base_fee + foreign_fee + slippage

        # Apply Forecaster-guided portfolio guardrails across the 4-stage state machine
        if state_neutral > 0.5:
            if use_forecaster:
                # Full Magnolia AI: combine 8h Forecaster reversion trajectory (A/B/D/E) & Transfer spread (C)
                if spread >= 0:
                    reversion_profit = max(spread - mean_pred, spread - min_pred)
                else:
                    reversion_profit = max(mean_pred - spread, max_pred - spread)
                transfer_profit = max(0.0, spread)
                expected_profit = max(reversion_profit, transfer_profit)

                if expected_profit > friction_wall * 1.3 and abs(spread) >= 0.006:
                    action_type = 0  # Entry must be TRADE
                    edge_weight = min(0.85, max(0.25, expected_profit / 0.018))
                    target_weight = float(np.clip(0.5 * target_weight + 0.5 * edge_weight, 0.25, 0.85))
                    action_score = 1.5 if spread >= 0 else 8.5
                else:
                    action_type = 0
                    target_weight = 0.0
                    action_score = 5.0
            else:
                # RL-Only Ablation (w/o Forecaster): rely on 24h SMA deviation & PPO weight without 8h Forecaster lookahead
                sma_edge = abs(spread - sma_24h)
                if sma_edge > friction_wall * 1.25 and abs(spread) >= 0.0065:
                    action_type = 0
                    target_weight = float(np.clip(0.5 * target_weight + 0.45, 0.30, 0.70))
                    action_score = 1.5 if spread >= 0 else 8.5
                else:
                    action_type = 0
                    target_weight = 0.0
                    action_score = 5.0
        elif state_holding > 0.5:
            if cur_exposure < 0:
                # Holding Short Base / Long Foreign
                # 1) If Forecaster confirms persistent positive spread (>= +1.1%), trigger Strategy C (Transfer Arbitrage)
                if use_forecaster and spread >= 0.011 and mean_pred >= 0.005:
                    action_type = 1  # TRANSFER
                    target_weight = abs(cur_exposure)
                    action_score = 1.5
                # 2) Lock in profit on convergence or Forecaster inflection point
                elif spread <= 0.0010 or (use_forecaster and unrealized_pnl > 30.0 and spread <= mean_pred):
                    action_type = 2  # REPAY / EXIT
                    target_weight = 0.0
                    action_score = 5.0
                elif not use_forecaster and spread <= 0.0012:
                    action_type = 2
                    target_weight = 0.0
                    action_score = 5.0
                else:
                    action_type = 0  # Hold position
                    target_weight = abs(cur_exposure)
                    action_score = 1.5
            elif cur_exposure > 0:
                # Holding Long Base / Short Foreign (entered on negative reverse premium)
                if spread >= -0.0010 or (use_forecaster and unrealized_pnl > 30.0 and spread >= mean_pred):
                    action_type = 2  # EXIT on convergence
                    target_weight = 0.0
                    action_score = 5.0
                elif not use_forecaster and spread >= -0.0012:
                    action_type = 2
                    target_weight = 0.0
                    action_score = 5.0
                else:
                    action_type = 0  # Hold position
                    target_weight = abs(cur_exposure)
                    action_score = 8.5
        elif state_transferring > 0.5:
            action_type = 1  # Position locked in on-chain transit
            target_weight = abs(cur_exposure)
        elif state_repaying > 0.5:
            action_type = 2  # Complete repayment
            target_weight = 0.0
            action_score = 5.0

        return action_score, target_weight, action_type
