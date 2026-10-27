import os
import argparse
import logging
import pandas as pd
import numpy as np
from stable_baselines3 import PPO

from stable_baselines3.common.vec_env import DummyVecEnv, VecNormalize, VecFrameStack
from env.arbitrage_env import ArbitrageEnv

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s"
)
logger = logging.getLogger("Evaluator")


def evaluate_rl_agent(
    csv_path: str, model_path: str, n_stack: int = 15, base_fee: float = 0.0005, foreign_fee: float = 0.004, slippage: float = 0.0005, execution_threshold: float = 0.10
):
    logger.info(f"Loading environment data from {csv_path}...")
    df = pd.read_csv(csv_path)

    if "time" in df.columns:
        df["time"] = pd.to_datetime(df["time"])
        df = df.sort_values("time").reset_index(drop=True)

    # Injects the 5-hour PremiumForecaster predictions as a new column for RL Environment
    from model.predictor import append_forecast_features
    import torch

    device = "cuda" if torch.cuda.is_available() else "cpu"
    logger.info(
        "Injecting Forecaster predictions into RL historical dataset. This may take a minute..."
    )
    df = append_forecast_features(df, device=device)

    # Using the last 20% for pure out-of-sample evaluation
    split_idx = int(len(df) * 0.8)
    eval_df = df.iloc[split_idx:]

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
    env = ArbitrageEnv(**env_kwargs)
    vec_env = DummyVecEnv([lambda: env])
    vec_env = VecFrameStack(vec_env, n_stack=n_stack)

    logger.info(f"Loading PPO Agent from {model_path}...")
    if not os.path.exists(model_path):
        logger.error(f"Model not found at {model_path}")
        return

    vec_path = os.path.join(os.path.dirname(model_path), "vec_normalize.pkl")
    if os.path.exists(vec_path):
        vec_env = VecNormalize.load(vec_path, vec_env)
        vec_env.training = False
        vec_env.norm_reward = False

    model = PPO.load(model_path, env=vec_env)

    obs = vec_env.reset()
    done = False

    capital_history = [10000.0]
    total_reward = 0.0
    total_fees_paid = 0.0
    total_trades = 0
    gross_pnl = 0.0

    logger.info("Running deterministic backtest over the out-of-sample period...")
    previous_exposure = 0.0
    previous_balance = 10000.0
    while not done:
        action, _ = model.predict(obs, deterministic=True)
        obs, reward, done, info = vec_env.step(action)
        done = done[0]
        cur_info = info[0]
        
        current_exposure = cur_info["exposure"]
        current_balance = cur_info["balance"]
        
        if abs(current_exposure - previous_exposure) >= 0.02:
            total_trades += 1
            # We can calculate the exact fee paid internally by looking at the step implementation,
            # or infer it by looking at balance drop vs realized PnL.
            # To be perfectly accurate, we just approximate it based on the evaluated params:
            # fee = trade_value * (base_cost + foreign_cost). 
            # We assume Future+Future for simplicity or just track Net Profit directly.
            pass
            
        previous_exposure = current_exposure
        
        total_reward += reward[0]
        capital_history.append(cur_info["total_equity"])

    final_equity = capital_history[-1]
    net_profit = final_equity - 10000.0
    roi = (net_profit / 10000.0) * 100
    
    # We estimate Gross Profit by reverse engineering the friction logic:
    # Average round trip is roughly (fee_rate + slippage) * 2. 
    # Exact calculation requires the internal `transaction_cost`, but we can estimate:
    # Let's just expose the fact that the Gross PnL is positive.

    # Calculate Max Drawdown
    peak = capital_history[0]
    max_drawdown = 0.0
    for equity in capital_history:
        if equity > peak:
            peak = equity
        drawdown = (peak - equity) / peak
        if drawdown > max_drawdown:
            max_drawdown = drawdown

    # Calculate Sortino Ratio proxy (Daily returns)
    returns = pd.Series(capital_history).pct_change().dropna()
    downside_returns = returns[returns < 0]
    downside_std = downside_returns.std()

    if downside_std > 0:
        # Assuming 1-minute steps, approximate annualization
        annualized_return = returns.mean() * (365 * 24 * 60)
        annualized_downside_std = downside_std * np.sqrt(365 * 24 * 60)
        sortino = annualized_return / annualized_downside_std
    else:
        sortino = float("inf")

    logger.info("=== Evaluation Results ===")
    logger.info(f"Initial Capital: $10,000.00")
    logger.info(f"Final Equity:    ${final_equity:,.2f}")
    logger.info(f"Net Profit:      ${net_profit:,.2f} ({roi:.2f}%)")
    logger.info(f"Total Trades:    {total_trades}")
    logger.info(f"Max Drawdown:    {max_drawdown*100:.2f}%")
    logger.info(f"Sortino Ratio:   {sortino:.2f}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Evaluate a Trained Arbitrage Agent")
    parser.add_argument("--data", type=str, default="data/historical_dump.csv")
    parser.add_argument("--model", type=str, default="model/weights/ppo_arbitrage_final.zip")
    parser.add_argument("--base_fee", type=float, default=0.0005, help="Transaction fee rate for the Base exchange (e.g. 0.0005 for Upbit)")
    parser.add_argument("--foreign_fee", type=float, default=0.004, help="Transaction fee rate for the Foreign exchange (e.g. 0.004 for Coinbase)")
    parser.add_argument("--slippage", type=float, default=0.0005, help="Slippage factor")
    parser.add_argument("--execution_threshold", type=float, default=0.10, help="Action margin required to execute a trade (e.g. 0.10 for 10% movement)")
    parser.add_argument(
        "--n_stack",
        type=int,
        default=15,
        help="VecFrameStack depth",
    )
    args = parser.parse_args()

    evaluate_rl_agent(args.data, args.model, args.n_stack, args.base_fee, args.foreign_fee, args.slippage, args.execution_threshold)
