import torch
import torch.nn as nn
import numpy as np
import pandas as pd
import os
import joblib
import logging
from tqdm import tqdm
from sklearn.metrics import mean_absolute_error, mean_squared_error

from data.dataset import CryptoArbitrageDataset
from model.predictor import PremiumForecaster
from torch.utils.data import DataLoader

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s"
)
logger = logging.getLogger("EvaluateForecaster")


def evaluate_forecaster(csv_path: str, batch_size: int = 64):
    """
    Evaluates the PremiumForecaster model on the Out-of-Sample (Validation) dataset.
    Calculates strict human-readable metrics (MAE, RMSE) in the real-world unscaled domain.
    """
    device = torch.device("cuda" if torch.cuda.is_available() else "cpu")
    logger.info(f"Initiating Forecaster Evaluation on {device} using {csv_path}")

    # 1. Load the Historical Dataset
    try:
        df = pd.read_csv(csv_path)
    except Exception as e:
        logger.error(f"Failed to load dataset: {e}")
        return

    # Ensure strict temporal ordering to prevent data leakage during validation reading
    if "time" in df.columns:
        df["time"] = pd.to_datetime(df["time"])
        df = df.sort_values("time").reset_index(drop=True)

    # ---------------------------------------------------------
    # 2. Extract Out-of-Sample Validation Split (Last 20%)
    # ---------------------------------------------------------
    split_ratio = 0.8
    train_size = int(len(df) * split_ratio)
    val_df = df.iloc[train_size:]

    seq_len = 24 * 60  # 24 hours of 1-minute historical data natively
    pred_len = int(os.getenv("AI_PRED_LEN", str(8 * 60)))  # Project future outputs over 8 hours (480 minutes) natively

    features = [
        "exchange_premium_spread",
        "local_price",
        "foreign_price",
        "fx_rate",
        "base_funding_rate",
        "foreign_funding_rate",
    ]
    targets = ["exchange_premium_spread", "base_funding_rate", "foreign_funding_rate"]

    # ---------------------------------------------------------
    # 3. Load Trained Scalers and Transform the Validation Data
    # ---------------------------------------------------------
    scaler_dir = "model/weights"
    feat_scaler_path = os.path.join(scaler_dir, "feature_scaler.pkl")
    target_scaler_path = os.path.join(scaler_dir, "target_scaler.pkl")

    if not os.path.exists(feat_scaler_path) or not os.path.exists(target_scaler_path):
        logger.error(
            "Required Scalers missing. Ensure that train_forecaster.py has been executed successfully before evaluating."
        )
        return

    # Loading the identical Normalization states strictly learned from the Training set (No temporal leakage here)
    feature_scaler = joblib.load(feat_scaler_path)
    target_scaler = joblib.load(target_scaler_path)

    val_x_scaled = feature_scaler.transform(val_df[features].values)
    val_y_scaled = target_scaler.transform(val_df[targets].values)

    val_dataset = CryptoArbitrageDataset(val_x_scaled, val_y_scaled, seq_len, pred_len)

    # Validation data is extracted consecutively without shuffling to maintain sequence integrity natively
    val_loader = DataLoader(
        val_dataset, batch_size=batch_size, shuffle=False, drop_last=False
    )

    # ---------------------------------------------------------
    # 4. Load the Trained Model Architecture & Weights
    # ---------------------------------------------------------
    model = PremiumForecaster(
        input_size=len(features),
        hidden_size=128,
        num_layers=2,
        pred_len=pred_len,
        output_size=len(targets),
    ).to(device)

    model_path = "model/weights/premium_forecaster_best.pth"
    if not os.path.exists(model_path):
        logger.error(f"Cannot find compiled model weights at {model_path}")
        return

    model.load_state_dict(
        torch.load(model_path, map_location=device, weights_only=True)
    )
    model.eval()  # Freeze Dropout layers and Batch Normalizations unconditionally

    # ---------------------------------------------------------
    # 5. Execute Predictions across the Validation Dataloader
    # ---------------------------------------------------------
    all_preds_scaled = []
    all_truths_scaled = []

    logger.info("Iterating across the validation timeline. This may take a moment.")
    with torch.no_grad():
        for batch_x, batch_y in tqdm(
            val_loader, desc="[Validation Prediction Inference]"
        ):
            batch_x = batch_x.to(device)
            # Fetch abstract neural prediction scaled bounds exclusively
            preds = model(batch_x)

            all_preds_scaled.append(preds.cpu().numpy())
            all_truths_scaled.append(batch_y.cpu().numpy())

    # Concatenate batched sequences computationally resulting in shape [Validation Size, Pred_Len, Num_Targets]
    flat_preds_scaled = np.concatenate(all_preds_scaled, axis=0)
    flat_truths_scaled = np.concatenate(all_truths_scaled, axis=0)

    # ---------------------------------------------------------
    # 6. Inverse Transform Neural Scales into True Market Metrics
    # ---------------------------------------------------------
    # Flattens temporal multi-dimensions ensuring StandardScaler interoperability cleanly handles Matrix reshaping
    shape_p = flat_preds_scaled.shape
    shape_t = flat_truths_scaled.shape

    flattened_preds_2d = flat_preds_scaled.reshape(-1, len(targets))
    flattened_truths_2d = flat_truths_scaled.reshape(-1, len(targets))

    # Apply mathematical inversions yielding Raw Native Percentages
    preds_native = target_scaler.inverse_transform(flattened_preds_2d)
    truths_native = target_scaler.inverse_transform(flattened_truths_2d)

    # Rescale vectors dynamically mapping back to Original 3D constraints efficiently
    preds_native = preds_native.reshape(shape_p)
    truths_native = truths_native.reshape(shape_t)

    # Note: Target index 0 matches mathematically strictly to 'exchange_premium_spread'
    premium_preds = preds_native[:, :, 0]
    premium_truths = truths_native[:, :, 0]

    # Calculate absolute Mean Statistics over all chronological windows comprehensively
    mae = mean_absolute_error(premium_truths.flatten(), premium_preds.flatten())
    rmse = np.sqrt(
        mean_squared_error(premium_truths.flatten(), premium_preds.flatten())
    )

    logger.info("\n" + "=" * 50)
    logger.info(f"FORECASTER VALIDATION EVALUATION COMPLETE")
    logger.info("=" * 50)
    logger.info(f"Target Feature       : Exchange Premium Spread (Kimchi Premium)")
    logger.info(f"Prediction Window    : Future {pred_len} minutes (5 Hours)")
    logger.info(f"Total Test Sequences : {shape_p[0]:,} time blocks")
    logger.info("-" * 50)
    # Multiplying by 100 converts raw float spread (e.g. 0.015) into human readable Percentages (e.g. 1.5%)
    logger.info(
        f"Mean Absolute Error (MAE) : {mae*100:.3f}% (Absolute magnitude of error)"
    )
    logger.info(
        f"Root Mean Sq. Error (RMSE): {rmse*100:.3f}% (Heavily penalizes extreme errors)"
    )
    logger.info("=" * 50)


if __name__ == "__main__":
    evaluate_forecaster("data/historical_dump.csv")
