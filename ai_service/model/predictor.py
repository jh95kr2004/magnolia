import torch
import torch.nn as nn
import numpy as np
import pandas as pd
import os
import joblib
import threading

class AttentionLayer(nn.Module):

    def __init__(self, hidden_size):
        super(AttentionLayer, self).__init__()
        self.attn = nn.Linear(hidden_size, 1)

    def forward(self, lstm_out):
        # lstm_out: [batch_size, seq_len, hidden_size]
        attn_weights = torch.softmax(self.attn(lstm_out), dim=1)  # [batch, seq_len, 1]
        context = torch.sum(attn_weights * lstm_out, dim=1)  # [batch, hidden_size]
        return context, attn_weights


class PremiumForecaster(nn.Module):
    """
    Encoder-Decoder based Attention-GRU for multi-step forecasting.
    Inputs: [Spread, Local Price, Foreign Price, FX Rate, Base Funding, Foreign Funding]
    Outputs: [Predicted Spread, Base Funding, Foreign Funding] arrays for pred_len steps.
    """

    def __init__(
        self, input_size=6, hidden_size=128, num_layers=2, pred_len=480, output_size=3
    ):
        super(PremiumForecaster, self).__init__()
        self.hidden_size = hidden_size
        self.num_layers = num_layers
        self.pred_len = pred_len
        self.output_size = output_size

        # Encoder
        self.gru = nn.GRU(
            input_size,
            hidden_size,
            num_layers,
            batch_first=True,
            dropout=0.2 if num_layers > 1 else 0,
        )
        self.attention = AttentionLayer(hidden_size)

        # Decoder (Predicting multiple steps)
        # Direct FC mapping from context vector to prediction horizon.
        self.fc_out = nn.Linear(hidden_size, pred_len * output_size)

    def forward(self, x):
        # x: [batch, seq_len, input_size]
        # 1. RevIN-style local 24h window centering to eliminate non-stationary macro level drift
        window_mean = x.mean(dim=1, keepdim=True)
        x_local = x - window_mean

        out, _ = self.gru(x_local)

        # Attention context over locally centered trajectory
        context, _ = self.attention(out)  # context: [batch, hidden_size]

        # Fully connected residual delta output
        flat_pred = self.fc_out(context)  # [batch, pred_len * output_size]
        delta_predictions = flat_pred.view(-1, self.pred_len, self.output_size)

        # 2. Anchor at current origin x_t and apply damped Ornstein-Uhlenbeck mean reversion + learned residual delta
        origin_anchor = x[:, -1:, [0, 4, 5]]
        target_window_mean = window_mean[:, :, [0, 4, 5]]
        local_deviation = target_window_mean - origin_anchor  # >0 when spread is below 24h mean, <0 when above

        t_grid = torch.linspace(1.0 / self.pred_len, 1.0, self.pred_len, device=x.device).view(1, self.pred_len, 1)
        # Exponential OU reversion factor (1 - exp(-lambda * t/T)) + bounded neural trajectory refinement
        ou_reversion = (1.0 - torch.exp(-0.35 * t_grid)) * local_deviation
        neural_residual = 0.25 * torch.tanh(delta_predictions) * torch.sqrt(t_grid)

        predictions = origin_anchor + ou_reversion + neural_residual
        return predictions


_predictors_cache = {}
_cache_lock = threading.Lock()

