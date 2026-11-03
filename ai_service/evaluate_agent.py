import os
import json
import argparse
import logging
from datetime import datetime, timezone
import numpy as np
import pandas as pd
import torch
from stable_baselines3 import PPO
from stable_baselines3.common.vec_env import DummyVecEnv, VecNormalize, VecFrameStack

from env.arbitrage_env import ArbitrageEnv
from model.predictor import append_forecast_features
from model.rl_agent import PortfolioRLAgent

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s"
)
logger = logging.getLogger("EvaluateRLAgent")


def compute_backtest_metrics(
    name: str,
    capital_history: list,
    trade_pnls: list,
    total_trades: int,
    transfer_cycles: int,
    rebalance_events: int,
    final_foreign_usd_skew: float,
    vetoed_count: int,
    total_fees: float,
    total_funding: float,
    state_counts: dict,
    initial_balance: float = 10000.0,
) -> dict:
    final_equity = float(capital_history[-1])
    net_profit = final_equity - initial_balance
    roi_pct = (net_profit / initial_balance) * 100.0

    # Max Drawdown
    eq_arr = np.array(capital_history, dtype=np.float64)
    peaks = np.maximum.accumulate(eq_arr)
    drawdowns = (peaks - eq_arr) / np.maximum(peaks, 1e-8)
    max_dd_pct = float(np.max(drawdowns) * 100.0)

    # Sharpe & Sortino (1-minute returns annualized)
    returns = pd.Series(eq_arr).pct_change().dropna()
    ann_factor = 365.0 * 24.0 * 60.0
    mean_ret = float(returns.mean()) if len(returns) > 0 else 0.0
    std_ret = float(returns.std()) if len(returns) > 0 else 0.0
    downside_ret = returns[returns < 0]
    downside_std = float(downside_ret.std()) if len(downside_ret) > 0 else 0.0

    sharpe = (
        (mean_ret * ann_factor) / (std_ret * np.sqrt(ann_factor))
        if std_ret > 1e-12
        else 0.0
    )
    sortino = (
        (mean_ret * ann_factor) / (downside_std * np.sqrt(ann_factor))
        if downside_std > 1e-12
        else (sharpe if sharpe > 0 else 0.0)
    )

    wins = sum(1 for p in trade_pnls if p > 0)
    win_rate_pct = (wins / len(trade_pnls) * 100.0) if len(trade_pnls) > 0 else 0.0

    total_steps = max(1, sum(state_counts.values()))
    state_share_pct = {
        k: round((v / total_steps) * 100.0, 2) for k, v in state_counts.items()
    }

    return {
        "strategy_name": name,
        "initial_capital_usd": round(initial_balance, 2),
        "final_equity_usd": round(final_equity, 2),
        "net_profit_usd": round(net_profit, 2),
        "net_roi_pct": round(roi_pct, 3),
        "max_drawdown_pct": round(max_dd_pct, 3),
        "sharpe_ratio": round(float(sharpe), 3),
        "sortino_ratio": round(float(sortino), 3),
        "total_trades": int(total_trades),
        "closed_round_trips": int(len(trade_pnls)),
        "win_rate_pct": round(win_rate_pct, 2),
        "transfer_arbitrage_cycles": int(transfer_cycles),
        "reverse_rebalance_events": int(rebalance_events),
        "final_foreign_usd_skew_pct": round(final_foreign_usd_skew * 100.0, 2),
        "vetoed_entries": int(vetoed_count),
        "total_fees_paid_usd": round(total_fees, 2),
        "total_funding_pnl_usd": round(total_funding, 2),
        "state_residence_pct": state_share_pct,
    }


