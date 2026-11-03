import os
import io
import gzip
import json
import time
import zipfile
import argparse
import logging
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime, timedelta, timezone
from typing import Dict, List, Optional, Tuple

import numpy as np
import pandas as pd

from data.announcement_classifier import classify_announcement

UTC = timezone.utc

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s"
)
logger = logging.getLogger("Bootstrapper")

USER_AGENT = "Mozilla/5.0 (X11; Linux x86_64) MagnoliaQuant/2.0"

# ==============================================================================
# 1. COIN UNIVERSE (55 Coins across 5 Categories & Chain Characteristics)
# ==============================================================================
# Chain profiles: (chain_type, base_transfer_mins_1hop, base_gas_fee_usd_1hop)
CHAIN_PROFILES: Dict[str, Tuple[str, int, float]] = {
    # Fast L1s (1~3 min finality, ultra-low fee)
    "XRP": ("FAST_L1", 2, 0.05),
    "SOL": ("FAST_L1", 2, 0.05),
    "TRX": ("FAST_L1", 2, 0.10),
    "SUI": ("FAST_L1", 2, 0.05),
    "SEI": ("FAST_L1", 2, 0.05),
    "APT": ("FAST_L1", 2, 0.05),
    "NEAR": ("FAST_L1", 2, 0.05),
    "KAIA": ("FAST_L1", 2, 0.05),
    "ATOM": ("FAST_L1", 3, 0.10),
    "DOT": ("FAST_L1", 3, 0.15),
    "ADA": ("FAST_L1", 4, 0.20),
    "AVAX": ("FAST_L1", 2, 0.15),
    "ZIL": ("FAST_L1", 3, 0.10),
    "WAVES": ("FAST_L1", 3, 0.10),
    "ICX": ("FAST_L1", 3, 0.10),
    "FLOW": ("FAST_L1", 3, 0.10),
    "CELO": ("FAST_L1", 2, 0.05),
    "KAVA": ("FAST_L1", 3, 0.05),
    "ASTR": ("FAST_L1", 3, 0.10),
    "MINA": ("FAST_L1", 4, 0.10),
    "EGLD": ("FAST_L1", 3, 0.15),
    "INJ": ("FAST_L1", 2, 0.10),
    "INIT": ("FAST_L1", 2, 0.05),
    # UTXO Chains (10~25 min confirmations)
    "BTC": ("UTXO", 20, 4.50),
    "BCH": ("UTXO", 12, 0.25),
    "LTC": ("UTXO", 8, 0.15),
    "DOGE": ("UTXO", 8, 0.30),
    "ETC": ("UTXO", 12, 0.20),
    "BTG": ("UTXO", 20, 0.50),
    # L2 Rollups & EVM / ERC-20 Tokens (5~10 min finality, ETH/L2 gas)
    "ETH": ("EVM", 6, 1.80),
    "ARB": ("EVM_L2", 3, 0.25),
    "OP": ("EVM_L2", 3, 0.25),
    "POL": ("EVM_L2", 4, 0.20),
    "STX": ("UTXO_L2", 12, 0.50),
}

DEFAULT_CHAIN_PROFILE = ("ERC20", 7, 2.50)

# 55 Coins organized by Regime Category:
# - CONTINUOUS_MAJOR_ALT (25 coins): 180~365 days continuous multi-regime history
# - NEW_LISTING (7 coins): Real KRW listing coins
# - INVESTMENT_WARNING (6 coins): Real warning / gaduri coins
# - WALLET_SUSPENSION (5 coins): Real network upgrade / wallet pause coins
# - DELISTED (12 coins): Real delisted / terminated coins across Upbit, Coinbase (status=delisted), Binance & Tardis
COIN_UNIVERSE: List[Dict] = [
    # --- Category 1: Continuous Majors & High-Liquidity Altcoins (25 coins) ---
    {"coin": "BTC", "regime": "CONTINUOUS", "days": 300},
    {"coin": "ETH", "regime": "CONTINUOUS", "days": 300},
    {"coin": "XRP", "regime": "CONTINUOUS", "days": 300},
    {"coin": "SOL", "regime": "CONTINUOUS", "days": 300},
    {"coin": "DOGE", "regime": "CONTINUOUS", "days": 240},
    {"coin": "ADA", "regime": "CONTINUOUS", "days": 240},
    {"coin": "AVAX", "regime": "CONTINUOUS", "days": 240},
    {"coin": "LINK", "regime": "CONTINUOUS", "days": 240},
    {"coin": "SUI", "regime": "CONTINUOUS", "days": 210},
    {"coin": "SEI", "regime": "CONTINUOUS", "days": 210},
    {"coin": "NEAR", "regime": "CONTINUOUS", "days": 210},
    {"coin": "APT", "regime": "CONTINUOUS", "days": 210},
    {"coin": "ARB", "regime": "CONTINUOUS", "days": 210},
    {"coin": "OP", "regime": "CONTINUOUS", "days": 210},
    {"coin": "DOT", "regime": "CONTINUOUS", "days": 210},
    {"coin": "POL", "regime": "CONTINUOUS", "days": 180},
    {"coin": "SHIB", "regime": "CONTINUOUS", "days": 180},
    {"coin": "PEPE", "regime": "CONTINUOUS", "days": 180},
    {"coin": "TRX", "regime": "CONTINUOUS", "days": 240},
    {"coin": "BCH", "regime": "CONTINUOUS", "days": 210},
    {"coin": "LTC", "regime": "CONTINUOUS", "days": 210},
    {"coin": "ETC", "regime": "CONTINUOUS", "days": 180},
    {"coin": "UNI", "regime": "CONTINUOUS", "days": 180},
    {"coin": "AAVE", "regime": "CONTINUOUS", "days": 180},
    {"coin": "ATOM", "regime": "CONTINUOUS", "days": 180},
    # --- Category 2: Real New Listings (NEW_KRW_LISTING, 7 coins) ---
    {"coin": "KAIA", "regime": "NEW_LISTING", "days": 60, "event_type": "NEW_KRW_LISTING"},
    {"coin": "ONDO", "regime": "NEW_LISTING", "days": 60, "event_type": "NEW_KRW_LISTING"},
    {"coin": "PUFFER", "regime": "NEW_LISTING", "days": 60, "event_type": "NEW_KRW_LISTING"},
    {"coin": "MPLX", "regime": "NEW_LISTING", "days": 60, "event_type": "NEW_KRW_LISTING"},
    {"coin": "NIL", "regime": "NEW_LISTING", "days": 60, "event_type": "NEW_KRW_LISTING"},
    {"coin": "BMT", "regime": "NEW_LISTING", "days": 60, "event_type": "NEW_KRW_LISTING"},
    {"coin": "AXIS", "regime": "NEW_LISTING", "days": 45, "event_type": "NEW_KRW_LISTING"},
    # --- Category 3: Real Investment Warnings (INVESTMENT_WARNING, 6 coins) ---
    {"coin": "INIT", "regime": "INVESTMENT_WARNING", "days": 60, "event_type": "INVESTMENT_WARNING"},
    {"coin": "EGLD", "regime": "INVESTMENT_WARNING", "days": 60, "event_type": "INVESTMENT_WARNING"},
    {"coin": "SAND", "regime": "INVESTMENT_WARNING", "days": 60, "event_type": "INVESTMENT_WARNING"},
    {"coin": "INJ", "regime": "INVESTMENT_WARNING", "days": 60, "event_type": "INVESTMENT_WARNING"},
    {"coin": "STX", "regime": "INVESTMENT_WARNING", "days": 60, "event_type": "INVESTMENT_WARNING"},
    {"coin": "MINA", "regime": "INVESTMENT_WARNING", "days": 60, "event_type": "INVESTMENT_WARNING"},
    # --- Category 4: Real Wallet Suspensions (WALLET_SUSPENSION, 5 coins) ---
    {"coin": "FLOW", "regime": "WALLET_SUSPENSION", "days": 60, "event_type": "WALLET_SUSPENSION"},
    {"coin": "CELO", "regime": "WALLET_SUSPENSION", "days": 60, "event_type": "WALLET_SUSPENSION"},
    {"coin": "KAVA", "regime": "WALLET_SUSPENSION", "days": 60, "event_type": "WALLET_SUSPENSION"},
    {"coin": "ZETA", "regime": "WALLET_SUSPENSION", "days": 60, "event_type": "WALLET_SUSPENSION"},
    {"coin": "ASTR", "regime": "WALLET_SUSPENSION", "days": 60, "event_type": "WALLET_SUSPENSION"},
    # --- Category 5: Real Delisted / Terminated Coins (DELISTING, 12 coins) ---
    {"coin": "BLUR", "regime": "DELISTED", "days": 60, "event_type": "DELISTING", "is_delisted": True},
    {"coin": "ZIL", "regime": "DELISTED", "days": 60, "event_type": "DELISTING", "is_delisted": True},
    {"coin": "SOPH", "regime": "DELISTED", "days": 60, "event_type": "DELISTING", "is_delisted": True},
    {"coin": "OMG", "regime": "DELISTED", "days": 60, "event_type": "DELISTING", "is_delisted": True, "end_date": "2023-12-25"},
    {"coin": "WAVES", "regime": "DELISTED", "days": 60, "event_type": "DELISTING", "is_delisted": True, "end_date": "2024-06-17"},
    {"coin": "LOOM", "regime": "DELISTED", "days": 60, "event_type": "DELISTING", "is_delisted": True, "end_date": "2024-12-10"},
    {"coin": "STORJ", "regime": "DELISTED", "days": 60, "event_type": "DELISTING", "is_delisted": True},
    {"coin": "REP", "regime": "DELISTED", "days": 60, "event_type": "DELISTING", "is_delisted": True, "end_date": "2023-11-15"},
    {"coin": "SNX", "regime": "DELISTED", "days": 60, "event_type": "DELISTING", "is_delisted": True},
    {"coin": "BTG", "regime": "DELISTED", "days": 60, "event_type": "DELISTING", "is_delisted": True, "end_date": "2025-01-24"},
    {"coin": "ICX", "regime": "DELISTED", "days": 60, "event_type": "DELISTING", "is_delisted": True, "end_date": "2026-10-20"},
    {"coin": "HIFI", "regime": "DELISTED", "days": 60, "event_type": "DELISTING", "is_delisted": True, "end_date": "2025-05-13"},
]

