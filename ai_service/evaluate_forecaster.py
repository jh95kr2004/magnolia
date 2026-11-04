import os
import json
import joblib
import logging
from datetime import datetime, timezone
import numpy as np
import pandas as pd
import torch
import torch.nn as nn
from tqdm import tqdm
from sklearn.metrics import mean_absolute_error, mean_squared_error
from torch.utils.data import DataLoader

from data.dataset import (
    FORECASTER_FEATURES_12,
    FORECASTER_TARGETS_3,
    CryptoArbitrageDataset,
    ensure_12_features,
    resample_to_dynamic_dollar_bars,
    split_train_val_episodes,
)
from model.predictor import PremiumForecaster, _clean_state_dict

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s"
)
logger = logging.getLogger("EvaluateForecaster")


def _compute_metrics_slice(
    preds_2d: np.ndarray,
    truths_2d: np.ndarray,
    current_1d: np.ndarray,
    horizon_steps: int,
) -> dict:
    pred_len = preds_2d.shape[1]
    h = min(horizon_steps, pred_len)
    p_slice = preds_2d[:, :h]
    t_slice = truths_2d[:, :h]
    naive_slice = np.repeat(current_1d[:, None], h, axis=1)

    mae_pct = float(mean_absolute_error(t_slice.flatten(), p_slice.flatten()) * 100.0)
    rmse_pct = float(
        np.sqrt(mean_squared_error(t_slice.flatten(), p_slice.flatten())) * 100.0
    )

    naive_mae_pct = float(
        mean_absolute_error(t_slice.flatten(), naive_slice.flatten()) * 100.0
    )
    naive_rmse_pct = float(
        np.sqrt(mean_squared_error(t_slice.flatten(), naive_slice.flatten())) * 100.0
    )

    skill_mae_pct = float((1.0 - (mae_pct / max(naive_mae_pct, 1e-8))) * 100.0)
    skill_rmse_pct = float((1.0 - (rmse_pct / max(naive_rmse_pct, 1e-8))) * 100.0)

    pred_delta = np.mean(p_slice, axis=1) - current_1d
    true_delta = np.mean(t_slice, axis=1) - current_1d
    dir_acc_all = float(np.mean(np.sign(pred_delta) == np.sign(true_delta)) * 100.0)

    sig_mask = np.abs(true_delta) >= 0.0005
    if np.any(sig_mask):
        dir_acc_sig = float(
            np.mean(np.sign(pred_delta[sig_mask]) == np.sign(true_delta[sig_mask]))
            * 100.0
        )
    else:
        dir_acc_sig = dir_acc_all

    return {
        "horizon_minutes": int(h),
        "mae_pct": round(mae_pct, 4),
        "rmse_pct": round(rmse_pct, 4),
        "naive_mae_pct": round(naive_mae_pct, 4),
        "naive_rmse_pct": round(naive_rmse_pct, 4),
        "skill_score_mae_pct": round(skill_mae_pct, 2),
        "skill_score_rmse_pct": round(skill_rmse_pct, 2),
        "directional_accuracy_all_pct": round(dir_acc_all, 2),
        "directional_accuracy_significant_pct": round(dir_acc_sig, 2),
    }