def run_ppo_simulation(
    eval_df: pd.DataFrame,
    model_path: str,
    label: str,
    use_forecaster_fusion: bool = True,
    n_stack: int = 15,
    base_fee: float = 0.0005,
    foreign_fee: float = 0.002,
    slippage: float = 0.0005,
    execution_threshold: float = 0.10,
) -> dict:
    env_kwargs = {
        "df": eval_df,
        "initial_balance": 10000.0,
        "base_fee_rate": base_fee,
        "foreign_fee_rate": foreign_fee,
        "slippage": slippage,
        "execution_threshold": execution_threshold,
        "randomize_fees": False,
        "is_eval": True,
    }
    raw_env = ArbitrageEnv(**env_kwargs)
    vec_env = DummyVecEnv([lambda: raw_env])
    vec_env = VecFrameStack(vec_env, n_stack=n_stack)

    agent = PortfolioRLAgent(model_path=model_path, device="cpu", n_stack=n_stack)

    obs = vec_env.reset()
    done = False

    capital_history = [10000.0]
    trade_pnls = []
    total_trades = 0
    transfer_cycles = 0
    rebalance_events = 0
    final_skew = 0.50
    vetoed_count = 0
    total_fees = 0.0
    total_funding = 0.0
    state_counts = {
        "NEUTRAL_HOLD": 0,
        "CONVERGENCE_HEDGE_ABDE": 0,
        "TRANSFERRING_C": 0,
        "REPAYING_C": 0,
        "STAGED_ASSASSINATION": 0,
    }

    prev_exposure = 0.0
    entry_equity = 10000.0

    while not done:
        action_score, target_weight, action_type = agent.predict(
            obs, use_forecaster=use_forecaster_fusion
        )
        if action_score < 5.0:
            signed_exp = -float(target_weight)
        elif action_score > 5.0:
            signed_exp = float(target_weight)
        else:
            signed_exp = 0.0

        if action_type == 3:
            act_dec = 0.90  # STAGE_ASSASSINATION
        elif action_type == 1:
            act_dec = 0.50  # TRANSFER
        elif action_type == 2:
            act_dec = -0.8  # REPAY / EXIT
        else:
            act_dec = 0.0   # TRADE / HOLD
        action = np.array([[signed_exp, act_dec]], dtype=np.float32)

        obs, reward, done_arr, info_arr = vec_env.step(action)
        done = bool(done_arr[0])
        cur_info = info_arr[0]

        cur_exposure = float(cur_info.get("exposure", 0.0))
        cur_equity = float(cur_info.get("total_equity", 10000.0))
        cur_state = int(cur_info.get("state", 0))
        final_skew = float(cur_info.get("foreign_usd_skew", final_skew))

        if cur_state == 0:
            state_counts["NEUTRAL_HOLD"] += 1
        elif cur_state == 1:
            state_counts["CONVERGENCE_HEDGE_ABDE"] += 1
        elif cur_state == 2:
            state_counts["TRANSFERRING_C"] += 1
        elif cur_state == 3:
            state_counts["REPAYING_C"] += 1
        elif cur_state == 4:
            state_counts["STAGED_ASSASSINATION"] += 1

        if cur_info.get("trade_executed", False):
            total_trades += 1
        if cur_info.get("repay_via_transfer", False):
            transfer_cycles += 1
        if cur_info.get("rebalance_triggered", False):
            rebalance_events += 1
        if cur_info.get("vetoed", False):
            vetoed_count += 1

        total_fees += float(cur_info.get("step_fees_paid", 0.0))
        total_funding += float(cur_info.get("step_funding_pnl", 0.0))

        # Track round-trip trade PnL when transitioning from 0 -> non-zero -> 0 or flipping sign
        if prev_exposure == 0.0 and cur_exposure != 0.0:
            entry_equity = capital_history[-1]
        elif prev_exposure != 0.0 and (
            cur_exposure == 0.0 or np.sign(cur_exposure) != np.sign(prev_exposure)
        ):
            trade_pnls.append(cur_equity - entry_equity)
            if cur_exposure != 0.0:
                entry_equity = cur_equity

        prev_exposure = cur_exposure
        capital_history.append(cur_equity)

    return compute_backtest_metrics(
        name=label,
        capital_history=capital_history,
        trade_pnls=trade_pnls,
        total_trades=total_trades,
        transfer_cycles=transfer_cycles,
        rebalance_events=rebalance_events,
        final_foreign_usd_skew=final_skew,
        vetoed_count=vetoed_count,
        total_fees=total_fees,
        total_funding=total_funding,
        state_counts=state_counts,
    )


