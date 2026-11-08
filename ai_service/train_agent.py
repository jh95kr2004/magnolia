import os
import json
import shutil
import logging
import numpy as np
import pandas as pd
import torch
from typing import Callable
from stable_baselines3 import PPO
from stable_baselines3.common.env_util import make_vec_env
from stable_baselines3.common.vec_env import DummyVecEnv, VecNormalize, VecFrameStack
from stable_baselines3.common.callbacks import EvalCallback

from env.arbitrage_env import ArbitrageEnv
from model.predictor import append_forecast_features
from model.rl_agent import TrajectoryStateExtractor, PortfolioRLAgent

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s"
)
logger = logging.getLogger("TrainRLAgent")


def linear_schedule(initial_value: float) -> Callable[[float], float]:
    def func(progress_remaining: float) -> float:
        return max(progress_remaining * initial_value, 1e-6)

    return func


def _slice_df_preserving_attrs(df: pd.DataFrame, mask_or_idx) -> pd.DataFrame:
    saved_traj = df.attrs.get("full_traj_deltas")
    df.attrs = {}
    try:
        if isinstance(mask_or_idx, (pd.Series, np.ndarray)) and mask_or_idx.dtype == bool:
            sub = df[mask_or_idx].copy().reset_index(drop=True)
        else:
            sub = df.iloc[mask_or_idx].copy().reset_index(drop=True)
    finally:
        if saved_traj is not None:
            df.attrs["full_traj_deltas"] = saved_traj
    if saved_traj is not None and "_row_id" in sub.columns:
        rids = sub["_row_id"].values.astype(np.int64)
        sub["_row_id"] = np.arange(len(sub), dtype=np.int64)
        sub.attrs["full_traj_deltas"] = np.asarray(saved_traj[rids], dtype=np.float32)
    return sub


def _build_balanced_val_slice(val_df: pd.DataFrame, max_rows_per_regime: int = 1600) -> pd.DataFrame:
    """
    Constructs a stratified validation slice containing held-out episodes across all 5 regimes
    (`CONTINUOUS`, `NEW_LISTING`, `INVESTMENT_WARNING`, `WALLET_SUSPENSION`, `DELISTED`)
    while preserving `full_traj_deltas` indexing (`_row_id`).
    """
    if "regime" not in val_df.columns:
        return _slice_df_preserving_attrs(val_df, slice(0, min(len(val_df), 4000)))

    saved_traj = val_df.attrs.get("full_traj_deltas")
    val_df.attrs = {}
    parts = []
    try:
        for reg in [
            "CONTINUOUS",
            "NEW_LISTING",
            "INVESTMENT_WARNING",
            "WALLET_SUSPENSION",
            "DELISTED",
        ]:
            sub = val_df[val_df["regime"] == reg]
            if len(sub) > 0:
                part = sub.iloc[: min(len(sub), max_rows_per_regime)].copy()
                part.attrs = {}
                parts.append(part)
    finally:
        if saved_traj is not None:
            val_df.attrs["full_traj_deltas"] = saved_traj

    if not parts:
        return _slice_df_preserving_attrs(val_df, slice(0, min(len(val_df), 4000)))

    combined = pd.concat(parts, ignore_index=True)
    if saved_traj is not None and "_row_id" in combined.columns:
        rids = combined["_row_id"].values.astype(np.int64)
        combined["_row_id"] = np.arange(len(combined), dtype=np.int64)
        combined.attrs["full_traj_deltas"] = np.asarray(saved_traj[rids], dtype=np.float32)
    return combined