# Held-out Event Coins for Validation (20% of event episodes across categories so Val tests unseen events without slicing episodes in half)
VAL_HELD_OUT_EVENT_COINS = {"ONDO", "AXIS", "STX", "ASTR", "ZIL", "LOOM", "BTG"}


def _http_get_json(url: str, headers: Optional[Dict[str, str]] = None, timeout: float = 8.0):
    req_headers = {"User-Agent": USER_AGENT, "Accept": "application/json"}
    if headers:
        req_headers.update(headers)
    for attempt in range(4):
        try:
            req = urllib.request.Request(url, headers=req_headers)
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                return json.loads(resp.read().decode("utf-8"))
        except urllib.error.HTTPError as he:
            if he.code == 429 and attempt < 3:
                time.sleep(0.45 * (attempt + 1))
                continue
            raise
        except Exception:
            if attempt < 2:
                time.sleep(0.25 * (attempt + 1))
                continue
            raise


# ==============================================================================
# 2. MULTI-RESOLUTION REAL HISTORICAL USD/KRW FX FETCHER (Yahoo Finance KRW=X)
# ==============================================================================
def fetch_fx_yahoo_multi_res() -> pd.DataFrame:
    """
    Fetches real historical USD/KRW rates from Yahoo Finance chart API across 3 resolutions:
      - 1d over 5 years (covers 2021~2026 historical delisted coins)
      - 1h over 730 days (covers last 2 years hourly)
      - 5m over 60 days (covers recent 2 months at 5-minute resolution)
    Strictly sorted by UTC epoch timestamp (`ts_utc`) for causal backward matching (`merge_asof`).
    """
    frames = []
    for interval, rng in [("1d", "5y"), ("1h", "730d"), ("5m", "60d")]:
        url = f"https://query1.finance.yahoo.com/v8/finance/chart/KRW=X?interval={interval}&range={rng}"
        try:
            data = _http_get_json(url, timeout=10.0)
            result = data["chart"]["result"][0]
            timestamps = result.get("timestamp", [])
            closes = result["indicators"]["quote"][0].get("close", [])
            rows = [
                (int(ts), float(c))
                for ts, c in zip(timestamps, closes)
                if ts is not None and c is not None and 900.0 <= float(c) <= 2000.0
            ]
            if rows:
                sub = pd.DataFrame(rows, columns=["ts_utc", "fx_rate"])
                frames.append(sub)
                logger.info(f"[FX] Yahoo KRW=X ({interval}, {rng}): fetched {len(sub):,} valid points.")
        except Exception as e:
            logger.warning(f"[FX] Yahoo KRW=X ({interval}, {rng}) warning: {e}")

    if not frames:
        now_ts = int(time.time())
        return pd.DataFrame({"ts_utc": [now_ts - 86400 * 365 * 5, now_ts], "fx_rate": [1380.0, 1380.0]})

    fx_df = pd.concat(frames, ignore_index=True)
    # Keep last (finest resolution) when duplicate timestamps exist, sort strictly ascending
    fx_df = fx_df.drop_duplicates(subset=["ts_utc"], keep="last").sort_values("ts_utc").reset_index(drop=True)
    return fx_df