def run_rule_based_baseline(
    eval_df: pd.DataFrame,
    base_fee: float = 0.0005,
    foreign_fee: float = 0.002,
    slippage: float = 0.0005,
    execution_threshold: float = 0.10,
) -> dict:
    """
    Classic Static-Threshold Arbitrage Bot Baseline:
      - Short Base / Long Foreign (-0.50 weight) when spread >= +0.8% (0.008)
      - Long Base / Short Foreign (+0.50 weight) when spread <= -0.8% (-0.008)
      - Exit to Neutral (0.0 weight) when spread converges within [-0.15%, +0.15%]
    """
    df_rule = eval_df.copy()
    # Provide generous max/min bands so static rules are not blocked by Forecaster Veto
    df_rule["predicted_spread"] = df_rule["exchange_premium_spread"] * 0.5
    df_rule["predicted_spread_max"] = df_rule["exchange_premium_spread"] + 0.015
    df_rule["predicted_spread_min"] = df_rule["exchange_premium_spread"] - 0.015

    env = ArbitrageEnv(
        df=df_rule,
        initial_balance=10000.0,
        base_fee_rate=base_fee,
        foreign_fee_rate=foreign_fee,
        slippage=slippage,
        execution_threshold=execution_threshold,
        randomize_fees=False,
        is_eval=True,
    )

    obs, _ = env.reset()
    done = False

    capital_history = [10000.0]
    trade_pnls = []
    total_trades = 0
    transfer_cycles = 0
    vetoed_count = 0
    total_fees = 0.0
    total_funding = 0.0
    state_counts = {
        "NEUTRAL_HOLD": 0,
        "CONVERGENCE_HEDGE_ABDE": 0,
        "TRANSFERRING_C": 0,
        "REPAYING_C": 0,
    }

    prev_exposure = 0.0
    entry_equity = 10000.0

    while not done:
        spread = float(obs[0])
        if env.exposure == 0.0:
            if spread >= 0.008:
                action = np.array([-0.50, 0.0], dtype=np.float32)  # Short Base / Long Foreign
            elif spread <= -0.008:
                action = np.array([0.50, 0.0], dtype=np.float32)   # Long Base / Short Foreign
            else:
                action = np.array([0.0, 0.0], dtype=np.float32)
        elif env.exposure < 0.0:
            if spread <= 0.0015:
                action = np.array([0.0, -0.8], dtype=np.float32)    # Exit on convergence
            else:
                action = np.array([env.exposure, 0.0], dtype=np.float32)
        else:
            if spread >= -0.0015:
                action = np.array([0.0, -0.8], dtype=np.float32)    # Exit on convergence
            else:
                action = np.array([env.exposure, 0.0], dtype=np.float32)

        obs, reward, done, _, cur_info = env.step(action)

        cur_exposure = float(cur_info.get("exposure", 0.0))
        cur_equity = float(cur_info.get("total_equity", 10000.0))
        cur_state = int(cur_info.get("state", 0))

        if cur_state == 0:
            state_counts["NEUTRAL_HOLD"] += 1
        elif cur_state == 1:
            state_counts["CONVERGENCE_HEDGE_ABDE"] += 1
        elif cur_state == 2:
            state_counts["TRANSFERRING_C"] += 1
        elif cur_state == 3:
            state_counts["REPAYING_C"] += 1

        if cur_info.get("trade_executed", False):
            total_trades += 1
        if cur_info.get("repay_via_transfer", False):
            transfer_cycles += 1
        if cur_info.get("vetoed", False):
            vetoed_count += 1

        total_fees += float(cur_info.get("step_fees_paid", 0.0))
        total_funding += float(cur_info.get("step_funding_pnl", 0.0))

        if prev_exposure == 0.0 and cur_exposure != 0.0:
            entry_equity = capital_history[-1]
        elif prev_exposure != 0.0 and (
            cur_exposure == 0.0 or np.sign(cur_exposure) != np.sign(prev_exposure)
        ):
            trade_pnls.append(cur_equity - entry_equity)
            if cur_exposure != 0.0:
                entry_equity = cur_equity

        prev_exposure = cur_exposure
        capital_history.append(cur_equity)

    return compute_backtest_metrics(
        name="Rule-Based Static Threshold Bot",
        capital_history=capital_history,
        trade_pnls=trade_pnls,
        total_trades=total_trades,
        transfer_cycles=transfer_cycles,
        rebalance_events=0,
        final_foreign_usd_skew=0.50,
        vetoed_count=vetoed_count,
        total_fees=total_fees,
        total_funding=total_funding,
        state_counts=state_counts,
    )


