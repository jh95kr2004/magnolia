import os
import sys
import collections
import numpy as np

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from app import FXCache, UserAccountCache
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


def test_ai_prediction_payload_schema_generation():
    ai_dir = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    weights_path = os.path.join(ai_dir, "model", "weights", "ppo_arbitrage_final.zip")
    rl_agent = PortfolioRLAgent(model_path=weights_path, device="cpu", n_stack=15)

    feature_deque = collections.deque(maxlen=rl_agent.n_stack)
    for i in range(rl_agent.n_stack):
        state_vec = np.array([
            0.028, 0.020, 0.010, 1.8, 0.008, 0.032, 0.002,
            0.0001, 0.0001, 0.15, 0.002, 0.0, 50000.0, 0.0,
            0.0005, 0.002, 0.0005, 0.003,
            1.0, 0.0, 0.0, 0.0
        ], dtype=np.float32)
        feature_deque.append(state_vec)

    stacked_obs = np.concatenate(list(feature_deque), axis=-1).reshape(1, -1)
    assert stacked_obs.shape == (1, 330)

    action_score, target_weight, action_type = rl_agent.predict(stacked_obs)
    payload = {
        "user_id": 1,
        "base_exchange": "UPBIT",
        "foreign_exchange": "COINBASE",
        "base_symbol": "BTC-KRW",
        "foreign_symbol": "BTC-USD",
        "action_score": float(action_score),
        "target_weight": float(target_weight),
        "action_type": int(action_type),
        "premium_pct": 0.028,
        "predicted_premium_pct": 0.008,
    }

    assert payload["base_exchange"] == "UPBIT"
    assert payload["foreign_exchange"] == "COINBASE"
    assert 0.0 <= payload["target_weight"] <= 1.0
    assert payload["action_type"] in (0, 1, 2)