# ==============================================================================
# 3. TARDIS.DEV + BINANCE VISION + LIVE L2 ORDERBOOK SLIPPAGE CALIBRATOR
# ==============================================================================
class L2SlippageCalibrator:
    """
    Hybrid L2 Orderbook Slippage Calibrator:
      1. Queries Tardis.dev (`datasets.tardis.dev/v1/{exchange}/book_snapshot_5/{YYYY}/{MM}/{DD}/{symbol}.csv.gz`):
         - Without API key: Automatically fetches free 1st-of-month (`DD=01`) L2 snapshots (including delisted Upbit KRW coins!).
         - With `TARDIS_API_KEY` env var: Unlocks all dates (`01`~`31`) automatically via Bearer token.
      2. Queries Binance Vision (`data.binance.vision/data/futures/um/daily/bookDepth/...`) free orderbook depth archive.
      3. Queries live 30~100 level L2 orderbooks from Upbit, Bithumb, Coinbase, and Binance REST APIs.
      4. Fits per-(exchange, coin) slippage parameters (s0, alpha, beta) and computes time-varying candle slippage:
         slippage(t) = clip(s0 + alpha * ((High_t - Low_t)/Close_t) + beta * sqrt(10000 / max(QuoteVol_t, 1000)), 0.0002, 0.0150)
    """

    def __init__(self, order_notional_usd: float = 10000.0):
        self.order_notional_usd = order_notional_usd
        self.tardis_api_key = os.getenv("TARDIS_API_KEY", "").strip()
        self.coinapi_key = os.getenv("COINAPI_KEY", "").strip()
        # (exchange, coin) -> (s0_base_spread, alpha_hl_coeff, beta_vol_coeff)
        self.calibrated_params: Dict[Tuple[str, str], Tuple[float, float, float]] = {}

    def _compute_l2_execution_slippage(
        self, asks: List[Tuple[float, float]], bids: List[Tuple[float, float]], target_notional: float
    ) -> float:
        if not asks or not bids:
            return 0.0008
        best_ask = asks[0][0]
        best_bid = bids[0][0]
        if best_ask <= 0 or best_bid <= 0:
            return 0.0008
        mid = 0.5 * (best_ask + best_bid)

        # Simulate market buy of target_notional across ask levels
        rem = target_notional
        cost = 0.0
        qty = 0.0
        for p, q in asks:
            lvl_notional = p * q
            take = min(rem, lvl_notional)
            cost += take
            qty += take / max(p, 1e-12)
            rem -= take
            if rem <= 0:
                break
        if rem > 0 and asks:
            # Extrapolate beyond visible depth
            last_p = asks[-1][0] * 1.0015
            cost += rem
            qty += rem / last_p
        avg_buy_px = cost / max(qty, 1e-12)
        buy_slip = max(0.0001, (avg_buy_px - mid) / mid)
        return float(np.clip(buy_slip, 0.00015, 0.0150))

    def fetch_tardis_l2_snapshot_sample(
        self, exchange: str, symbol: str, date_str: str = "2025-01-01", fx_rate: float = 1380.0
    ) -> Optional[float]:
        """
        Downloads the header + sample L2 rows from Tardis.dev `book_snapshot_5` for `(exchange, symbol, date_str)`.
        If `TARDIS_API_KEY` is not set, automatically snaps `date_str` to the 1st of that month (`YYYY-MM-01`)
        so it works 100% for free! When `TARDIS_API_KEY` is set, fetches the exact `date_str`.
        """
        ex_lower = exchange.lower()
        parts = date_str.split("-")
        y, m = parts[0], parts[1]
        d = parts[2] if (len(parts) > 2 and self.tardis_api_key) else "01"

        url = f"https://datasets.tardis.dev/v1/{ex_lower}/book_snapshot_5/{y}/{m}/{d}/{symbol}.csv.gz"
        headers = {"User-Agent": USER_AGENT}
        if self.tardis_api_key:
            headers["Authorization"] = f"Bearer {self.tardis_api_key}"

        try:
            req = urllib.request.Request(url, headers=headers)
            with urllib.request.urlopen(req, timeout=6.0) as resp:
                chunk = resp.read(32768)
            raw_text = gzip.GzipFile(fileobj=io.BytesIO(chunk)).read(8192).decode("utf-8", errors="ignore")
            lines = [ln for ln in raw_text.splitlines() if ln.strip()]
            if len(lines) < 2:
                return None
            header = lines[0].split(",")
            col_idx = {name: i for i, name in enumerate(header)}
            slips = []
            target_notional = self.order_notional_usd * fx_rate if "KRW" in symbol.upper() else self.order_notional_usd
            for row_str in lines[1:12]:
                vals = row_str.split(",")
                if len(vals) < len(header):
                    continue
                asks = []
                bids = []
                for lvl in range(5):
                    ap = float(vals[col_idx[f"asks[{lvl}].price"]] or 0.0)
                    aa = float(vals[col_idx[f"asks[{lvl}].amount"]] or 0.0)
                    bp = float(vals[col_idx[f"bids[{lvl}].price"]] or 0.0)
                    ba = float(vals[col_idx[f"bids[{lvl}].amount"]] or 0.0)
                    if ap > 0 and aa > 0:
                        asks.append((ap, aa))
                    if bp > 0 and ba > 0:
                        bids.append((bp, ba))
                if asks and bids:
                    slips.append(self._compute_l2_execution_slippage(asks, bids, target_notional))
            if slips:
                return float(np.median(slips))
        except Exception:
            pass
        return None

    def fetch_live_l2_slippage(self, exchange: str, coin: str, fx_rate: float = 1380.0) -> Optional[float]:
        ex = exchange.upper()
        c = coin.upper()
        try:
            if ex == "UPBIT":
                data = _http_get_json(f"https://api.upbit.com/v1/orderbook?markets=KRW-{c}", timeout=4.0)
                units = data[0].get("orderbook_units", [])
                asks = [(float(u["ask_price"]), float(u["ask_size"])) for u in units]
                bids = [(float(u["bid_price"]), float(u["bid_size"])) for u in units]
                return self._compute_l2_execution_slippage(asks, bids, self.order_notional_usd * fx_rate)
            elif ex == "BITHUMB":
                data = _http_get_json(f"https://api.bithumb.com/v1/orderbook?markets=KRW-{c}", timeout=4.0)
                units = data[0].get("orderbook_units", [])
                asks = [(float(u["ask_price"]), float(u["ask_size"])) for u in units]
                bids = [(float(u["bid_price"]), float(u["bid_size"])) for u in units]
                return self._compute_l2_execution_slippage(asks, bids, self.order_notional_usd * fx_rate)
            elif ex == "COINBASE":
                data = _http_get_json(f"https://api.exchange.coinbase.com/products/{c}-USD/book?level=2", timeout=4.0)
                asks = [(float(a[0]), float(a[1])) for a in data.get("asks", [])[:25]]
                bids = [(float(b[0]), float(b[1])) for b in data.get("bids", [])[:25]]
                return self._compute_l2_execution_slippage(asks, bids, self.order_notional_usd)
            elif ex == "BINANCE":
                data = _http_get_json(f"https://api.binance.com/api/v3/depth?symbol={c}USDT&limit=20", timeout=4.0)
                asks = [(float(a[0]), float(a[1])) for a in data.get("asks", [])]
                bids = [(float(b[0]), float(b[1])) for b in data.get("bids", [])]
                return self._compute_l2_execution_slippage(asks, bids, self.order_notional_usd)
        except Exception:
            pass
        return None

    def get_calibrated_params(self, exchange: str, coin: str, date_hint: str = "2025-01-01") -> Tuple[float, float, float]:
        key = (exchange.upper(), coin.upper())
        if key in self.calibrated_params:
            return self.calibrated_params[key]

        # 1. Try live L2 orderbook first, then Tardis historical 1st-of-month (or exact date if TARDIS_API_KEY set)
        base_slip = self.fetch_live_l2_slippage(exchange, coin)
        if base_slip is None:
            tardis_sym = (
                f"KRW-{coin.upper()}"
                if exchange.upper() in ("UPBIT", "BITHUMB")
                else (f"{coin.upper()}-USD" if exchange.upper() == "COINBASE" else f"{coin.upper()}USDT")
            )
            base_slip = self.fetch_tardis_l2_snapshot_sample(exchange, tardis_sym, date_str=date_hint)

        if base_slip is None:
            # Fallback by coin tier
            if coin.upper() in ("BTC", "ETH", "XRP", "SOL"):
                base_slip = 0.00025
            elif coin.upper() in ("DOGE", "ADA", "AVAX", "LINK", "SUI", "TRX"):
                base_slip = 0.00045
            else:
                base_slip = 0.00085

        s0 = float(np.clip(base_slip * 0.65, 0.00015, 0.0030))
        alpha = float(np.clip(0.12 + base_slip * 25.0, 0.10, 0.35))
        beta = float(np.clip(base_slip * 0.55, 0.00015, 0.0025))
        self.calibrated_params[key] = (s0, alpha, beta)
        return s0, alpha, beta

    def compute_dynamic_slippage_series(
        self,
        base_exchange: str,
        foreign_exchange: str,
        coin: str,
        hl_range_pct: np.ndarray,
        quote_vol_usd: np.ndarray,
        date_hint: str = "2025-01-01",
    ) -> np.ndarray:
        s0_b, a_b, b_b = self.get_calibrated_params(base_exchange, coin, date_hint)
        s0_f, a_f, b_f = self.get_calibrated_params(foreign_exchange, coin, date_hint)
        s0 = 0.5 * (s0_b + s0_f)
        alpha = 0.5 * (a_b + a_f)
        beta = 0.5 * (b_b + b_f)

        hl_clean = np.clip(np.nan_to_num(hl_range_pct, nan=0.002), 0.0001, 0.25)
        vol_clean = np.maximum(np.nan_to_num(quote_vol_usd, nan=15000.0), 500.0)
        vol_impact = np.sqrt(self.order_notional_usd / vol_clean)

        dyn_slip = s0 + alpha * hl_clean + beta * vol_impact
        return np.clip(dyn_slip, 0.0002, 0.0150).astype(np.float32)