def _quick_eval_candidate(
    eval_df: pd.DataFrame, model_path: str, n_stack: int = 15
) -> dict:
    raw_env = ArbitrageEnv(
        df=eval_df,
        initial_balance=10000.0,
        base_fee_rate=0.0005,
        foreign_fee_rate=0.002,
        slippage=0.0005,
        execution_threshold=0.10,
        randomize_fees=False,
        is_eval=True,
    )
    vec_env = DummyVecEnv([lambda: raw_env])
    vec_env = VecFrameStack(vec_env, n_stack=n_stack)
    agent = PortfolioRLAgent(model_path=model_path, device="cpu", n_stack=n_stack)

    obs = vec_env.reset()
    done = False
    cap_hist = [10000.0]
    while not done:
        step_idx = min(raw_env.current_step, len(raw_env._col_real_spread) - 1)
        cur_real_spread = float(raw_env._col_real_spread[step_idx])
        cur_wd_fee = float(raw_env._col_gas_fee[step_idx]) / max(raw_env.initial_balance * 0.25, 500.0)
        action_score, target_weight, action_type = agent.predict(
            obs,
            use_forecaster=True,
            real_spread=cur_real_spread,
            withdraw_fee_rate=cur_wd_fee,
            foreign_share=float(raw_env.foreign_usd_skew),
        )
        signed_exp = (
            -float(target_weight)
            if action_score < 5.0
            else (float(target_weight) if action_score > 5.0 else 0.0)
        )
        act_dec = (
            0.90
            if action_type == 3
            else (0.50 if action_type == 1 else (-0.80 if action_type == 2 else 0.0))
        )
        action = np.array([[signed_exp, act_dec]], dtype=np.float32)
        obs, _, done_arr, info_arr = vec_env.step(action)
        done = bool(done_arr[0])
        cap_hist.append(float(info_arr[0].get("total_equity", 10000.0)))

    eq_arr = np.asarray(cap_hist, dtype=np.float64)
    roi_pct = ((eq_arr[-1] - 10000.0) / 10000.0) * 100.0
    peaks = np.maximum.accumulate(eq_arr)
    max_dd_pct = float(np.max((peaks - eq_arr) / np.maximum(peaks, 1e-8)) * 100.0)
    rets = pd.Series(eq_arr).pct_change().dropna()
    ann = 365.0 * 24.0 * 60.0
    sharpe = (
        float((rets.mean() * ann) / (rets.std() * np.sqrt(ann)))
        if len(rets) > 1 and rets.std() > 1e-12
        else 0.0
    )
    score = roi_pct + 0.35 * sharpe - 1.5 * max_dd_pct
    return {
        "net_roi_pct": round(float(roi_pct), 3),
        "max_drawdown_pct": round(float(max_dd_pct), 3),
        "sharpe_ratio": round(float(sharpe), 3),
        "selection_score": round(float(score), 3),
    }


