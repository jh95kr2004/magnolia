import torch
from torch.utils.data import Dataset, DataLoader
import pandas as pd
import numpy as np
from typing import Tuple
from sklearn.preprocessing import StandardScaler


class CryptoArbitrageDataset(Dataset):
    """
    Time-series Dataset for Premium and Funding Rate forecasting.
    Expects pre-scaled numpy arrays to safely support Deep Learning requirements.
    """

    def __init__(
        self,
        data_x: np.ndarray,
        data_y: np.ndarray,
        seq_len: int,
        pred_len: int,
    ):
        """
        Args:
            data_x: Fully scaled historical features array.
            data_y: Fully scaled target prediction variables array.
            seq_len: Past timesteps size corresponding to Context Sequence length.
            pred_len: Future timesteps dimension mapping the Recurrent Prediction Output.
        """
        self.seq_len = seq_len
        self.pred_len = pred_len

        # Assure correct bit precision for standard PyTorch matrix integrations
        self.data_x = data_x.astype(np.float32)
        self.data_y = data_y.astype(np.float32)

        # The maximum possible starting index is (total - seq_len - pred_len + 1)
        self.length = len(data_x) - self.seq_len - self.pred_len + 1

    def __len__(self):
        return max(0, self.length)

    def __getitem__(self, idx) -> Tuple[torch.Tensor, torch.Tensor]:
        # Input sequence range [idx : idx + seq_len]
        s_begin = idx
        s_end = s_begin + self.seq_len

        # Target sequence projection window [idx + seq_len : idx + seq_len + pred_len]
        r_begin = s_end
        r_end = r_begin + self.pred_len

        # Slice multidimensional np arrays iteratively
        seq_x = self.data_x[s_begin:s_end]
        seq_y = self.data_y[r_begin:r_end]

        return torch.tensor(seq_x), torch.tensor(seq_y)


def create_dataloaders(
    df: pd.DataFrame,
    seq_len: int,
    pred_len: int,
    features: list,
    targets: list,
    batch_size: int = 64,
    split_ratio: float = 0.8,
):
    """
    Helper function to normalize data, safely split domains, and return DataLoaders alongside the Scalers
    so that inverse transformations can happen predictably mapping neural signals to true real world dollars/decimals.
    """
    # 1. Sequentially Splitting Time Series data to absolutely prevent data leakage
    # We never train a temporal RNN horizontally over random epochs. Time flows forward.
    train_size = int(len(df) * split_ratio)
    train_df = df.iloc[:train_size]
    val_df = df.iloc[train_size:]

    # 2. Data Normalization (Scaling down huge asset prices to match tight volatility indexes)
    feature_scaler = StandardScaler()
    target_scaler = StandardScaler()

    # CRITICAL: We only .fit() scalers on the training set. If we peeked at validation set max/mins, 
    # the network would theoretically have a mathematical data leak of future metrics!
    feature_scaler.fit(train_df[features].values)
    target_scaler.fit(train_df[targets].values)

    # 3. Transform (Scale down) BOTH sets using exclusively Training set thresholds
    train_x_scaled = feature_scaler.transform(train_df[features].values)
    train_y_scaled = target_scaler.transform(train_df[targets].values)

    val_x_scaled = feature_scaler.transform(val_df[features].values)
    val_y_scaled = target_scaler.transform(val_df[targets].values)

    # 4. Instantiate Custom Datasets bypassing original pandas raw ingestion
    train_dataset = CryptoArbitrageDataset(train_x_scaled, train_y_scaled, seq_len, pred_len)
    val_dataset = CryptoArbitrageDataset(val_x_scaled, val_y_scaled, seq_len, pred_len)

    # 5. Build Dataloaders (Overlapping time-series blocks technically form i.i.d samples, so shuffle=True is fine)
    train_loader = DataLoader(
        train_dataset, batch_size=batch_size, shuffle=True, drop_last=True
    )
    val_loader = DataLoader(
        val_dataset, batch_size=batch_size, shuffle=False, drop_last=True
    )

    return train_loader, val_loader, feature_scaler, target_scaler