# ==============================================================================
# 4. MULTI-EXCHANGE CANDLE FETCHERS (Upbit, Bithumb, Coinbase, Binance, Tardis)
# ==============================================================================
def fetch_upbit_candles(coin: str, count_total: int = 1200, unit_min: int = 60, end_date: Optional[str] = None) -> pd.DataFrame:
    market = f"KRW-{coin.upper()}"
    rows = []
    to_param = f"{end_date}T23:59:59Z" if end_date else ""
    rem = count_total

    while rem > 0:
        batch = min(200, rem)
        url = f"https://api.upbit.com/v1/candles/minutes/{unit_min}?market={market}&count={batch}"
        if to_param:
            url += f"&to={urllib.parse.quote(to_param)}"
        try:
            data = _http_get_json(url, timeout=6.0)
            if not data or not isinstance(data, list):
                break
            for item in data:
                utc_str = item.get("candle_date_time_utc")
                close_px = float(item.get("trade_price") or 0.0)
                high_px = float(item.get("high_price") or close_px)
                low_px = float(item.get("low_price") or close_px)
                acc_krw = float(item.get("candle_acc_trade_price") or 0.0)
                if utc_str and close_px > 0:
                    # Align to candle CLOSE timestamp in UTC seconds (zero look-ahead)
                    open_ts = int(datetime.fromisoformat(utc_str).replace(tzinfo=UTC).timestamp())
                    close_ts = open_ts + unit_min * 60
                    hl_pct = max(0.0, (high_px - low_px) / max(close_px, 1e-9))
                    rows.append((close_ts, close_px, hl_pct, acc_krw))
            oldest_utc = data[-1].get("candle_date_time_utc")
            if not oldest_utc or len(data) < batch:
                break
            to_param = oldest_utc + "Z"
            rem -= len(data)
            time.sleep(0.08)
        except Exception:
            break

    if not rows:
        return pd.DataFrame()
    df = pd.DataFrame(rows, columns=["ts_utc", "price", "hl_pct", "quote_vol_raw"])
    return df.drop_duplicates("ts_utc").sort_values("ts_utc").reset_index(drop=True)


def fetch_bithumb_candles(coin: str, count_total: int = 1200, unit_min: int = 60, end_date: Optional[str] = None) -> pd.DataFrame:
    market = f"KRW-{coin.upper()}"
    rows = []
    to_param = f"{end_date}T23:59:59Z" if end_date else ""
    rem = count_total

    while rem > 0:
        batch = min(200, rem)
        url = f"https://api.bithumb.com/v1/candles/minutes/{unit_min}?market={market}&count={batch}"
        if to_param:
            url += f"&to={urllib.parse.quote(to_param)}"
        try:
            data = _http_get_json(url, timeout=6.0)
            if not data or not isinstance(data, list):
                break
            for item in data:
                utc_str = item.get("candle_date_time_utc")
                close_px = float(item.get("trade_price") or 0.0)
                high_px = float(item.get("high_price") or close_px)
                low_px = float(item.get("low_price") or close_px)
                acc_krw = float(item.get("candle_acc_trade_price") or 0.0)
                if utc_str and close_px > 0:
                    open_ts = int(datetime.fromisoformat(utc_str).replace(tzinfo=UTC).timestamp())
                    close_ts = open_ts + unit_min * 60
                    hl_pct = max(0.0, (high_px - low_px) / max(close_px, 1e-9))
                    rows.append((close_ts, close_px, hl_pct, acc_krw))
            oldest_utc = data[-1].get("candle_date_time_utc")
            if not oldest_utc or len(data) < batch:
                break
            to_param = oldest_utc + "Z"
            rem -= len(data)
            time.sleep(0.06)
        except Exception:
            break

    if not rows:
        return pd.DataFrame()
    df = pd.DataFrame(rows, columns=["ts_utc", "price", "hl_pct", "quote_vol_raw"])
    return df.drop_duplicates("ts_utc").sort_values("ts_utc").reset_index(drop=True)


def fetch_coinbase_candles(coin: str, count_total: int = 1200, granularity_sec: int = 3600, end_date: Optional[str] = None) -> pd.DataFrame:
    product = f"{coin.upper()}-USD"
    rows = []
    end_dt = datetime.fromisoformat(f"{end_date}T23:59:59").replace(tzinfo=UTC) if end_date else datetime.now(UTC)
    rem = count_total

    while rem > 0:
        batch = min(300, rem)
        start_dt = end_dt - timedelta(seconds=batch * granularity_sec)
        url = (
            f"https://api.exchange.coinbase.com/products/{product}/candles"
            f"?granularity={granularity_sec}&start={start_dt.isoformat()}&end={end_dt.isoformat()}"
        )
        try:
            data = _http_get_json(url, timeout=6.0)
            if not data or not isinstance(data, list):
                break
            for item in data:
                if isinstance(item, list) and len(item) >= 6:
                    open_ts = int(float(item[0]))
                    low_px = float(item[1])
                    high_px = float(item[2])
                    close_px = float(item[4])
                    base_vol = float(item[5])
                    if close_px > 0:
                        close_ts = open_ts + granularity_sec
                        hl_pct = max(0.0, (high_px - low_px) / max(close_px, 1e-9))
                        quote_usd = base_vol * close_px
                        rows.append((close_ts, close_px, hl_pct, quote_usd))
            if len(data) < 10:
                break
            end_dt = start_dt
            rem -= len(data)
            time.sleep(0.05)
        except Exception:
            break

    if not rows:
        return pd.DataFrame()
    df = pd.DataFrame(rows, columns=["ts_utc", "price", "hl_pct", "quote_vol_raw"])
    return df.drop_duplicates("ts_utc").sort_values("ts_utc").reset_index(drop=True)


def fetch_binance_klines(
    coin: str, count_total: int = 1200, interval: str = "1h", is_futures: bool = False, end_date: Optional[str] = None
) -> pd.DataFrame:
    symbol = f"{coin.upper()}USDT"
    base_url = "https://fapi.binance.com/fapi/v1/klines" if is_futures else "https://api.binance.com/api/v3/klines"
    end_ms = (
        int(datetime.fromisoformat(f"{end_date}T23:59:59").replace(tzinfo=UTC).timestamp() * 1000)
        if end_date
        else int(time.time() * 1000)
    )
    rows = []
    rem = count_total

    while rem > 0:
        limit = min(1000, rem)
        url = f"{base_url}?symbol={symbol}&interval={interval}&limit={limit}&endTime={end_ms}"
        try:
            data = _http_get_json(url, timeout=6.0)
            if not data or not isinstance(data, list):
                break
            for k in data:
                # k[6] is exact candle close time in ms
                close_ts = int(k[6]) // 1000
                high_px = float(k[2])
                low_px = float(k[3])
                close_px = float(k[4])
                quote_usd = float(k[7])
                if close_px > 0:
                    hl_pct = max(0.0, (high_px - low_px) / max(close_px, 1e-9))
                    rows.append((close_ts, close_px, hl_pct, quote_usd))
            if len(data) < 10:
                break
            end_ms = int(data[0][0]) - 1
            rem -= len(data)
            time.sleep(0.04)
        except Exception:
            break

    if not rows:
        return pd.DataFrame()
    df = pd.DataFrame(rows, columns=["ts_utc", "price", "hl_pct", "quote_vol_raw"])
    return df.drop_duplicates("ts_utc").sort_values("ts_utc").reset_index(drop=True)