def predict_reversion(historical_df: pd.DataFrame, pred_len=480, device="cpu") -> tuple:
    """
    Loads pretrained scalers/model and dynamically predicts the future premium spread and funding rates
    over an 8-hour horizon (pred_len=480 1-minute steps, matching 1 full perpetual funding cycle).
    Uses in-memory cache to avoid disk I/O bottlenecks.
    Returns:
        mean_premium: float
        max_premium: float
        min_premium: float
        premium_traj: list of floats
    """
    if historical_df is None or len(historical_df) < 10:
        return 0.0, 0.0, 0.0, []

    global _predictors_cache
    cache_key = (pred_len, device)

    with _cache_lock:
        if cache_key not in _predictors_cache:
            scaler_dir = "model/weights"
            feat_scaler_path = os.path.join(scaler_dir, "feature_scaler.pkl")
            target_scaler_path = os.path.join(scaler_dir, "target_scaler.pkl")
            
            # Check bounds mapping constraints rigorously verifying correct scale integrity mappings
            if not os.path.exists(feat_scaler_path) or not os.path.exists(target_scaler_path):
                print("Required Scalers missing. Cannot scale input correctly. Aborting forecasting.")
                return 0.0, 0.0, 0.0, []

            # Inject strictly saved scalar normalization algorithms 
            feature_scaler = joblib.load(feat_scaler_path)
            target_scaler = joblib.load(target_scaler_path)

            # Ideally load best weights (optional snippet placeholder for integration flexibility)
            model_path = "model/weights/premium_forecaster_best.pth"
            loaded_pred_len = pred_len
            if os.path.exists(model_path):
                checkpoint = torch.load(model_path, map_location="cpu", weights_only=True)
                if "fc_out.bias" in checkpoint:
                    bias_size = checkpoint["fc_out.bias"].shape[0]
                    loaded_pred_len = bias_size // 3

            model = PremiumForecaster(
                input_size=6, hidden_size=128, pred_len=loaded_pred_len, output_size=3
            ).to(device)
            
            if os.path.exists(model_path):
                 # Mapped to the dedicated deployment device memory boundary
                 model.load_state_dict(torch.load(model_path, map_location=device, weights_only=True))

            model.eval()
            _predictors_cache[cache_key] = (feature_scaler, target_scaler, model)

        feature_scaler, target_scaler, model = _predictors_cache[cache_key]

    features = [
        "exchange_premium_spread",
        "local_price",
        "foreign_price",
        "fx_rate",
        "base_funding_rate",
        "foreign_funding_rate",
    ]

    try:
        raw_data = historical_df[features].values

        # Downscale asset prices and clamp non-stationary level features [1, 2, 3] to [-3.0, 3.0]
        # while keeping spread & funding [0, 4, 5] exact for residual anchoring.
        scaled_x = feature_scaler.transform(raw_data)
        scaled_x[:, [1, 2, 3]] = np.clip(scaled_x[:, [1, 2, 3]], -3.0, 3.0)

        # Create a batched tensor shape constraint validating properly (batch_size=1, seq_len=len(df), input_size=6)
        input_tensor = torch.tensor(np.array([scaled_x], dtype=np.float32), dtype=torch.float32).to(device)

        with torch.no_grad():
            predictions = model(input_tensor)  # Computes outputs scaled relative predicting Normalized constraints [1, loaded_pred_len, 3]

        preds_scaled = predictions[0].cpu().numpy() # Extract [loaded_pred_len, 3] from inner batch dimension structure
        if len(preds_scaled) >= pred_len:
            preds_scaled = preds_scaled[:pred_len]
        else:
            pad_len = pred_len - len(preds_scaled)
            preds_scaled = np.pad(preds_scaled, ((0, pad_len), (0, 0)), mode="edge")

        # Inverse transformation maps abstract standardized numericals back identically simulating Real Life USD/Spread
        real_preds = target_scaler.inverse_transform(preds_scaled)

        # The initial index '0' refers physically to Exchange_Premium_Spread outputs mapped specifically via targets variable array configuration
        premium_traj = real_preds[:, 0].tolist()

        mean_premium = float(np.mean(premium_traj))
        max_premium = float(np.max(premium_traj))
        min_premium = float(np.min(premium_traj))

        return mean_premium, max_premium, min_premium, premium_traj

    except Exception as e:
        print(f"Error during forecasting inference computations: {e}")
        return 0.0, 0.0, 0.0, []

