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

        # 2. Multi-scale trajectory synthesis anchored at current origin x_t:
        #    Combines (a) multi-horizon OU equilibrium reversion (1h / 4h / 24h),
        #    (b) short-term microstructure momentum impulse & pullback (0-90m),
        #    (c) phase-continuous damped empirical spectral harmonics (rFFT on winsorized local history),
        #    and (d) GRU-Attention neural context residual.
        target_seq = x[:, :, [0, 4, 5]]  # [batch, seq_len, 3]
        seq_len = target_seq.size(1)
        origin_anchor = target_seq[:, -1:, :]  # [batch, 1, 3]

        w_1h = min(seq_len, 60)
        w_4h = min(seq_len, 240)
        w_15m = min(seq_len, 15)

        mean_1h = target_seq[:, -w_1h:, :].mean(dim=1, keepdim=True)
        mean_4h = target_seq[:, -w_4h:, :].mean(dim=1, keepdim=True)
        mean_24h = window_mean[:, :, [0, 4, 5]]

        dev_1h = mean_1h - origin_anchor
        dev_4h = mean_4h - origin_anchor
        dev_24h = mean_24h - origin_anchor

        t_grid = torch.linspace(
            1.0 / self.pred_len, 1.0, self.pred_len, device=x.device, dtype=x.dtype
        ).view(1, self.pred_len, 1)

        # (a) Multi-rate Ornstein-Uhlenbeck reversion across 1h, 4h, and 24h horizons
        fast_pullback = (1.0 - torch.exp(-5.0 * t_grid)) * torch.exp(-2.2 * t_grid) * dev_1h
        med_reversion = (1.0 - torch.exp(-1.4 * t_grid)) * 0.45 * dev_4h
        macro_reversion = (1.0 - torch.exp(-0.45 * t_grid)) * 0.55 * dev_24h
        ou_reversion = fast_pullback + med_reversion + macro_reversion

        # (b) Regime-aware short-term dynamics:
        #     Distinguishes sustained multi-step breakout acceleration from 1-2m flash-spike exhaustion
        w_10m = min(seq_len, 10)
        w_30m = min(seq_len, 30)
        recent_10m_med = target_seq[:, -w_10m:, :].median(dim=1, keepdim=True).values
        recent_15m_mean = target_seq[:, -w_15m:, :].mean(dim=1, keepdim=True)
        std_30m = target_seq[:, -w_30m:, :].std(dim=1, keepdim=True).clamp(min=1e-4)

        if seq_len >= 3:
            vel_1 = target_seq[:, -1:, :] - target_seq[:, -2:-1, :]
            vel_2 = target_seq[:, -2:-1, :] - target_seq[:, -3:-2, :]
            accel = torch.clamp(vel_1 - vel_2, -2.0 * std_30m, 2.0 * std_30m)
        else:
            vel_1 = origin_anchor - recent_10m_med
            vel_2 = vel_1
            accel = torch.zeros_like(vel_1)

        spike_dev = origin_anchor - recent_10m_med
        is_sustained = (vel_1 * vel_2 > 0) & (vel_1 * spike_dev >= 0)
        is_exhausted = (spike_dev.abs() > 1.3 * std_30m) & (~is_sustained)

        # Sustained breakout carries forward with acceleration lead; exhausted flash spike snaps back within minutes
        effective_short_vel = torch.where(
            is_sustained,
            0.55 * vel_1 + 0.25 * accel + 0.35 * (origin_anchor - recent_15m_mean),
            torch.where(
                is_exhausted,
                -0.25 * spike_dev,
                0.35 * (origin_anchor - recent_15m_mean),
            ),
        )
        flash_snapback = torch.where(
            is_exhausted,
            -(1.0 - torch.exp(-32.0 * t_grid)) * torch.exp(-1.5 * t_grid) * (0.65 * spike_dev),
            torch.zeros_like(t_grid),
        )

        med_vel = recent_15m_mean - mean_1h
        tau_fast = 0.055  # peaks around ~26 minutes
        tau_med = 0.22    # peaks around ~105 minutes
        impulse_fast = (t_grid / tau_fast) * torch.exp(1.0 - t_grid / tau_fast) * effective_short_vel
        impulse_med = (t_grid / tau_med) * torch.exp(1.0 - t_grid / tau_med) * (0.40 * med_vel)
        momentum_wave = impulse_fast + impulse_med + flash_snapback

        # (c) Multi-band empirical spectral harmonics from winsorized recent window
        #     Captures macro (1.5h-3h), meso (30m-60m), and micro (10m-25m) cyclical market structure
        w_spec = min(seq_len, 360)
        if w_spec >= 16:
            recent_win = target_seq[:, -w_spec:, :]
            # Linear chord detrending between start and end of recent_win
            tau_hist = torch.linspace(0.0, 1.0, w_spec, device=x.device, dtype=x.dtype).view(1, w_spec, 1)
            chord = recent_win[:, :1, :] + (recent_win[:, -1:, :] - recent_win[:, :1, :]) * tau_hist
            detrended = recent_win - chord
            # Winsorize extreme outlier flash spikes to +/- 1.8 sigma while preserving microstructure texture
            local_std = detrended.std(dim=1, keepdim=True).clamp(min=1e-5)
            detrended_clean = torch.clamp(detrended, -1.8 * local_std, 1.8 * local_std)

            spec = torch.fft.rfft(detrended_clean, dim=1)  # [batch, w_spec//2 + 1, 3]
            max_k = min(spec.size(1) - 1, 36)
            m_steps = torch.arange(1, self.pred_len + 1, device=x.device, dtype=x.dtype).view(1, self.pred_len, 1)
            harmonic_sum = torch.zeros((x.size(0), self.pred_len, self.output_size), device=x.device, dtype=x.dtype)

            for k in range(2, max_k + 1):
                ck = spec[:, k : k + 1, :] / float(w_spec)  # [batch, 1, 3]
                omega_k = 2.0 * np.pi * float(k) / float(w_spec)
                # Phase at the final historical step (n = w_spec - 1 == -1 mod w_spec)
                theta_end = -omega_k
                real_end = ck.real * np.cos(theta_end) - ck.imag * np.sin(theta_end)
                imag_end = ck.real * np.sin(theta_end) + ck.imag * np.cos(theta_end)
                phase_m = omega_k * m_steps
                # Zero-anchored at m=0 so there is zero discontinuity at NOW
                wave_k = 2.0 * (real_end * (torch.cos(phase_m) - 1.0) - imag_end * torch.sin(phase_m))
                # Higher frequency modes decay slightly faster over the 8h horizon as phase uncertainty grows
                freq_decay = torch.exp(-0.035 * float(k - 2) * t_grid)
                scale_w = 1.0 / (float(k - 1) ** 0.35)
                harmonic_sum = harmonic_sum + wave_k * scale_w * freq_decay

            # Gate harmonic amplitude by GRU attention context and damp smoothly over 8h horizon
            ctx_gate = 1.0 + 0.25 * torch.tanh(context.mean(dim=1, keepdim=True).unsqueeze(1))
            spectral_damping = torch.exp(-0.75 * t_grid) * (1.0 - torch.exp(-18.0 * t_grid))
            spectral_wave = 0.62 * harmonic_sum * spectral_damping * ctx_gate
            spectral_wave = torch.clamp(spectral_wave, -1.6 * local_std, 1.6 * local_std)
        else:
            spectral_wave = 0.0

        # (d) Learned GRU-Attention residual refinement
        neural_residual = 0.25 * torch.tanh(delta_predictions) * torch.sqrt(t_grid)

        predictions = origin_anchor + ou_reversion + momentum_wave + spectral_wave + neural_residual
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