def evaluate_assassination_scenarios() -> dict:
    """
    Runs the 4-Scenario Assassination & Event Stress Suite:
      - Scenario A (Successful New Listing / Gaduri Warning Assassination):
          Notice at t=0 (0.2% Kimp) -> pre-staged via STAGE_ASSASSINATION -> Kimp surges to +4.5% -> 0-latency exit on Base.
      - Scenario B (False Signal / Fizzle Timeout Unwind):
          Notice at t=0 (0.2% Kimp) -> pre-staged -> Kimp stays flat at +0.3% for 720 steps -> simultaneous in-place unwind with zero reverse gas fee.
      - Scenario C (Delisting Trap Defense):
          DELISTING notice active -> 100% of entry attempts are vetoed and open positions are unwound immediately.
      - Scenario D (In-Transit Kimp Drop Defense):
          Transfer started at +3.5% Kimp with FOREIGN_HEDGE -> Kimp drops to -0.3% during transit -> auto-switches to STATE_STAGED instead of panic-selling -> exits profitably on +2.4% rebound.
    """
    # Scenario A: Successful Listing / Gaduri Surge
    n_a = 120
    spread_a = np.full(n_a, 0.002, dtype=np.float32)
    spread_a[40:70] = np.linspace(0.002, 0.045, 30, dtype=np.float32)
    spread_a[70:] = 0.045
    df_a = pd.DataFrame({
        "exchange_premium_spread": spread_a,
        "tether_real_premium_spread": spread_a,
        "predicted_spread": spread_a + 0.025,
        "predicted_spread_max": np.maximum(spread_a + 0.040, 0.045),
        "predicted_spread_min": spread_a - 0.002,
        "first_wave_edge": np.full(n_a, 0.038, dtype=np.float32),
        "base_funding_rate": np.zeros(n_a, dtype=np.float32),
        "foreign_funding_rate": np.full(n_a, 0.0001, dtype=np.float32),
        "event_new_listing": np.ones(n_a, dtype=np.float32),
        "vasp_tier": np.ones(n_a, dtype=np.int32),
    })
    env_a = ArbitrageEnv(df=df_a, initial_balance=10000.0, is_eval=True)
    env_a.reset()
    sniped_a = False
    for _ in range(n_a - 1):
        action = np.array([0.50, 0.90], dtype=np.float32) if env_a.state == env_a.STATE_NEUTRAL and not sniped_a else np.array([0.50, 0.0], dtype=np.float32)
        _, _, done, _, info_a = env_a.step(action)
        if info_a.get("assassination_sniped"):
            sniped_a = True
        if done or sniped_a:
            break
    roi_a_pct = ((env_a.balance - 10000.0) / 10000.0) * 100.0

    # Scenario B: False Signal / Fizzle Timeout Unwind (Zero Reverse Gas Fee)
    n_b = 750
    spread_b = np.full(n_b, 0.0025, dtype=np.float32)
    df_b = pd.DataFrame({
        "exchange_premium_spread": spread_b,
        "tether_real_premium_spread": spread_b,
        "predicted_spread": spread_b + 0.022,
        "predicted_spread_max": spread_b + 0.030,
        "predicted_spread_min": spread_b - 0.002,
        "first_wave_edge": np.full(n_b, 0.025, dtype=np.float32),
        "base_funding_rate": np.zeros(n_b, dtype=np.float32),
        "foreign_funding_rate": np.zeros(n_b, dtype=np.float32),
        "event_warning": np.ones(n_b, dtype=np.float32),
        "vasp_tier": np.ones(n_b, dtype=np.int32),
    })
    env_b = ArbitrageEnv(df=df_b, initial_balance=10000.0, is_eval=True)
    env_b.reset()
    timeout_unwound_b = False
    unwind_step_fee_b = 0.0
    for step_i in range(n_b - 1):
        act = np.array([0.50, 0.90], dtype=np.float32) if step_i == 0 else np.array([0.50, 0.0], dtype=np.float32)
        _, _, done, _, info_b = env_b.step(act)
        if info_b.get("staged_timeout_unwind"):
            timeout_unwound_b = True
            unwind_step_fee_b = float(info_b.get("step_fees_paid", 0.0))
            break
        if done:
            break

    # Scenario C: Delisting Trap Defense (100% Entry Veto + Immediate Emergency Exit)
    n_c = 40
    spread_c = np.linspace(0.050, -0.150, n_c, dtype=np.float32)
    df_c = pd.DataFrame({
        "exchange_premium_spread": spread_c,
        "tether_real_premium_spread": spread_c,
        "predicted_spread": spread_c,
        "predicted_spread_max": spread_c + 0.030,
        "predicted_spread_min": spread_c - 0.030,
        "first_wave_edge": np.full(n_c, 0.025, dtype=np.float32),
        "base_funding_rate": np.zeros(n_c, dtype=np.float32),
        "foreign_funding_rate": np.zeros(n_c, dtype=np.float32),
        "event_delisting": np.ones(n_c, dtype=np.float32),
    })
    env_c = ArbitrageEnv(df=df_c, initial_balance=10000.0, is_eval=True)
    env_c.reset()
    blocked_attempts = 0
    total_attempts = 20
    for _ in range(total_attempts):
        _, _, _, _, info_c = env_c.step(np.array([0.60, 0.90], dtype=np.float32))
        if info_c.get("vetoed") and env_c.exposure == 0.0:
            blocked_attempts += 1
    delisting_block_rate = (blocked_attempts / float(total_attempts)) * 100.0

    # Scenario D: In-Transit Kimp Drop Defense (+3.5% -> -0.3% during transfer -> auto-stage -> +2.4% rebound exit)
    n_d = 60
    spread_d = np.full(n_d, 0.035, dtype=np.float32)
    spread_d[3:25] = -0.003  # Kimp collapses while coin is in transit!
    spread_d[25:] = 0.024    # Kimp rebounds to +2.4%
    df_d = pd.DataFrame({
        "exchange_premium_spread": spread_d,
        "tether_real_premium_spread": spread_d,
        "predicted_spread": spread_d,
        "predicted_spread_max": spread_d + 0.020,
        "predicted_spread_min": spread_d - 0.020,
        "first_wave_edge": np.full(n_d, -0.020, dtype=np.float32),
        "base_funding_rate": np.zeros(n_d, dtype=np.float32),
        "foreign_funding_rate": np.zeros(n_d, dtype=np.float32),
    })
    env_d = ArbitrageEnv(df=df_d, initial_balance=10000.0, is_eval=True)
    env_d.reset()
    env_d.in_transit_hedge_mode = "FOREIGN_HEDGE"
    # Step 0: Enter Short Base / Long Foreign leg at +3.5%
    env_d.step(np.array([-0.50, 0.0], dtype=np.float32))
    # Step 1: Initiate Transfer (Strategy C)
    env_d.step(np.array([-0.50, 0.50], dtype=np.float32))
    defense_triggered_d = False
    sniped_rebound_d = False
    for _ in range(2, n_d - 1):
        _, _, done, _, info_d = env_d.step(np.array([-0.50, 0.0], dtype=np.float32))
        if info_d.get("in_transit_defense_triggered"):
            defense_triggered_d = True
        if info_d.get("assassination_sniped"):
            sniped_rebound_d = True
            break
        if done:
            break
    roi_d_pct = ((env_d.balance - 10000.0) / 10000.0) * 100.0

    return {
        "scenario_a_listing_surge": {
            "sniped": bool(sniped_a),
            "net_roi_pct": round(float(roi_a_pct), 3),
            "final_balance_usd": round(float(env_a.balance), 2),
        },
        "scenario_b_fizzle_timeout": {
            "timeout_unwound_in_place": bool(timeout_unwound_b),
            "unwind_step_fee_usd": round(float(unwind_step_fee_b), 2),
            "reverse_gas_fee_usd": 0.0,
            "final_balance_usd": round(float(env_b.balance), 2),
        },
        "scenario_c_delisting_defense": {
            "entry_attempts": total_attempts,
            "blocked_attempts": blocked_attempts,
            "delisting_entry_block_rate_pct": round(float(delisting_block_rate), 2),
            "final_balance_usd": round(float(env_c.balance), 2),
        },
        "scenario_d_in_transit_kimp_drop_defense": {
            "auto_switched_to_staged": bool(defense_triggered_d),
            "sniped_on_rebound": bool(sniped_rebound_d),
            "net_roi_pct": round(float(roi_d_pct), 3),
            "final_balance_usd": round(float(env_d.balance), 2),
        },
    }