def evaluate_forecaster(
    csv_path: str = "data/historical_dump.csv",
    batch_size: int = 512,
    seq_len: int = 120,
    report_path: str = "model/weights/eval_report.json",
) -> dict:
    """
    Evaluates the 12-Feature Multi-Coin PremiumForecaster on the Out-of-Sample Validation Dataset:
      1. Multi-Horizon MAE / RMSE (+1h, +4h, +8h full funding cycle) in %p for both
         Nominal Fiat Kimp (`exchange_premium_spread`) and Real Tether Kimp (`tether_real_premium_spread`).
      2. Extrema (8h Mean / Max / Min Peak Spread) MAE in %p.
      3. Directional Accuracy (Widening vs. Narrowing) and Skill Score vs. Naive Persistence.
      4. Per-Regime Breakdown across `CONTINUOUS`, `NEW_LISTING`, `INVESTMENT_WARNING`,
         `WALLET_SUSPENSION`, and `DELISTED` episodes.
    """
    device = torch.device("cuda:0" if torch.cuda.is_available() else "cpu")
    n_gpus = torch.cuda.device_count() if torch.cuda.is_available() else 0
    logger.info(
        f"Initiating 12-Feature Forecaster Evaluation on {device} (GPUs: {n_gpus}) using {csv_path}"
    )

    try:
        df = pd.read_csv(csv_path)
    except Exception as e:
        logger.error(f"Failed to load dataset: {e}")
        return {}

    if "pair_id" in df.columns and "ts_utc" in df.columns:
        df = df.sort_values(["pair_id", "ts_utc"]).reset_index(drop=True)
    elif "pair_id" in df.columns and "time" in df.columns:
        df["time"] = pd.to_datetime(df["time"])
        df = df.sort_values(["pair_id", "time"]).reset_index(drop=True)
    elif "time" in df.columns:
        df["time"] = pd.to_datetime(df["time"])
        df = df.sort_values("time").reset_index(drop=True)

    scaler_dir = "model/weights"
    feat_scaler_path = os.path.join(scaler_dir, "feature_scaler.pkl")
    target_scaler_path = os.path.join(scaler_dir, "target_scaler.pkl")
    model_path = os.path.join(scaler_dir, "premium_forecaster_best.pth")

    if not os.path.exists(feat_scaler_path) or not os.path.exists(target_scaler_path):
        logger.error("Required scalers missing. Run train_forecaster.py first.")
        return {}
    if not os.path.exists(model_path):
        logger.error(f"Cannot find model weights at {model_path}")
        return {}

    feature_scaler = joblib.load(feat_scaler_path)
    target_scaler = joblib.load(target_scaler_path)

    raw_ckpt = torch.load(model_path, map_location="cpu", weights_only=True)
    checkpoint = _clean_state_dict(raw_ckpt)
    loaded_in_size = (
        int(checkpoint["gru.weight_ih_l0"].shape[1])
        if "gru.weight_ih_l0" in checkpoint
        else 12
    )
    default_pred_len = int(os.getenv("AI_PRED_LEN", str(8 * 60)))
    pred_len = (
        int(checkpoint["fc_out.bias"].shape[0] // 3)
        if "fc_out.bias" in checkpoint
        else default_pred_len
    )

    df_proc = ensure_12_features(df)
    if "quote_volume_usd" in df_proc.columns and len(df_proc) > (seq_len + pred_len) * 3:
        df_proc = resample_to_dynamic_dollar_bars(df_proc)

    _, val_df = split_train_val_episodes(
        df_proc, seq_len=seq_len, pred_len=pred_len, split_ratio=0.8
    )

    if loaded_in_size == 12:
        features = FORECASTER_FEATURES_12
        targets = FORECASTER_TARGETS_3
        val_x_scaled = feature_scaler.transform(val_df[features].values)
        val_y_scaled = target_scaler.transform(val_df[targets].values)
    else:
        features = [
            "exchange_premium_spread",
            "local_price",
            "foreign_price",
            "fx_rate",
            "base_funding_rate",
            "foreign_funding_rate",
        ]
        targets = [
            "exchange_premium_spread",
            "base_funding_rate",
            "foreign_funding_rate",
        ]
        val_x_scaled = feature_scaler.transform(val_df[features].values)
        val_x_scaled[:, [1, 2, 3]] = np.clip(val_x_scaled[:, [1, 2, 3]], -3.0, 3.0)
        val_y_scaled = target_scaler.transform(val_df[targets].values)

    val_pairs = val_df["pair_id"].values if "pair_id" in val_df.columns else None
    val_dataset = CryptoArbitrageDataset(
        val_x_scaled, val_y_scaled, seq_len, pred_len, pair_ids=val_pairs
    )
    val_loader = DataLoader(
        val_dataset, batch_size=batch_size, shuffle=False, drop_last=False
    )

    base_model = PremiumForecaster(
        input_size=len(features),
        hidden_size=128,
        num_layers=2,
        pred_len=pred_len,
        output_size=len(targets),
    ).to(device)
    base_model.load_state_dict(checkpoint)
    model = (
        nn.DataParallel(base_model, device_ids=list(range(n_gpus)))
        if n_gpus > 1
        else base_model
    )
    model.eval()

    all_preds_scaled = []
    all_truths_scaled = []

    logger.info(
        f"Running out-of-sample inference over {len(val_dataset):,} sequences "
        f"(seq_len={seq_len}, pred_len={pred_len}, coins={val_df['coin'].nunique() if 'coin' in val_df.columns else 1})..."
    )
    with torch.no_grad():
        for batch_x, batch_y in tqdm(val_loader, desc="[Forecaster Eval]"):
            batch_x = batch_x.to(device, non_blocking=True)
            preds = model(batch_x)
            all_preds_scaled.append(preds.cpu().numpy())
            all_truths_scaled.append(batch_y.cpu().numpy())

    flat_preds_scaled = np.concatenate(all_preds_scaled, axis=0)
    flat_truths_scaled = np.concatenate(all_truths_scaled, axis=0)

    shape_p = flat_preds_scaled.shape  # [N, pred_len, 3]
    preds_native = target_scaler.inverse_transform(
        flat_preds_scaled.reshape(-1, len(targets))
    ).reshape(shape_p)
    truths_native = target_scaler.inverse_transform(
        flat_truths_scaled.reshape(-1, len(targets))
    ).reshape(shape_p)

    # 1. Nominal Fiat Kimp (index 0)
    premium_preds = preds_native[:, :, 0]
    premium_truths = truths_native[:, :, 0]
    origin_indices = val_dataset.valid_indices + seq_len - 1
    current_spreads = val_df["exchange_premium_spread"].values[origin_indices]

    h_1h = _compute_metrics_slice(premium_preds, premium_truths, current_spreads, 60)
    h_4h = _compute_metrics_slice(premium_preds, premium_truths, current_spreads, 240)
    h_8h = _compute_metrics_slice(
        premium_preds, premium_truths, current_spreads, pred_len
    )

    # 2. Real Tether Kimp (index 1 when 12-feature model)
    real_preds = preds_native[:, :, 1] if loaded_in_size == 12 else premium_preds
    real_truths = truths_native[:, :, 1] if loaded_in_size == 12 else premium_truths
    current_real_spreads = (
        val_df["tether_real_premium_spread"].values[origin_indices]
        if "tether_real_premium_spread" in val_df.columns
        else current_spreads
    )
    real_h_1h = _compute_metrics_slice(
        real_preds, real_truths, current_real_spreads, 60
    )
    real_h_4h = _compute_metrics_slice(
        real_preds, real_truths, current_real_spreads, 240
    )
    real_h_8h = _compute_metrics_slice(
        real_preds, real_truths, current_real_spreads, pred_len
    )

    # 3. Extrema (Max / Min Peak) metrics across the full 8h window
    pred_max = np.max(premium_preds, axis=1)
    true_max = np.max(premium_truths, axis=1)
    pred_min = np.min(premium_preds, axis=1)
    true_min = np.min(premium_truths, axis=1)
    pred_mean = np.mean(premium_preds, axis=1)
    true_mean = np.mean(premium_truths, axis=1)

    max_mae_pct = float(mean_absolute_error(true_max, pred_max) * 100.0)
    min_mae_pct = float(mean_absolute_error(true_min, pred_min) * 100.0)
    mean_mae_pct = float(mean_absolute_error(true_mean, pred_mean) * 100.0)

    # 4. Per-Regime Breakdown (CONTINUOUS, NEW_LISTING, INVESTMENT_WARNING, WALLET_SUSPENSION, DELISTED)
    regime_breakdown = {}
    if "regime" in val_df.columns:
        origin_regimes = val_df["regime"].values[origin_indices]
        for reg_name in [
            "CONTINUOUS",
            "NEW_LISTING",
            "INVESTMENT_WARNING",
            "WALLET_SUSPENSION",
            "DELISTED",
        ]:
            mask = origin_regimes == reg_name
            if int(np.sum(mask)) > 0:
                r_1h = _compute_metrics_slice(
                    premium_preds[mask],
                    premium_truths[mask],
                    current_spreads[mask],
                    60,
                )
                r_4h = _compute_metrics_slice(
                    premium_preds[mask],
                    premium_truths[mask],
                    current_spreads[mask],
                    240,
                )
                r_8h = _compute_metrics_slice(
                    premium_preds[mask],
                    premium_truths[mask],
                    current_spreads[mask],
                    pred_len,
                )
                regime_breakdown[reg_name] = {
                    "evaluated_sequences": int(np.sum(mask)),
                    "1h_mae_pct": r_1h["mae_pct"],
                    "4h_mae_pct": r_4h["mae_pct"],
                    "8h_mae_pct": r_8h["mae_pct"],
                    "8h_skill_score_pct": r_8h["skill_score_mae_pct"],
                    "8h_directional_accuracy_pct": r_8h["directional_accuracy_all_pct"],
                    "8h_significant_dir_acc_pct": r_8h[
                        "directional_accuracy_significant_pct"
                    ],
                }

    forecaster_summary = {
        "evaluated_at": datetime.now(timezone.utc).isoformat(),
        "dataset_path": csv_path,
        "input_features_count": int(loaded_in_size),
        "total_raw_rows": int(len(df)),
        "total_dollar_bar_rows": int(len(df_proc)),
        "validation_rows": int(len(val_df)),
        "evaluated_sequences": int(shape_p[0]),
        "evaluated_coins": int(val_df["coin"].nunique()) if "coin" in val_df.columns else 1,
        "evaluated_pairs": int(val_df["pair_id"].nunique()) if "pair_id" in val_df.columns else 1,
        "seq_len_bars": int(seq_len),
        "pred_len_minutes": int(pred_len),
        "horizons": {
            "1h": h_1h,
            "4h": h_4h,
            "full_horizon": h_8h,
        },
        "tether_real_spread_horizons": {
            "1h": real_h_1h,
            "4h": real_h_4h,
            "full_horizon": real_h_8h,
        },
        "summary_features_mae_pct": {
            "window_mean_spread_mae_pct": round(mean_mae_pct, 4),
            "window_max_spread_mae_pct": round(max_mae_pct, 4),
            "window_min_spread_mae_pct": round(min_mae_pct, 4),
        },
        "regime_breakdown": regime_breakdown,
    }

    logger.info("\n" + "=" * 84)
    logger.info(
        "12-FEATURE MULTI-COIN FORECASTER OUT-OF-SAMPLE VALIDATION REPORT (UNSCALED REAL %p)"
    )
    logger.info("=" * 84)
    logger.info(
        f"Validation Sequences : {shape_p[0]:,} windows across {forecaster_summary['evaluated_coins']} coins "
        f"({forecaster_summary['evaluated_pairs']} pairs) | Lookback: {seq_len} bars | Horizon: {pred_len}m"
    )
    logger.info("-" * 84)
    for label, h_stat in [
        ("+1 Hour (60m)", h_1h),
        ("+4 Hours (240m)", h_4h),
        (f"+Full ({pred_len}m)", h_8h),
    ]:
        logger.info(
            f"[Nominal Kimp] {label:<14} | MAE: {h_stat['mae_pct']:.3f}%p (Naive: {h_stat['naive_mae_pct']:.3f}%p, Skill: {h_stat['skill_score_mae_pct']:+.1f}%) "
            f"| RMSE: {h_stat['rmse_pct']:.3f}%p | Dir Acc: {h_stat['directional_accuracy_all_pct']:.1f}% (Sig: {h_stat['directional_accuracy_significant_pct']:.1f}%)"
        )
    logger.info("-" * 84)
    for label, h_stat in [
        ("+1 Hour (60m)", real_h_1h),
        ("+4 Hours (240m)", real_h_4h),
        (f"+Full ({pred_len}m)", real_h_8h),
    ]:
        logger.info(
            f"[Tether Kimp]  {label:<14} | MAE: {h_stat['mae_pct']:.3f}%p (Naive: {h_stat['naive_mae_pct']:.3f}%p, Skill: {h_stat['skill_score_mae_pct']:+.1f}%) "
            f"| RMSE: {h_stat['rmse_pct']:.3f}%p | Dir Acc: {h_stat['directional_accuracy_all_pct']:.1f}% (Sig: {h_stat['directional_accuracy_significant_pct']:.1f}%)"
        )
    logger.info("-" * 84)
    logger.info(
        f"RL Input Summary MAE | 8h Mean Spread: {mean_mae_pct:.3f}%p | 8h Max Peak: {max_mae_pct:.3f}%p | 8h Min Trough: {min_mae_pct:.3f}%p"
    )
    if regime_breakdown:
        logger.info("-" * 84)
        logger.info("PER-REGIME OUT-OF-SAMPLE BREAKDOWN:")
        for r_name, r_stat in regime_breakdown.items():
            logger.info(
                f"  • {r_name:<20} (n={r_stat['evaluated_sequences']:>4}) | "
                f"1h MAE: {r_stat['1h_mae_pct']:.3f}%p | 4h MAE: {r_stat['4h_mae_pct']:.3f}%p | "
                f"8h MAE: {r_stat['8h_mae_pct']:.3f}%p | Skill: {r_stat['8h_skill_score_pct']:+.1f}% | "
                f"Dir Acc: {r_stat['8h_directional_accuracy_pct']:.1f}% (Sig: {r_stat['8h_significant_dir_acc_pct']:.1f}%)"
            )
    logger.info("=" * 84)

    os.makedirs(os.path.dirname(report_path), exist_ok=True)
    report_data = {}
    if os.path.exists(report_path):
        try:
            with open(report_path, "r", encoding="utf-8") as f:
                report_data = json.load(f)
        except Exception:
            report_data = {}
    report_data["forecaster"] = forecaster_summary
    with open(report_path, "w", encoding="utf-8") as f:
        json.dump(report_data, f, indent=2, ensure_ascii=False)
    logger.info(f"Saved Forecaster evaluation metrics to {report_path}")

    return forecaster_summary


if __name__ == "__main__":
    evaluate_forecaster("data/historical_dump.csv")
