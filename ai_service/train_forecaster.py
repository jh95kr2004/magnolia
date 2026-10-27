import torch
import torch.nn as nn
import torch.optim as optim
import pandas as pd
import numpy as np
import os
import joblib
import logging
from tqdm import tqdm

from data.dataset import create_dataloaders
from model.predictor import PremiumForecaster

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s"
)
logger = logging.getLogger("TrainForecaster")


def train_forecaster(
    csv_path: str, epochs: int = 50, batch_size: int = 64, lr: float = 1e-3
):
    # Set target compute device globally enforcing GPU matrix acceleration if accessible
    device = torch.device("cuda" if torch.cuda.is_available() else "cpu")
    logger.info(f"Starting training on {device} using {csv_path}")

    # 1. Sequence DataFrame Loading and Mapping
    try:
        df = pd.read_csv(csv_path)
    except Exception as e:
        logger.error(f"Failed to load dataset: {e}")
        return

    # Sort strict temporal datasets guaranteeing no chronologically leaked frames exist
    if "time" in df.columns:
        df["time"] = pd.to_datetime(df["time"])
        df = df.sort_values("time").reset_index(drop=True)

    # 2. Forecasting Topology Configurations
    seq_len = 24 * 60  # 24 hours of accumulated context records (1-minute frames)
    pred_len = int(os.getenv("AI_PRED_LEN", str(8 * 60)))  # Project future outputs spanning the next 8 hours (480 minutes = 1 full funding cycle)
    features = [
        "exchange_premium_spread",
        "local_price",
        "foreign_price",
        "fx_rate",
        "base_funding_rate",
        "foreign_funding_rate",
    ]
    targets = ["exchange_premium_spread", "base_funding_rate", "foreign_funding_rate"]

    # Verify column existence natively mapped to the pipeline specification requirements
    for col in features + targets:
        if col not in df.columns:
            logger.error(f"Missing required continuous column in dataset: {col}")
            return

    # Extract DataLoaders and correctly Fitted Scalers preventing catastrophic model loss divergence
    train_loader, val_loader, feature_scaler, target_scaler = create_dataloaders(
        df,
        seq_len=seq_len,
        pred_len=pred_len,
        features=features,
        targets=targets,
        batch_size=batch_size,
        split_ratio=0.8,
    )

    # Establish weight directory structures to prevent path IO exceptions on early stops
    save_path = "model/weights/premium_forecaster_best.pth"
    os.makedirs(os.path.dirname(save_path), exist_ok=True)
    
    # Securely dump the scalers using joblib for exact symmetrical usage later by the Inference Application (predictor.py)
    joblib.dump(feature_scaler, "model/weights/feature_scaler.pkl")
    joblib.dump(target_scaler, "model/weights/target_scaler.pkl")
    logger.info("Saved strictly fitted data scalers securely to disk via joblib.")

    # 3. Model & Loss Optimizations Setup
    model = PremiumForecaster(
        input_size=len(features),
        hidden_size=128,
        num_layers=2,
        pred_len=pred_len,
        output_size=len(targets),
    ).to(device)

    # Huber Loss is highly advantageous mathematically avoiding excessive penalty scaling spanning anomaly flash crashes
    criterion = nn.HuberLoss(delta=1.0)
    optimizer = optim.Adam(model.parameters(), lr=lr, weight_decay=1e-5)
    
    # Dynamically clamp gradients ensuring extreme volatile outputs exponentially drop parameters safely
    scheduler = optim.lr_scheduler.ReduceLROnPlateau(
        optimizer, mode="min", factor=0.5, patience=5
    )

    best_val_loss = float("inf")
    
    # -------------------------------------------------------------
    # Initialization parameters mapping the core Early Stopping limit 
    # -------------------------------------------------------------
    patience_counter = 0
    early_stop_patience = 10  # Stop training unconditionally if Validation loss doesn't improve for 10 sequential Epochs

    for epoch in range(epochs):
        model.train()
        train_loss = 0.0

        loop = tqdm(train_loader, desc=f"Epoch {epoch+1}/{epochs} [Train]")
        for batch_x, batch_y in loop:
            batch_x, batch_y = batch_x.to(device), batch_y.to(device)

            optimizer.zero_grad()
            preds = model(batch_x)  # Yield tensor shape constraints: [batch, pred_len, output_size]

            loss = criterion(preds, batch_y)
            loss.backward()

            # Absolute explicit Gradient limitation clipping
            torch.nn.utils.clip_grad_norm_(model.parameters(), max_norm=1.0)

            optimizer.step()
            train_loss += loss.item() * batch_x.size(0)
            loop.set_postfix(loss=loss.item())

        train_loss /= len(train_loader.dataset)

        # -------------------------------------------------------------
        # Secure Validation Testing Output Processing Routine
        # -------------------------------------------------------------
        model.eval()
        val_loss = 0.0
        with torch.no_grad():
            for batch_x, batch_y in tqdm(
                val_loader, desc=f"Epoch {epoch+1}/{epochs} [Val]"
            ):
                batch_x, batch_y = batch_x.to(device), batch_y.to(device)
                preds = model(batch_x)
                loss = criterion(preds, batch_y)
                val_loss += loss.item() * batch_x.size(0)

        val_loss /= len(val_loader.dataset)

        logger.info(
            f"Epoch {epoch+1} | Train Loss: {train_loss:.6f} | Val Loss: {val_loss:.6f}"
        )

        scheduler.step(val_loss)

        # -------------------------------------------------------------
        # Early Stopping Logic Condition
        # -------------------------------------------------------------
        if val_loss < best_val_loss:
            best_val_loss = val_loss
            patience_counter = 0 # Reset patience since it improved!
            torch.save(model.state_dict(), save_path)
            logger.info(f"Saved new best model with Val Loss: {best_val_loss:.6f}")
        else:
            patience_counter += 1
            if patience_counter >= early_stop_patience:
                logger.info(f"Early Stopping triggered globally at epoch {epoch+1}! No improvements achieved over {early_stop_patience} consecutive epochs.")
                break # Exit the overall Epoch bounds


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(description="Train PremiumForecaster (8h Horizon)")
    parser.add_argument("--data", type=str, default="data/historical_dump.csv")
    parser.add_argument("--epochs", type=int, default=15)
    parser.add_argument("--batch_size", type=int, default=256)
    parser.add_argument("--lr", type=float, default=1e-3)
    args = parser.parse_args()
    train_forecaster(args.data, epochs=args.epochs, batch_size=args.batch_size, lr=args.lr)
