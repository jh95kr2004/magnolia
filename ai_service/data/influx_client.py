import os
import json
import urllib.parse
import urllib.request
import pandas as pd
from datetime import datetime, timedelta, timezone
from influxdb_client import InfluxDBClient
from typing import Dict, Optional
import logging

logger = logging.getLogger("InfluxClient")


class MarketDataFetcher:

    def __init__(self, url: str, token: str, org: str, bucket: str):
        self.url = url
        self.token = token
        self.org = org
        self.bucket = bucket
        self.client = InfluxDBClient(url=url, token=token, org=org)
        self.query_api = self.client.query_api()

    def fetch_recent_window(
        self,
        local_exchange: str,
        local_symbol: str,
        local_is_future: bool,
        foreign_exchange: str,
        foreign_symbol: str,
        foreign_is_future: bool,
        window_hours: int = 24,
    ) -> Optional[pd.DataFrame]:
        """
        Fetches the last N hours of tick data, funding rates, and FX rates,
        then merges and interpolates them into a continuous pandas DataFrame
        ready to be converted to a PyTorch Tensor.
        """
        # 1. Fetch Local Price (e.g. KRW)
        local_future_str = "true" if local_is_future else "false"
        query_local = f"""
            from(bucket:"{self.bucket}")
            |> range(start: -{window_hours}h)
            |> filter(fn: (r) => r._measurement == "exchange_tickers")
            |> filter(fn: (r) => r.exchange == "{local_exchange}" and r.symbol == "{local_symbol}" and r.is_future == "{local_future_str}")
            |> filter(fn: (r) => r._field == "ask_usd" or r._field == "bid_usd")
            |> aggregateWindow(every: 1m, fn: mean, createEmpty: true)
            |> yield(name: "local")
        """

        # 2. Fetch Foreign Price (e.g. USD)
        foreign_future_str = "true" if foreign_is_future else "false"
        query_foreign = f"""
            from(bucket:"{self.bucket}")
            |> range(start: -{window_hours}h)
            |> filter(fn: (r) => r._measurement == "exchange_tickers")
            |> filter(fn: (r) => r.exchange == "{foreign_exchange}" and r.symbol == "{foreign_symbol}" and r.is_future == "{foreign_future_str}")
            |> filter(fn: (r) => r._field == "ask_usd" or r._field == "bid_usd")
            |> aggregateWindow(every: 1m, fn: mean, createEmpty: true)
            |> yield(name: "foreign")
        """

        # 3. Fetch FX Rate (e.g. USD/KRW)
        query_fx = f"""
            from(bucket:"{self.bucket}")
            |> range(start: -{window_hours}h)
            |> filter(fn: (r) => r._measurement == "fx_rate" or r._measurement == "macro_indicators")
            |> filter(fn: (r) => r.currency_pair == "USD/KRW")
            |> filter(fn: (r) => r._field == "fx_rate")
            |> aggregateWindow(every: 1m, fn: mean, createEmpty: true)
            |> yield(name: "fx")
        """

        # 4. Fetch Base Funding Rate
        query_base_funding = f"""
            from(bucket:"{self.bucket}")
            |> range(start: -{window_hours}h)
            |> filter(fn: (r) => r._measurement == "funding_rates")
            |> filter(fn: (r) => r.exchange == "{local_exchange}" and r.symbol == "{local_symbol}")
            |> filter(fn: (r) => r._field == "funding_rate")
            |> aggregateWindow(every: 1m, fn: mean, createEmpty: true)
            |> yield(name: "base_funding")
        """

        # 5. Fetch Foreign Funding Rate
        query_foreign_funding = f"""
            from(bucket:"{self.bucket}")
            |> range(start: -{window_hours}h)
            |> filter(fn: (r) => r._measurement == "funding_rates")
            |> filter(fn: (r) => r.exchange == "{foreign_exchange}" and r.symbol == "{foreign_symbol}")
            |> filter(fn: (r) => r._field == "funding_rate")
            |> aggregateWindow(every: 1m, fn: mean, createEmpty: true)
            |> yield(name: "foreign_funding")
        """

        try:
            # Query as pandas DataFrames
            # Note: query_data_frame returns a DataFrame or a List of DataFrames
            df_local = self._query_to_df(query_local, "local_price")
            df_foreign = self._query_to_df(query_foreign, "foreign_price")
            df_fx = self._query_to_df(query_fx, "fx_rate")
            df_base_funding = self._query_to_df(query_base_funding, "base_funding_rate")
            df_foreign_funding = self._query_to_df(
                query_foreign_funding, "foreign_funding_rate"
            )

            if df_local.empty or df_foreign.empty:
                logger.warning(
                    f"Missing core prices for {local_symbol}/{foreign_symbol} window. Cannot construct tensor."
                )
                return None

            # Merge all time-series on Timestamp using forward fill to handle sparse data
            merged = pd.merge_asof(
                df_local.sort_values("_time"),
                df_foreign.sort_values("_time"),
                on="_time",
                direction="backward",
            )
            
            if not df_fx.empty:
                merged = pd.merge_asof(
                    merged, df_fx.sort_values("_time"), on="_time", direction="backward"
                )
            else:
                merged["fx_rate"] = 1400.0

            # Funding rates might be sparse or non-existent in spot markets, fill with 0 if missing.
            if not df_base_funding.empty:
                merged = pd.merge_asof(
                    merged,
                    df_base_funding.sort_values("_time"),
                    on="_time",
                    direction="backward",
                )
            else:
                merged["base_funding_rate"] = 0.0

            if not df_foreign_funding.empty:
                merged = pd.merge_asof(
                    merged,
                    df_foreign_funding.sort_values("_time"),
                    on="_time",
                    direction="backward",
                )
            else:
                merged["foreign_funding_rate"] = 0.0

            # Fill FX rate first so any KRW candle backfill can convert to USD accurately
            merged["fx_rate"] = merged["fx_rate"].ffill().bfill().fillna(1385.0)

            # Backfill any missing (NaN) or frozen WebSocket outage windows (>10 mins flat) using public 1m candles
            merged = self._backfill_missing_candles(
                merged, local_exchange, local_symbol, foreign_exchange, foreign_symbol
            )

            # Forward fill and drop NaNs
            merged = merged.ffill().dropna()

            # Calculate dynamic premium (both local_price and foreign_price from ask_usd/bid_usd are in USD)
            merged["exchange_premium_spread"] = (
                merged["local_price"] / merged["foreign_price"]
            ) - 1.0

            # Convert local_price from USD to native quote currency (e.g. KRW) to match feature_scaler and live ticks
            if local_symbol.upper().endswith("KRW"):
                merged["local_price"] = merged["local_price"] * merged["fx_rate"]

            # The resulting dataframe contains [exchange_premium_spread, local_price, foreign_price, fx_rate, base_funding_rate, foreign_funding_rate]
            return merged

        except Exception as e:
            logger.error(f"Failed to fetch context window from InfluxDB: {e}")
            return None

    def _query_to_df(self, query_str: str, val_col_name: str) -> pd.DataFrame:
        df = self.query_api.query_data_frame(query_str)
        if isinstance(df, list):
            if len(df) == 0:
                return pd.DataFrame()
            # Concatenate if multiple tables are returned (e.g. bid and ask as separate tables, we can average them or take one)
            df = pd.concat(df)

        if df.empty or "_value" not in df.columns or "_time" not in df.columns:
            return pd.DataFrame()

        # Group by time and take mean (e.g. average of bid/ask)
        df = df[["_time", "_value"]].groupby("_time").mean().reset_index()
        df.rename(columns={"_value": val_col_name}, inplace=True)
        return df

    def _fetch_upbit_1m_candles(self, symbol: str, minutes: int = 600) -> Dict[int, float]:
        parts = symbol.upper().split("-")
        if len(parts) == 2:
            upbit_market = f"{parts[1]}-{parts[0]}"
        else:
            upbit_market = symbol.upper()

        candles: Dict[int, float] = {}
        to_param = ""
        remaining = max(1, min(minutes, 1440))
        while remaining > 0:
            count = min(200, remaining)
            url = f"https://api.upbit.com/v1/candles/minutes/1?market={upbit_market}&count={count}"
            if to_param:
                url += f"&to={ urllib.parse.quote(to_param) }"
            try:
                req = urllib.request.Request(url, headers={"User-Agent": "magnolia-ai/1.0"})
                with urllib.request.urlopen(req, timeout=6) as resp:
                    data = json.loads(resp.read().decode("utf-8"))
                if not data or not isinstance(data, list):
                    break
                for item in data:
                    utc_str = item.get("candle_date_time_utc")
                    price = float(item.get("trade_price", 0.0))
                    if utc_str and price > 0:
                        dt = datetime.fromisoformat(utc_str).replace(tzinfo=timezone.utc)
                        mb = int(dt.timestamp() // 60)
                        candles[mb] = price
                oldest_utc = data[-1].get("candle_date_time_utc")
                if not oldest_utc or len(data) < count:
                    break
                to_param = oldest_utc + "Z"
                remaining -= len(data)
            except Exception as e:
                logger.warning(f"Upbit 1m candle backfill fetch warning ({upbit_market}): {e}")
                break
        return candles

    def _fetch_coinbase_1m_candles(self, symbol: str, minutes: int = 600) -> Dict[int, float]:
        cb_product = symbol.upper()
        candles: Dict[int, float] = {}
        now_utc = datetime.now(timezone.utc)
        remaining = max(1, min(minutes, 1440))
        end_dt = now_utc
        while remaining > 0:
            batch_min = min(300, remaining)
            start_dt = end_dt - timedelta(minutes=batch_min)
            url = (
                f"https://api.exchange.coinbase.com/products/{cb_product}/candles"
                f"?granularity=60&start={start_dt.isoformat()}&end={end_dt.isoformat()}"
            )
            try:
                req = urllib.request.Request(url, headers={"User-Agent": "magnolia-ai/1.0"})
                with urllib.request.urlopen(req, timeout=6) as resp:
                    data = json.loads(resp.read().decode("utf-8"))
                if not data or not isinstance(data, list):
                    break
                for row in data:
                    if isinstance(row, list) and len(row) >= 5:
                        mb = int(float(row[0]) // 60)
                        close_price = float(row[4])
                        if close_price > 0:
                            candles[mb] = close_price
                end_dt = start_dt
                remaining -= batch_min
            except Exception as e:
                logger.warning(f"Coinbase 1m candle backfill fetch warning ({cb_product}): {e}")
                break
        return candles

    def _backfill_missing_candles(
        self,
        merged: pd.DataFrame,
        local_exchange: str,
        local_symbol: str,
        foreign_exchange: str,
        foreign_symbol: str,
    ) -> pd.DataFrame:
        if merged is None or merged.empty:
            return merged

        # Detect either NaN rows or frozen runs (>10 identical consecutive minutes)
        local_s = merged["local_price"]
        foreign_s = merged["foreign_price"]

        local_frozen = local_s.ffill().diff().abs().fillna(0.0) < 1e-9
        local_frozen_run = local_frozen.groupby((~local_frozen).cumsum()).transform("sum") >= 10
        local_needs_fill = local_s.isna() | local_frozen_run

        foreign_frozen = foreign_s.ffill().diff().abs().fillna(0.0) < 1e-9
        foreign_frozen_run = foreign_frozen.groupby((~foreign_frozen).cumsum()).transform("sum") >= 10
        foreign_needs_fill = foreign_s.isna() | foreign_frozen_run

        n_missing_local = int(local_needs_fill.sum())
        n_missing_foreign = int(foreign_needs_fill.sum())
        if n_missing_local <= 3 and n_missing_foreign <= 3:
            return merged

        # Determine how many minutes back from now we need to fetch
        mb_series = (pd.to_datetime(merged["_time"], utc=True).astype("int64") // 10**9 // 60).astype(int)
        now_mb = int(datetime.now(timezone.utc).timestamp() // 60)

        if n_missing_local > 3 and local_exchange.upper() == "UPBIT":
            first_missing_mb = int(mb_series[local_needs_fill].min())
            fetch_mins = max(60, min(1440, now_mb - first_missing_mb + 15))
            logger.info(
                f"Backfilling {n_missing_local} missing/frozen 1m buckets for {local_exchange}:{local_symbol} "
                f"(fetching {fetch_mins}m of REST candles)..."
            )
            ub_candles = self._fetch_upbit_1m_candles(local_symbol, minutes=fetch_mins)
            if ub_candles:
                mapped_krw = mb_series.map(ub_candles)
                if local_symbol.upper().endswith("KRW"):
                    mapped_usd = mapped_krw / merged["fx_rate"].clip(lower=500.0)
                else:
                    mapped_usd = mapped_krw
                merged.loc[local_needs_fill & mapped_usd.notna(), "local_price"] = mapped_usd[
                    local_needs_fill & mapped_usd.notna()
                ]

        if n_missing_foreign > 3 and foreign_exchange.upper() == "COINBASE":
            first_missing_mb = int(mb_series[foreign_needs_fill].min())
            fetch_mins = max(60, min(1440, now_mb - first_missing_mb + 15))
            logger.info(
                f"Backfilling {n_missing_foreign} missing/frozen 1m buckets for {foreign_exchange}:{foreign_symbol} "
                f"(fetching {fetch_mins}m of REST candles)..."
            )
            cb_candles = self._fetch_coinbase_1m_candles(foreign_symbol, minutes=fetch_mins)
            if cb_candles:
                mapped_cb = mb_series.map(cb_candles)
                merged.loc[foreign_needs_fill & mapped_cb.notna(), "foreign_price"] = mapped_cb[
                    foreign_needs_fill & mapped_cb.notna()
                ]

        return merged