def train_agent(
    csv_path: str = "data/historical_dump.csv",
    total_timesteps: int = 120_000,
    n_stack: int = 15,
    sample_ratios: list = None,
):
    if sample_ratios is None:
        sample_ratios = [0.10, 0.30, 0.50]

    logger.info(f"Loading historical data from {csv_path} for Multi-Coin Gym Environment...")
    try:
        df = pd.read_csv(csv_path)
    except Exception as e:
        logger.error(f"Failed to load dataset: {e}")
        return

    if "pair_id" in df.columns and "ts_utc" in df.columns:
        df = df.sort_values(["pair_id", "ts_utc"]).reset_index(drop=True)
    elif "pair_id" in df.columns and "time" in df.columns:
        df["time"] = pd.to_datetime(df["time"])
        df = df.sort_values(["pair_id", "time"]).reset_index(drop=True)
    elif "time" in df.columns:
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

    device_fc = "cuda:0" if torch.cuda.is_available() else "cpu"
    rl_device = "cuda:1" if torch.cuda.device_count() > 1 else device_fc
    logger.info(
        f"Injecting 12-Feature Forecaster predictions on {device_fc} (RL device: {rl_device})..."
    )
    df = append_forecast_features(df, device=device_fc)

    if "split_group" in df.columns and (df["split_group"] == "val").any():
        train_df = _slice_df_preserving_attrs(df, df["split_group"] == "train")
        val_full_df = _slice_df_preserving_attrs(df, df["split_group"] == "val")
    else:
        split_idx = int(len(df) * 0.8)
        train_df = _slice_df_preserving_attrs(df, slice(0, split_idx))
        val_full_df = _slice_df_preserving_attrs(df, slice(split_idx, len(df)))

    eval_df = _build_balanced_val_slice(val_full_df, max_rows_per_regime=800)
    logger.info(
        f"Prepared Multi-Coin RL Split: Train={len(train_df):,} steps | Eval Slice={len(eval_df):,} steps"
    )

    os.makedirs("./model/weights/", exist_ok=True)
    os.makedirs("./logs/", exist_ok=True)
    final_save_path = "model/weights/ppo_arbitrage_final.zip"
    final_vec_path = "model/weights/vec_normalize.pkl"
    report_path = "model/weights/eval_report.json"

    best_score = -float("inf")
    best_ratio_label = None
    ratio_results = []

    for ratio in sample_ratios:
        norm_pct = int(round((1.0 - ratio) * 100))
        ev_pct = int(round(ratio * 100))
        ratio_label = f"{norm_pct}:{ev_pct}"
        cand_path = f"model/weights/ppo_cand_{norm_pct}_{ev_pct}.zip"
        cand_vec = f"model/weights/vec_cand_{norm_pct}_{ev_pct}.pkl"

        logger.info("=" * 76)
        logger.info(
            f"[RL Experiment] Training PPO Agent with Normal:Event Ratio = {ratio_label} "
            f"(event_sample_ratio={ratio:.2f}, timesteps={total_timesteps:,})"
        )
        logger.info("=" * 76)

        env_kwargs_train = {
            "df": train_df,
            "initial_balance": 10000.0,
            "base_fee_rate": 0.0005,
            "foreign_fee_rate": 0.002,
            "slippage": 0.0005,
            "execution_threshold": 0.10,
            "randomize_fees": True,
            "is_eval": False,
            "event_sample_ratio": float(ratio),
        }
        env_kwargs_eval = {
            "df": eval_df,
            "initial_balance": 10000.0,
            "base_fee_rate": 0.0005,
            "foreign_fee_rate": 0.002,
            "slippage": 0.0005,
            "execution_threshold": 0.10,
            "randomize_fees": False,
            "is_eval": True,
        }

        train_env = make_vec_env(lambda: ArbitrageEnv(**env_kwargs_train), n_envs=4)
        train_env = VecFrameStack(train_env, n_stack=n_stack)
        train_env = VecNormalize(
            train_env, norm_obs=True, norm_reward=True, clip_obs=10.0
        )

        eval_env = make_vec_env(lambda: ArbitrageEnv(**env_kwargs_eval), n_envs=1)
        eval_env = VecFrameStack(eval_env, n_stack=n_stack)
        eval_env = VecNormalize(
            eval_env, norm_obs=True, norm_reward=False, clip_obs=10.0
        )

        model = PPO(
            "MlpPolicy",
            train_env,
            verbose=0,
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
            gamma=0.999,
            ent_coef=0.0,
            max_grad_norm=0.5,
            device=rl_device,
        )

        eval_callback = EvalCallback(
            eval_env,
            best_model_save_path="./model/weights/",
            log_path="./logs/",
            eval_freq=10000,
            n_eval_episodes=1,
            deterministic=True,
            render=False,
            verbose=0,
        )

        model.learn(total_timesteps=total_timesteps, callback=eval_callback)
        model.save(cand_path)
        train_env.save(cand_vec)

        del model, train_env, eval_env, eval_callback
        import gc
        gc.collect()
        if torch.cuda.is_available():
            torch.cuda.empty_cache()

        metrics = _quick_eval_candidate(eval_df, cand_path, n_stack=n_stack)
        metrics["normal_event_ratio"] = ratio_label
        metrics["event_sample_ratio"] = float(ratio)
        ratio_results.append(metrics)

        logger.info(
            f"[RL Ratio {ratio_label}] Eval ROI: {metrics['net_roi_pct']:+.2f}% | "
            f"Max DD: {metrics['max_drawdown_pct']:.2f}% | Sharpe: {metrics['sharpe_ratio']:.2f} | "
            f"Score: {metrics['selection_score']:+.2f}"
        )

        if metrics["selection_score"] > best_score:
            best_score = metrics["selection_score"]
            best_ratio_label = ratio_label
            shutil.copyfile(cand_path, final_save_path)
            shutil.copyfile(cand_vec, final_vec_path)

        # Clean up temporary candidate files
        for tmp_f in (cand_path, cand_vec):
            if os.path.exists(tmp_f):
                try:
                    os.remove(tmp_f)
                except Exception:
                    pass

    logger.info("=" * 76)
    logger.info(
        f"Selected Best RL PPO Agent: Normal:Event = {best_ratio_label} "
        f"(Score: {best_score:+.2f}) -> Saved to {final_save_path}"
    )
    logger.info("=" * 76)

    report_data = {}
    if os.path.exists(report_path):
        try:
            with open(report_path, "r", encoding="utf-8") as f:
                report_data = json.load(f)
        except Exception:
            report_data = {}
    report_data["rl_ratio_experiments"] = {
        "selected_best_ratio": best_ratio_label,
        "candidates": ratio_results,
    }
    with open(report_path, "w", encoding="utf-8") as f:
        json.dump(report_data, f, indent=2, ensure_ascii=False)


if __name__ == "__main__":
    import argparse

    parser = argparse.ArgumentParser(description="Train Multi-Coin PPO Arbitrage Agent")
    parser.add_argument("--data", type=str, default="data/historical_dump.csv")
    parser.add_argument("--timesteps", type=int, default=80000)
    parser.add_argument("--n_stack", type=int, default=15)
    parser.add_argument(
        "--ratios",
        type=str,
        default="0.10,0.30,0.50",
        help="Comma-separated event_sample_ratio candidates",
    )
    args = parser.parse_args()
    ratios = [float(x.strip()) for x in args.ratios.split(",") if x.strip()]
    train_agent(
        args.data,
        total_timesteps=args.timesteps,
        n_stack=args.n_stack,
        sample_ratios=ratios,
    )
