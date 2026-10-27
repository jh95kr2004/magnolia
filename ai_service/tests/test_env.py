import os
import sys
import numpy as np
import pandas as pd

try:
    import pytest
except ImportError:
    pytest = None

# Ensure the root of ai_service is in python path
sys.path.append(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from env.arbitrage_env import ArbitrageEnv
from model.rl_agent import PortfolioRLAgent

def _make_sample_dataframe():
    # Construct a small mock DataFrame for testing
    data = {
        "exchange_premium_spread": [0.1, 0.2, -0.1, 0.3, 0.5],
        "local_price": [1400.0, 1400.0, 1400.0, 1400.0, 1400.0],
        "foreign_price": [1.0, 1.0, 1.0, 1.0, 1.0],
        "fx_rate": [1400.0, 1400.0, 1400.0, 1400.0, 1400.0],
        "base_funding_rate": [0.01, -0.01, 0.02, 0.0, 0.01],
        "foreign_funding_rate": [-0.01, 0.01, 0.03, 0.01, -0.01],
        "sma_1h": [0.1, 0.15, 0.06, 0.125, 0.2],
        "sma_24h": [0.1, 0.15, 0.06, 0.125, 0.2],
        "z_score": [0.0, 0.0, 0.0, 0.0, 0.0],
        "predicted_spread": [0.1, 0.2, 0.0, 0.3, 0.4],
        "predicted_spread_max": [2.0, 3.0, 2.1, 2.4, 2.6],
        "predicted_spread_min": [-2.0, -1.0, -2.1, -2.2, -2.2],
        "bb_width": [0.1, 0.1, 0.1, 0.1, 0.1],
        "macd": [0.0, 0.0, 0.0, 0.0, 0.0],
        "borrow_rate_pct": [0.004, 0.004, 0.004, 0.004, 0.004]
    }
    return pd.DataFrame(data)

if pytest is not None:
    @pytest.fixture
    def sample_dataframe():
        return _make_sample_dataframe()
else:
    def sample_dataframe():
        return _make_sample_dataframe()

def test_env_initialization(sample_dataframe):
    env = ArbitrageEnv(df=sample_dataframe, initial_balance=10000.0)
    obs, info = env.reset()
    
    assert obs.shape == (22,)
    assert env.balance == 10000.0
    assert env.exposure == 0.0
    assert env.state == env.STATE_NEUTRAL

def test_env_step_trade_entry(sample_dataframe):
    # Set execution threshold to 0.05 so a 0.2 exposure can execute
    env = ArbitrageEnv(df=sample_dataframe, initial_balance=10000.0, execution_threshold=0.05)
    env.reset()
    
    # Action index 0: exposure, Index 1: action_decision (-0.5 triggers TRADE)
    action = np.array([0.5, -0.5], dtype=np.float32)
    obs, reward, done, truncated, info = env.step(action)
    
    # State should transition to HOLDING_LEG (1) because we placed a TRADE action
    # and the target exposure (0.5) is >= execution_threshold (0.05)
    assert env.state == env.STATE_HOLDING_LEG
    assert env.exposure == 0.5
    assert abs(env.entry_spread - 0.1) < 1e-6 # from current_step = 0's spread

def test_env_step_transfer_transition(sample_dataframe):
    env = ArbitrageEnv(df=sample_dataframe, initial_balance=10000.0, execution_threshold=0.05)
    env.reset()
    
    # Step 1: Open a position
    env.step(np.array([0.5, -0.5], dtype=np.float32))
    assert env.state == env.STATE_HOLDING_LEG
    
    # Step 2: Trigger TRANSFER (action_decision = 0.0 triggers TRANSFER since it is in [-0.33, 0.33])
    obs, reward, done, truncated, info = env.step(np.array([0.5, 0.0], dtype=np.float32))
    assert env.state == env.STATE_TRANSFERRING
    assert env.transfer_remaining_steps > 0

def test_agent_wrapper_fallback():
    # If no model is loaded, the agent should return basic math heuristics instead of crashing
    agent = PortfolioRLAgent(model_path="nonexistent_path.zip", n_stack=15)
    assert agent.model is None
    
    # Build a realistic decimal spread frame (3% spread = 0.03, predicted = 0.03 -> Strategy C transfer edge = 0.03)
    frame = np.zeros(22, dtype=np.float32)
    frame[0] = 0.03     # current_spread (3%)
    frame[4] = 0.03     # mean_pred (3% persistent premium)
    frame[14] = 0.0005  # base_fee
    frame[15] = 0.002   # foreign_fee
    frame[16] = 0.0005  # slippage
    frame[18] = 1.0     # state_neutral = 1.0
    mock_obs = np.tile(frame, (1, 15))
    action_score, target_weight, action_type = agent.predict(mock_obs)
    
    assert isinstance(action_score, float)
    assert isinstance(target_weight, float)
    assert action_score == 2.0
    assert 0.10 <= target_weight <= 0.50
    assert action_type == 0 # TRADE

if __name__ == "__main__":
    df = _make_sample_dataframe()
    test_env_initialization(df)
    test_env_step_trade_entry(df)
    test_env_step_transfer_transition(df)
    test_agent_wrapper_fallback()
    print("All AI environment & agent tests passed!")
