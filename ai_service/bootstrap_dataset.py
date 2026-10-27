import os
import time
import argparse
import logging
import pandas as pd
import numpy as np
from datetime import datetime, timedelta, UTC

# Requires: pip install ccxt pandas numpy yfinance
import ccxt
import yfinance as yf

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s"
)
logger = logging.getLogger("Bootstrapper")


def fetch_ohlcv_ccxt(exchange_name: str, symbol: str, days: int) -> pd.DataFrame:
    """
    Fetches 1-minute OHLCV data using CCXT.
    Handles pagination to go back `days` in time.
    """
    exchange_class = getattr(ccxt, exchange_name.lower())
    exchange = exchange_class({"enableRateLimit": True})

    timeframe = "1m"
    utc_time = datetime.now(UTC)
    since = int((utc_time - timedelta(days=days)).timestamp() * 1000)
    now = int(utc_time.timestamp() * 1000)

    all_data = []

    logger.info(f"Fetching {days} days of {symbol} from {exchange_name}...")

    while since < now:
        try:
            # Fetch up to 1000 candles per request
            ohlcv = exchange.fetch_ohlcv(symbol, timeframe, since, limit=1000)
            if not ohlcv:
                break

            all_data.extend(ohlcv)
            # update since to the last timestamp + 1 minute (60000 ms)
            since = ohlcv[-1][0] + 60000

            # rate limit sleep is handled by ccxt
            time.sleep(0.5)

        except Exception as e:
            logger.warning(f"Error fetching data: {e}")
            time.sleep(5)

    if not all_data:
        return pd.DataFrame()

    df = pd.DataFrame(
        all_data, columns=["timestamp", "open", "high", "low", "close", "volume"]
    )
    df["time"] = pd.to_datetime(df["timestamp"], unit="ms")

    # We only care about the closing price as the "price"
    df = df[["time", "close"]].rename(columns={"close": "price"})
    df.set_index("time", inplace=True)
    return df


def fetch_fx_yfinance(symbol: str, days: int) -> pd.DataFrame:
    """
    Fetches historical FX rates (e.g., USD/KRW) using yfinance at 1-hour intervals.
    """
    logger.info(f"Fetching {days} days of FX data for {symbol} via yfinance...")
    end_date = datetime.now(UTC)
    start_date = end_date - timedelta(days=days)

    df = yf.download(
        symbol, start=start_date, end=end_date, interval="1h", progress=False
    )
    if df.empty:
        return pd.DataFrame()

    fx_df = pd.DataFrame()
    fx_df["time"] = pd.to_datetime(df.index, utc=True).tz_localize(None)

    if isinstance(df.columns, pd.MultiIndex):
        fx_df["fx_rate"] = df["Close"].iloc[:, 0].values
    else:
        fx_df["fx_rate"] = df["Close"].values

    fx_df.set_index("time", inplace=True)
    return fx_df


