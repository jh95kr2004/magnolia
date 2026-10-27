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

    def predict(self, stacked_obs: np.ndarray) -> tuple:
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
        mean_pred = float(latest_frame[4])
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

        # Map action_decision to action_type (Discrete 0, 1, 2)
        if action_decision < -0.33:
            action_type = 0  # TRADE
        elif action_decision < 0.33:
            action_type = 1  # TRANSFER
        else:
            action_type = 2  # REPAY

        # Apply non-destructive safety guardrails while respecting PPO decisions in holding state
        if state_neutral > 0.5:
            # In Neutral state, we cannot TRANSFER or REPAY a non-existent position.
            # Evaluate both convergence profit (Strategies A/B/D/E) and instant transfer spread (Strategy C).
            friction_wall = base_fee + foreign_fee + slippage
            reversion_profit = (spread - mean_pred) if spread >= 0 else (mean_pred - spread)
            transfer_profit = max(0.0, spread)
            expected_profit = max(reversion_profit, transfer_profit)

            if expected_profit > friction_wall * 1.2 and abs(spread) >= 0.005:
                action_type = 0  # Entry must be TRADE
                if target_weight < 0.10:
                    target_weight = min(0.50, max(0.15, expected_profit / 0.05))
                if spread >= 0 and action_score >= 5.0:
                    action_score = 2.0  # Short Base / Long Foreign
                elif spread < 0 and action_score <= 5.0:
                    action_score = 8.0  # Long Base / Short Foreign
            else:
                # Veto unprofitable neutral entry
                action_type = 0
                target_weight = 0.0
                action_score = 5.0
        elif state_holding > 0.5:
            # Respect PPO's learned choice among 0 (Hold / Scale-In / Scale-Out), 1 (Transfer), or 2 (Repay/Exit).
            pass
        elif state_transferring > 0.5:
            action_type = 1  # Position locked in on-chain transit
        elif state_repaying > 0.5:
            action_type = 2  # Complete repayment

        return action_score, target_weight, action_type
