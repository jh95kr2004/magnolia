import os
import sys
import numpy as np
import pandas as pd
import torch

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from model.predictor import PremiumForecaster, predict_reversion, extract_trajectory_metrics


def test_forecaster_forward_shape_and_ou_reversion():
    model = PremiumForecaster(input_size=6, hidden_size=128, num_layers=2, pred_len=480, output_size=3)
    model.eval()

    # Construct batch of 2 sequences, length 60, 6 features
    # Sequence 0: Historical spread around 0.0, sudden spike at final step to +3.0
    # Sequence 1: Historical spread around 0.0, sudden drop at final step to -3.0
    x = torch.zeros((2, 60, 6), dtype=torch.float32)
    x[0, -1, 0] = 3.0
    x[1, -1, 0] = -3.0

    with torch.no_grad():
        out = model(x)

    assert out.shape == (2, 480, 3)
    assert torch.isfinite(out).all()

    # Verify Ornstein-Uhlenbeck mean reversion anchor pulls spiked spread back toward window mean
    end_pred_high = float(out[0, -1, 0].item())
    end_pred_low = float(out[1, -1, 0].item())
    assert end_pred_high < 3.0
    assert end_pred_low > -3.0


def test_predict_reversion_short_or_empty_df():
    mean_p, max_p, min_p, traj = predict_reversion(None)
    assert (mean_p, max_p, min_p, traj) == (0.0, 0.0, 0.0, [])

    short_df = pd.DataFrame({"exchange_premium_spread": [0.01, 0.02]})
    mean_p2, max_p2, min_p2, traj2 = predict_reversion(short_df)
    assert (mean_p2, max_p2, min_p2, traj2) == (0.0, 0.0, 0.0, [])


def test_predict_reversion_with_pretrained_weights():
    ai_dir = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    cwd = os.getcwd()
    try:
        os.chdir(ai_dir)
        n = 60
        df = pd.DataFrame({
            "exchange_premium_spread": np.linspace(0.01, 0.025, n),
            "local_price": np.full(n, 95000000.0),
            "foreign_price": np.full(n, 68000.0),
            "fx_rate": np.full(n, 1380.0),
            "base_funding_rate": np.full(n, 0.0001),
            "foreign_funding_rate": np.full(n, 0.0001),
        })
        mean_p, max_p, min_p, traj = predict_reversion(df, pred_len=480, device="cpu")
        assert isinstance(mean_p, float)
        assert isinstance(max_p, float)
        assert isinstance(min_p, float)
        assert len(traj) == 480
        assert min_p <= mean_p <= max_p
        assert np.isfinite([mean_p, max_p, min_p]).all()
    finally:
        os.chdir(cwd)


def test_extract_trajectory_metrics_two_wave_detection():
    # Wave 1: starts at +0.2% (0.002), spikes to +2.8% (0.028) at 60m, then reverts to +0.1% (0.001) at 120m
    traj = np.full(480, 0.001, dtype=np.float32)
    traj[:60] = np.linspace(0.002, 0.028, 60)
    traj[60:120] = np.linspace(0.028, 0.001, 60)

    metrics = extract_trajectory_metrics(current_spread=0.002, premium_traj=traj, friction_threshold=0.0025)
    assert abs(metrics["max"] - 0.028) < 1e-4
    assert metrics["max_offset_min"] == 60
    assert metrics["first_wave_edge"] > 0.020
    assert metrics["first_wave_offset_min"] == 60
    assert len(metrics["traj_delta"]) == 480