def append_forecast_features(df: pd.DataFrame, pred_len=480, seq_len=1440, device="cpu") -> pd.DataFrame:
    """
    Batches the entire DataFrame through the pretrained Forecaster to generate an offline 
    'predicted_spread' column over an 8-hour (480-minute) horizon.
    """
    scaler_dir = "model/weights"
    feat_scaler_path = os.path.join(scaler_dir, "feature_scaler.pkl")
    target_scaler_path = os.path.join(scaler_dir, "target_scaler.pkl")
    model_path = "model/weights/premium_forecaster_best.pth"

    if not os.path.exists(model_path) or not os.path.exists(feat_scaler_path):
        print("Forecaster weights missing. Generating fallback zeroes.")
        df_copy = df.copy()
        df_copy["predicted_spread"] = 0.0
        df_copy["predicted_spread_max"] = 0.0
        df_copy["predicted_spread_min"] = 0.0
        return df_copy

    from torch.utils.data import DataLoader
    from data.dataset import CryptoArbitrageDataset
    import joblib
    
    feature_scaler = joblib.load(feat_scaler_path)
    target_scaler = joblib.load(target_scaler_path)

    loaded_pred_len = pred_len
    checkpoint = torch.load(model_path, map_location="cpu", weights_only=True)
    if "fc_out.bias" in checkpoint:
        loaded_pred_len = checkpoint["fc_out.bias"].shape[0] // 3
    
    model = PremiumForecaster(input_size=6, hidden_size=128, pred_len=loaded_pred_len, output_size=3).to(device)
    model.load_state_dict(checkpoint)
    model.eval()

    features = ["exchange_premium_spread", "local_price", "foreign_price", "fx_rate", "base_funding_rate", "foreign_funding_rate"]
    targets = ["exchange_premium_spread", "base_funding_rate", "foreign_funding_rate"]

    scaled_x = feature_scaler.transform(df[features].values)
    scaled_x[:, [1, 2, 3]] = np.clip(scaled_x[:, [1, 2, 3]], -3.0, 3.0)
    dummy_y = np.zeros((len(df), len(targets))) # Bypassed internally during inference
    
    dataset = CryptoArbitrageDataset(scaled_x, dummy_y, seq_len, loaded_pred_len)
    loader = DataLoader(dataset, batch_size=512, shuffle=False)

    all_preds = []
    print(f"Precomputing {len(dataset)} predictions using PremiumForecaster. Please wait...")
    with torch.no_grad():
        for batch_x, _ in loader:
            batch_x = batch_x.to(device)
            preds = model(batch_x)
            all_preds.append(preds.cpu().numpy())
            
    if len(all_preds) == 0:
        df_copy = df.copy()
        df_copy["predicted_spread"] = 0.0
        df_copy["predicted_spread_max"] = 0.0
        df_copy["predicted_spread_min"] = 0.0
        return df_copy

    flat_preds_scaled = np.concatenate(all_preds, axis=0) # [Total_Samples, pred_len, Num_Targets]
    
    # Calculate the average, max, min expected spread globally across the 5 hour projection
    mean_scaled_spread_forecasts = np.mean(flat_preds_scaled[:, :, 0], axis=1)
    max_scaled_spread_forecasts = np.max(flat_preds_scaled[:, :, 0], axis=1)
    min_scaled_spread_forecasts = np.min(flat_preds_scaled[:, :, 0], axis=1)
    
    def inverse_scale_traj(scaled_arr):
        dummy_for_inverse = np.zeros((len(scaled_arr), len(targets)))
        dummy_for_inverse[:, 0] = scaled_arr
        return target_scaler.inverse_transform(dummy_for_inverse)[:, 0]

    unscaled_mean = inverse_scale_traj(mean_scaled_spread_forecasts)
    unscaled_max = inverse_scale_traj(max_scaled_spread_forecasts)
    unscaled_min = inverse_scale_traj(min_scaled_spread_forecasts)

    def pad_and_fill(unscaled):
        # Map the predictions chronologically correctly. 
        # An output from seq_len window [0:1440] provides a prediction explicitly for t=1440.
        final_preds = np.zeros(len(df))
        start_idx = seq_len
        end_idx = start_idx + len(unscaled)
        
        final_preds[start_idx:end_idx] = unscaled
        if len(unscaled) > 0:
            final_preds[:start_idx] = unscaled[0] # Pad initial warmups preventing edge case zero leaks
            final_preds[end_idx:] = unscaled[-1]
        return final_preds

    df_copy = df.copy()
    df_copy["predicted_spread"] = pad_and_fill(unscaled_mean)
    df_copy["predicted_spread_max"] = pad_and_fill(unscaled_max)
    df_copy["predicted_spread_min"] = pad_and_fill(unscaled_min)
    print("Precomputation successfully integrated predicted_spread(mean, max, min) into memory.")
    return df_copy