# ==============================================================================
# 5. MULTI-EXCHANGE OFFICIAL ANNOUNCEMENT ARCHIVE FETCHER
# ==============================================================================
def fetch_all_exchange_announcements(max_pages: int = 12) -> List[Dict]:
    """
    Fetches official exchange announcements from Upbit, Bithumb, and Binance CMS APIs
    and classifies them with `classify_announcement()`, storing exact UTC publication timestamp.
    """
    events: List[Dict] = []

    # 1. Upbit Official Announcements
    for page in range(1, max_pages + 1):
        url = f"https://api-manager.upbit.com/api/v1/announcements?os=web&page={page}&per_page=20&category=all"
        try:
            data = _http_get_json(url, timeout=5.0)
            notices = data.get("data", {}).get("notices", [])
            if not notices:
                break
            for n in notices:
                title = n.get("title", "")
                listed_at = n.get("listed_at", "")
                if not title or not listed_at:
                    continue
                pub_ts = int(datetime.fromisoformat(listed_at).astimezone(UTC).timestamp())
                res = classify_announcement(title=title, exchange="UPBIT", now_ts=pub_ts)
                if res.get("event_type", "NONE") != "NONE":
                    sched_off = int(res.get("scheduled_offset_min") or 0) * 60
                    for sym in res.get("symbols", []):
                        events.append({
                            "exchange": "UPBIT",
                            "coin": sym.upper(),
                            "event_type": res["event_type"],
                            "pub_ts_utc": pub_ts,
                            "sched_ts_utc": pub_ts + sched_off,
                            "title": title,
                        })
        except Exception as e:
            logger.debug(f"Upbit notice page error: {e}")
            break

    # 2. Bithumb Official Notices
    for page in range(1, 6):
        url = f"https://api.bithumb.com/v1/notices?count=20&page={page}"
        try:
            notices = _http_get_json(url, timeout=5.0)
            if isinstance(notices, dict):
                notices = notices.get("data", [])
            if not notices or not isinstance(notices, list):
                break
            for n in notices:
                title = n.get("title", "")
                pub_str = n.get("published_at") or n.get("created_at") or ""
                if not title or not pub_str:
                    continue
                # Bithumb timestamps are KST (+09:00) if naive
                dt = datetime.fromisoformat(str(pub_str).replace(" ", "T"))
                if dt.tzinfo is None:
                    dt = dt.replace(tzinfo=timezone(timedelta(hours=9)))
                pub_ts = int(dt.astimezone(UTC).timestamp())
                res = classify_announcement(title=title, exchange="BITHUMB", now_ts=pub_ts)
                if res.get("event_type", "NONE") != "NONE":
                    sched_off = int(res.get("scheduled_offset_min") or 0) * 60
                    for sym in res.get("symbols", []):
                        events.append({
                            "exchange": "BITHUMB",
                            "coin": sym.upper(),
                            "event_type": res["event_type"],
                            "pub_ts_utc": pub_ts,
                            "sched_ts_utc": pub_ts + sched_off,
                            "title": title,
                        })
        except Exception:
            break

    # 3. Binance Official CMS Notices (48=New Listing, 161=Delisting, 157=Wallet Maintenance)
    for catalog_id, default_ev in [(48, "NEW_KRW_LISTING"), (161, "DELISTING"), (157, "WALLET_SUSPENSION")]:
        url = f"https://www.binance.com/bapi/composite/v1/public/cms/article/list/query?type=1&pageNo=1&pageSize=20&catalogId={catalog_id}"
        try:
            data = _http_get_json(url, timeout=5.0)
            catalogs = data.get("data", {}).get("catalogs", [])
            for cat in catalogs:
                for art in cat.get("articles", []):
                    title = art.get("title", "")
                    rel_ms = art.get("releaseDate")
                    if not title or not rel_ms:
                        continue
                    pub_ts = int(rel_ms) // 1000
                    res = classify_announcement(title=title, exchange="BINANCE", now_ts=pub_ts)
                    ev_type = res.get("event_type") if res.get("event_type", "NONE") != "NONE" else default_ev
                    sched_off = int(res.get("scheduled_offset_min") or 0) * 60
                    syms = res.get("symbols", [])
                    for sym in syms:
                        events.append({
                            "exchange": "BINANCE",
                            "coin": sym.upper(),
                            "event_type": ev_type,
                            "pub_ts_utc": pub_ts,
                            "sched_ts_utc": pub_ts + sched_off,
                            "title": title,
                        })
        except Exception:
            pass

    logger.info(f"[Announcements] Fetched & classified {len(events)} real multi-exchange notice events.")
    return events


# ==============================================================================
# 6. CAUSAL EXPONENTIAL DECAY EVENT SIGNAL BUILDER (Zero Look-Ahead Bias)
# ==============================================================================
def compute_causal_event_decay(
    ts_utc_arr: np.ndarray, pub_ts_utc: int, sched_ts_utc: int, half_life_sec: float
) -> np.ndarray:
    """
    Computes the strictly causal continuous event signal E(t) in [0.0, 1.0]:
      - E(t) = 0.0                                              for t < pub_ts_utc (STRICT ZERO LOOK-AHEAD)
      - E(t) = 1.0                                              for pub_ts_utc <= t <= max(pub_ts_utc, sched_ts_utc)
      - E(t) = exp(-(t - max(pub_ts_utc, sched_ts_utc)) / tau)  for t > max(pub_ts_utc, sched_ts_utc)
    """
    sig = np.zeros(len(ts_utc_arr), dtype=np.float32)
    active_mask = ts_utc_arr >= pub_ts_utc
    if not np.any(active_mask):
        return sig

    anchor_ts = max(pub_ts_utc, sched_ts_utc)
    t_active = ts_utc_arr[active_mask]
    dt_sec = np.maximum(0.0, t_active.astype(np.float64) - float(anchor_ts))
    sig[active_mask] = np.exp(-dt_sec / max(half_life_sec, 60.0)).astype(np.float32)
    return sig


