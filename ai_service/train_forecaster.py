import os
import json
import joblib
import logging
import numpy as np
import pandas as pd
import torch
import torch.nn as nn
import torch.optim as optim
from tqdm import tqdm

from data.dataset import (
    FORECASTER_FEATURES_12,
    FORECASTER_TARGETS_3,
    ensure_12_features,
    create_dataloaders,
)
from model.predictor import PremiumForecaster, _clean_state_dict

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s"
)
logger = logging.getLogger("TrainForecaster")


def _unwrap_state_dict(model: nn.Module) -> dict:
    raw = model.module.state_dict() if isinstance(model, nn.DataParallel) else model.state_dict()
    return _clean_state_dict(raw)


def train_forecaster(
    csv_path: str = "data/historical_dump.csv",
    epochs: int = 15,
    batch_size: int = 512,
    lr: float = 1.5e-3,
    seq_len: int = 120,
    sample_ratios: list = None,
):
    """
    Trains the 12-Feature Context-Conditioned Attention-GRU PremiumForecaster on Dynamic Dollar Bars:
      - Utilizes Dual RTX 3090 GPUs (`nn.DataParallel`) when multiple CUDA devices are available.
      - Optimizes Volume- & Event-Weighted Huber Loss so high-volume bursts and announcement shocks
        receive proportional gradient priority without destabilizing normal +/-5% spreads.
      - Evaluates multiple Stratified Regime Sampling ratios (`Normal:Event` = `90:10`, `70:30`, `50:50`,
        i.e. `event_sample_ratio` in `[0.10, 0.30, 0.50]`) and automatically selects the best checkpoint.
    """
    if sample_ratios is None:
        sample_ratios = [0.10, 0.30, 0.50]

    device = torch.device("cuda:0" if torch.cuda.is_available() else "cpu")
    n_gpus = torch.cuda.device_count() if torch.cuda.is_available() else 0
    logger.info(
        f"Starting 12-Feature Multi-Coin Forecaster Training on {device} (GPUs detected: {n_gpus}) using {csv_path}"
    )

    try:
        df = pd.read_csv(csv_path)
    except Exception as e:
        logger.error(f"Failed to load dataset: {e}")
        return

    # Sort strictly by (pair_id, ts_utc / time) so each episode stays contiguous
    if "pair_id" in df.columns and "ts_utc" in df.columns:
        df = df.sort_values(["pair_id", "ts_utc"]).reset_index(drop=True)
    elif "pair_id" in df.columns and "time" in df.columns:
        df["time"] = pd.to_datetime(df["time"])
        df = df.sort_values(["pair_id", "time"]).reset_index(drop=True)
    elif "time" in df.columns:
        df["time"] = pd.to_datetime(df["time"])
        df = df.sort_values("time").reset_index(drop=True)

    df = ensure_12_features(df)
    pred_len = int(os.getenv("AI_PRED_LEN", str(8 * 60)))
    features = FORECASTER_FEATURES_12
    targets = FORECASTER_TARGETS_3

    save_dir = "model/weights"
    os.makedirs(save_dir, exist_ok=True)
    save_path = os.path.join(save_dir, "premium_forecaster_best.pth")
    feat_scaler_path = os.path.join(save_dir, "feature_scaler.pkl")
    targ_scaler_path = os.path.join(save_dir, "target_scaler.pkl")
    report_path = os.path.join(save_dir, "eval_report.json")

    criterion_elem = nn.HuberLoss(delta=1.0, reduction="none")

    best_global_score = float("inf")
    best_global_ratio = None
    best_global_state = None
    best_feat_scaler = None
    best_targ_scaler = None
    ratio_experiments = []

    for ratio in sample_ratios:
        norm_pct = int(round((1.0 - ratio) * 100))
        ev_pct = int(round(ratio * 100))
        ratio_label = f"{norm_pct}:{ev_pct}"
        logger.info("=" * 76)
        logger.info(
            f"[Experiment] Training Forecaster with Normal:Event Ratio = {ratio_label} (event_sample_ratio={ratio:.2f})"
        )
        logger.info("=" * 76)

        train_loader, val_loader, feature_scaler, target_scaler = create_dataloaders(
            df,
            seq_len=seq_len,
            pred_len=pred_len,
            features=features,
            targets=targets,
            batch_size=batch_size,
            split_ratio=0.8,
            event_sample_ratio=float(ratio),
            use_dollar_bars=True,
        )

        if len(train_loader.dataset) == 0 or len(val_loader.dataset) == 0:
            logger.error("Empty train or validation dataset! Aborting.")
            return

        base_model = PremiumForecaster(
            input_size=len(features),
            hidden_size=128,
            num_layers=2,
            pred_len=pred_len,
            output_size=len(targets),
        ).to(device)

        if n_gpus > 1:
            model = nn.DataParallel(base_model, device_ids=list(range(n_gpus)))
        else:
            model = base_model

        optimizer = optim.AdamW(model.parameters(), lr=lr, weight_decay=1e-4)
        scheduler = optim.lr_scheduler.ReduceLROnPlateau(
            optimizer, mode="min", factor=0.5, patience=3
        )

        best_ratio_val_loss = float("inf")
        best_ratio_real_mae = float("inf")
        best_ratio_state = None
        patience_counter = 0
        early_stop_patience = 5

        for epoch in range(epochs):
            model.train()
            train_loss = 0.0

            loop = tqdm(
                train_loader,
                desc=f"[Ratio {ratio_label}] Epoch {epoch+1}/{epochs} [Train]",
                leave=False,
            )
            for batch_x, batch_y in loop:
                batch_x = batch_x.to(device, non_blocking=True)
                batch_y = batch_y.to(device, non_blocking=True)

                optimizer.zero_grad()
                preds = model(batch_x)  # [B, pred_len, 3]

                raw_loss = criterion_elem(preds, batch_y).mean(dim=(1, 2))  # [B]
                # Volume- & Event-Weighted Huber Loss:
                # col 6 is scaled dollar_vol_ratio; cols 8..11 are causal event intensities
                vol_signal = torch.clamp(batch_x[:, -1, 6], -1.0, 3.0)
                ev_signal = batch_x[:, -1, 8:12].abs().max(dim=1).values.clamp(0.0, 3.0)
                sample_w = 1.0 + 0.15 * torch.relu(vol_signal) + 0.25 * torch.tanh(ev_signal)
                loss = (raw_loss * sample_w).mean()

                loss.backward()
                torch.nn.utils.clip_grad_norm_(model.parameters(), max_norm=1.0)
                optimizer.step()

                train_loss += loss.item() * batch_x.size(0)
                loop.set_postfix(loss=f"{loss.item():.4f}")

            train_loss /= max(1, len(train_loader.dataset))

            # Validation Evaluation (Unweighted Huber + Unscaled %p MAE)
            model.eval()
            val_loss = 0.0
            val_preds_list = []
            val_truths_list = []
            with torch.no_grad():
                for batch_x, batch_y in val_loader:
                    batch_x = batch_x.to(device, non_blocking=True)
                    batch_y = batch_y.to(device, non_blocking=True)
                    preds = model(batch_x)
                    v_loss = criterion_elem(preds, batch_y).mean()
                    val_loss += v_loss.item() * batch_x.size(0)
                    val_preds_list.append(preds[:, :240, 0].cpu().numpy())
                    val_truths_list.append(batch_y[:, :240, 0].cpu().numpy())

            val_loss /= max(1, len(val_loader.dataset))
            scheduler.step(val_loss)

            # Compute unscaled 4h MAE (%p) on nominal spread for authentic model selection
            vp = np.concatenate(val_preds_list, axis=0)
            vt = np.concatenate(val_truths_list, axis=0)
            dummy_p = np.zeros((vp.size, len(targets)), dtype=np.float32)
            dummy_t = np.zeros((vt.size, len(targets)), dtype=np.float32)
            dummy_p[:, 0] = vp.reshape(-1)
            dummy_t[:, 0] = vt.reshape(-1)
            unscaled_p = target_scaler.inverse_transform(dummy_p)[:, 0]
            unscaled_t = target_scaler.inverse_transform(dummy_t)[:, 0]
            val_mae_4h_pct = float(np.mean(np.abs(unscaled_p - unscaled_t)) * 100.0)

            logger.info(
                f"[Ratio {ratio_label}] Epoch {epoch+1:02d}/{epochs:02d} | "
                f"Train Loss: {train_loss:.5f} | Val Loss: {val_loss:.5f} | Val 4h MAE: {val_mae_4h_pct:.4f}%p"
            )

            if val_loss < best_ratio_val_loss:
                best_ratio_val_loss = val_loss
                best_ratio_real_mae = val_mae_4h_pct
                best_ratio_state = {
                    k: v.detach().cpu().clone()
                    for k, v in _unwrap_state_dict(model).items()
                }
                patience_counter = 0
            else:
                patience_counter += 1
                if patience_counter >= early_stop_patience:
                    logger.info(
                        f"[Ratio {ratio_label}] Early stopping triggered at epoch {epoch+1}."
                    )
                    break

        ratio_experiments.append(
            {
                "normal_event_ratio": ratio_label,
                "event_sample_ratio": float(ratio),
                "best_val_huber_loss": round(float(best_ratio_val_loss), 6),
                "best_val_4h_mae_pct": round(float(best_ratio_real_mae), 4),
            }
        )

        # Select global winner by lowest unscaled validation MAE (%p) + Huber Loss composite
        composite_score = best_ratio_real_mae + 0.10 * best_ratio_val_loss
        if composite_score < best_global_score and best_ratio_state is not None:
            best_global_score = composite_score
            best_global_ratio = ratio_label
            best_global_state = best_ratio_state
            best_feat_scaler = feature_scaler
            best_targ_scaler = target_scaler

    if best_global_state is not None:
        torch.save(best_global_state, save_path)
        joblib.dump(best_feat_scaler, feat_scaler_path)
        joblib.dump(best_targ_scaler, targ_scaler_path)
        logger.info("=" * 76)
        logger.info(
            f"Selected Best Forecaster Checkpoint: Normal:Event = {best_global_ratio} "
            f"(Composite Score: {best_global_score:.4f}) -> Saved to {save_path}"
        )
        logger.info("=" * 76)

        report_data = {}
        if os.path.exists(report_path):
            try:
                with open(report_path, "r", encoding="utf-8") as f:
                    report_data = json.load(f)
            except Exception:
                report_data = {}
        report_data["forecaster_ratio_experiments"] = {
            "selected_best_ratio": best_global_ratio,
            "candidates": ratio_experiments,
        }
        with open(report_path, "w", encoding="utf-8") as f:
            json.dump(report_data, f, indent=2, ensure_ascii=False)


if __name__ == "__main__":
    import argparse

    parser = argparse.ArgumentParser(
        description="Train 12-Feature Multi-Coin PremiumForecaster (8h Horizon, Dual GPU)"
    )
    parser.add_argument("--data", type=str, default="data/historical_dump.csv")
    parser.add_argument("--epochs", type=int, default=12)
    parser.add_argument("--batch_size", type=int, default=512)
    parser.add_argument("--lr", type=float, default=1.5e-3)
    parser.add_argument("--seq_len", type=int, default=120)
    parser.add_argument(
        "--ratios",
        type=str,
        default="0.10,0.30,0.50",
        help="Comma-separated event_sample_ratio candidates (e.g. 0.10,0.30,0.50)",
    )
    args = parser.parse_args()
    ratios = [float(x.strip()) for x in args.ratios.split(",") if x.strip()]
    train_forecaster(
        args.data,
        epochs=args.epochs,
        batch_size=args.batch_size,
        lr=args.lr,
        seq_len=args.seq_len,
        sample_ratios=ratios,
    )
