import torch
from torch.utils.data import Dataset, DataLoader, WeightedRandomSampler
import pandas as pd
import numpy as np
from typing import Tuple, Optional, List
from sklearn.preprocessing import RobustScaler

FORECASTER_FEATURES_12 = [
    "exchange_premium_spread",
    "tether_real_premium_spread",
    "rel_local_price",
    "rel_foreign_price",
    "fx_rate",
    "foreign_funding_rate",
    "dollar_vol_ratio",
    "bar_duration_min",
    "base_event_listing",
    "base_event_warning_or_susp",
    "base_event_delisting",
    "peer_or_foreign_event",
]

FORECASTER_TARGETS_3 = [
    "exchange_premium_spread",
    "tether_real_premium_spread",
    "foreign_funding_rate",
]


def ensure_12_features(df: pd.DataFrame) -> pd.DataFrame:
    """
    Ensures all 12 scale-invariant forecaster features and 3 target columns exist on `df`.
    Provides 100% backward compatibility when a legacy 6-column DataFrame is passed.
    """
    out = df.copy()
    n = len(out)
    if n == 0:
        return out

    if "fx_rate" not in out.columns:
        out["fx_rate"] = 1380.0
    if "local_price" not in out.columns:
        out["local_price"] = 95000000.0
    if "foreign_price" not in out.columns:
        out["foreign_price"] = 68000.0
    if "base_funding_rate" not in out.columns:
        out["base_funding_rate"] = 0.0
    if "foreign_funding_rate" not in out.columns:
        out["foreign_funding_rate"] = 0.0001

    if "tether_real_premium_spread" not in out.columns:
        if "usdt_krw_rate" in out.columns:
            out["tether_real_premium_spread"] = (
                out["local_price"] / (out["foreign_price"].clip(lower=1e-9) * out["usdt_krw_rate"].clip(lower=500.0))
            ) - 1.0
        else:
            # Default tether basis ~ +1.0% over nominal FX when not explicitly provided
            out["tether_real_premium_spread"] = out["exchange_premium_spread"] - 0.010

    if "rel_local_price" not in out.columns:
        lp = pd.Series(out["local_price"].values.astype(np.float64))
        ema_l = lp.ewm(span=min(240, max(2, n)), min_periods=1).mean().clip(lower=1e-9)
        out["rel_local_price"] = ((lp / ema_l) - 1.0).astype(np.float32)

    if "rel_foreign_price" not in out.columns:
        fp = pd.Series(out["foreign_price"].values.astype(np.float64))
        ema_f = fp.ewm(span=min(240, max(2, n)), min_periods=1).mean().clip(lower=1e-9)
        out["rel_foreign_price"] = ((fp / ema_f) - 1.0).astype(np.float32)

    if "dollar_vol_ratio" not in out.columns:
        if "quote_volume_usd" in out.columns:
            qv = pd.Series(out["quote_volume_usd"].values.astype(np.float64))
            qv_mean = qv.rolling(min(240, max(2, n)), min_periods=1).mean().clip(lower=100.0)
            out["dollar_vol_ratio"] = np.clip((qv / qv_mean).values, 0.05, 20.0).astype(np.float32)
        else:
            out["dollar_vol_ratio"] = np.ones(n, dtype=np.float32)

    if "bar_duration_min" not in out.columns:
        out["bar_duration_min"] = np.ones(n, dtype=np.float32)

    if "base_event_listing" not in out.columns:
        out["base_event_listing"] = (
            out["event_new_listing"].astype(np.float32)
            if "event_new_listing" in out.columns
            else np.zeros(n, dtype=np.float32)
        )
    if "base_event_warning_or_susp" not in out.columns:
        w = out["event_warning"].values if "event_warning" in out.columns else np.zeros(n)
        s = out["event_wallet_suspension"].values if "event_wallet_suspension" in out.columns else np.zeros(n)
        out["base_event_warning_or_susp"] = np.maximum(w, s).astype(np.float32)
    if "base_event_delisting" not in out.columns:
        out["base_event_delisting"] = (
            out["event_delisting"].astype(np.float32)
            if "event_delisting" in out.columns
            else np.zeros(n, dtype=np.float32)
        )
    if "peer_or_foreign_event" not in out.columns:
        out["peer_or_foreign_event"] = np.zeros(n, dtype=np.float32)

    return out


