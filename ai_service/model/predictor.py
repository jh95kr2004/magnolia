import torch
import torch.nn as nn
import numpy as np
import pandas as pd
import os
import joblib
import threading

from data.dataset import (
    FORECASTER_FEATURES_12,
    FORECASTER_TARGETS_3,
    ensure_12_features,
    resample_to_dynamic_dollar_bars,
    project_dollar_bars_to_time_horizon,
)


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
    Context-Conditioned Attention-GRU Forecaster operating on Dynamic Dollar Bars.
    12 Scale-Invariant Inputs:
      0: exchange_premium_spread (Nominal Fiat Kimp)
      1: tether_real_premium_spread (Per-Exchange Real USDT Kimp)
      2: rel_local_price (Rolling normalized Base price)
      3: rel_foreign_price (Rolling normalized Foreign price)
      4: fx_rate (Historical USD/KRW FX rate)
      5: foreign_funding_rate (Foreign Perpetual Implied Funding Rate)
      6: dollar_vol_ratio (Current vs 24h rolling Dollar Volume ratio)
      7: bar_duration_min (Time taken to form Dollar Bar Delta t)
      8: base_event_listing (Causal Domestic New Listing decay signal [0..1])
      9: base_event_warning_or_susp (Causal Domestic Warning / Wallet Suspension gaduri signal [0..1])
      10: base_event_delisting (Causal Domestic Delisting decay signal [0..1])
      11: peer_or_foreign_event (Causal Cross-Exchange spillover signal [-1..1])
    3 Multi-Step Outputs (pred_len steps):
      0: exchange_premium_spread
      1: tether_real_premium_spread
      2: foreign_funding_rate
    """

    def __init__(
        self, input_size=12, hidden_size=128, num_layers=2, pred_len=480, output_size=3
    ):
        super(PremiumForecaster, self).__init__()
        self.input_size = input_size
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
        self.fc_out = nn.Linear(hidden_size, pred_len * output_size)

    def forward(self, x):
        # x: [batch, seq_len, input_size]
        # 1. RevIN-style local window centering to eliminate non-stationary macro level drift
        window_mean = x.mean(dim=1, keepdim=True)
        x_local = x - window_mean

        out, _ = self.gru(x_local)
        context, _ = self.attention(out)  # context: [batch, hidden_size]

        flat_pred = self.fc_out(context)  # [batch, pred_len * output_size]
        delta_predictions = flat_pred.view(-1, self.pred_len, self.output_size)

        # 2. Target Anchor Selection:
        #    - For 12-feature model: [0: exchange_premium_spread, 1: tether_real_premium_spread, 5: foreign_funding_rate]
        #    - For legacy 6-feature model: [0: exchange_premium_spread, 4: base_funding_rate, 5: foreign_funding_rate]
        target_cols = [0, 1, 5] if x.size(2) >= 12 else [0, 4, 5]
        target_seq = x[:, :, target_cols]  # [batch, seq_len, 3]
        seq_len = target_seq.size(1)
        origin_anchor = target_seq[:, -1:, :]  # [batch, 1, 3]

        w_1h = min(seq_len, 60)
        w_4h = min(seq_len, 240)
        w_15m = min(seq_len, 15)

        mean_1h = target_seq[:, -w_1h:, :].mean(dim=1, keepdim=True)
        mean_4h = target_seq[:, -w_4h:, :].mean(dim=1, keepdim=True)
        mean_24h = window_mean[:, :, target_cols]

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

        # (b) Regime-aware short-term dynamics
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
        tau_fast = 0.055
        tau_med = 0.22
        impulse_fast = (t_grid / tau_fast) * torch.exp(1.0 - t_grid / tau_fast) * effective_short_vel
        impulse_med = (t_grid / tau_med) * torch.exp(1.0 - t_grid / tau_med) * (0.40 * med_vel)
        momentum_wave = impulse_fast + impulse_med + flash_snapback

        # (c) Multi-band empirical spectral harmonics from winsorized recent window
        w_spec = min(seq_len, 360)
        if w_spec >= 16:
            recent_win = target_seq[:, -w_spec:, :]
            tau_hist = torch.linspace(0.0, 1.0, w_spec, device=x.device, dtype=x.dtype).view(1, w_spec, 1)
            chord = recent_win[:, :1, :] + (recent_win[:, -1:, :] - recent_win[:, :1, :]) * tau_hist
            detrended = recent_win - chord
            recent_mad = (target_seq[:, -w_1h:, :] - mean_1h).abs().mean(dim=1, keepdim=True).clamp(min=1e-5) * 1.35
            local_std = torch.minimum(detrended.std(dim=1, keepdim=True).clamp(min=1e-5), recent_mad * 1.6)
            detrended_clean = torch.clamp(detrended, -1.5 * local_std, 1.5 * local_std)

            spec = torch.fft.rfft(detrended_clean, dim=1)
            max_k = min(spec.size(1) - 1, 36)
            m_steps = torch.arange(1, self.pred_len + 1, device=x.device, dtype=x.dtype).view(1, self.pred_len, 1)
            harmonic_sum = torch.zeros((x.size(0), self.pred_len, self.output_size), device=x.device, dtype=x.dtype)

            for k in range(2, max_k + 1):
                ck = spec[:, k : k + 1, :] / float(w_spec)
                omega_k = 2.0 * np.pi * float(k) / float(w_spec)
                theta_end = -omega_k
                real_end = ck.real * np.cos(theta_end) - ck.imag * np.sin(theta_end)
                imag_end = ck.real * np.sin(theta_end) + ck.imag * np.cos(theta_end)
                phase_m = omega_k * m_steps
                wave_k = 2.0 * (real_end * (torch.cos(phase_m) - 1.0) - imag_end * torch.sin(phase_m))
                freq_decay = torch.exp(-0.035 * float(k - 2) * t_grid)
                scale_w = 1.0 / (float(k - 1) ** 0.35)
                harmonic_sum = harmonic_sum + wave_k * scale_w * freq_decay

            ctx_gate = 1.0 + 0.25 * torch.tanh(context.mean(dim=1, keepdim=True).unsqueeze(1))
            spectral_damping = torch.exp(-0.75 * t_grid) * (1.0 - torch.exp(-18.0 * t_grid))
            spectral_wave = 0.62 * harmonic_sum * spectral_damping * ctx_gate
            spectral_wave = torch.clamp(spectral_wave, -1.6 * local_std, 1.6 * local_std)
        else:
            spectral_wave = 0.0

        # (d) Learned GRU-Attention residual refinement (amplified when active event catalysts exist)
        if x.size(2) >= 12:
            ev_intensity = x[:, -1:, 8:12].abs().max(dim=2, keepdim=True).values.clamp(0.0, 2.0)
            res_gain = 0.35 + 0.45 * torch.tanh(ev_intensity)
        else:
            res_gain = 0.25
        neural_residual = res_gain * torch.tanh(delta_predictions) * torch.sqrt(t_grid)

        predictions = origin_anchor + ou_reversion + momentum_wave + spectral_wave + neural_residual
        return predictions


_predictors_cache = {}
_cache_lock = threading.Lock()


def _clean_state_dict(checkpoint: dict) -> dict:
    return {k.replace("module.", ""): v for k, v in checkpoint.items()}


def predict_reversion(historical_df: pd.DataFrame, pred_len=480, device="cpu") -> tuple:
    """
    Loads pretrained scalers/model, resamples historical_df into Dynamic Dollar Bars,
    predicts the future trajectory in Dollar Bar space, and re-projects back onto the
    physical 480-minute wall-clock horizon (`+1m ~ +480m`) for UI charts and RL execution.
    """
    if historical_df is None or len(historical_df) < 10:
        return 0.0, 0.0, 0.0, []

    global _predictors_cache
    scaler_dir = "model/weights"
    feat_scaler_path = os.path.join(scaler_dir, "feature_scaler.pkl")
    target_scaler_path = os.path.join(scaler_dir, "target_scaler.pkl")
    model_path = os.path.join(scaler_dir, "premium_forecaster_best.pth")

    if not os.path.exists(feat_scaler_path) or not os.path.exists(target_scaler_path):
        print("Required Scalers missing. Cannot scale input correctly. Aborting forecasting.")
        return 0.0, 0.0, 0.0, []

    mtime = os.path.getmtime(model_path) if os.path.exists(model_path) else 0.0
    cache_key = (pred_len, str(device), mtime)

    with _cache_lock:
        if cache_key not in _predictors_cache:
            feature_scaler = joblib.load(feat_scaler_path)
            target_scaler = joblib.load(target_scaler_path)

            loaded_pred_len = pred_len
            loaded_in_size = 12
            loaded_out_size = 3
            state_dict = None
            if os.path.exists(model_path):
                raw_ckpt = torch.load(model_path, map_location="cpu", weights_only=True)
                state_dict = _clean_state_dict(raw_ckpt)
                if "gru.weight_ih_l0" in state_dict:
                    loaded_in_size = int(state_dict["gru.weight_ih_l0"].shape[1])
                if "fc_out.bias" in state_dict:
                    bias_size = int(state_dict["fc_out.bias"].shape[0])
                    loaded_pred_len = bias_size // loaded_out_size

            model = PremiumForecaster(
                input_size=loaded_in_size,
                hidden_size=128,
                pred_len=loaded_pred_len,
                output_size=loaded_out_size,
            ).to(device)

            if state_dict is not None:
                model.load_state_dict(state_dict)

            model.eval()
            _predictors_cache[cache_key] = (feature_scaler, target_scaler, model, loaded_in_size, loaded_pred_len)

        feature_scaler, target_scaler, model, loaded_in_size, loaded_pred_len = _predictors_cache[cache_key]

    try:
        df_proc = ensure_12_features(historical_df)
        if "quote_volume_usd" in df_proc.columns and len(df_proc) >= 24:
            df_bars = resample_to_dynamic_dollar_bars(df_proc, min_bars_out=12)
        else:
            df_bars = df_proc

        if loaded_in_size == 12:
            features = FORECASTER_FEATURES_12
            raw_data = df_bars[features].values
            scaled_x = feature_scaler.transform(raw_data)
        else:
            features = [
                "exchange_premium_spread",
                "local_price",
                "foreign_price",
                "fx_rate",
                "base_funding_rate",
                "foreign_funding_rate",
            ]
            raw_data = df_bars[features].values
            scaled_x = feature_scaler.transform(raw_data)
            scaled_x[:, [1, 2, 3]] = np.clip(scaled_x[:, [1, 2, 3]], -3.0, 3.0)

        input_tensor = torch.tensor(np.array([scaled_x], dtype=np.float32), dtype=torch.float32).to(device)

        with torch.no_grad():
            predictions = model(input_tensor)

        preds_scaled = predictions[0].cpu().numpy()
        real_preds_dollar = target_scaler.inverse_transform(preds_scaled)

        # Re-project Dollar Bar predictions onto uniform 1-minute wall-clock horizon (+1m .. +pred_len)
        recent_dt = (
            float(np.median(df_bars["bar_duration_min"].values[-10:]))
            if "bar_duration_min" in df_bars.columns and len(df_bars) > 0
            else 1.0
        )
        real_preds_wall = project_dollar_bars_to_time_horizon(
            real_preds_dollar, expected_bar_duration_min=recent_dt, pred_len_minutes=pred_len
        )

        premium_traj = real_preds_wall[:, 0].tolist()
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

    reversal_pullback = 0.0015
    wave_dir = 0
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


def append_forecast_features(df: pd.DataFrame, pred_len=480, seq_len=120, device="cpu") -> pd.DataFrame:
    """
    Batches the DataFrame through the pretrained Forecaster to generate offline
    8-hour (480-minute) trajectory features and attaches the full 480-step relative trajectory matrix.
    """
    scaler_dir = "model/weights"
    feat_scaler_path = os.path.join(scaler_dir, "feature_scaler.pkl")
    target_scaler_path = os.path.join(scaler_dir, "target_scaler.pkl")
    model_path = "model/weights/premium_forecaster_best.pth"

    if not os.path.exists(model_path) or not os.path.exists(feat_scaler_path):
        print("Forecaster weights missing. Generating fallback zeroes.")
        df_copy = ensure_12_features(df).reset_index(drop=True)
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

    df_proc = ensure_12_features(df).reset_index(drop=True)
    feature_scaler = joblib.load(feat_scaler_path)
    target_scaler = joblib.load(target_scaler_path)

    raw_ckpt = torch.load(model_path, map_location="cpu", weights_only=True)
    checkpoint = _clean_state_dict(raw_ckpt)
    loaded_in_size = int(checkpoint["gru.weight_ih_l0"].shape[1]) if "gru.weight_ih_l0" in checkpoint else 12
    loaded_pred_len = int(checkpoint["fc_out.bias"].shape[0] // 3) if "fc_out.bias" in checkpoint else pred_len

    # Adapt seq_len so episodes of length 200~1600 are fully covered
    eff_seq_len = min(seq_len, max(16, len(df_proc) // 4))

    model = PremiumForecaster(
        input_size=loaded_in_size, hidden_size=128, pred_len=loaded_pred_len, output_size=3
    ).to(device)
    model.load_state_dict(checkpoint)
    if str(device).startswith("cuda") and torch.cuda.device_count() > 1:
        model = nn.DataParallel(model)
    model.eval()

    if loaded_in_size == 12:
        features = FORECASTER_FEATURES_12
        scaled_x = feature_scaler.transform(df_proc[features].values)
    else:
        features = [
            "exchange_premium_spread",
            "local_price",
            "foreign_price",
            "fx_rate",
            "base_funding_rate",
            "foreign_funding_rate",
        ]
        scaled_x = feature_scaler.transform(df_proc[features].values)
        scaled_x[:, [1, 2, 3]] = np.clip(scaled_x[:, [1, 2, 3]], -3.0, 3.0)

    dummy_y = np.zeros((len(df_proc), 3), dtype=np.float32)
    # Use seq_len-only sliding window for inference so every row >= eff_seq_len gets a prediction
    dataset = CryptoArbitrageDataset(scaled_x, dummy_y, eff_seq_len, 1)
    loader = DataLoader(dataset, batch_size=2048, shuffle=False)

    n_samples = len(dataset)
    if n_samples == 0:
        df_copy = df_proc.copy()
        df_copy["predicted_spread"] = df_copy["exchange_premium_spread"]
        df_copy["predicted_spread_max"] = df_copy["exchange_premium_spread"] + 0.005
        df_copy["predicted_spread_min"] = df_copy["exchange_premium_spread"] - 0.005
        df_copy["time_to_max"] = 0.5
        df_copy["time_to_min"] = 0.5
        df_copy["first_wave_edge"] = 0.005
        df_copy["first_wave_time"] = 0.5
        df_copy["_row_id"] = np.arange(len(df_copy))
        return df_copy

    unscaled_mean = np.empty(n_samples, dtype=np.float32)
    unscaled_real_mean = np.empty(n_samples, dtype=np.float32)
    unscaled_max = np.empty(n_samples, dtype=np.float32)
    unscaled_min = np.empty(n_samples, dtype=np.float32)
    unscaled_time_to_max = np.empty(n_samples, dtype=np.float32)
    unscaled_time_to_min = np.empty(n_samples, dtype=np.float32)
    first_wave_edge = np.empty(n_samples, dtype=np.float32)
    first_wave_time = np.empty(n_samples, dtype=np.float32)

    start_idx = eff_seq_len - 1
    all_cur_spreads = df_proc["exchange_premium_spread"].values.astype(np.float32)
    friction_thresh = 0.0025
    early_len = min(120, pred_len)

    print(f"Precomputing {n_samples:,} predictions using PremiumForecaster on {device} (streaming batch mode)...")
    offset = 0
    with torch.no_grad():
        for batch_x, _ in loader:
            bsz = batch_x.size(0)
            batch_x = batch_x.to(device, non_blocking=True)
            preds_b = model(batch_x).cpu().numpy()  # [bsz, loaded_pred_len, 3]
            _, p_len, n_targs = preds_b.shape

            unscaled_b = target_scaler.inverse_transform(preds_b.reshape(-1, n_targs)).reshape(
                bsz, p_len, n_targs
            )
            traj_b = unscaled_b[:, : min(p_len, pred_len), 0]
            real_b = unscaled_b[:, : min(p_len, pred_len), 1] if n_targs >= 2 else traj_b

            b_mean = np.mean(traj_b, axis=1)
            b_real_mean = np.mean(real_b, axis=1)
            b_max = np.max(traj_b, axis=1)
            b_min = np.min(traj_b, axis=1)

            b_max_idx = np.argmax(traj_b, axis=1) + 1
            b_min_idx = np.argmin(traj_b, axis=1) + 1
            b_t_max = (b_max_idx / float(pred_len)).astype(np.float32)
            b_t_min = (b_min_idx / float(pred_len)).astype(np.float32)

            cur_s = all_cur_spreads[start_idx + offset : start_idx + offset + bsz]
            deltas_b = traj_b - cur_s[:, None]

            up_e = b_max - cur_s
            dn_e = cur_s - b_min

            e_len = min(early_len, deltas_b.shape[1])
            e_max_d = np.max(deltas_b[:, :e_len], axis=1)
            e_min_d = np.min(deltas_b[:, :e_len], axis=1)
            e_max_i = np.argmax(deltas_b[:, :e_len], axis=1) + 1
            e_min_i = np.argmin(deltas_b[:, :e_len], axis=1) + 1

            e_dn_abs = -e_min_d
            c_e_up = (e_max_d >= friction_thresh) & ((e_dn_abs < friction_thresh) | (e_max_i <= e_min_i))
            c_e_dn = (~c_e_up) & (e_dn_abs >= friction_thresh)
            c_g_both = (up_e >= friction_thresh) & (dn_e >= friction_thresh)
            c_g_up = np.where(c_g_both, b_max_idx <= b_min_idx, up_e >= dn_e)

            b_fw_edge = np.where(
                c_e_up, e_max_d, np.where(c_e_dn, e_min_d, np.where(c_g_up, up_e, -dn_e))
            ).astype(np.float32)
            b_fw_time = np.where(
                c_e_up,
                e_max_i / float(pred_len),
                np.where(c_e_dn, e_min_i / float(pred_len), np.where(c_g_up, b_t_max, b_t_min)),
            ).astype(np.float32)

            unscaled_mean[offset : offset + bsz] = b_mean
            unscaled_real_mean[offset : offset + bsz] = b_real_mean
            unscaled_max[offset : offset + bsz] = b_max
            unscaled_min[offset : offset + bsz] = b_min
            unscaled_time_to_max[offset : offset + bsz] = b_t_max
            unscaled_time_to_min[offset : offset + bsz] = b_t_min
            first_wave_edge[offset : offset + bsz] = b_fw_edge
            first_wave_time[offset : offset + bsz] = b_fw_time
            offset += bsz

    end_idx = start_idx + n_samples

    def pad_and_fill(unscaled):
        final_preds = np.zeros(len(df_proc), dtype=np.float32)
        final_preds[start_idx:end_idx] = unscaled
        if len(unscaled) > 0:
            final_preds[:start_idx] = unscaled[0]
            final_preds[end_idx:] = unscaled[-1]
        return final_preds

    df_copy = df_proc.copy()
    df_copy["predicted_spread"] = pad_and_fill(unscaled_mean)
    df_copy["predicted_real_spread"] = pad_and_fill(unscaled_real_mean)
    df_copy["predicted_spread_max"] = pad_and_fill(unscaled_max)
    df_copy["predicted_spread_min"] = pad_and_fill(unscaled_min)
    df_copy["time_to_max"] = pad_and_fill(unscaled_time_to_max)
    df_copy["time_to_min"] = pad_and_fill(unscaled_time_to_min)
    df_copy["first_wave_edge"] = pad_and_fill(first_wave_edge)
    df_copy["first_wave_time"] = pad_and_fill(first_wave_time)
    df_copy["_row_id"] = np.arange(len(df_copy))
    print("Precomputation successfully integrated 480-step trajectory features into memory.")
    return df_copy
