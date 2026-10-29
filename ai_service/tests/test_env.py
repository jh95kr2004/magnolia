import os
import sys
import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from env.arbitrage_env import ArbitrageEnv


def _make_sample_dataframe(spreads=None):
    if spreads is None:
        spreads = [0.03, 0.032, 0.028, 0.025, 0.02, 0.015, 0.01, 0.005, 0.002, 0.001, -0.002, -0.003, 0.0, 0.01, 0.02]
    n = len(spreads)
    data = {
        "exchange_premium_spread": spreads,
        "local_price": [95000000.0] * n,
        "foreign_price": [68000.0] * n,
        "fx_rate": [1380.0] * n,
        "base_funding_rate": [0.0001] * n,
        "foreign_funding_rate": [0.0001] * n,
        "predicted_spread": [s * 0.6 for s in spreads],
        "predicted_spread_max": [s + 0.005 for s in spreads],
        "predicted_spread_min": [s - 0.015 for s in spreads],
        "borrow_rate_pct": [0.004] * n,
    }
    return pd.DataFrame(data)


@pytest.fixture
def sample_dataframe():
    return _make_sample_dataframe()


def test_env_initialization(sample_dataframe):
    env = ArbitrageEnv(df=sample_dataframe, initial_balance=10000.0, is_eval=True)
    obs, info = env.reset()

    assert obs.shape == (506,)
    assert env.balance == 10000.0
    assert env.exposure == 0.0
    assert env.state == env.STATE_NEUTRAL
    assert abs(env.foreign_usd_skew - 0.50) < 1e-6


def test_env_step_trade_entry(sample_dataframe):
    env = ArbitrageEnv(df=sample_dataframe, initial_balance=10000.0, execution_threshold=0.05, is_eval=True)
    env.reset()

    # Short Base / Long Foreign entry (exposure = -0.5, action_decision = 0.0 -> TRADE)
    action = np.array([-0.5, 0.0], dtype=np.float32)
    obs, reward, done, truncated, info = env.step(action)

    assert env.state == env.STATE_HOLDING_LEG
    assert abs(env.exposure - (-0.5)) < 1e-6
    assert abs(env.entry_spread - 0.03) < 1e-6
    assert obs[23] == 1.0  # state_holding one-hot (index 23 in 26-scalar block)


def test_env_full_4state_fsm_transfer_and_repay():
    spreads = [0.035] * 20
    df = _make_sample_dataframe(spreads)
    env = ArbitrageEnv(df=df, initial_balance=10000.0, execution_threshold=0.05, is_eval=True)
    env.reset()

    # 1. NEUTRAL -> HOLDING_LEG
    env.step(np.array([-0.5, 0.0], dtype=np.float32))
    assert env.state == env.STATE_HOLDING_LEG

    # 2. HOLDING_LEG -> TRANSFERRING (action_dec = 0.8 > 0.33 triggers Strategy C Transfer)
    env.step(np.array([-0.5, 0.8], dtype=np.float32))
    assert env.state == env.STATE_TRANSFERRING
    assert env.transfer_remaining_steps == 8
    assert env.foreign_usd_skew < 0.50  # Consumed Foreign USD liquidity

    # 3. Wait through on-chain transfer blocks -> STATE_REPAYING
    for _ in range(8):
        env.step(np.array([-0.5, 0.0], dtype=np.float32))
    assert env.state == env.STATE_REPAYING

    # 4. REPAYING -> NEUTRAL (action_dec = -0.8 < -0.33 triggers loan repayment)
    env.step(np.array([0.0, -0.8], dtype=np.float32))
    assert env.state == env.STATE_NEUTRAL
    assert env.exposure == 0.0
    assert env.balance > 10000.0  # Locked in positive net transfer arbitrage profit


def test_env_balance_skew_threshold_blocks_transfer(sample_dataframe):
    env = ArbitrageEnv(df=sample_dataframe, initial_balance=10000.0, execution_threshold=0.05, is_eval=True)
    env.reset()

    # Enter position first
    env.step(np.array([-0.5, 0.0], dtype=np.float32))
    assert env.state == env.STATE_HOLDING_LEG

    # Deplete foreign_usd_skew below 15% threshold
    env.foreign_usd_skew = 0.10
    env.step(np.array([-0.5, 0.8], dtype=np.float32))

    # Transfer should be blocked by skew guard, staying in HOLDING_LEG
    assert env.state == env.STATE_HOLDING_LEG


def test_env_dual_trigger_reverse_rebalance():
    # Negative spread (-0.2%) allows 100% delta-neutral reverse transfer rebalancing
    spreads = [-0.002, -0.002, -0.001]
    df = _make_sample_dataframe(spreads)
    env = ArbitrageEnv(df=df, initial_balance=10000.0, execution_threshold=0.05, is_eval=True)
    env.reset()

    # Simulate skewed Foreign USD balance (15% <= 20% trigger skew) in NEUTRAL state
    env.foreign_usd_skew = 0.15
    obs, reward, done, truncated, info = env.step(np.array([0.0, 0.0], dtype=np.float32))

    assert info.get("rebalance_triggered") is True
    assert abs(env.foreign_usd_skew - 0.50) < 1e-6