def main():
    parser = argparse.ArgumentParser(
        description="Bootstrap Historical Data directly from Exchange APIs"
    )
    parser.add_argument(
        "--days", type=int, default=7, help="Number of days to fetch (Default: 7)"
    )
    parser.add_argument(
        "--base-exchange", type=str, default="UPBIT", help="Base spot exchange name"
    )
    parser.add_argument(
        "--base-symbol", type=str, default="BTC/KRW", help="Base spot symbol"
    )
    parser.add_argument(
        "--foreign-exchange",
        type=str,
        default="COINBASE",
        help="Foreign spot exchange name",
    )
    parser.add_argument(
        "--foreign-symbol", type=str, default="BTC/USD", help="Foreign spot symbol"
    )
    parser.add_argument(
        "--foreign-future-exchange",
        type=str,
        default="BINANCE",
        help="Foreign future exchange name (for funding proxy)",
    )
    parser.add_argument(
        "--foreign-future-symbol",
        type=str,
        default="BTC/USDT:USDT",
        help="Foreign future symbol",
    )
    parser.add_argument(
        "--fx-symbol",
        type=str,
        default="KRW=X",
        help="Yahoo Finance FX Symbol (e.g., KRW=X for USD->KRW)",
    )
    parser.add_argument(
        "--output", type=str, default="data/historical_dump.csv", help="Output path"
    )
    args = parser.parse_args()

    # 1. Fetch Upbit Data (Base)
    upbit_df = fetch_ohlcv_ccxt(args.base_exchange, args.base_symbol, args.days)
    if upbit_df.empty:
        logger.error("Failed to fetch Upbit data.")
        return
    upbit_df.rename(columns={"price": "local_price"}, inplace=True)

    # 2. Fetch Coinbase Data (Foreign)
    coinbase_df = fetch_ohlcv_ccxt(
        args.foreign_exchange, args.foreign_symbol, args.days
    )
    if coinbase_df.empty:
        logger.error("Failed to fetch Coinbase data.")
        return
    coinbase_df.rename(columns={"price": "foreign_price"}, inplace=True)

    # 3. Fetch Foreign Futures Data (for Implied Funding Rate)
    future_df = fetch_ohlcv_ccxt(
        args.foreign_future_exchange, args.foreign_future_symbol, args.days
    )
    if future_df.empty:
        logger.warning(
            "Failed to fetch Foreign Future data. Funding rates will be mocked."
        )
    else:
        future_df.rename(columns={"price": "future_price"}, inplace=True)

    # 4. Merge dataframes
    logger.info("Merging datasets and forward-filling missing values...")
    merged_df = upbit_df.join(coinbase_df, how="outer")
    if not future_df.empty:
        merged_df = merged_df.join(future_df, how="outer")

    merged_df.sort_index(inplace=True)
    merged_df.ffill(inplace=True)
    merged_df.dropna(subset=["local_price", "foreign_price"], inplace=True)

    # 5. Synthesize FX and Funding Rates
    fx_df = fetch_fx_yfinance(args.fx_symbol, args.days)
    if not fx_df.empty:
        logger.info("Merging real historical FX rates...")
        # Since FX is 1h and crypto is 1m, we merge and forward-fill
        merged_df = pd.merge(
            merged_df, fx_df, left_index=True, right_index=True, how="left"
        )
        merged_df["fx_rate"] = merged_df["fx_rate"].ffill().bfill()
    else:
        logger.warning(
            "Failed to fetch FX rates from yfinance. Falling back to static 1400.0."
        )
        merged_df["fx_rate"] = 1400.0

    # [Upbit Base Market]: 한국 거래소(Upbit)는 법적으로 현물(Spot) 거래만 지원하므로
    # 차입이나 선물 포지션 등에 의한 펀딩비 교환이 발생하지 않습니다. 따라서 항상 0.0 입니다.
    merged_df["base_funding_rate"] = 0.0

    # [Foreign Market Implied Funding]:
    # 해당 시점의 (선물가 - 현물가) / 현물가 를 기반으로 8시간 기준 펀딩비 프리미엄을 역산합니다.
    if "future_price" in merged_df.columns:
        logger.info("Calculating implied funding rates from Futures vs Spot basis...")
        basis_spread = (
            merged_df["future_price"] - merged_df["foreign_price"]
        ) / merged_df["foreign_price"]
        # Rough proxy: 8-hour funding rate is historically correlated to the immediate basis spread divided by time to expiry or a dampening factor.
        # We divide by 8 to approximate the per-hour scaling effect typically seen in perps.
        merged_df["foreign_funding_rate"] = basis_spread / 8.0
        # drop the temporary future_price column
        merged_df.drop(columns=["future_price"], inplace=True)
    else:
        np.random.seed(42)
        merged_df["foreign_funding_rate"] = np.random.normal(
            loc=0.00001, scale=0.000005, size=len(merged_df)
        )

    # 6. Calculate premium spread
    merged_df["exchange_premium_spread"] = (
        merged_df["local_price"] / (merged_df["foreign_price"] * merged_df["fx_rate"])
    ) - 1.0

    # 6. Save and exit
    merged_df.reset_index(inplace=True)

    os.makedirs(os.path.dirname(args.output), exist_ok=True)
    merged_df.to_csv(args.output, index=False)

    logger.info(f"Done! Saved {len(merged_df)} rows to {args.output}")
    logger.info(
        f"Sample Spread: {merged_df['exchange_premium_spread'].mean()*100:.2f}%"
    )


if __name__ == "__main__":
    main()