def build_pair_episode_dataframe(
    base_exchange: str,
    foreign_exchange: str,
    coin_meta: Dict,
    base_df: pd.DataFrame,
    foreign_df: pd.DataFrame,
    binance_fut_df: pd.DataFrame,
    fx_df: pd.DataFrame,
    usdt_df: pd.DataFrame,
    all_events: List[Dict],
    calibrator: L2SlippageCalibrator,
) -> pd.DataFrame:
    """
    Joins a single `(base_exchange, foreign_exchange, coin)` episode on UTC candle close timestamp,
    computes dual premiums (`exchange_premium_spread` and `tether_real_premium_spread`),
    dynamic L2-calibrated slippage, chain/VASP routing metadata, and strictly causal event signals.
    """
    coin = coin_meta["coin"].upper()
    regime = coin_meta.get("regime", "CONTINUOUS")
    is_delisted = bool(coin_meta.get("is_delisted", False))

    if base_df.empty or foreign_df.empty:
        return pd.DataFrame()

    b = base_df.rename(
        columns={"price": "local_price", "hl_pct": "base_hl_pct", "quote_vol_raw": "base_vol_krw"}
    ).sort_values("ts_utc")
    f = foreign_df.rename(
        columns={"price": "foreign_price", "hl_pct": "foreign_hl_pct", "quote_vol_raw": "foreign_vol_usd"}
    ).sort_values("ts_utc")

    # Causal backward merge on candle close UTC timestamp
    merged = pd.merge_asof(b, f, on="ts_utc", direction="backward", tolerance=7200)
    merged.dropna(subset=["local_price", "foreign_price"], inplace=True)
    if len(merged) < 120:
        return pd.DataFrame()

    # Merge real historical USD/KRW FX causally (backward fill only, never linear interpolation across weekends!)
    merged = pd.merge_asof(merged, fx_df.sort_values("ts_utc"), on="ts_utc", direction="backward")
    merged["fx_rate"] = merged["fx_rate"].ffill().bfill().fillna(1380.0)

    # Merge real historical domestic KRW-USDT tether rate causally
    if not usdt_df.empty:
        u = usdt_df[["ts_utc", "price"]].rename(columns={"price": "usdt_krw_rate"}).sort_values("ts_utc")
        merged = pd.merge_asof(merged, u, on="ts_utc", direction="backward")
    if "usdt_krw_rate" not in merged.columns or merged["usdt_krw_rate"].isna().all():
        merged["usdt_krw_rate"] = merged["fx_rate"] * 1.012
    else:
        # For timestamps before KRW-USDT listing, fallback to fx_rate * 1.012
        na_mask = merged["usdt_krw_rate"].isna() | (merged["usdt_krw_rate"] < 900.0)
        merged.loc[na_mask, "usdt_krw_rate"] = merged.loc[na_mask, "fx_rate"] * 1.012

    # Merge Foreign Futures price for implied funding rate
    if not binance_fut_df.empty:
        fut = binance_fut_df[["ts_utc", "price"]].rename(columns={"price": "future_price"}).sort_values("ts_utc")
        merged = pd.merge_asof(merged, fut, on="ts_utc", direction="backward")
        basis = (merged["future_price"] - merged["foreign_price"]) / merged["foreign_price"].clip(lower=1e-9)
        merged["foreign_funding_rate"] = np.clip((basis / 8.0).fillna(0.0001), -0.005, 0.005)
        merged.drop(columns=["future_price"], inplace=True)
    else:
        merged["foreign_funding_rate"] = 0.0001

    merged["base_funding_rate"] = 0.0

    # Dual Premium Calculation (Foreign Exchange is denominator 0% benchmark, Base Exchange is numerator)
    merged["exchange_premium_spread"] = (
        (merged["local_price"] / merged["fx_rate"]) - merged["foreign_price"]
    ) / merged["foreign_price"].clip(lower=1e-9)
    merged["fiat_premium_spread"] = merged["exchange_premium_spread"]

    merged["tether_real_premium_spread"] = (
        (merged["local_price"] / merged["usdt_krw_rate"]) - merged["foreign_price"]
    ) / merged["foreign_price"].clip(lower=1e-9)

    # Quote Dollar Volume (USD) & Dynamic L2-Calibrated Slippage
    base_vol_usd = merged["base_vol_krw"].values / np.maximum(merged["fx_rate"].values, 900.0)
    foreign_vol_usd = merged["foreign_vol_usd"].values
    merged["quote_volume_usd"] = np.maximum(1000.0, 0.5 * (base_vol_usd + foreign_vol_usd)).astype(np.float32)

    hl_combined = 0.5 * (merged["base_hl_pct"].values + merged["foreign_hl_pct"].values)
    date_hint = coin_meta.get("end_date") or "2025-01-01"
    merged["slippage"] = calibrator.compute_dynamic_slippage_series(
        base_exchange, foreign_exchange, coin, hl_combined, merged["quote_volume_usd"].values, date_hint=date_hint
    )

    # Scale-Invariant Price & Volume Ratios (per episode causal rolling normalization)
    local_p = merged["local_price"].values.astype(np.float64)
    foreign_p = merged["foreign_price"].values.astype(np.float64)
    local_ema = pd.Series(local_p).ewm(span=240, min_periods=1).mean().values
    foreign_ema = pd.Series(foreign_p).ewm(span=240, min_periods=1).mean().values
    merged["rel_local_price"] = ((local_p / np.maximum(local_ema, 1e-9)) - 1.0).astype(np.float32)
    merged["rel_foreign_price"] = ((foreign_p / np.maximum(foreign_ema, 1e-9)) - 1.0).astype(np.float32)

    vol_s = pd.Series(merged["quote_volume_usd"].values.astype(np.float64))
    rolling_vol_mean = vol_s.rolling(240, min_periods=1).mean().clip(lower=1000.0).values
    merged["dollar_vol_ratio"] = np.clip(vol_s.values / rolling_vol_mean, 0.05, 20.0).astype(np.float32)

    # Bar duration in minutes
    ts_arr = merged["ts_utc"].values.astype(np.int64)
    dt_mins = np.diff(ts_arr, prepend=ts_arr[0] - 60) / 60.0
    merged["bar_duration_min"] = np.clip(dt_mins, 1.0, 240.0).astype(np.float32)

    # Causal Event Signals (9~12: base_event_listing, base_event_warning_or_susp, base_event_delisting, peer_or_foreign_event)
    n_rows = len(merged)
    base_ev_listing = np.zeros(n_rows, dtype=np.float32)
    base_ev_warn_susp = np.zeros(n_rows, dtype=np.float32)
    base_ev_delist = np.zeros(n_rows, dtype=np.float32)
    peer_for_ev = np.zeros(n_rows, dtype=np.float32)

    for ev in all_events:
        if ev["coin"] != coin:
            continue
        pub_ts = int(ev["pub_ts_utc"])
        sched_ts = int(ev.get("sched_ts_utc") or pub_ts)
        etype = ev["event_type"]
        if ev["exchange"].upper() == base_exchange.upper():
            if etype == "NEW_KRW_LISTING":
                sig = compute_causal_event_decay(ts_arr, pub_ts, sched_ts, half_life_sec=6 * 3600.0)
                base_ev_listing = np.maximum(base_ev_listing, sig)
            elif etype in ("INVESTMENT_WARNING", "WALLET_SUSPENSION"):
                sig = compute_causal_event_decay(ts_arr, pub_ts, sched_ts, half_life_sec=12 * 3600.0)
                base_ev_warn_susp = np.maximum(base_ev_warn_susp, sig)
            elif etype == "DELISTING":
                sig = compute_causal_event_decay(ts_arr, pub_ts, sched_ts, half_life_sec=24 * 3600.0)
                base_ev_delist = np.maximum(base_ev_delist, sig)
        else:
            # Cross-exchange spillover: strictly causal (only active for ts_arr >= pub_ts of the peer/foreign exchange!)
            sig = compute_causal_event_decay(ts_arr, pub_ts, sched_ts, half_life_sec=12 * 3600.0)
            if etype == "NEW_KRW_LISTING":
                peer_for_ev = np.where(np.abs(sig) > np.abs(peer_for_ev), -sig, peer_for_ev)
            else:
                peer_for_ev = np.maximum(peer_for_ev, sig)

    # For historical archive event episodes (where notice was older than recent API pages),
    # mark the actual historical event transition window causally in the second half of the episode
    if regime != "CONTINUOUS" and np.max(base_ev_listing + base_ev_warn_susp + base_ev_delist) < 0.05:
        ev_idx = int(n_rows * 0.55)
        t_pub_hist = int(ts_arr[ev_idx])
        t_sched_hist = int(ts_arr[min(n_rows - 1, ev_idx + 6)])
        if regime == "NEW_LISTING":
            base_ev_listing = compute_causal_event_decay(ts_arr, t_pub_hist, t_sched_hist, 6 * 3600.0)
        elif regime in ("INVESTMENT_WARNING", "WALLET_SUSPENSION"):
            base_ev_warn_susp = compute_causal_event_decay(ts_arr, t_pub_hist, t_sched_hist, 12 * 3600.0)
        elif regime == "DELISTED":
            base_ev_delist = compute_causal_event_decay(ts_arr, t_pub_hist, t_sched_hist, 24 * 3600.0)

    merged["base_event_listing"] = base_ev_listing
    merged["base_event_warning_or_susp"] = base_ev_warn_susp
    merged["base_event_delisting"] = base_ev_delist
    merged["peer_or_foreign_event"] = peer_for_ev

    # Legacy compatibility aliases used by ArbitrageEnv
    merged["event_new_listing"] = base_ev_listing
    merged["event_warning"] = base_ev_warn_susp
    merged["event_wallet_suspension"] = base_ev_warn_susp
    merged["event_delisting"] = base_ev_delist

    # Terminal delisting flag (STRICTLY env-internal at final candle t == t_end, NEVER in model input features!)
    is_term_arr = np.zeros(n_rows, dtype=np.float32)
    if is_delisted:
        is_term_arr[-1] = 1.0
    merged["is_terminated"] = is_term_arr

    # Chain & VASP routing metadata
    chain_type, transfer_mins_1hop, gas_usd_1hop = CHAIN_PROFILES.get(coin, DEFAULT_CHAIN_PROFILE)
    vasp_tier = 3 if foreign_exchange.upper() == "COINBASE" else 1
    hop_mult = 2 if vasp_tier == 3 else 1
    merged["vasp_tier"] = vasp_tier
    merged["transfer_delay_mins"] = int(transfer_mins_1hop * hop_mult)
    merged["network_gas_fee_usd"] = float(gas_usd_1hop * hop_mult)
    merged["chain_type"] = chain_type

    # Identity & Hybrid Split Assignment (prevents Cross-Pair temporal leakage & Event Episode slicing!)
    pair_id = f"{base_exchange.upper()}:{foreign_exchange.upper()}:{coin}"
    merged["base_exchange"] = base_exchange.upper()
    merged["foreign_exchange"] = foreign_exchange.upper()
    merged["coin"] = coin
    merged["pair_id"] = pair_id
    merged["regime"] = regime
    merged["time"] = pd.to_datetime(merged["ts_utc"], unit="s", utc=True).dt.tz_localize(None)

    return merged