class SymlogRobustScaler:
    """
    Bi-symmetric logarithmic + IQR RobustScaler (`Decision #2` from /grill-me):
      1. Applies `symlog(x) = sign(x) * c * ln(1 + |x| / c)` with `c = 0.05` on heavy-tailed
         spread/price columns so normal +/-5% arbitrage spreads preserve 100% linear precision
         while extreme +300% listing beams and -80% delisting collapses are smoothly compressed.
      2. Applies Median & Interquartile Range (IQR) scaling so outlier episodes never distort
         normal spread scaling.
      3. Exposes `.mean_` and `.scale_` attributes for backward compatibility with direct vector math.
    """

    def __init__(self, symlog_cols: Optional[List[int]] = None, c: float = 0.05):
        self.symlog_cols = symlog_cols if symlog_cols is not None else [0, 1]
        self.c = float(c)
        self.mean_: Optional[np.ndarray] = None
        self.scale_: Optional[np.ndarray] = None

    def _symlog(self, arr: np.ndarray) -> np.ndarray:
        out = arr.astype(np.float64, copy=True)
        for col in self.symlog_cols:
            if col < out.shape[1]:
                x = out[:, col]
                out[:, col] = np.sign(x) * self.c * np.log1p(np.abs(x) / self.c)
        return out

    def _symexp(self, arr: np.ndarray) -> np.ndarray:
        out = arr.astype(np.float64, copy=True)
        for col in self.symlog_cols:
            if col < out.shape[1]:
                y = np.clip(out[:, col], -2.5, 2.5)
                out[:, col] = np.sign(y) * self.c * np.expm1(np.abs(y) / self.c)
        return out

    def fit(self, X: np.ndarray):
        X_sym = self._symlog(np.nan_to_num(X, nan=0.0, posinf=1.0, neginf=-1.0))
        med = np.median(X_sym, axis=0)
        q75 = np.percentile(X_sym, 75, axis=0)
        q25 = np.percentile(X_sym, 25, axis=0)
        iqr = (q75 - q25) / 1.349  # Normal-consistent IQR standard deviation estimator
        std_fallback = np.std(X_sym, axis=0)
        scale = np.where(iqr > 1e-6, iqr, np.where(std_fallback > 1e-6, std_fallback, 1.0))
        self.mean_ = med.astype(np.float64)
        self.scale_ = scale.astype(np.float64)
        return self

    def transform(self, X: np.ndarray) -> np.ndarray:
        X_sym = self._symlog(np.nan_to_num(X, nan=0.0, posinf=1.0, neginf=-1.0))
        scaled = (X_sym - self.mean_[None, :]) / self.scale_[None, :]
        return np.clip(scaled, -8.0, 8.0).astype(np.float32)

    def fit_transform(self, X: np.ndarray) -> np.ndarray:
        return self.fit(X).transform(X)

    def inverse_transform(self, X_scaled: np.ndarray) -> np.ndarray:
        unscaled_sym = X_scaled.astype(np.float64) * self.scale_[None, :] + self.mean_[None, :]
        return self._symexp(unscaled_sym).astype(np.float32)


