import os
import json
import joblib
import logging
from datetime import datetime, timezone
import numpy as np
import pandas as pd
import torch
from tqdm import tqdm
from sklearn.metrics import mean_absolute_error, mean_squared_error
from torch.utils.data import DataLoader

from data.dataset import CryptoArbitrageDataset
from model.predictor import PremiumForecaster

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s"
)
logger = logging.getLogger("EvaluateForecaster")


def evaluate_forecaster(
    csv_path: str = "data/historical_dump.csv",
    batch_size: int = 128,
    report_path: str = "model/weights/eval_report.json",
) -> dict:
    """
    Evaluates the PremiumForecaster model on the Out-of-Sample (Validation, last 20%) dataset.
    Computes:
      1. Multi-Horizon MAE / RMSE (+1h, +4h, +8h full funding cycle) in %p
      2. Extrema (8h Max / Min Peak Spread) MAE / RMSE in %p
      3. Directional Accuracy (Widening vs. Narrowing) for +1h, +4h, +8h
      4. Skill Score (% error reduction) vs. Naive Persistence Baseline (future = current spread)
    """
    device = torch.device("cuda" if torch.cuda.is_available() else "cpu")
    logger.info(f"Initiating Forecaster Evaluation on {device} using {csv_path}")

    try:
        df = pd.read_csv(csv_path)
    except Exception as e:
        logger.error(f"Failed to load dataset: {e}")
        return {}

    if "time" in df.columns:
        df["time"] = pd.to_datetime(df["time"])
        df = df.sort_values("time").reset_index(drop=True)

    split_ratio = 0.8
    train_size = int(len(df) * split_ratio)
    val_df = df.iloc[train_size:].reset_index(drop=True)

    seq_len = 24 * 60  # 24 hours (1,440 minutes) lookback window
    default_pred_len = int(os.getenv("AI_PRED_LEN", str(8 * 60)))

    features = [
        "exchange_premium_spread",
        "local_price",
        "foreign_price",
        "fx_rate",
        "base_funding_rate",
        "foreign_funding_rate",
    ]
    targets = ["exchange_premium_spread", "base_funding_rate", "foreign_funding_rate"]

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

    checkpoint = torch.load(model_path, map_location=device, weights_only=True)
    pred_len = default_pred_len
    if "fc_out.bias" in checkpoint:
        pred_len = int(checkpoint["fc_out.bias"].shape[0] // len(targets))

    val_x_scaled = feature_scaler.transform(val_df[features].values)
    val_x_scaled[:, [1, 2, 3]] = np.clip(val_x_scaled[:, [1, 2, 3]], -3.0, 3.0)
    val_y_scaled = target_scaler.transform(val_df[targets].values)

    val_dataset = CryptoArbitrageDataset(val_x_scaled, val_y_scaled, seq_len, pred_len)
    val_loader = DataLoader(
        val_dataset, batch_size=batch_size, shuffle=False, drop_last=False
    )

    model = PremiumForecaster(
        input_size=len(features),
        hidden_size=128,
        num_layers=2,
        pred_len=pred_len,
        output_size=len(targets),
    ).to(device)
    model.load_state_dict(checkpoint)
    model.eval()

    all_preds_scaled = []
    all_truths_scaled = []

    logger.info(
        f"Running out-of-sample inference over {len(val_dataset):,} sequences (seq_len={seq_len}, pred_len={pred_len}m)..."
    )
    with torch.no_grad():
        for batch_x, batch_y in tqdm(val_loader, desc="[Forecaster Eval]"):
            batch_x = batch_x.to(device)
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

    # Extract Exchange Premium Spread (index 0)
    premium_preds = preds_native[:, :, 0]   # [N, pred_len]
    premium_truths = truths_native[:, :, 0] # [N, pred_len]

    # Current spread at forecast origin t (last step of each input window: idx + seq_len - 1)
    raw_val_spreads = val_df["exchange_premium_spread"].values
    origin_indices = np.arange(len(val_dataset)) + seq_len - 1
    current_spreads = raw_val_spreads[origin_indices]  # [N]

    # Helper to compute horizon metrics
    def compute_horizon_metrics(horizon_steps: int) -> dict:
        h = min(horizon_steps, pred_len)
        p_slice = premium_preds[:, :h]
        t_slice = premium_truths[:, :h]
        naive_slice = np.repeat(current_spreads[:, None], h, axis=1)

        mae_pct = float(mean_absolute_error(t_slice.flatten(), p_slice.flatten()) * 100.0)
        rmse_pct = float(np.sqrt(mean_squared_error(t_slice.flatten(), p_slice.flatten())) * 100.0)

        naive_mae_pct = float(mean_absolute_error(t_slice.flatten(), naive_slice.flatten()) * 100.0)
        naive_rmse_pct = float(np.sqrt(mean_squared_error(t_slice.flatten(), naive_slice.flatten())) * 100.0)

        skill_mae_pct = float((1.0 - (mae_pct / max(naive_mae_pct, 1e-8))) * 100.0)
        skill_rmse_pct = float((1.0 - (rmse_pct / max(naive_rmse_pct, 1e-8))) * 100.0)

        # Directional accuracy on mean future spread change vs current spread
        pred_delta = np.mean(p_slice, axis=1) - current_spreads
        true_delta = np.mean(t_slice, axis=1) - current_spreads
        dir_acc_all = float(np.mean(np.sign(pred_delta) == np.sign(true_delta)) * 100.0)

        # Significant moves (> 0.05%p change)
        sig_mask = np.abs(true_delta) >= 0.0005
        if np.any(sig_mask):
            dir_acc_sig = float(
                np.mean(np.sign(pred_delta[sig_mask]) == np.sign(true_delta[sig_mask])) * 100.0
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

    h_1h = compute_horizon_metrics(60)
    h_4h = compute_horizon_metrics(240)
    h_8h = compute_horizon_metrics(pred_len)

    # Extrema (Max / Min Peak) metrics across the full window (directly used by RL Agent!)
    pred_max = np.max(premium_preds, axis=1)
    true_max = np.max(premium_truths, axis=1)
    pred_min = np.min(premium_preds, axis=1)
    true_min = np.min(premium_truths, axis=1)
    pred_mean = np.mean(premium_preds, axis=1)
    true_mean = np.mean(premium_truths, axis=1)

    max_mae_pct = float(mean_absolute_error(true_max, pred_max) * 100.0)
    min_mae_pct = float(mean_absolute_error(true_min, pred_min) * 100.0)
    mean_mae_pct = float(mean_absolute_error(true_mean, pred_mean) * 100.0)

    forecaster_summary = {
        "evaluated_at": datetime.now(timezone.utc).isoformat(),
        "dataset_path": csv_path,
        "total_rows": int(len(df)),
        "validation_rows": int(len(val_df)),
        "evaluated_sequences": int(shape_p[0]),
        "seq_len_minutes": int(seq_len),
        "pred_len_minutes": int(pred_len),
        "horizons": {
            "1h": h_1h,
            "4h": h_4h,
            "full_horizon": h_8h,
        },
        "summary_features_mae_pct": {
            "window_mean_spread_mae_pct": round(mean_mae_pct, 4),
            "window_max_spread_mae_pct": round(max_mae_pct, 4),
            "window_min_spread_mae_pct": round(min_mae_pct, 4),
        },
    }

    logger.info("\n" + "=" * 72)
    logger.info("FORECASTER OUT-OF-SAMPLE VALIDATION REPORT (UNSCALED REAL %p)")
    logger.info("=" * 72)
    logger.info(f"Validation Sequences : {shape_p[0]:,} windows | Lookback: {seq_len}m | Horizon: {pred_len}m")
    logger.info("-" * 72)
    for label, h_stat in [("+1 Hour (60m)", h_1h), ("+4 Hours (240m)", h_4h), (f"+Full ({pred_len}m)", h_8h)]:
        logger.info(
            f"{label:<16} | MAE: {h_stat['mae_pct']:.3f}%p (Naive: {h_stat['naive_mae_pct']:.3f}%p, Skill: {h_stat['skill_score_mae_pct']:+.1f}%) "
            f"| RMSE: {h_stat['rmse_pct']:.3f}%p | Dir Acc: {h_stat['directional_accuracy_all_pct']:.1f}% (Sig: {h_stat['directional_accuracy_significant_pct']:.1f}%)"
        )
    logger.info("-" * 72)
    logger.info(
        f"RL Input Summary MAE | 8h Mean Spread: {mean_mae_pct:.3f}%p | 8h Max Peak: {max_mae_pct:.3f}%p | 8h Min Trough: {min_mae_pct:.3f}%p"
    )
    logger.info("=" * 72)

    # Save / merge into eval_report.json
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
