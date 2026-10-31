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

    def _fetch_upbit_candles_generic(
        self, symbol: str, step_min: int, count_total: int
    ) -> Dict[int, float]:
        parts = symbol.upper().split("-")
        upbit_market = f"{parts[1]}-{parts[0]}" if len(parts) == 2 else symbol.upper()
        candles: Dict[int, float] = {}
        to_param = ""
        remaining = max(1, count_total)
        endpoint = (
            "https://api.upbit.com/v1/candles/days"
            if step_min >= 1440
            else f"https://api.upbit.com/v1/candles/minutes/{step_min}"
        )

        while remaining > 0:
            batch = min(200, remaining)
            url = f"{endpoint}?market={upbit_market}&count={batch}"
            if to_param:
                url += f"&to={urllib.parse.quote(to_param)}"
            try:
                req = urllib.request.Request(url, headers={"User-Agent": "magnolia-ai/1.0"})
                with urllib.request.urlopen(req, timeout=8) as resp:
                    data = json.loads(resp.read().decode("utf-8"))
                if not data or not isinstance(data, list):
                    break
                for item in data:
                    utc_str = item.get("candle_date_time_utc")
                    price = float(item.get("trade_price", 0.0))
                    if utc_str and price > 0:
                        dt = datetime.fromisoformat(utc_str).replace(tzinfo=timezone.utc)
                        bucket_idx = int(dt.timestamp() // (step_min * 60))
                        candles[bucket_idx] = price
                oldest_utc = data[-1].get("candle_date_time_utc")
                if not oldest_utc or len(data) < batch:
                    break
                to_param = oldest_utc + "Z"
                remaining -= len(data)
            except Exception as e:
                logger.warning(f"Upbit {step_min}m candle fetch warning ({upbit_market}): {e}")
                break
        return candles

    def _fetch_coinbase_candles_generic(
        self, symbol: str, step_min: int, count_total: int
    ) -> Dict[int, float]:
        cb_product = symbol.upper()
        granularity = step_min * 60  # 900 (15m), 3600 (1h), 86400 (1d)
        candles: Dict[int, float] = {}
        end_dt = datetime.now(timezone.utc)
        remaining = max(1, count_total)

        while remaining > 0:
            batch = min(280, remaining)
            start_dt = end_dt - timedelta(minutes=batch * step_min)
            url = (
                f"https://api.exchange.coinbase.com/products/{cb_product}/candles"
                f"?granularity={granularity}&start={start_dt.isoformat()}&end={end_dt.isoformat()}"
            )
            try:
                req = urllib.request.Request(url, headers={"User-Agent": "magnolia-ai/1.0"})
                with urllib.request.urlopen(req, timeout=8) as resp:
                    data = json.loads(resp.read().decode("utf-8"))
                if not data or not isinstance(data, list):
                    break
                for row in data:
                    if isinstance(row, list) and len(row) >= 5:
                        bucket_idx = int(float(row[0]) // granularity)
                        close_price = float(row[4])
                        if close_price > 0:
                            candles[bucket_idx] = close_price
                end_dt = start_dt
                remaining -= batch
            except Exception as e:
                logger.warning(f"Coinbase {step_min}m candle fetch warning ({cb_product}): {e}")
                break
        return candles

    def _fetch_historical_fx_krw(
        self, step_min: int, count_total: int, default_fx: float = 1400.0
    ) -> Dict[int, float]:
        fx_map: Dict[int, float] = {}
        total_days = max(2, int((count_total * step_min) / 1440) + 3)
        yf_range = "10d" if total_days <= 10 else ("1mo" if total_days <= 35 else "1y")
        yf_interval = "1h" if step_min < 1440 else "1d"
        url = f"https://query1.finance.yahoo.com/v8/finance/chart/KRW=X?range={yf_range}&interval={yf_interval}"
        try:
            req = urllib.request.Request(url, headers={"User-Agent": "Mozilla/5.0"})
            with urllib.request.urlopen(req, timeout=8) as resp:
                payload = json.loads(resp.read().decode("utf-8"))
            result = (payload.get("chart", {}).get("result") or [None])[0]
            if result:
                timestamps = result.get("timestamp") or []
                closes = (
                    (result.get("indicators", {}).get("quote") or [{}])[0].get("close")
                    or []
                )
                for ts, val in zip(timestamps, closes):
                    if ts and val and float(val) > 500.0:
                        b_idx = int(float(ts) // (step_min * 60))
                        fx_map[b_idx] = float(val)
        except Exception as e:
            logger.warning(f"Yahoo FX fetch warning ({yf_range}): {e}")

        if not fx_map:
            try:
                end_d = datetime.now(timezone.utc).date()
                start_d = end_d - timedelta(days=total_days)
                f_url = f"https://api.frankfurter.app/{start_d.isoformat()}..{end_d.isoformat()}?from=USD&to=KRW"
                req = urllib.request.Request(f_url, headers={"User-Agent": "magnolia-ai/1.0"})
                with urllib.request.urlopen(req, timeout=8) as resp:
                    f_data = json.loads(resp.read().decode("utf-8"))
                for d_str, r_obj in (f_data.get("rates") or {}).items():
                    krw_val = float(r_obj.get("KRW", 0.0))
                    if krw_val > 500.0:
                        dt = datetime.fromisoformat(d_str).replace(tzinfo=timezone.utc)
                        b_idx = int(dt.timestamp() // (step_min * 60))
                        fx_map[b_idx] = krw_val
            except Exception as e:
                logger.warning(f"Frankfurter FX fallback warning: {e}")

        return fx_map

    def fetch_long_range_spreads(
        self,
        local_exchange: str,
        local_symbol: str,
        foreign_exchange: str,
        foreign_symbol: str,
        range_code: str = "7D",
        default_fx: float = 1400.0,
    ) -> Optional[pd.DataFrame]:
        """
        Builds a continuous downsampled historical spread DataFrame for:
          - '7D': 15m buckets, 672 points (7 days)
          - '1M': 60m (1h) buckets, 720 points (30 days)
          - '1Y': 1440m (1d) buckets, 365 points (365 days)
        Combines InfluxDB aggregated data with public exchange candle & historical FX backfill.
        """
        rc = range_code.upper()
        if rc == "7D":
            step_min, n_buckets, influx_every, hours = 15, 672, "15m", 168
        elif rc == "1M":
            step_min, n_buckets, influx_every, hours = 60, 720, "1h", 720
        elif rc == "1Y":
            step_min, n_buckets, influx_every, hours = 1440, 365, "1d", 8760
        else:
            return None

        step_sec = step_min * 60
        now_sec = int(datetime.now(timezone.utc).timestamp())
        cur_bucket = now_sec // step_sec
        buckets = [cur_bucket - (n_buckets - 1 - i) for i in range(n_buckets)]

        grid = pd.DataFrame({
            "bucket_idx": buckets,
            "minute_bucket": [b * step_min for b in buckets],
            "offset_min": [-(n_buckets - 1 - i) * step_min for i in range(n_buckets)],
            "_time": [
                datetime.fromtimestamp(b * step_sec, tz=timezone.utc) for b in buckets
            ],
            "local_price": [float("nan")] * n_buckets,
            "foreign_price": [float("nan")] * n_buckets,
            "fx_rate": [float("nan")] * n_buckets,
        })

        # 1. Query InfluxDB aggregated series first
        try:
            q_local = f"""
                from(bucket:"{self.bucket}")
                |> range(start: -{hours}h)
                |> filter(fn: (r) => r._measurement == "exchange_tickers")
                |> filter(fn: (r) => r.exchange == "{local_exchange}" and r.symbol == "{local_symbol}" and r.is_future == "false")
                |> filter(fn: (r) => r._field == "ask_usd" or r._field == "bid_usd")
                |> aggregateWindow(every: {influx_every}, fn: mean, createEmpty: false)
                |> yield(name: "local")
            """
            q_foreign = f"""
                from(bucket:"{self.bucket}")
                |> range(start: -{hours}h)
                |> filter(fn: (r) => r._measurement == "exchange_tickers")
                |> filter(fn: (r) => r.exchange == "{foreign_exchange}" and r.symbol == "{foreign_symbol}" and r.is_future == "false")
                |> filter(fn: (r) => r._field == "ask_usd" or r._field == "bid_usd")
                |> aggregateWindow(every: {influx_every}, fn: mean, createEmpty: false)
                |> yield(name: "foreign")
            """
            q_fx = f"""
                from(bucket:"{self.bucket}")
                |> range(start: -{hours}h)
                |> filter(fn: (r) => r._measurement == "fx_rate" or r._measurement == "macro_indicators")
                |> filter(fn: (r) => r.currency_pair == "USD/KRW")
                |> filter(fn: (r) => r._field == "fx_rate")
                |> aggregateWindow(every: {influx_every}, fn: mean, createEmpty: false)
                |> yield(name: "fx")
            """
            df_l = self._query_to_df(q_local, "local_price")
            df_f = self._query_to_df(q_foreign, "foreign_price")
            df_x = self._query_to_df(q_fx, "fx_rate")

            if not df_l.empty:
                b_l = (pd.to_datetime(df_l["_time"], utc=True).astype("int64") // 10**9 // step_sec).astype(int)
                l_map = dict(zip(b_l, df_l["local_price"]))
                grid["local_price"] = grid["bucket_idx"].map(l_map)
            if not df_f.empty:
                b_f = (pd.to_datetime(df_f["_time"], utc=True).astype("int64") // 10**9 // step_sec).astype(int)
                f_map = dict(zip(b_f, df_f["foreign_price"]))
                grid["foreign_price"] = grid["bucket_idx"].map(f_map)
            if not df_x.empty:
                b_x = (pd.to_datetime(df_x["_time"], utc=True).astype("int64") // 10**9 // step_sec).astype(int)
                x_map = dict(zip(b_x, df_x["fx_rate"]))
                grid["fx_rate"] = grid["bucket_idx"].map(x_map)
        except Exception as e:
            logger.warning(f"InfluxDB long-range ({rc}) query warning: {e}")

        # 2. Fill missing FX rates using historical FX API
        if grid["fx_rate"].isna().sum() > 2:
            hist_fx = self._fetch_historical_fx_krw(step_min, n_buckets, default_fx=default_fx)
            if hist_fx:
                fx_fill = grid["bucket_idx"].map(hist_fx)
                grid.loc[grid["fx_rate"].isna() & fx_fill.notna(), "fx_rate"] = fx_fill[
                    grid["fx_rate"].isna() & fx_fill.notna()
                ]
        grid["fx_rate"] = grid["fx_rate"].interpolate(method="linear").ffill().bfill().fillna(default_fx)

        # 3. Backfill missing Local & Foreign candles from public REST APIs
        missing_local = int(grid["local_price"].isna().sum())
        if missing_local > 2 and local_exchange.upper() == "UPBIT":
            ub_map = self._fetch_upbit_candles_generic(local_symbol, step_min, n_buckets + 5)
            if ub_map:
                mapped_krw = grid["bucket_idx"].map(ub_map)
                if local_symbol.upper().endswith("KRW"):
                    mapped_usd = mapped_krw / grid["fx_rate"].clip(lower=500.0)
                else:
                    mapped_usd = mapped_krw
                mask = grid["local_price"].isna() & mapped_usd.notna()
                grid.loc[mask, "local_price"] = mapped_usd[mask]

        missing_foreign = int(grid["foreign_price"].isna().sum())
        if missing_foreign > 2 and foreign_exchange.upper() == "COINBASE":
            cb_map = self._fetch_coinbase_candles_generic(foreign_symbol, step_min, n_buckets + 5)
            if cb_map:
                mapped_cb = grid["bucket_idx"].map(cb_map)
                mask = grid["foreign_price"].isna() & mapped_cb.notna()
                grid.loc[mask, "foreign_price"] = mapped_cb[mask]

        grid["local_price"] = grid["local_price"].ffill().bfill()
        grid["foreign_price"] = grid["foreign_price"].ffill().bfill()
        if grid["local_price"].isna().all() or grid["foreign_price"].isna().all():
            return None

        grid["exchange_premium_spread"] = (grid["local_price"] / grid["foreign_price"]) - 1.0
        return grid