def resample_to_dynamic_dollar_bars(
    df: pd.DataFrame, rolling_window_bars: int = 240, min_bars_out: int = 16
) -> pd.DataFrame:
    """
    Resamples a chronological DataFrame (or per-pair episodes) into Dynamic Relative Dollar Bars:
      - Computes a rolling dollar volume threshold `Theta_dollar` equal to the rolling mean bar quote volume
        (with Cold-Start adaptation when `< 24h` of history exists, e.g. newly listed coins).
      - Accumulates `quote_volume_usd` and emits a bar whenever cumulative dollar volume >= `Theta_dollar`
        (or max 4 merged calm bars), compressing low-volume overnight noise and expanding high-volume
        announcement/volatility bursts.
      - Updates `bar_duration_min` (Delta t) and `dollar_vol_ratio`.
    """
    df_ensured = ensure_12_features(df)
    if len(df_ensured) <= min_bars_out or "quote_volume_usd" not in df_ensured.columns:
        return df_ensured.reset_index(drop=True)

    def _resample_single_episode(ep: pd.DataFrame) -> pd.DataFrame:
        n = len(ep)
        if n <= min_bars_out:
            return ep

        qvol = np.maximum(ep["quote_volume_usd"].values.astype(np.float64), 100.0)
        # Cold-Start safe rolling baseline: uses min_periods=1 so newly listed coins immediately get a valid threshold
        thresh_arr = (
            pd.Series(qvol)
            .rolling(min(rolling_window_bars, max(2, n)), min_periods=1)
            .mean()
            .clip(lower=100.0)
            .values
        )
        base_dur = ep["bar_duration_min"].values.astype(np.float64)

        # Fast volume-clock emission indices (compresses quiet bars < Theta, expands burst bars >= 1.45 * Theta)
        emit_indices = []
        emit_durations = []
        emit_vol_ratios = []

        acc_vol = 0.0
        acc_dur = 0.0
        steps_merged = 0

        for i in range(n):
            v = qvol[i]
            th = thresh_arr[i]
            d = base_dur[i]

            if v >= 1.45 * th:
                # Flush any pending quiet accumulation first
                if steps_merged > 0:
                    emit_indices.append(max(0, i - 1))
                    emit_durations.append(float(np.clip(acc_dur, 0.25, 360.0)))
                    emit_vol_ratios.append(float(np.clip((acc_vol / steps_merged) / max(th, 100.0), 0.05, 20.0)))
                    acc_vol = 0.0
                    acc_dur = 0.0
                    steps_merged = 0

                # Subdivide high-volume burst candle into k Dollar Bars
                k_sub = min(int(round(v / max(th, 100.0))), 3)
                sub_dur = float(np.clip(d / float(k_sub), 0.20, 360.0))
                vol_r = float(np.clip(v / max(th, 100.0), 0.05, 20.0))
                for _ in range(k_sub):
                    emit_indices.append(i)
                    emit_durations.append(sub_dur)
                    emit_vol_ratios.append(vol_r)
                continue

            acc_vol += v
            acc_dur += d
            steps_merged += 1

            if acc_vol >= th or steps_merged >= 2 or i == n - 1:
                emit_indices.append(i)
                emit_durations.append(float(np.clip(acc_dur, 0.25, 360.0)))
                vol_rate_ratio = (acc_vol / max(steps_merged, 1)) / max(th, 100.0)
                emit_vol_ratios.append(float(np.clip(vol_rate_ratio, 0.05, 20.0)))
                acc_vol = 0.0
                acc_dur = 0.0
                steps_merged = 0

        out_ep = ep.iloc[emit_indices].copy().reset_index(drop=True)
        out_ep["bar_duration_min"] = np.array(emit_durations, dtype=np.float32)
        out_ep["dollar_vol_ratio"] = np.array(emit_vol_ratios, dtype=np.float32)
        return out_ep

    if "pair_id" in df_ensured.columns and df_ensured["pair_id"].nunique() > 1:
        parts = [_resample_single_episode(g) for _, g in df_ensured.groupby("pair_id", sort=False)]
        return pd.concat(parts, ignore_index=True)
    return _resample_single_episode(df_ensured)


def project_dollar_bars_to_time_horizon(
    dollar_bar_preds: np.ndarray,
    expected_bar_duration_min: float = 1.0,
    pred_len_minutes: int = 480,
) -> np.ndarray:
    """
    Re-projects predicted Dollar Bar trajectories `[K, num_targets]` or `[K]` back onto the
    uniform physical wall-clock horizon (`+1m, +2m, ..., +pred_len_minutes`) for UI rendering
    (`PremiumTrajectoryChart.jsx`) and RL 8-hour funding cycle alignment.
    """
    arr = np.asarray(dollar_bar_preds, dtype=np.float32)
    if arr.size == 0:
        return np.zeros((pred_len_minutes,), dtype=np.float32)

    dt = float(np.clip(expected_bar_duration_min, 0.25, 15.0))
    if abs(dt - 1.0) < 0.05 and len(arr) == pred_len_minutes:
        return arr

    k_len = arr.shape[0]
    # Cumulative physical minutes represented by Dollar Bar steps 1..k_len
    dollar_time_grid = np.arange(1, k_len + 1, dtype=np.float32) * dt
    wall_clock_grid = np.arange(1, pred_len_minutes + 1, dtype=np.float32)

    if arr.ndim == 1:
        return np.interp(wall_clock_grid, dollar_time_grid, arr).astype(np.float32)

    out = np.empty((pred_len_minutes, arr.shape[1]), dtype=np.float32)
    for col_i in range(arr.shape[1]):
        out[:, col_i] = np.interp(wall_clock_grid, dollar_time_grid, arr[:, col_i])
    return out


