import os
import sys
import numpy as np

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from model.rl_agent import PortfolioRLAgent


def _build_stacked_obs(
    spread=0.03,
    mean_pred=0.005,
    min_pred=0.001,
    max_pred=0.035,
    first_wave_edge=None,
    state_idx=0,
    cur_exposure=None,
    n_stack=15,
):
    frame = np.zeros(506, dtype=np.float32)
    frame[0] = spread
    frame[1] = spread * 0.8
    frame[2] = 0.005
    frame[3] = 2.0
    frame[4] = mean_pred
    frame[5] = max_pred
    frame[6] = min_pred
    frame[7] = 0.20  # time_to_max
    frame[8] = 0.50  # time_to_min
    if first_wave_edge is None:
        up_e = max_pred - spread
        dn_e = spread - min_pred
        first_wave_edge = up_e if up_e > dn_e else -dn_e
    frame[9] = first_wave_edge
    frame[10] = 0.20 if first_wave_edge >= 0 else 0.50
    frame[15] = cur_exposure if cur_exposure is not None else (-0.35 if state_idx > 0 else 0.0)
    frame[16] = 10000.0
    frame[18] = 0.0005
    frame[19] = 0.0020
    frame[20] = 0.0005
    frame[21] = 0.0030
    frame[22 + state_idx] = 1.0  # One-hot state
    frame[26:] = np.linspace(0.0, first_wave_edge, 480, dtype=np.float32)
    return np.tile(frame, (1, n_stack))


def test_agent_fallback_heuristic():
    agent = PortfolioRLAgent(model_path="nonexistent_model.zip", n_stack=15)
    assert agent.model is None

    # 1. Tiny spread below friction wall -> flat (target_weight = 0.0)
    obs_flat = _build_stacked_obs(
        spread=0.0005, mean_pred=0.0004, min_pred=0.0003, max_pred=0.0006, first_wave_edge=-0.0001
    )
    score_f, weight_f, act_f = agent.predict(obs_flat)
    assert score_f == 5.0
    assert weight_f == 0.0
    assert act_f == 0

    # 2. High 3% spread with narrowing forecast -> Short Base / Long Foreign (score < 5.0)
    obs_edge = _build_stacked_obs(
        spread=0.03, mean_pred=0.005, min_pred=0.001, max_pred=0.031, first_wave_edge=-0.025
    )
    score_e, weight_e, act_e = agent.predict(obs_edge)
    assert score_e == 2.0
    assert 0.15 <= weight_e <= 0.50
    assert act_e == 0

    # 3. Positive spread (+0.2%) expected to WIDEN to +2.8% in 1h -> Long Base / Short Foreign (score > 5.0)
    obs_widen = _build_stacked_obs(
        spread=0.002, mean_pred=0.008, min_pred=0.001, max_pred=0.028, first_wave_edge=0.026
    )
    score_w, weight_w, act_w = agent.predict(obs_widen)
    assert score_w == 8.0
    assert weight_w >= 0.20
    assert act_w == 0


def test_agent_trained_ppo_inference_across_all_4_states():
    ai_dir = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    weights_path = os.path.join(ai_dir, "model", "weights", "ppo_arbitrage_final.zip")
    agent = PortfolioRLAgent(model_path=weights_path, device="cpu", n_stack=15)
    assert agent.model is not None

    for state_idx in (0, 1, 2, 3):
        obs = _build_stacked_obs(spread=0.025, mean_pred=0.005, state_idx=state_idx)
        score, weight, act_type = agent.predict(obs, use_forecaster=True)
        assert 0.0 <= score <= 10.0
        assert 0.0 <= weight <= 1.0
        assert act_type in (0, 1, 2)

        # Also test RL-Only ablation mode (use_forecaster=False)
        score_no_fc, weight_no_fc, act_no_fc = agent.predict(obs, use_forecaster=False)
        assert 0.0 <= score_no_fc <= 10.0
        assert 0.0 <= weight_no_fc <= 1.0
        assert act_no_fc in (0, 1, 2)
