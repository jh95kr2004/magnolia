import os
import pandas as pd
from datetime import datetime, timedelta
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
            |> filter(fn: (r) => r._measurement == "macro_indicators")
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

            # Forward fill and drop NaNs
            merged = merged.ffill().dropna()

            # Calculate dynamic premium
            # Spread = (Local Price) / (Foreign Price * FX Rate) - 1.0
            merged["exchange_premium_spread"] = (
                merged["local_price"] / (merged["foreign_price"] * merged["fx_rate"])
            ) - 1.0

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