class CryptoArbitrageDataset(Dataset):
    """
    Episode-Boundary-Safe Time-Series Dataset for Multi-Coin Premium & Funding Rate Forecasting.
    Ensures sliding windows `[s_begin : r_end]` never cross between different `(exchange_pair, coin)` episodes.
    """

    def __init__(
        self,
        data_x: np.ndarray,
        data_y: np.ndarray,
        seq_len: int,
        pred_len: int,
        pair_ids: Optional[np.ndarray] = None,
        vol_weights: Optional[np.ndarray] = None,
    ):
        self.seq_len = seq_len
        self.pred_len = pred_len
        self.data_x = data_x.astype(np.float32)
        self.data_y = data_y.astype(np.float32)

        total_win = self.seq_len + self.pred_len
        n = len(data_x)
        if n < total_win:
            self.valid_indices = np.empty(0, dtype=np.int64)
        elif pair_ids is not None and len(pair_ids) == n:
            # Vectorized boundary check: start and end of window must belong to the same pair_id!
            starts = np.arange(0, n - total_win + 1, dtype=np.int64)
            ends = starts + total_win - 1
            same_ep = pair_ids[starts] == pair_ids[ends]
            self.valid_indices = starts[same_ep]
        else:
            self.valid_indices = np.arange(0, n - total_win + 1, dtype=np.int64)

        self.length = len(self.valid_indices)
        self.vol_weights = (
            vol_weights.astype(np.float32)
            if vol_weights is not None and len(vol_weights) == n
            else np.ones(n, dtype=np.float32)
        )

    def __len__(self):
        return self.length

    def __getitem__(self, idx) -> Tuple[torch.Tensor, torch.Tensor]:
        s_begin = int(self.valid_indices[idx])
        s_end = s_begin + self.seq_len
        r_begin = s_end
        r_end = r_begin + self.pred_len

        seq_x = self.data_x[s_begin:s_end]
        seq_y = self.data_y[r_begin:r_end]
        return torch.tensor(seq_x), torch.tensor(seq_y)


def split_train_val_episodes(
    df_proc: pd.DataFrame,
    seq_len: int,
    pred_len: int,
    split_ratio: float = 0.8,
) -> Tuple[pd.DataFrame, pd.DataFrame]:
    """
    Splits `df_proc` into `(train_df, val_df)` following Decision #1 from `/grill-me`:
      - `train_df` strictly contains `split_group == 'train'` (zero future leakage).
      - For held-out event/delisted pairs (where the entire `pair_id` is in `val`), the full episode is in `val_df`.
      - For `CONTINUOUS` pairs (split chronologically at 80% time cutoff), `val_df` includes the causal
        lookback warmup rows right before the validation tail so out-of-sample windows of length
        `seq_len + pred_len` can be evaluated on every continuous coin pair as well.
    """
    min_win = seq_len + pred_len
    if "split_group" in df_proc.columns and (df_proc["split_group"] == "val").any():
        train_df = df_proc[df_proc["split_group"] == "train"].reset_index(drop=True)
        if "pair_id" in df_proc.columns:
            val_parts = []
            for _, g in df_proc.groupby("pair_id", sort=False):
                val_mask = g["split_group"] == "val"
                if not val_mask.any():
                    continue
                if val_mask.all():
                    val_parts.append(g)
                else:
                    # Continuous pair split in time: include causal warmup lookback from the end of g
                    need_len = max(int(val_mask.sum()) + seq_len, min_win + 24)
                    if len(g) >= min_win:
                        val_parts.append(g.iloc[-min(len(g), need_len) :])
            val_df = pd.concat(val_parts, ignore_index=True) if val_parts else df_proc[df_proc["split_group"] == "val"].reset_index(drop=True)
        else:
            val_df = df_proc[df_proc["split_group"] == "val"].reset_index(drop=True)

        if len(train_df) < min_win or len(val_df) < min_win:
            train_size = int(len(df_proc) * split_ratio)
            train_df = df_proc.iloc[:train_size].reset_index(drop=True)
            val_df = df_proc.iloc[max(0, train_size - seq_len) :].reset_index(drop=True)
    else:
        train_size = int(len(df_proc) * split_ratio)
        train_df = df_proc.iloc[:train_size].reset_index(drop=True)
        val_df = df_proc.iloc[max(0, train_size - seq_len) :].reset_index(drop=True)

    return train_df, val_df