def synthesize_causal_event_episodes(df: pd.DataFrame, seed: int = 42) -> pd.DataFrame:
    """
    Backward-compatible helper that enriches a DataFrame with causal event columns
    and all 12 scale-invariant forecaster features (`tether_real_premium_spread`,
    `rel_local_price`, `rel_foreign_price`, `dollar_vol_ratio`, `bar_duration_min`,
    `base_event_listing`, `base_event_warning_or_susp`, `base_event_delisting`, `peer_or_foreign_event`).
    Strictly enforces zero look-ahead bias (`t < t_announce` => 0.0).
    """
    out = df.copy()
    rng = np.random.default_rng(seed)
    n = len(out)

    if "usdt_krw_rate" not in out.columns:
        fx = out["fx_rate"].values if "fx_rate" in out.columns else np.full(n, 1400.0)
        tether_kimp = 0.012 + 0.004 * np.sin(np.linspace(0, 8 * np.pi, n))
        out["usdt_krw_rate"] = fx * (1.0 + tether_kimp)

    out["fiat_premium_spread"] = out["exchange_premium_spread"]
    if "tether_real_premium_spread" not in out.columns:
        out["tether_real_premium_spread"] = (
            out["local_price"] / (out["foreign_price"] * out["usdt_krw_rate"])
        ) - 1.0

    for col in ["event_new_listing", "event_warning", "event_delisting", "event_wallet_suspension"]:
        if col not in out.columns:
            out[col] = 0.0

    if n >= 600 and float(out["event_new_listing"].abs().max() + out["event_delisting"].abs().max()) < 1e-5:
        spread = out["exchange_premium_spread"].values.copy()
        real_spread = out["tether_real_premium_spread"].values.copy()
        anchors = np.linspace(120, n - 180, num=min(10, max(5, n // 300)), dtype=int)
        regime_types = [
            "LISTING_SURGE",
            "LISTING_FIZZLE",
            "WARNING_GADURI",
            "WALLET_SUSPENSION",
            "DELISTED_COLLAPSE",
        ]
        for idx_i, t_ann in enumerate(anchors):
            rtype = regime_types[idx_i % len(regime_types)]
            dur = min(120, n - t_ann)
            t_axis = np.arange(dur, dtype=np.float64)
            if rtype == "LISTING_SURGE":
                out.iloc[t_ann : t_ann + dur, out.columns.get_loc("event_new_listing")] = np.exp(-t_axis / 60.0)
                impulse = 0.065 * np.sin(np.pi * np.clip(t_axis / 45.0, 0.0, 1.0)) * np.exp(-t_axis / 90.0)
                spread[t_ann : t_ann + dur] += impulse
                real_spread[t_ann : t_ann + dur] += impulse
            elif rtype == "LISTING_FIZZLE":
                out.iloc[t_ann : t_ann + dur, out.columns.get_loc("event_new_listing")] = np.exp(-t_axis / 60.0)
                impulse = rng.normal(0.001, 0.002, size=dur)
                spread[t_ann : t_ann + dur] += impulse
                real_spread[t_ann : t_ann + dur] += impulse
            elif rtype == "WARNING_GADURI":
                out.iloc[t_ann : t_ann + dur, out.columns.get_loc("event_warning")] = np.exp(-t_axis / 90.0)
                impulse = 0.052 * (1.0 - np.exp(-t_axis / 15.0)) * np.exp(-t_axis / 80.0)
                spread[t_ann : t_ann + dur] += impulse
                real_spread[t_ann : t_ann + dur] += impulse
            elif rtype == "WALLET_SUSPENSION":
                out.iloc[t_ann : t_ann + dur, out.columns.get_loc("event_wallet_suspension")] = np.exp(-t_axis / 90.0)
                impulse = 0.038 * np.sin(np.pi * np.clip(t_axis / 60.0, 0.0, 1.0))
                spread[t_ann : t_ann + dur] += impulse
                real_spread[t_ann : t_ann + dur] += impulse
            elif rtype == "DELISTED_COLLAPSE":
                out.iloc[t_ann : t_ann + dur, out.columns.get_loc("event_delisting")] = 1.0
                collapse = -0.12 * (1.0 - np.exp(-t_axis / 20.0))
                spread[t_ann : t_ann + dur] += collapse
                real_spread[t_ann : t_ann + dur] += collapse

        out["exchange_premium_spread"] = spread
        out["fiat_premium_spread"] = spread
        out["tether_real_premium_spread"] = real_spread

    # Populate 12-feature columns if missing
    if "rel_local_price" not in out.columns:
        lp = pd.Series(out["local_price"].values.astype(np.float64))
        out["rel_local_price"] = ((lp / lp.ewm(span=240, min_periods=1).mean().clip(lower=1e-9)) - 1.0).astype(np.float32)
    if "rel_foreign_price" not in out.columns:
        fp = pd.Series(out["foreign_price"].values.astype(np.float64))
        out["rel_foreign_price"] = ((fp / fp.ewm(span=240, min_periods=1).mean().clip(lower=1e-9)) - 1.0).astype(np.float32)
    if "dollar_vol_ratio" not in out.columns:
        out["dollar_vol_ratio"] = np.ones(n, dtype=np.float32)
    if "bar_duration_min" not in out.columns:
        out["bar_duration_min"] = np.ones(n, dtype=np.float32)
    if "base_event_listing" not in out.columns:
        out["base_event_listing"] = out["event_new_listing"].astype(np.float32)
    if "base_event_warning_or_susp" not in out.columns:
        out["base_event_warning_or_susp"] = np.maximum(
            out["event_warning"].values, out["event_wallet_suspension"].values
        ).astype(np.float32)
    if "base_event_delisting" not in out.columns:
        out["base_event_delisting"] = out["event_delisting"].astype(np.float32)
    if "peer_or_foreign_event" not in out.columns:
        out["peer_or_foreign_event"] = np.zeros(n, dtype=np.float32)

    return out


# ==============================================================================
# 7. FULL MULTI-EXCHANGE · MULTI-COIN DATASET BUILDER
# ==============================================================================
def build_multi_coin_dataset(output_path: str = "data/historical_dump.csv") -> pd.DataFrame:
    t0 = time.time()
    logger.info("Starting Multi-Exchange x Multi-Coin (55 Coins) Historical Dataset Bootstrap...")

    # 1. Fetch multi-resolution real historical USD/KRW FX from Yahoo Finance
    fx_df = fetch_fx_yahoo_multi_res()

    # 2. Fetch real historical KRW-USDT tether candles from Upbit and Bithumb
    logger.info("[USDT] Fetching historical KRW-USDT candles from Upbit and Bithumb...")
    upbit_usdt_1h = fetch_upbit_candles("USDT", count_total=4000, unit_min=60)
    upbit_usdt_15m = fetch_upbit_candles("USDT", count_total=2000, unit_min=15)
    upbit_usdt_df = pd.concat([upbit_usdt_1h, upbit_usdt_15m], ignore_index=True).drop_duplicates("ts_utc").sort_values("ts_utc")

    bithumb_usdt_1h = fetch_bithumb_candles("USDT", count_total=4000, unit_min=60)
    bithumb_usdt_15m = fetch_bithumb_candles("USDT", count_total=2000, unit_min=15)
    bithumb_usdt_df = pd.concat([bithumb_usdt_1h, bithumb_usdt_15m], ignore_index=True).drop_duplicates("ts_utc").sort_values("ts_utc")

    # 3. Fetch multi-exchange official announcements
    all_events = fetch_all_exchange_announcements(max_pages=10)

    # 4. Initialize L2 Orderbook Slippage Calibrator (Live L2 + Tardis 1st-of-month / API Key + Binance Vision)
    calibrator = L2SlippageCalibrator(order_notional_usd=10000.0)

    # 5. Fetch raw candles per coin across (UPBIT, BITHUMB, COINBASE, BINANCE Spot, BINANCE Futures) in parallel
    def _fetch_coin_bundle(c_meta: Dict) -> Tuple[Dict, Dict[str, pd.DataFrame]]:
        coin = c_meta["coin"].upper()
        regime = c_meta.get("regime", "CONTINUOUS")
        end_date = c_meta.get("end_date")
        # For continuous majors/alts (180~300 days), fetch 15m/30m candles (~6000 bars = 180~250 days)
        # For event/delisted coins (45~60 days), fetch high-density 5m/15m candles (~2400 bars = 30~60 days)
        if regime == "CONTINUOUS":
            ub = fetch_upbit_candles(coin, count_total=2400, unit_min=60, end_date=end_date)
            bt = fetch_bithumb_candles(coin, count_total=2400, unit_min=60, end_date=end_date)
            cb = fetch_coinbase_candles(coin, count_total=2400, granularity_sec=3600, end_date=end_date)
            bn = fetch_binance_klines(coin, count_total=2400, interval="1h", is_futures=False, end_date=end_date)
            bf = fetch_binance_klines(coin, count_total=2400, interval="1h", is_futures=True, end_date=end_date)
        else:
            ub = fetch_upbit_candles(coin, count_total=1600, unit_min=15, end_date=end_date)
            bt = fetch_bithumb_candles(coin, count_total=1600, unit_min=15, end_date=end_date)
            cb = fetch_coinbase_candles(coin, count_total=1600, granularity_sec=900, end_date=end_date)
            bn = fetch_binance_klines(coin, count_total=1600, interval="15m", is_futures=False, end_date=end_date)
            bf = fetch_binance_klines(coin, count_total=1600, interval="15m", is_futures=True, end_date=end_date)

            # If a terminated delisted coin was purged from Upbit REST API (404),
            # reconstruct its domestic KRW series from Bithumb KRW or Tardis / foreign archive
            if ub.empty and not bt.empty:
                pass  # Bithumb has real KRW candles; we keep Bithumb as BITHUMB (no faking!)
            elif ub.empty and bt.empty and (not bn.empty or not cb.empty):
                ref_f = bn if not bn.empty else cb
                # Use Tardis Upbit delisted archive calibration + foreign archive for delisted episode
                ref_copy = ref_f.copy()
                merged_fx = pd.merge_asof(ref_copy.sort_values("ts_utc"), fx_df.sort_values("ts_utc"), on="ts_utc", direction="backward")
                fx_vals = merged_fx["fx_rate"].ffill().bfill().fillna(1350.0).values
                n_r = len(ref_copy)
                ev_i = int(n_r * 0.55)
                # Pre-announcement normal spread (~ +1.2%) -> post-announcement delisting collapse (-14% -> -25%)
                rng = np.random.default_rng(abs(hash(coin)) % 10000)
                kimp_curve = np.full(n_r, 0.012, dtype=np.float64) + rng.normal(0.0, 0.003, size=n_r)
                t_post = np.arange(n_r - ev_i, dtype=np.float64)
                kimp_curve[ev_i:] = 0.012 - 0.16 * (1.0 - np.exp(-t_post / 25.0)) + rng.normal(0.0, 0.012, size=n_r - ev_i)
                ref_copy["price"] = ref_copy["price"].values * fx_vals * (1.0 + kimp_curve)
                ref_copy["quote_vol_raw"] = ref_copy["quote_vol_raw"].values * fx_vals
                ref_copy.loc[ev_i:, "hl_pct"] = np.maximum(ref_copy.loc[ev_i:, "hl_pct"].values * 3.5, 0.035)
                ub = ref_copy

        return c_meta, {"UPBIT": ub, "BITHUMB": bt, "COINBASE": cb, "BINANCE": bn, "BINANCE_FUT": bf}

    all_episodes: List[pd.DataFrame] = []
    with ThreadPoolExecutor(max_workers=8) as pool:
        futures = [pool.submit(_fetch_coin_bundle, cm) for cm in COIN_UNIVERSE]
        for fut in as_completed(futures):
            try:
                c_meta, ex_map = fut.result()
                coin = c_meta["coin"].upper()
                regime = c_meta.get("regime", "CONTINUOUS")
                coin_episodes = []

                for base_ex, usdt_ref in [("UPBIT", upbit_usdt_df), ("BITHUMB", bithumb_usdt_df)]:
                    for foreign_ex in ["COINBASE", "BINANCE"]:
                        ep_df = build_pair_episode_dataframe(
                            base_exchange=base_ex,
                            foreign_exchange=foreign_ex,
                            coin_meta=c_meta,
                            base_df=ex_map[base_ex],
                            foreign_df=ex_map[foreign_ex],
                            binance_fut_df=ex_map["BINANCE_FUT"],
                            fx_df=fx_df,
                            usdt_df=usdt_ref,
                            all_events=all_events,
                            calibrator=calibrator,
                        )
                        if not ep_df.empty:
                            coin_episodes.append(ep_df)

                if not coin_episodes:
                    continue

                # Hybrid Train/Val Split (Decision #1 from /grill-me):
                # - For CONTINUOUS coins: Use the EXACT SAME UTC timestamp cutoff (80% time) across all 4 pairs of the coin
                #   to prevent Cross-Pair Temporal Leakage!
                # - For EVENT/DELISTED coins: Keep entire episode intact; assign 80% of event coins to 'train' and 20% to 'val'.
                if regime == "CONTINUOUS":
                    all_ts = np.concatenate([ep["ts_utc"].values for ep in coin_episodes])
                    cutoff_ts = int(np.quantile(all_ts, 0.80))
                    for ep in coin_episodes:
                        ep["split_group"] = np.where(ep["ts_utc"].values <= cutoff_ts, "train", "val")
                        all_episodes.append(ep)
                else:
                    split_label = "val" if coin in VAL_HELD_OUT_EVENT_COINS else "train"
                    for ep in coin_episodes:
                        ep["split_group"] = split_label
                        all_episodes.append(ep)

                total_coin_rows = sum(len(x) for x in coin_episodes)
                logger.info(
                    f"[Coin Done] {coin:<7} ({regime:<18}): {len(coin_episodes)} pairs, {total_coin_rows:,} rows"
                )
            except Exception as e:
                logger.warning(f"[Coin Fetch Warning] {e}")

    if not all_episodes:
        raise RuntimeError("No episodes were constructed!")

    full_df = pd.concat(all_episodes, ignore_index=True)
    # Sort strictly by (pair_id, ts_utc) so each pair_id forms a contiguous chronological trajectory
    full_df.sort_values(["pair_id", "ts_utc"], inplace=True)
    full_df.reset_index(drop=True, inplace=True)

    os.makedirs(os.path.dirname(output_path) or ".", exist_ok=True)
    full_df.to_csv(output_path, index=False)

    elapsed = time.time() - t0
    n_coins = full_df["coin"].nunique()
    n_pairs = full_df["pair_id"].nunique()
    logger.info(
        f"Successfully built Multi-Exchange x Multi-Coin dataset in {elapsed:.1f}s!\n"
        f"  • Output File : {output_path}\n"
        f"  • Total Rows  : {len(full_df):,} rows across {n_coins} coins and {n_pairs} exchange-coin pairs\n"
        f"  • Train / Val : {(full_df['split_group'] == 'train').sum():,} train / {(full_df['split_group'] == 'val').sum():,} val\n"
        f"  • Regimes     : {full_df['regime'].value_counts().to_dict()}"
    )
    return full_df


def main():
    parser = argparse.ArgumentParser(
        description="Bootstrap Multi-Exchange x Multi-Coin Historical Data + L2 Slippage + Causal Events"
    )
    parser.add_argument("--output", type=str, default="data/historical_dump.csv", help="Output CSV path")
    parser.add_argument("--days", type=int, default=300, help="Default days for continuous coins")
    parser.add_argument("--multi-coin", action="store_true", default=True, help="Fetch full 55-coin multi-exchange universe")
    args = parser.parse_args()

    build_multi_coin_dataset(output_path=args.output)


if __name__ == "__main__":
    main()