def extract_trajectory_metrics(
    current_spread: float,
    premium_traj,
    friction_threshold: float = 0.0025,
    pred_len: int = 480,
) -> dict:
    """
    Extracts timing, first-inflection wave, and 480-minute relative delta trajectory
    from a raw 8-hour (480-step) predicted spread sequence.
    """
    if premium_traj is None or len(premium_traj) == 0:
        traj = np.full(pred_len, float(current_spread), dtype=np.float32)
    else:
        traj = np.asarray(premium_traj, dtype=np.float32).flatten()
        if len(traj) >= pred_len:
            traj = traj[:pred_len]
        else:
            traj = np.pad(traj, (0, pred_len - len(traj)), mode="edge")

    cur = float(current_spread)
    deltas = (traj - cur).astype(np.float32)

    mean_pred = float(np.mean(traj))
    max_idx_0 = int(np.argmax(traj))
    min_idx_0 = int(np.argmin(traj))

    max_pred = float(traj[max_idx_0])
    min_pred = float(traj[min_idx_0])
    max_offset_min = max_idx_0 + 1
    min_offset_min = min_idx_0 + 1
    time_to_max = float(max_offset_min) / float(pred_len)
    time_to_min = float(min_offset_min) / float(pred_len)

    # Scan chronologically for the first actionable inflection wave exceeding friction_threshold
    reversal_pullback = 0.0015
    wave_dir = 0  # +1 for upward wave, -1 for downward wave
    running_ext_delta = 0.0
    running_ext_idx = 0
    found_wave = False

    for idx in range(len(deltas)):
        d = float(deltas[idx])
        if wave_dir == 0:
            if d >= friction_threshold:
                wave_dir = 1
                running_ext_delta = d
                running_ext_idx = idx
            elif d <= -friction_threshold:
                wave_dir = -1
                running_ext_delta = d
                running_ext_idx = idx
        elif wave_dir == 1:
            if d > running_ext_delta:
                running_ext_delta = d
                running_ext_idx = idx
            elif running_ext_delta - d >= reversal_pullback:
                found_wave = True
                break
        elif wave_dir == -1:
            if d < running_ext_delta:
                running_ext_delta = d
                running_ext_idx = idx
            elif d - running_ext_delta >= reversal_pullback:
                found_wave = True
                break

    if found_wave or wave_dir != 0:
        first_wave_edge = float(running_ext_delta)
        first_wave_offset_min = int(running_ext_idx + 1)
    else:
        up_edge = max_pred - cur
        down_edge = cur - min_pred
        if up_edge >= friction_threshold and down_edge >= friction_threshold:
            if max_offset_min <= min_offset_min:
                first_wave_edge = float(up_edge)
                first_wave_offset_min = max_offset_min
            else:
                first_wave_edge = float(-down_edge)
                first_wave_offset_min = min_offset_min
        elif up_edge >= down_edge:
            first_wave_edge = float(up_edge)
            first_wave_offset_min = max_offset_min
        else:
            first_wave_edge = float(-down_edge)
            first_wave_offset_min = min_offset_min

    first_wave_time = float(first_wave_offset_min) / float(pred_len)
    first_wave_target = float(cur + first_wave_edge)

    return {
        "mean": mean_pred,
        "max": max_pred,
        "min": min_pred,
        "max_offset_min": int(max_offset_min),
        "min_offset_min": int(min_offset_min),
        "time_to_max": float(time_to_max),
        "time_to_min": float(time_to_min),
        "first_wave_edge": float(first_wave_edge),
        "first_wave_time": float(first_wave_time),
        "first_wave_offset_min": int(first_wave_offset_min),
        "first_wave_target": float(first_wave_target),
        "traj": traj.tolist(),
        "traj_delta": deltas,
    }