def create_dataloaders(
    df: pd.DataFrame,
    seq_len: int,
    pred_len: int,
    features: list,
    targets: list,
    batch_size: int = 64,
    split_ratio: float = 0.8,
    event_sample_ratio: float = 0.30,
    use_dollar_bars: bool = True,
):
    """
    Normalizes multi-coin data with `SymlogRobustScaler` (fitted strictly on Train set),
    respects the Hybrid Train/Val Split (`split_group` column when present),
    prevents cross-episode window leakage, and applies Stratified Regime Sampling (`event_sample_ratio`).
    """
    df_proc = ensure_12_features(df)
    if use_dollar_bars and "quote_volume_usd" in df_proc.columns and len(df_proc) > (seq_len + pred_len) * 3:
        df_proc = resample_to_dynamic_dollar_bars(df_proc)

    # 1. Hybrid Train/Val Split (Decision #1 from /grill-me)
    train_df, val_df = split_train_val_episodes(df_proc, seq_len=seq_len, pred_len=pred_len, split_ratio=split_ratio)

    # Identify spread/relative-price columns for Symlog compression
    symlog_feat_cols = [
        i
        for i, col in enumerate(features)
        if col in ("exchange_premium_spread", "tether_real_premium_spread", "rel_local_price", "rel_foreign_price")
    ]
    symlog_targ_cols = [
        i
        for i, col in enumerate(targets)
        if col in ("exchange_premium_spread", "tether_real_premium_spread")
    ]

    feature_scaler = SymlogRobustScaler(symlog_cols=symlog_feat_cols, c=0.05)
    target_scaler = SymlogRobustScaler(symlog_cols=symlog_targ_cols, c=0.05)

    # CRITICAL: Fit scalers strictly on training set only!
    feature_scaler.fit(train_df[features].values)
    target_scaler.fit(train_df[targets].values)

    train_x_scaled = feature_scaler.transform(train_df[features].values)
    train_y_scaled = target_scaler.transform(train_df[targets].values)

    val_x_scaled = feature_scaler.transform(val_df[features].values)
    val_y_scaled = target_scaler.transform(val_df[targets].values)

    train_pairs = train_df["pair_id"].values if "pair_id" in train_df.columns else None
    val_pairs = val_df["pair_id"].values if "pair_id" in val_df.columns else None

    train_dataset = CryptoArbitrageDataset(train_x_scaled, train_y_scaled, seq_len, pred_len, pair_ids=train_pairs)
    val_dataset = CryptoArbitrageDataset(val_x_scaled, val_y_scaled, seq_len, pred_len, pair_ids=val_pairs)

    # Stratified Regime + Volume-Weighted Sampler (Decision #4 from /grill-me)
    sampler = None
    if len(train_dataset) > 0 and event_sample_ratio > 0.0:
        origins = train_dataset.valid_indices + seq_len - 1
        ev_Listing = train_df["base_event_listing"].values[origins] if "base_event_listing" in train_df.columns else 0.0
        ev_Warn = train_df["base_event_warning_or_susp"].values[origins] if "base_event_warning_or_susp" in train_df.columns else 0.0
        ev_Delist = train_df["base_event_delisting"].values[origins] if "base_event_delisting" in train_df.columns else 0.0
        ev_Peer = np.abs(train_df["peer_or_foreign_event"].values[origins]) if "peer_or_foreign_event" in train_df.columns else 0.0
        vol_r = np.clip(train_df["dollar_vol_ratio"].values[origins], 0.5, 4.0) if "dollar_vol_ratio" in train_df.columns else np.ones(len(origins))

        is_ev = (ev_Listing + ev_Warn + ev_Delist + ev_Peer) > 0.05
        n_ev = int(np.sum(is_ev))
        n_norm = len(is_ev) - n_ev
        if n_ev > 0 and n_norm > 0:
            w_ev = float(event_sample_ratio) / float(n_ev)
            w_norm = float(1.0 - event_sample_ratio) / float(n_norm)
            weights = np.where(is_ev, w_ev, w_norm) * np.sqrt(vol_r)
            weights = weights / np.sum(weights)
            sampler = WeightedRandomSampler(
                weights=torch.from_numpy(weights.astype(np.float64)),
                num_samples=len(train_dataset),
                replacement=True,
            )

    train_loader = DataLoader(
        train_dataset,
        batch_size=batch_size,
        shuffle=(sampler is None),
        sampler=sampler,
        drop_last=(len(train_dataset) >= batch_size),
    )
    val_loader = DataLoader(
        val_dataset,
        batch_size=batch_size,
        shuffle=False,
        drop_last=False,
    )

    return train_loader, val_loader, feature_scaler, target_scaler
