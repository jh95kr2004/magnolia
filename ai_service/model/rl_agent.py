import os
import logging
import gymnasium as gym
import numpy as np
import pandas as pd
import torch
import torch.nn as nn
from stable_baselines3 import PPO
from stable_baselines3.common.torch_layers import BaseFeaturesExtractor
from stable_baselines3.common.vec_env import DummyVecEnv, VecNormalize, VecFrameStack
from env.arbitrage_env import ArbitrageEnv

logger = logging.getLogger("RL_Agent")


class TrajectoryStateExtractor(BaseFeaturesExtractor):
    """
    Two-Branch Feature Extractor for Phase 14 Magnolia PPO Agent.
    Splits each stacked observation (n_stack x 506) into:
      - Branch A (1D-CNN Trajectory Encoder): Encodes the latest 480-minute relative forecast curve
        (batch, 1, 480) into a 64-dim temporal pattern embedding using Conv1d + Dual Max/Avg Pooling.
      - Branch B (State MLP Encoder): Encodes all n_stack frames of the 26 scalar state/inflection features
        (batch, n_stack * 26 = 390) into a 64-dim portfolio & market state embedding.
      - Fusion Layer: Concatenates [Branch A (64), Branch B (64)] -> 128-dim joint representation.
    """

    def __init__(
        self,
        observation_space: gym.spaces.Box,
        features_dim: int = 128,
        n_stack: int = 15,
        scalar_dim: int = ArbitrageEnv.SCALAR_DIM,
        traj_dim: int = ArbitrageEnv.TRAJ_DIM,
    ):
        super().__init__(observation_space, features_dim)
        self.n_stack = n_stack
        self.scalar_dim = scalar_dim
        self.traj_dim = traj_dim
        self.step_dim = scalar_dim + traj_dim

        # Branch B: 15-step history of 26 scalar state & inflection features (390 dims -> 64 dims)
        self.state_mlp = nn.Sequential(
            nn.Linear(n_stack * scalar_dim, 128),
            nn.LayerNorm(128),
            nn.ReLU(),
            nn.Linear(128, 64),
            nn.ReLU(),
        )

        # Branch A: Latest 480-minute relative forecast trajectory (1 x 480 -> 64 dims)
        self.traj_conv = nn.Sequential(
            nn.Conv1d(1, 16, kernel_size=7, stride=2, padding=3),
            nn.ReLU(),
            nn.Conv1d(16, 32, kernel_size=5, stride=2, padding=2),
            nn.ReLU(),
        )
        self.max_pool = nn.AdaptiveMaxPool1d(8)
        self.avg_pool = nn.AdaptiveAvgPool1d(8)
        self.traj_fc = nn.Sequential(
            nn.Linear(32 * 16, 64),
            nn.LayerNorm(64),
            nn.ReLU(),
        )

        # Joint Fusion -> 128 dims
        self.fusion = nn.Sequential(
            nn.Linear(128, features_dim),
            nn.ReLU(),
        )

    def forward(self, observations: torch.Tensor) -> torch.Tensor:
        batch_size = observations.shape[0]
        x = observations.view(batch_size, self.n_stack, self.step_dim)

        state_in = x[:, :, : self.scalar_dim].reshape(
            batch_size, self.n_stack * self.scalar_dim
        )
        traj_in = x[:, -1:, self.scalar_dim :]  # (batch_size, 1, 480)

        state_emb = self.state_mlp(state_in)
        conv_out = self.traj_conv(traj_in)
        pooled = torch.cat(
            [self.max_pool(conv_out), self.avg_pool(conv_out)], dim=-1
        ).view(batch_size, -1)
        traj_emb = self.traj_fc(pooled)

        return self.fusion(torch.cat([state_emb, traj_emb], dim=-1))