def append_forecast_features(df: pd.DataFrame, pred_len=480, seq_len=1440, device="cpu") -> pd.DataFrame:
    """
    Batches the entire DataFrame through the pretrained Forecaster to generate offline 
    8-hour (480-minute) trajectory features and attaches the full 480-step relative trajectory matrix.
    """
    scaler_dir = "model/weights"
    feat_scaler_path = os.path.join(scaler_dir, "feature_scaler.pkl")
    target_scaler_path = os.path.join(scaler_dir, "target_scaler.pkl")
    model_path = "model/weights/premium_forecaster_best.pth"

    if not os.path.exists(model_path) or not os.path.exists(feat_scaler_path):
        print("Forecaster weights missing. Generating fallback zeroes.")
        df_copy = df.copy().reset_index(drop=True)
        df_copy["predicted_spread"] = 0.0
        df_copy["predicted_spread_max"] = 0.0
        df_copy["predicted_spread_min"] = 0.0
        df_copy["time_to_max"] = 0.5
        df_copy["time_to_min"] = 0.5
        df_copy["first_wave_edge"] = 0.0
        df_copy["first_wave_time"] = 0.5
        df_copy["_row_id"] = np.arange(len(df_copy))
        df_copy.attrs["full_traj_deltas"] = np.zeros((len(df_copy), pred_len), dtype=np.float32)
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
        df_copy = df.copy().reset_index(drop=True)
        df_copy["predicted_spread"] = 0.0
        df_copy["predicted_spread_max"] = 0.0
        df_copy["predicted_spread_min"] = 0.0
        df_copy["time_to_max"] = 0.5
        df_copy["time_to_min"] = 0.5
        df_copy["first_wave_edge"] = 0.0
        df_copy["first_wave_time"] = 0.5
        df_copy["_row_id"] = np.arange(len(df_copy))
        df_copy.attrs["full_traj_deltas"] = np.zeros((len(df_copy), pred_len), dtype=np.float32)
        return df_copy

    flat_preds_scaled = np.concatenate(all_preds, axis=0) # [Total_Samples, loaded_pred_len, Num_Targets]
    spread_scaled_matrix = flat_preds_scaled[:, :, 0]
    if spread_scaled_matrix.shape[1] >= pred_len:
        spread_scaled_matrix = spread_scaled_matrix[:, :pred_len]
    else:
        pad_w = pred_len - spread_scaled_matrix.shape[1]
        spread_scaled_matrix = np.pad(spread_scaled_matrix, ((0, 0), (0, pad_w)), mode="edge")

    # Inverse scale full 480-step spread trajectories directly using target_scaler column 0 parameters
    scale_0 = float(target_scaler.scale_[0])
    mean_0 = float(target_scaler.mean_[0])
    unscaled_traj_matrix = (spread_scaled_matrix * scale_0 + mean_0).astype(np.float32)

    unscaled_mean = np.mean(unscaled_traj_matrix, axis=1)
    unscaled_max = np.max(unscaled_traj_matrix, axis=1)
    unscaled_min = np.min(unscaled_traj_matrix, axis=1)
    max_idx = np.argmax(unscaled_traj_matrix, axis=1) + 1
    min_idx = np.argmin(unscaled_traj_matrix, axis=1) + 1
    unscaled_time_to_max = (max_idx / float(pred_len)).astype(np.float32)
    unscaled_time_to_min = (min_idx / float(pred_len)).astype(np.float32)

    start_idx = seq_len
    end_idx = start_idx + len(unscaled_traj_matrix)
    sample_cur_spreads = df["exchange_premium_spread"].values[start_idx:end_idx].astype(np.float32)
    sample_deltas = unscaled_traj_matrix - sample_cur_spreads[:, None]

    # Compute first-wave edge and time across all samples
    friction_thresh = 0.0025
    up_edges = unscaled_max - sample_cur_spreads
    down_edges = sample_cur_spreads - unscaled_min

    # Also inspect early 2-hour window (120 mins) for immediate wave inflection
    early_len = min(120, pred_len)
    early_max_delta = np.max(sample_deltas[:, :early_len], axis=1)
    early_min_delta = np.min(sample_deltas[:, :early_len], axis=1)
    early_max_idx = np.argmax(sample_deltas[:, :early_len], axis=1) + 1
    early_min_idx = np.argmin(sample_deltas[:, :early_len], axis=1) + 1

    e_dn_abs = -early_min_delta
    cond_early_up = (early_max_delta >= friction_thresh) & (
        (e_dn_abs < friction_thresh) | (early_max_idx <= early_min_idx)
    )
    cond_early_dn = (~cond_early_up) & (e_dn_abs >= friction_thresh)
    cond_global_both = (up_edges >= friction_thresh) & (down_edges >= friction_thresh)
    cond_global_up = np.where(cond_global_both, max_idx <= min_idx, up_edges >= down_edges)

    first_wave_edge = np.where(
        cond_early_up,
        early_max_delta,
        np.where(
            cond_early_dn,
            early_min_delta,
            np.where(cond_global_up, up_edges, -down_edges),
        ),
    ).astype(np.float32)

    first_wave_time = np.where(
        cond_early_up,
        early_max_idx / float(pred_len),
        np.where(
            cond_early_dn,
            early_min_idx / float(pred_len),
            np.where(cond_global_up, unscaled_time_to_max, unscaled_time_to_min),
        ),
    ).astype(np.float32)

    def pad_and_fill(unscaled):
        final_preds = np.zeros(len(df), dtype=np.float32)
        final_preds[start_idx:end_idx] = unscaled
        if len(unscaled) > 0:
            final_preds[:start_idx] = unscaled[0]
            final_preds[end_idx:] = unscaled[-1]
        return final_preds

    full_traj_deltas = np.zeros((len(df), pred_len), dtype=np.float32)
    full_traj_deltas[start_idx:end_idx] = sample_deltas
    if len(sample_deltas) > 0:
        full_traj_deltas[:start_idx] = sample_deltas[0]
        full_traj_deltas[end_idx:] = sample_deltas[-1]

    df_copy = df.copy().reset_index(drop=True)
    df_copy["predicted_spread"] = pad_and_fill(unscaled_mean)
    df_copy["predicted_spread_max"] = pad_and_fill(unscaled_max)
    df_copy["predicted_spread_min"] = pad_and_fill(unscaled_min)
    df_copy["time_to_max"] = pad_and_fill(unscaled_time_to_max)
    df_copy["time_to_min"] = pad_and_fill(unscaled_time_to_min)
    df_copy["first_wave_edge"] = pad_and_fill(first_wave_edge)
    df_copy["first_wave_time"] = pad_and_fill(first_wave_time)
    df_copy["_row_id"] = np.arange(len(df_copy))
    df_copy.attrs["full_traj_deltas"] = full_traj_deltas
    print("Precomputation successfully integrated 480-step trajectories & inflection features into memory.")
    return df_copy