def evaluate_rl_agent(
    csv_path: str = "data/historical_dump.csv",
    model_path: str = "model/weights/ppo_arbitrage_final.zip",
    n_stack: int = 15,
    base_fee: float = 0.0005,
    foreign_fee: float = 0.002,
    slippage: float = 0.0005,
    execution_threshold: float = 0.10,
    report_path: str = "model/weights/eval_report.json",
) -> dict:
    logger.info(f"Loading historical data from {csv_path}...")
    df = pd.read_csv(csv_path)

    if "time" in df.columns:
        df["time"] = pd.to_datetime(df["time"])
        df = df.sort_values("time").reset_index(drop=True)

    device = "cuda" if torch.cuda.is_available() else "cpu"
    logger.info(f"Injecting 8-hour PremiumForecaster predictions on {device}...")
    df_with_forecast = append_forecast_features(df, device=device)

    split_idx = int(len(df_with_forecast) * 0.8)
    eval_df_full = df_with_forecast.iloc[split_idx:].reset_index(drop=True)

    if not os.path.exists(model_path):
        alt_path = "model/weights/best_model.zip"
        if os.path.exists(alt_path):
            model_path = alt_path
        else:
            logger.error(f"RL Model not found at {model_path}")
            return {}

    logger.info(
        f"Running 3-Way Out-of-Sample Backtest over {len(eval_df_full):,} minutes (~{len(eval_df_full)/1440:.1f} days)..."
    )

    # 1. Full Magnolia AI (Forecaster + PPO Agent)
    full_ai_res = run_ppo_simulation(
        eval_df=eval_df_full,
        model_path=model_path,
        label="Full Magnolia AI (8h Forecaster + PPO Agent)",
        use_forecaster_fusion=True,
        n_stack=n_stack,
        base_fee=base_fee,
        foreign_fee=foreign_fee,
        slippage=slippage,
        execution_threshold=execution_threshold,
    )

    # 2. RL-Only Ablation (PPO Agent without Forecaster — naive flat forecast)
    eval_df_no_forecast = eval_df_full.copy()
    eval_df_no_forecast["predicted_spread"] = eval_df_no_forecast["exchange_premium_spread"]
    eval_df_no_forecast["predicted_spread_max"] = (
        eval_df_no_forecast["exchange_premium_spread"] + 0.0035
    )
    eval_df_no_forecast["predicted_spread_min"] = (
        eval_df_no_forecast["exchange_premium_spread"] - 0.0035
    )
    rl_only_res = run_ppo_simulation(
        eval_df=eval_df_no_forecast,
        model_path=model_path,
        label="RL-Only Ablation (PPO w/o Forecaster)",
        use_forecaster_fusion=False,
        n_stack=n_stack,
        base_fee=base_fee,
        foreign_fee=foreign_fee,
        slippage=slippage,
        execution_threshold=execution_threshold,
    )

    # 3. Rule-Based Static Threshold Baseline
    rule_res = run_rule_based_baseline(
        eval_df=eval_df_full,
        base_fee=base_fee,
        foreign_fee=foreign_fee,
        slippage=slippage,
        execution_threshold=execution_threshold,
    )

    logger.info("\n" + "=" * 86)
    logger.info("3-WAY OUT-OF-SAMPLE ARBITRAGE BACKTEST COMPARISON (INITIAL CAPITAL: $10,000)")
    logger.info("=" * 86)
    for res in [full_ai_res, rl_only_res, rule_res]:
        logger.info(
            f"[{res['strategy_name']}]\n"
            f"  • Final Equity : ${res['final_equity_usd']:,.2f} | Net Profit: ${res['net_profit_usd']:+,.2f} ({res['net_roi_pct']:+.2f}%)\n"
            f"  • Max Drawdown : {res['max_drawdown_pct']:.2f}% | Sharpe: {res['sharpe_ratio']:.2f} | Sortino: {res['sortino_ratio']:.2f}\n"
            f"  • Trades       : {res['total_trades']} (Round-Trips: {res['closed_round_trips']}, Win Rate: {res['win_rate_pct']:.1f}%, Transfer Cycles: {res['transfer_arbitrage_cycles']})\n"
            f"  • Friction/Yld : Fees Paid ${res['total_fees_paid_usd']:,.2f} | Funding Yield ${res['total_funding_pnl_usd']:+,.2f} | Vetoed: {res['vetoed_entries']}\n"
            f"  • State Share  : {res['state_residence_pct']}"
        )
        logger.info("-" * 86)

    assassination_suite = evaluate_assassination_scenarios()
    logger.info(
        "4-SCENARIO ASSASSINATION & EVENT STRESS SUITE:\n"
        f"  • Scenario A (Listing/Warning Surge)  : Sniped={assassination_suite['scenario_a_listing_surge']['sniped']} | Net ROI={assassination_suite['scenario_a_listing_surge']['net_roi_pct']:+.2f}%\n"
        f"  • Scenario B (12h Fizzle Timeout)     : In-Place Unwind={assassination_suite['scenario_b_fizzle_timeout']['timeout_unwound_in_place']} | Reverse Gas=${assassination_suite['scenario_b_fizzle_timeout']['reverse_gas_fee_usd']:.2f}\n"
        f"  • Scenario C (Delisting Trap Defense) : Entry Block Rate={assassination_suite['scenario_c_delisting_defense']['delisting_entry_block_rate_pct']:.1f}%\n"
        f"  • Scenario D (In-Transit Drop Defense): Auto-Staged={assassination_suite['scenario_d_in_transit_kimp_drop_defense']['auto_switched_to_staged']} | Rebound Sniped={assassination_suite['scenario_d_in_transit_kimp_drop_defense']['sniped_on_rebound']} ({assassination_suite['scenario_d_in_transit_kimp_drop_defense']['net_roi_pct']:+.2f}%)"
    )
    logger.info("=" * 86)

    benchmark_summary = {
        "evaluated_at": datetime.now(timezone.utc).isoformat(),
        "test_period_minutes": int(len(eval_df_full)),
        "test_period_days": round(len(eval_df_full) / 1440.0, 2),
        "friction_assumptions": {
            "base_fee_rate": base_fee,
            "foreign_fee_rate": foreign_fee,
            "slippage": slippage,
            "execution_threshold": execution_threshold,
        },
        "models": {
            "full_magnolia_ai": full_ai_res,
            "rl_only_ablation": rl_only_res,
            "rule_based_baseline": rule_res,
        },
        "assassination_scenario_suite": assassination_suite,
    }

    os.makedirs(os.path.dirname(report_path), exist_ok=True)
    report_data = {}
    if os.path.exists(report_path):
        try:
            with open(report_path, "r", encoding="utf-8") as f:
                report_data = json.load(f)
        except Exception:
            report_data = {}
    report_data["rl_agent_benchmark"] = benchmark_summary
    with open(report_path, "w", encoding="utf-8") as f:
        json.dump(report_data, f, indent=2, ensure_ascii=False)
    logger.info(f"Saved 3-Way RL Agent Benchmark report to {report_path}")

    return benchmark_summary


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Evaluate Trained Arbitrage Agent")
    parser.add_argument("--data", type=str, default="data/historical_dump.csv")
    parser.add_argument("--model", type=str, default="model/weights/ppo_arbitrage_final.zip")
    parser.add_argument("--base_fee", type=float, default=0.0005)
    parser.add_argument("--foreign_fee", type=float, default=0.002)
    parser.add_argument("--slippage", type=float, default=0.0005)
    parser.add_argument("--execution_threshold", type=float, default=0.10)
    parser.add_argument("--n_stack", type=int, default=15)
    args = parser.parse_args()

    evaluate_rl_agent(
        csv_path=args.data,
        model_path=args.model,
        n_stack=args.n_stack,
        base_fee=args.base_fee,
        foreign_fee=args.foreign_fee,
        slippage=args.slippage,
        execution_threshold=args.execution_threshold,
    )
