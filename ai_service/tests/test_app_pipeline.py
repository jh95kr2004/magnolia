import os
import sys
import collections
import numpy as np

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from app import FXCache, UserAccountCache, build_chart_payload
from model.predictor import extract_trajectory_metrics
from model.rl_agent import PortfolioRLAgent


def test_fx_cache_and_user_account_cache():
    fx = FXCache(r=None)
    assert fx.get_rate("KRW") == 1400.0
    assert fx.get_rate("USD") == 1.0

    with fx.lock:
        fx.fx_rates["KRW"] = 1345.5
    assert fx.get_rate("krw") == 1345.5

    uc = UserAccountCache(r=None)
    default_state = uc.get_state("99")
    assert default_state["available_balance"] == 10000.0
    assert default_state["state"] == 0

    with uc.lock:
        uc.users["1"] = {
            "available_balance": 45000.0,
            "current_exposures": {"UPBIT|BTC-KRW": -0.35},
            "unrealized_pnls": {"UPBIT|BTC-KRW": 180.0},
            "pair_states": {"UPBIT|BTC-KRW": 1},
            "state": 1,
        }
    assert "1" in uc.get_all_users()
    u1 = uc.get_state("1")
    assert u1["available_balance"] == 45000.0
    assert u1["pair_states"]["UPBIT|BTC-KRW"] == 1


def test_ai_prediction_payload_and_chart_schema():
    ai_dir = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    weights_path = os.path.join(ai_dir, "model", "weights", "ppo_arbitrage_final.zip")
    rl_agent = PortfolioRLAgent(model_path=weights_path, device="cpu", n_stack=15)

    feature_deque = collections.deque(maxlen=rl_agent.n_stack)
    for _ in range(rl_agent.n_stack):
        scalar_26 = np.array([
            0.028, 0.020, 0.010, 1.8, 0.008, 0.032, 0.002,
            0.15, 0.50, -0.026, 0.50,
            0.0001, 0.0001, 0.15, 0.002, 0.0, 50000.0, 0.0,
            0.0005, 0.002, 0.0005, 0.003,
            1.0, 0.0, 0.0, 0.0
        ], dtype=np.float32)
        traj_480 = np.linspace(0.0, -0.026, 480, dtype=np.float32)
        state_vec = np.concatenate([scalar_26, traj_480], axis=0)
        feature_deque.append(state_vec)

    stacked_obs = np.concatenate(list(feature_deque), axis=-1).reshape(1, -1)
    assert stacked_obs.shape == (1, 506 * 15)

    action_score, target_weight, action_type = rl_agent.predict(stacked_obs)
    assert 0.0 <= target_weight <= 1.0
    assert action_type in (0, 1, 2)

    # Verify chart payload builder
    history_deque = collections.deque(maxlen=1440)
    history_deque.append({
        "minute_bucket": 1000,
        "timestamp_iso": "2026-10-30T00:00:00+00:00",
        "exchange_premium_spread": 0.0027,
        "ai_past_pred_spread": 0.0024,
    })
    traj = np.linspace(0.0027, 0.028, 480, dtype=np.float32)
    metrics = extract_trajectory_metrics(0.0027, traj, friction_threshold=0.0025)
    chart = build_chart_payload(
        pair_key=("UPBIT", "BTC-KRW", "COINBASE", "BTC-USD"),
        history_deque=history_deque,
        metrics=metrics,
        current_spread=0.0027,
    )
    assert chart["base_exchange"] == "UPBIT"
    assert len(chart["history"]) == 1
    assert "ai_past_pred_pct" in chart["history"][0]
    assert len(chart["forecast"]) >= 96
    assert chart["summary"]["direction"] == "WIDENING"