class PortfolioRLAgent:
    """
    Wrapper for the trained Two-Branch PPO Portfolio Manager DRL Model.
    Provides inference capability for integration with Python Daemon and Backtester.
    """

    def __init__(
        self,
        model_path: str = "model/weights/ppo_arbitrage_final.zip",
        device="cpu",
        n_stack: int = 15,
    ):
        self.model_path = model_path
        self.model = None
        self.vec_normalize = None
        self.device = device
        self.n_stack = n_stack

        self.load_model()

    def load_model(self):
        if os.path.exists(self.model_path):
            try:
                custom_objects = {
                    "TrajectoryStateExtractor": TrajectoryStateExtractor,
                }
                self.model = PPO.load(
                    self.model_path, device=self.device, custom_objects=custom_objects
                )

                vec_path = os.path.join(
                    os.path.dirname(self.model_path), "vec_normalize.pkl"
                )
                if os.path.exists(vec_path):
                    dummy_df = pd.DataFrame(
                        {
                            "exchange_premium_spread": [0.0, 0.0],
                            "base_funding_rate": [0.0, 0.0],
                            "foreign_funding_rate": [0.0, 0.0],
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
                    f"Successfully loaded Two-Branch DRL Portfolio Manager from {self.model_path}"
                )
            except Exception as e:
                logger.error(f"Failed to load RL model: {e}")
                self.model = None
                self.vec_normalize = None
        else:
            logger.warning(
                f"RL Agent model not found at {self.model_path}. Inference will fallback to trajectory math heuristics."
            )
            self.model = None

    def _ensure_506_stacked_obs(self, stacked_obs: np.ndarray) -> np.ndarray:
        """
        Upgrades legacy 22-dim or 26-dim per-step stacked observations to 506-dim per step
        so all callers work seamlessly with the Two-Branch 506-dim architecture.
        """
        arr = np.asarray(stacked_obs, dtype=np.float32)
        if arr.ndim == 1:
            arr = arr.reshape(1, -1)
        total_len = arr.shape[1]
        expected_len = ArbitrageEnv.OBS_DIM * self.n_stack
        if total_len == expected_len:
            return arr

        step_dim = total_len // self.n_stack
        frames = arr.reshape(arr.shape[0], self.n_stack, step_dim)
        upgraded = np.zeros(
            (arr.shape[0], self.n_stack, ArbitrageEnv.OBS_DIM), dtype=np.float32
        )

        for b in range(arr.shape[0]):
            for s in range(self.n_stack):
                f = frames[b, s]
                spread = float(f[0]) if len(f) > 0 else 0.0
                mean_p = float(f[4]) if len(f) > 4 else spread
                max_p = float(f[5]) if len(f) > 5 else spread + 0.005
                min_p = float(f[6]) if len(f) > 6 else spread - 0.005
                if step_dim == 22:
                    up_e = max_p - spread
                    dn_e = spread - min_p
                    fw_edge = up_e if up_e >= dn_e else -dn_e
                    fw_time = 0.25 if fw_edge >= 0 else 0.50
                    scalar_26 = np.concatenate(
                        [
                            f[:7],
                            np.array([0.25, 0.50, fw_edge, fw_time], dtype=np.float32),
                            f[7:22],
                        ]
                    )
                elif step_dim >= 26:
                    scalar_26 = f[:26]
                    fw_edge = float(scalar_26[9])
                else:
                    scalar_26 = np.pad(f, (0, max(0, 26 - len(f))))[:26]
                    fw_edge = 0.0

                t_grid = np.linspace(
                    1.0 / ArbitrageEnv.TRAJ_DIM,
                    1.0,
                    ArbitrageEnv.TRAJ_DIM,
                    dtype=np.float32,
                )
                traj_480 = (
                    0.5 * fw_edge * np.sin(np.pi * np.clip(t_grid * 2.0, 0.0, 1.0))
                    + 0.5 * (mean_p - spread) * t_grid
                ).astype(np.float32)
                upgraded[b, s, :26] = scalar_26
                upgraded[b, s, 26:] = traj_480

        return upgraded.reshape(arr.shape[0], expected_len)

    def predict(self, stacked_obs: np.ndarray, use_forecaster: bool = True) -> tuple:
        """
        Takes the stacked environment observation `(1, 506 * n_stack)` and returns:
            action_score (float): 0.0 ~ 10.0 (<5.0 = Base Short / Foreign Long [Narrowing/Transfer],
                                  5.0 = Flat/Hold, >5.0 = Base Long / Foreign Short [Widening]).
            target_weight (float): 0.0 ~ 1.0 portfolio allocation weight.
            action_type (int): 0=TRADE, 1=TRANSFER, 2=REPAY/EXIT.
        """
        if stacked_obs is None or len(np.asarray(stacked_obs).flatten()) == 0:
            return 5.0, 0.0, 0

        obs_506 = self._ensure_506_stacked_obs(stacked_obs)
        latest_frame = obs_506[0, -ArbitrageEnv.OBS_DIM :]

        spread = float(latest_frame[0])
        sma_24h = float(latest_frame[2])
        mean_pred = float(latest_frame[4])
        max_pred = float(latest_frame[5])
        min_pred = float(latest_frame[6])
        time_to_max = float(latest_frame[7])
        time_to_min = float(latest_frame[8])
        first_wave_edge = float(latest_frame[9])
        cur_exposure = float(latest_frame[15])
        unrealized_pnl = float(latest_frame[17])
        base_fee = float(latest_frame[18]) if latest_frame[18] > 0 else 0.0005
        foreign_fee = float(latest_frame[19]) if latest_frame[19] > 0 else 0.0020
        slippage = float(latest_frame[20]) if latest_frame[20] > 0 else 0.0005
        state_neutral = float(latest_frame[22])
        state_holding = float(latest_frame[23])
        state_transferring = float(latest_frame[24])
        state_repaying = float(latest_frame[25])

        friction_wall = base_fee + foreign_fee + slippage
        # L2 Liquidity-Aware Position Cap: scale down clip size on thin orderbooks to avoid quadratic L2 impact
        max_liq_weight = float(np.clip(0.0016 / max(slippage, 0.0004), 0.20, 0.85))
        eff_slippage = slippage * (1.0 + ((max_liq_weight * 10000.0) / 5000.0) ** 2)
        eff_friction_wall = base_fee + foreign_fee + eff_slippage

        # Bidirectional edges independent of current spread sign
        widen_edge = max(
            max_pred - spread,
            mean_pred - spread,
            max(0.0, first_wave_edge),
        )
        transfer_edge = spread if spread >= 0.0060 else 0.0
        narrow_edge = max(
            spread - min_pred,
            spread - mean_pred,
            max(0.0, -first_wave_edge),
            transfer_edge,
        )

        if self.model is None:
            # Fallback trajectory heuristic when model weights are not loaded
            best_edge = max(widen_edge, narrow_edge)
            if best_edge <= friction_wall * 1.15:
                return 5.0, 0.0, 0

            if abs(first_wave_edge) > friction_wall * 1.15:
                is_widening = first_wave_edge > 0
                active_edge = max(abs(first_wave_edge), widen_edge if is_widening else narrow_edge)
            else:
                is_widening = widen_edge > narrow_edge
                active_edge = widen_edge if is_widening else narrow_edge

            action_score = 8.0 if is_widening else 2.0
            target_weight = min(0.50, max(0.15, active_edge / 0.05))
            return action_score, float(target_weight), 0

        # Normalize the full 7,590-dim stacked vector if VecNormalize statistics match
        if (
            self.vec_normalize is not None
            and hasattr(self.vec_normalize, "obs_rms")
            and self.vec_normalize.obs_rms.mean.shape[-1] == obs_506.shape[-1]
        ):
            norm_obs = self.vec_normalize.normalize_obs(obs_506)
        else:
            norm_obs = obs_506

        action, _ = self.model.predict(norm_obs, deterministic=True)
        base_target_exposure = float(np.clip(action[0][0], -1.0, 1.0).item())
        action_decision = float(np.clip(action[0][1], -1.0, 1.0).item())

        target_weight = abs(base_target_exposure)
        action_score = (base_target_exposure + 1.0) * 5.0

        if action_decision > 0.66:
            action_type = 3  # STAGE_ASSASSINATION
        elif action_decision > 0.33:
            action_type = 1  # TRANSFER
        elif action_decision < -0.33:
            action_type = 2  # REPAY / EXIT
        else:
            action_type = 0  # TRADE / HOLD

        is_staged_state = state_holding > 0.5 and (0.25 < state_repaying < 0.75)

        # Bidirectional Trajectory & State Machine Execution Logic (No spread sign lock!)
        if state_neutral > 0.5:
            if use_forecaster:
                # Check if Forecaster predicts a massive +2.5%p+ Kimp explosion from a low/near-zero base (Assassination Pre-Positioning)
                if spread <= 0.006 and (max_pred - spread) >= 0.028 and first_wave_edge >= 0.020:
                    action_type = 3  # STAGE_ASSASSINATION
                    edge_weight = min(0.85, max(0.35, (max_pred - spread) / 0.04))
                    target_weight = float(edge_weight)
                    action_score = 8.8
                    return float(action_score), float(target_weight), int(action_type)

                # Determine optimal first-wave or dominant trajectory direction
                if abs(first_wave_edge) > friction_wall * 1.15:
                    is_widening = first_wave_edge > 0
                    active_edge = (
                        max(first_wave_edge, widen_edge)
                        if is_widening
                        else max(-first_wave_edge, narrow_edge)
                    )
                elif (
                    widen_edge > friction_wall * 1.15
                    and narrow_edge > friction_wall * 1.15
                ):
                    if abs(base_target_exposure) >= 0.15:
                        is_widening = base_target_exposure > 0
                    else:
                        is_widening = time_to_max <= time_to_min
                    active_edge = widen_edge if is_widening else narrow_edge
                elif widen_edge > narrow_edge:
                    is_widening = True
                    active_edge = widen_edge
                else:
                    is_widening = False
                    active_edge = narrow_edge

                # Digital Twin Veto Manager: enter whenever expected directional move beats L2-adjusted round-trip friction
                if active_edge > eff_friction_wall * 1.30:
                    action_type = 0
                    edge_weight = min(max_liq_weight, max(0.20, active_edge / 0.018))
                    target_weight = float(
                        np.clip(0.5 * target_weight + 0.5 * edge_weight, 0.20, max_liq_weight)
                    )
                    action_score = 8.5 if is_widening else 1.5
                else:
                    action_type = 0
                    target_weight = 0.0
                    action_score = 5.0
            else:
                # RL-Only Ablation (w/o Forecaster): rely on 24h SMA deviation & PPO output
                sma_edge = abs(spread - sma_24h)
                if sma_edge > eff_friction_wall * 1.30 and abs(spread) >= 0.0065:
                    action_type = 0
                    target_weight = float(
                        np.clip(0.5 * target_weight + 0.35, 0.20, max_liq_weight)
                    )
                    action_score = 1.5 if spread >= 0 else 8.5
                else:
                    action_type = 0
                    target_weight = 0.0
                    action_score = 5.0

        elif is_staged_state:
            # In STAGED_ASSASSINATION: exit immediately when Kimp surges to target or unrealized_pnl >= $150
            if spread >= 0.020 or unrealized_pnl >= 120.0:
                action_type = 2  # Instant 0-latency exit on Base
                target_weight = 0.0
                action_score = 5.0
            else:
                action_type = 3
                target_weight = abs(cur_exposure)
                action_score = 8.5

        elif state_holding > 0.5:
            min_tp_usd = max(
                25.0,
                abs(cur_exposure) * 10000.0 * (base_fee + foreign_fee + eff_slippage) * 0.75 + 15.0,
            )
            if cur_exposure < 0:
                # Holding Short Base / Long Foreign (Narrowing / Transfer position)
                if use_forecaster and spread >= 0.011 and mean_pred >= 0.005 and slippage <= 0.0025:
                    action_type = 1  # TRANSFER (Strategy C)
                    target_weight = abs(cur_exposure)
                    action_score = 1.5
                elif (
                    use_forecaster
                    and first_wave_edge > eff_friction_wall * 1.5
                    and (max_pred - spread) > (spread - min_pred) * 1.6
                    and unrealized_pnl > min_tp_usd
                ):
                    # Two-Wave Flip: Narrowing wave bottomed out and strong Widening wave starting
                    action_type = 0
                    edge_weight = min(max_liq_weight, max(0.20, first_wave_edge / 0.020))
                    target_weight = float(edge_weight)
                    action_score = 8.5
                elif (
                    (spread <= 0.0010 and unrealized_pnl > min_tp_usd)
                    or (use_forecaster and unrealized_pnl > min_tp_usd and spread <= mean_pred)
                ) or (not use_forecaster and spread <= 0.0012 and unrealized_pnl > min_tp_usd):
                    action_type = 2  # REPAY / EXIT
                    target_weight = 0.0
                    action_score = 5.0
                else:
                    action_type = 0
                    target_weight = abs(cur_exposure)
                    action_score = 1.5
            elif cur_exposure > 0:
                # Holding Long Base / Short Foreign (Widening position)
                if (
                    use_forecaster
                    and (-first_wave_edge) > eff_friction_wall * 1.5
                    and (spread - min_pred) > (max_pred - spread) * 1.6
                    and unrealized_pnl > min_tp_usd
                ):
                    # Two-Wave Flip: Widening wave peaked and strong Narrowing reversion wave starting
                    action_type = 0
                    edge_weight = min(max_liq_weight, max(0.20, (-first_wave_edge) / 0.020))
                    target_weight = float(edge_weight)
                    action_score = 1.5
                elif (
                    (spread >= -0.0010 and unrealized_pnl > min_tp_usd)
                    or (use_forecaster and unrealized_pnl > min_tp_usd and (spread >= mean_pred or (max_pred - spread) <= eff_friction_wall * 0.5))
                ) or (not use_forecaster and spread >= -0.0012 and unrealized_pnl > min_tp_usd):
                    action_type = 2  # EXIT
                    target_weight = 0.0
                    action_score = 5.0
                else:
                    action_type = 0
                    target_weight = abs(cur_exposure)
                    action_score = 8.5

        elif state_transferring > 0.5:
            action_type = 1
            target_weight = abs(cur_exposure)
        elif state_repaying > 0.5:
            action_type = 2
            target_weight = 0.0
            action_score = 5.0

        return float(action_score), float(target_weight), int(action_type)
