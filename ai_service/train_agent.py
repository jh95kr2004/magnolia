import os
import logging
import pandas as pd
from stable_baselines3 import PPO
from typing import Callable
from stable_baselines3.common.env_util import make_vec_env
from stable_baselines3.common.vec_env import VecNormalize, VecFrameStack
from stable_baselines3.common.callbacks import EvalCallback

from env.arbitrage_env import ArbitrageEnv
from model.rl_agent import TrajectoryStateExtractor

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s"
)
logger = logging.getLogger("TrainRLAgent")


def linear_schedule(initial_value: float) -> Callable[[float], float]:
    """
    Linear learning rate schedule.
    """

    def func(progress_remaining: float) -> float:
        """
        Progress will decrease from 1 (beginning) to 0.
        """
        return max(
            progress_remaining * initial_value, 1e-6
        )  # To avoid exactly 0 LR or clip range

    return func


def train_agent(csv_path: str, total_timesteps: int = 500_000, n_stack: int = 15):
    logger.info(f"Loading historical data from {csv_path} for Gym Environment...")

    try:
        df = pd.read_csv(csv_path)
    except Exception as e:
        logger.error(f"Failed to load dataset: {e}")
        return

    if "time" in df.columns:
        df["time"] = pd.to_datetime(df["time"])
        df = df.sort_values("time").reset_index(drop=True)

    required_cols = [
        "exchange_premium_spread",
        "base_funding_rate",
        "foreign_funding_rate",
    ]
    for col in required_cols:
        if col not in df.columns:
            logger.error(f"Missing required col {col} in dataset.")
            return

    # Injects the 5-hour PremiumForecaster predictions as a new column for RL Environment
    from model.predictor import append_forecast_features
    import torch

    device = "cuda" if torch.cuda.is_available() else "cpu"
    logger.info(
        "Injecting Forecaster predictions into RL historical dataset. This may take a minute..."
    )
    df = append_forecast_features(df, device=device)

    split_idx = int(len(df) * 0.8)
    train_df = df.iloc[:split_idx]
    eval_df = df.iloc[split_idx : min(len(df), split_idx + 2000)]

    # Inject domain randomizations natively into the Train Env pushing robust adaptation logic
    env_kwargs_train = {
        "df": train_df,
        "initial_balance": 10000.0,
        "base_fee_rate": 0.0005,
        "foreign_fee_rate": 0.002,
        "slippage": 0.0005,
        "execution_threshold": 0.10,
        "randomize_fees": False,  # Disable randomization for cleaner threshold learning
        "is_eval": False,
    }

    # Validation uses strict statically bound real-world simulations preventing false positive biases
    env_kwargs_eval = {
        "df": eval_df,
        "initial_balance": 10000.0,
        "base_fee_rate": env_kwargs_train["base_fee_rate"],
        "foreign_fee_rate": env_kwargs_train["foreign_fee_rate"],
        "slippage": env_kwargs_train["slippage"], 
        "execution_threshold": env_kwargs_train["execution_threshold"],
        "randomize_fees": False,
        "is_eval": True,
    }

    train_env = make_vec_env(lambda: ArbitrageEnv(**env_kwargs_train), n_envs=4)
    train_env = VecFrameStack(train_env, n_stack=n_stack)
    train_env = VecNormalize(train_env, norm_obs=True, norm_reward=True, clip_obs=10.0)

    eval_env = make_vec_env(lambda: ArbitrageEnv(**env_kwargs_eval), n_envs=1)
    eval_env = VecFrameStack(eval_env, n_stack=n_stack)
    eval_env = VecNormalize(eval_env, norm_obs=True, norm_reward=False, clip_obs=10.0)

    model = PPO(
        "MlpPolicy",
        train_env,
        verbose=1,
        policy_kwargs=dict(
            features_extractor_class=TrajectoryStateExtractor,
            features_extractor_kwargs=dict(
                features_dim=128,
                n_stack=n_stack,
                scalar_dim=ArbitrageEnv.SCALAR_DIM,
                traj_dim=ArbitrageEnv.TRAJ_DIM,
            ),
            net_arch=dict(pi=[128, 128], vf=[128, 128]),
        ),
        learning_rate=linear_schedule(1e-4),
        clip_range=linear_schedule(0.2),
        n_steps=2048,
        batch_size=512,
        gamma=0.999,  # Expanded temporal vision to look deeply for Forecaster 8-Hour extremas
        ent_coef=0.0,  # CRITICAL: Continuous actions must have 0.0 entropy coeff to allow stddev to anneal!
        max_grad_norm=0.5,
        device="cpu",
    )

    os.makedirs("./model/weights/", exist_ok=True)
    os.makedirs("./logs/", exist_ok=True)

    eval_callback = EvalCallback(
        eval_env,
        best_model_save_path="./model/weights/",
        log_path="./logs/",
        eval_freq=10000,
        n_eval_episodes=1,
        deterministic=True,
        render=False,
    )

    logger.info(f"Starting PPO Agent Training for {total_timesteps} timesteps...")
    model.learn(total_timesteps=total_timesteps, callback=eval_callback)

    save_path = "model/weights/ppo_arbitrage_final.zip"
    model.save(save_path)
    train_env.save("model/weights/vec_normalize.pkl")
    logger.info(f"Training completed. Final model saved to {save_path}")


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(description="Train PPO Arbitrage Agent")
    parser.add_argument("--data", type=str, default="data/historical_dump.csv")
    parser.add_argument("--timesteps", type=int, default=120000)
    parser.add_argument("--n_stack", type=int, default=15)
    args = parser.parse_args()
    train_agent(args.data, total_timesteps=args.timesteps, n_stack=args.n_stack)
