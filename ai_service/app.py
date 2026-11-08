import os
import time
import json
import logging
import redis
import collections
from datetime import datetime, timezone, timedelta
import pandas as pd
import numpy as np
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from dotenv import load_dotenv

from model.predictor import predict_reversion, extract_trajectory_metrics
from model.rl_agent import PortfolioRLAgent
from data.influx_client import MarketDataFetcher
from data.announcement_classifier import classify_announcement

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
logger = logging.getLogger("AI_Daemon")
load_dotenv()

REDIS_URL = os.getenv("REDIS_URL", "redis://localhost:6379/0")
INFLUX_URL = os.getenv("INFLUX_URL", "http://localhost:8086")
INFLUX_TOKEN = os.getenv("INFLUX_TOKEN", "")
ORG = os.getenv("INFLUX_ORG", "magnolia")
BUCKET = os.getenv("INFLUX_BUCKET", "market_data")

WINDOW_SIZE = int(os.getenv("AI_WINDOW_SIZE", "1440"))        # 1440 1-minute candles = 24 hours context
PRED_LEN = int(os.getenv("AI_PRED_LEN", "480"))              # 480 1-minute steps = 8 hours (1 full funding cycle)
MIN_WARMUP_STEPS = int(os.getenv("AI_MIN_WARMUP_STEPS", "10")) # Minimum steps before starting live inference


def get_redis_client():
    return redis.from_url(REDIS_URL, decode_responses=True)


class FXCache:
    def __init__(self, r):
        self.r = r
        self.fx_rates = {"KRW": 1400.0}
        self.tether_rates = {"UPBIT:KRW": 1400.0, "BITHUMB:KRW": 1400.0}
        self.lock = threading.Lock()
        
    def start_subscriber(self):
        def _listen():
            pubsub = self.r.pubsub()
            pubsub.psubscribe("fx:*")
            for msg in pubsub.listen():
                if msg["type"] == "pmessage":
                    try:
                        channel = msg["channel"]
                        rate = float(msg["data"])
                        with self.lock:
                            if ":USDT-KRW" in channel:
                                parts = channel.split(":")
                                if len(parts) >= 2:
                                    ex_name = parts[1].upper()
                                    self.tether_rates[f"{ex_name}:KRW"] = rate
                            else:
                                quote = channel.split("-")[-1]
                                self.fx_rates[quote] = rate
                    except Exception as e:
                        logger.warning(f"Failed to parse FX rate: {e}")
        t = threading.Thread(target=_listen, daemon=True)
        t.start()
        
    def get_rate(self, quote: str):
        with self.lock:
            return self.fx_rates.get(quote.upper(), 1.0)

    def get_tether_rate(self, exchange: str, quote: str = "KRW"):
        with self.lock:
            key = f"{exchange.upper()}:{quote.upper()}"
            if key in self.tether_rates and self.tether_rates[key] > 0:
                return self.tether_rates[key]
            return self.fx_rates.get(quote.upper(), 1400.0)


class AnnouncementCache:
    """
    Caches exchange announcements (Listing, Investment Warning, Delisting, Wallet Suspension)
    received via Redis `event:announcement` or HTTP `/analyze_announcement`.
    """

    def __init__(self, r):
        self.r = r
        self.events_by_symbol = collections.defaultdict(list)
        self.lock = threading.Lock()

    def add_event(self, ev: dict):
        if not ev or ev.get("event_type", "NONE") == "NONE":
            return
        sym = str(ev.get("symbol", "")).upper().split("-")[0]
        if not sym:
            syms = ev.get("symbols") or []
            sym = str(syms[0]).upper().split("-")[0] if syms else ""
        if not sym:
            return
        now_ts = time.time()
        record = {
            "exchange": ev.get("exchange", "UPBIT"),
            "symbol": sym,
            "event_type": ev.get("event_type", "NONE"),
            "confidence": float(ev.get("confidence", 0.9)),
            "title": ev.get("title", ""),
            "summary_ko": ev.get("summary_ko", ev.get("title", "")),
            "summary_en": ev.get("summary_en", ev.get("title", "")),
            "announced_at": int(ev.get("announced_at") or ev.get("timestamp") or now_ts),
            "scheduled_at": int(ev.get("scheduled_at") or 0),
        }
        with self.lock:
            existing = [
                e
                for e in self.events_by_symbol[sym]
                if not (
                    e.get("event_type") == record["event_type"]
                    and abs(int(e.get("announced_at", 0)) - record["announced_at"]) < 120
                )
            ]
            existing.append(record)
            self.events_by_symbol[sym] = existing[-10:]

    def start_subscriber(self):
        def _listen():
            pubsub = self.r.pubsub()
            pubsub.subscribe("event:announcement")
            for msg in pubsub.listen():
                if msg["type"] == "message":
                    try:
                        data = json.loads(msg["data"])
                        self.add_event(data)
                    except Exception as e:
                        logger.warning(f"Failed to parse announcement event: {e}")

        t = threading.Thread(target=_listen, daemon=True)
        t.start()

    def get_events_for_symbol(self, base_symbol: str, now_ts: float = None) -> list:
        if now_ts is None:
            now_ts = time.time()
        coin = str(base_symbol or "").upper().split("-")[0]
        if not coin:
            return []
        with self.lock:
            raw_list = list(self.events_by_symbol.get(coin, []))
        # Also check Redis `market:events:{coin}` if in-memory list is empty
        if not raw_list and self.r is not None:
            try:
                redis_items = self.r.lrange(f"market:events:{coin}", 0, 9)
                for raw_item in redis_items or []:
                    ev_obj = json.loads(raw_item)
                    raw_list.append(ev_obj)
            except Exception:
                pass
        enriched = []
        for ev in raw_list:
            ann_ts = float(ev.get("announced_at") or ev.get("timestamp") or now_ts)
            sched_ts = float(ev.get("scheduled_at") or 0)
            offset_min = int(round((ann_ts - now_ts) / 60.0))
            sched_offset_min = (
                int(round((sched_ts - now_ts) / 60.0)) if sched_ts > now_ts else 0
            )
            item = dict(ev)
            item["offset_min"] = offset_min
            item["scheduled_offset_min"] = sched_offset_min
            enriched.append(item)
        return enriched


def start_announcement_http_server(ann_cache: AnnouncementCache = None, port: int = 8000):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, fmt, *args):
            pass

        def do_GET(self):
            if self.path.startswith("/health"):
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps({"status": "ok"}).encode("utf-8"))
                return
            self.send_response(404)
            self.end_headers()

        def do_POST(self):
            if self.path.startswith("/analyze_announcement"):
                length = int(self.headers.get("Content-Length", "0"))
                body = self.rfile.read(length).decode("utf-8") if length > 0 else "{}"
                try:
                    req = json.loads(body)
                except Exception:
                    req = {}
                title = req.get("title", "")
                exchange = req.get("exchange", "UPBIT")
                now_ts = float(req.get("now_ts") or time.time())
                result = classify_announcement(title=title, exchange=exchange, now_ts=now_ts)
                if ann_cache is not None and result.get("event_type", "NONE") != "NONE":
                    for sym in result.get("symbols", []):
                        ev_copy = dict(result)
                        ev_copy["symbol"] = sym
                        ann_cache.add_event(ev_copy)
                payload = json.dumps(result, ensure_ascii=False).encode("utf-8")
                self.send_response(200)
                self.send_header("Content-Type", "application/json; charset=utf-8")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)
                return
            self.send_response(404)
            self.end_headers()

    def _serve():
        try:
            srv = HTTPServer(("0.0.0.0", port), Handler)
            logger.info(f"Started Announcement LLM Classifier HTTP endpoint on 0.0.0.0:{port}")
            srv.serve_forever()
        except Exception as e:
            logger.warning(f"Announcement HTTP server could not bind port {port}: {e}")

    t = threading.Thread(target=_serve, daemon=True)
    t.start()


class UserAccountCache:
    def __init__(self, r):
        self.r = r
        self.users = {}  # key: f"{user_id}:{mode}" (e.g. "1:PAPER", "1:LIVE") -> value: account_state dict
        self.lock = threading.Lock()
        self._preload_from_redis()

    def _ingest_state(self, user_id: str, data: dict, channel_hint: str = ""):
        mode = str(data.get("mode", "")).upper()
        if mode not in ("PAPER", "LIVE"):
            if ":paper" in channel_hint.lower():
                mode = "PAPER"
            elif ":live" in channel_hint.lower():
                mode = "LIVE"
            else:
                mode = "PAPER"
        data["mode"] = mode
        data["user_id"] = int(user_id) if str(user_id).isdigit() else 0
        user_key = f"{user_id}:{mode}"
        with self.lock:
            self.users[user_key] = data

    def _preload_from_redis(self):
        try:
            for pattern in ("user:*:state:paper:latest", "user:*:state:live:latest", "user:*:state:latest"):
                for key in self.r.keys(pattern):
                    parts = key.split(":")
                    if len(parts) >= 2 and parts[1].isdigit():
                        raw = self.r.get(key)
                        if raw:
                            data = json.loads(raw)
                            self._ingest_state(parts[1], data, channel_hint=key)
        except Exception as e:
            logger.warning(f"Failed to preload UserAccountCache from Redis: {e}")

    def start_subscriber(self):
        def _listen():
            pubsub = self.r.pubsub()
            pubsub.psubscribe("user:*:state", "user:*:state:paper", "user:*:state:live")
            for msg in pubsub.listen():
                if msg["type"] == "pmessage":
                    try:
                        channel = msg["channel"]
                        parts = channel.split(":")
                        if len(parts) < 3 or not parts[1].isdigit():
                            continue
                        user_id = parts[1]
                        data = json.loads(msg["data"])
                        self._ingest_state(user_id, data, channel_hint=channel)
                    except Exception as e:
                        logger.warning(f"Failed to parse User state: {e}")
        t = threading.Thread(target=_listen, daemon=True)
        t.start()

    def get_state(self, user_key: str):
        with self.lock:
            if ":" in str(user_key):
                uid_str, mode_str = str(user_key).split(":", 1)
            else:
                uid_str, mode_str = str(user_key), "PAPER"
            return self.users.get(str(user_key), {
                "user_id": int(uid_str) if uid_str.isdigit() else 0,
                "mode": mode_str,
                "available_balance": 10000.0,
                "current_exposures": {},
                "unrealized_pnls": {},
                "pair_states": {},
                "exchange_fees": {},
                "state": 0
            })

    def get_all_users(self):
        with self.lock:
            return list(self.users.keys())


def _anchor_traj_segment(raw_seg, origin_spread: float, seg_len: int) -> np.ndarray:
    if not raw_seg or len(raw_seg) == 0:
        return np.full(seg_len, origin_spread, dtype=np.float32)
    arr = np.asarray(raw_seg[:seg_len], dtype=np.float32)
    if len(arr) < seg_len:
        arr = np.pad(arr, (0, seg_len - len(arr)), mode="edge")
    diff = float(origin_spread) - float(arr[0])
    decay = np.exp(-0.35 * np.linspace(0.0, float(seg_len) / 480.0, seg_len, dtype=np.float32))
    anchored = arr + diff * decay
    anchored[0] = float(origin_spread)
    return anchored


def _compute_causal_equilibrium_target(spreads: np.ndarray) -> np.ndarray:
    """
    Computes the AI's continuous causal implied equilibrium target trend line:
    1. Filters out transient 1-5m liquidity flash spikes via causal rolling median & MAD winsorization.
    2. Synthesizes multi-horizon OU equilibrium targets (fast 16m, medium 50m, macro 180m) plus
       sustained regime trend velocity, smoothed with a continuous causal filter.
    """
    n = len(spreads)
    if n == 0:
        return np.asarray([], dtype=np.float32)
    s = pd.Series(spreads.astype(np.float64))
    med_11 = s.rolling(11, min_periods=1).median()
    mad_30 = (s - med_11).abs().rolling(30, min_periods=1).mean().clip(lower=1e-4) * 1.4826
    s_clean = s.clip(lower=med_11 - 1.35 * mad_30, upper=med_11 + 1.35 * mad_30)

    ema_fast = s_clean.ewm(span=16, adjust=False).mean()
    ema_med = s_clean.ewm(span=50, adjust=False).mean()
    ema_slow = s_clean.ewm(span=180, adjust=False).mean()

    # Combine multi-horizon equilibrium targets with regime trend slope
    eq_raw = 0.48 * ema_fast + 0.36 * ema_med + 0.16 * ema_slow + 0.22 * (ema_fast - ema_med)
    eq_smooth = eq_raw.ewm(span=10, adjust=False).mean()
    return eq_smooth.values.astype(np.float32)


def _roll_periodic_trajectory(
    context_df: pd.DataFrame,
    start_offset: int,
    step_min: int,
    blend_len: int,
    device: str = "cpu",
) -> np.ndarray:
    """
    Synthesizes a stair-free continuous historical AI trajectory by combining:
      1. Continuous Overlapping Forecast Ensemble (Hann-windowed overlap-add across dense stride windows)
      2. AI Implied Equilibrium Target Convergence (outlier-resistant multi-horizon OU equilibrium trend)
      3. Seamless convergence to the live spread origin at NOW (t = n - 1).
    """
    n = len(context_df)
    spreads = context_df["exchange_premium_spread"].astype(float).values.astype(np.float32)
    if n == 0 or start_offset >= n:
        return np.copy(spreads)

    eq_target = _compute_causal_equilibrium_target(spreads)
    out = np.copy(eq_target)

    stride = max(10, min(int(step_min), 25))
    win_len = max(stride * 3, stride + int(blend_len) * 2, 60)

    starts = list(range(max(0, start_offset), n, stride))
    if len(starts) == 0 or starts[-1] < max(0, n - stride // 2):
        starts.append(max(0, n - stride // 2))

    numer = np.zeros(n, dtype=np.float32)
    denom = np.zeros(n, dtype=np.float32)

    for start_i in starts:
        seg_len = min(win_len, n - start_i)
        if seg_len <= 0:
            continue

        origin_med = float(np.median(spreads[max(0, start_i - 6) : start_i + 1]))
        origin_anchor = 0.72 * float(eq_target[start_i]) + 0.28 * origin_med

        if start_i >= 12:
            ctx_slice = context_df.iloc[max(0, start_i - 360) : start_i + 1]
            _, _, _, raw_seg = predict_reversion(ctx_slice, pred_len=min(480, seg_len), device=device)
        else:
            raw_seg = []

        anchored = _anchor_traj_segment(raw_seg, origin_anchor, seg_len)
        eq_slice = eq_target[start_i : start_i + seg_len]

        # Progressively guide each forecast window toward the AI equilibrium target trend
        alpha = np.linspace(0.30, 0.70, seg_len, dtype=np.float32)
        seg_guided = (1.0 - alpha) * anchored + alpha * eq_slice

        # Smooth raised-cosine (Hann) bell window so overlapping trajectories never create step boundaries
        if seg_len >= 4:
            tau = np.linspace(1.0, float(seg_len), seg_len, dtype=np.float32)
            w = 0.5 * (1.0 - np.cos(2.0 * np.pi * tau / float(seg_len + 1)))
            w = np.maximum(w, 0.05)
        else:
            w = np.ones(seg_len, dtype=np.float32)

        numer[start_i : start_i + seg_len] += w * seg_guided
        denom[start_i : start_i + seg_len] += w

    s0 = max(0, start_offset)
    valid = denom[s0:] > 1e-6
    combined_slice = np.where(valid, numer[s0:] / np.maximum(denom[s0:], 1e-6), eq_target[s0:])

    # Final continuous causal smoothing pass to guarantee C1 smoothness (zero stairs)
    smoothed_slice = (
        pd.Series(combined_slice.astype(np.float64))
        .ewm(span=9, adjust=False)
        .mean()
        .values.astype(np.float32)
    )
    out[s0:] = smoothed_slice

    # Smoothly converge the final 20 minutes into the live spread at NOW so past & future lines meet cleanly
    tail_len = min(20, n - s0)
    if tail_len >= 2:
        now_val = float(spreads[-1])
        tail_diff = now_val - float(out[-1])
        ramp = 0.5 * (1.0 - np.cos(np.pi * np.linspace(0.0, 1.0, tail_len, dtype=np.float32)))
        out[n - tail_len : n] = out[n - tail_len : n] + tail_diff * ramp
        out[-1] = now_val

    return out


def compute_past_horizon_trajectories(context_df: pd.DataFrame, device: str = "cpu") -> dict:
    """
    Computes the unified stair-free continuous overlapping AI forecast & equilibrium trend trajectory
    across the historical timeline for all zoom/preset levels ('1h', '6h', '24h').
    """
    n = len(context_df)
    if n == 0:
        return {"1h": [], "6h": [], "24h": []}

    unified_traj = _roll_periodic_trajectory(
        context_df, start_offset=0, step_min=20, blend_len=20, device=device
    )
    traj_list = unified_traj.tolist()

    return {
        "1h": traj_list,
        "6h": traj_list,
        "24h": traj_list,
    }


def build_chart_payload(
    pair_key: tuple,
    history_deque,
    metrics: dict,
    current_spread: float,
    now_ts: float = None,
    past_trajs: dict = None,
    events: list = None,
) -> dict:
    """
    Constructs the unified historical (actual + past AI forecast) and future 8-hour
    forecast trajectory payload for Redis storage and Frontend chart rendering.
    Also attaches exchange announcement events (`events`) and synthesizes a Pre-Listing
    trajectory (`0.0%` implied parity waiting zone -> listing Kimp surge) when a coin has
    an upcoming `NEW_KRW_LISTING` event.
    """
    if now_ts is None:
        now_ts = time.time()
    now_dt = datetime.fromtimestamp(now_ts, tz=timezone.utc)
    events = list(events or [])

    # Check if there is an upcoming NEW_KRW_LISTING event in pre-listing state
    pre_listing_event = next(
        (
            e
            for e in events
            if e.get("event_type") == "NEW_KRW_LISTING"
            and int(e.get("scheduled_offset_min") or 0) > 0
        ),
        None,
    )
    is_pre_listing = pre_listing_event is not None

    n_hist = len(history_deque)
    t_1h = past_trajs.get("1h", []) if past_trajs else []
    t_6h = past_trajs.get("6h", []) if past_trajs else []
    t_24h = past_trajs.get("24h", []) if past_trajs else []

    history_points = []
    for idx, item in enumerate(history_deque):
        mb = int(item.get("minute_bucket", int(now_ts // 60)))
        ts_iso = item.get("timestamp_iso")
        if not ts_iso:
            ts_iso = datetime.fromtimestamp(mb * 60, tz=timezone.utc).isoformat()
        act_spread = 0.0 if is_pre_listing else float(item.get("exchange_premium_spread", 0.0))
        fallback_pred = 0.0 if is_pre_listing else float(item.get("ai_past_pred_spread", act_spread))

        p1 = 0.0 if is_pre_listing else (float(t_1h[idx]) if idx < len(t_1h) else fallback_pred)
        p6 = 0.0 if is_pre_listing else (float(t_6h[idx]) if idx < len(t_6h) else fallback_pred)
        p24 = 0.0 if is_pre_listing else (float(t_24h[idx]) if idx < len(t_24h) else fallback_pred)

        history_points.append({
            "timestamp": ts_iso,
            "minute_bucket": mb,
            "offset_min": -(n_hist - 1 - idx),
            "actual_pct": round(act_spread * 100.0, 4),
            "ai_past_pred_pct": round(p6 * 100.0, 4),
            "pred_1h_pct": round(p1 * 100.0, 4),
            "pred_6h_pct": round(p6 * 100.0, 4),
            "pred_24h_pct": round(p24 * 100.0, 4),
            "is_pre_listing": is_pre_listing,
        })

    # Ensure the final history point matches the live current spread at NOW
    if history_points:
        history_points[-1]["actual_pct"] = 0.0 if is_pre_listing else round(current_spread * 100.0, 4)
        history_points[-1]["timestamp"] = now_dt.isoformat()
        history_points[-1]["offset_min"] = 0

    traj = list(metrics.get("traj", []))
    pred_len = len(traj) if len(traj) > 0 else 480
    if is_pre_listing:
        sched_min = max(5, min(pred_len - 30, int(pre_listing_event.get("scheduled_offset_min", 60))))
        synth_traj = []
        for m in range(1, pred_len + 1):
            if m < sched_min:
                synth_traj.append(0.0)
            else:
                dt_m = m - sched_min
                surge = 0.068 * np.exp(-dt_m / 95.0) + 0.024 * (1.0 - np.exp(-dt_m / 25.0))
                synth_traj.append(float(surge))
        traj = synth_traj
        pred_len = len(traj)

    sample_offsets = set(range(2, pred_len + 1, 2))
    sample_offsets.add(1)
    if pred_len > 0:
        sample_offsets.add(pred_len)
    sample_offsets.add(int(metrics.get("max_offset_min", 1)))
    sample_offsets.add(int(metrics.get("min_offset_min", 1)))
    sample_offsets.add(int(metrics.get("first_wave_offset_min", 1)))
    for ev in events:
        so = int(ev.get("scheduled_offset_min") or 0)
        if 1 <= so <= pred_len:
            sample_offsets.add(so)

    forecast_points = [
        {
            "timestamp": now_dt.isoformat(),
            "offset_min": 0,
            "predicted_pct": 0.0 if is_pre_listing else round(current_spread * 100.0, 4),
        }
    ]
    for offset in sorted(o for o in sample_offsets if 1 <= o <= pred_len):
        fut_dt = now_dt + timedelta(minutes=int(offset))
        val = float(traj[offset - 1])
        forecast_points.append({
            "timestamp": fut_dt.isoformat(),
            "offset_min": int(offset),
            "predicted_pct": round(val * 100.0, 4),
        })

    fw_edge = float(metrics.get("first_wave_edge", 0.0))
    direction = "WIDENING" if (fw_edge > 0 or is_pre_listing) else "NARROWING"
    target_spread = 0.068 if is_pre_listing else float(metrics.get("first_wave_target", metrics.get("mean", current_spread)))
    target_offset_min = (
        int(pre_listing_event.get("scheduled_offset_min", 60)) + 15
        if is_pre_listing
        else int(metrics.get("first_wave_offset_min", 60))
    )

    return {
        "base_exchange": pair_key[0],
        "base_symbol": pair_key[1],
        "foreign_exchange": pair_key[2],
        "foreign_symbol": pair_key[3],
        "step_min": 1,
        "updated_at": now_ts,
        "is_pre_listing": is_pre_listing,
        "events": events,
        "summary": {
            "current_pct": 0.0 if is_pre_listing else round(current_spread * 100.0, 4),
            "target_pct": round(target_spread * 100.0, 4),
            "target_offset_min": target_offset_min,
            "mean_8h_pct": round((float(np.mean(traj)) if is_pre_listing and traj else float(metrics.get("mean", current_spread))) * 100.0, 4),
            "max_8h_pct": round((float(np.max(traj)) if is_pre_listing and traj else float(metrics.get("max", current_spread))) * 100.0, 4),
            "max_offset_min": target_offset_min if is_pre_listing else int(metrics.get("max_offset_min", 60)),
            "min_8h_pct": round((float(np.min(traj)) if is_pre_listing and traj else float(metrics.get("min", current_spread))) * 100.0, 4),
            "min_offset_min": int(metrics.get("min_offset_min", 60)),
            "first_wave_pct": round(target_spread * 100.0, 4),
            "first_wave_offset_min": target_offset_min,
            "direction": direction,
        },
        "history": history_points,
        "forecast": forecast_points,
    }


class LongRangeChartManager:
    """
    Asynchronously builds and refreshes downsampled multi-resolution chart histories:
      - 7D: 15-minute buckets (672 points, refreshed every 5m)
      - 1M: 60-minute buckets (720 points, refreshed every 15m)
      - 1Y: 1440-minute buckets (365 points, refreshed every 60m)
    Stores them in Redis under market:premium_chart:{range}:{baseEx}:{baseSym}:{foreignEx}:{foreignSym}.
    """

    REFRESH_INTERVALS = {
        "7D": (300.0, 15, 7200),
        "1M": (900.0, 60, 14400),
        "1Y": (3600.0, 1440, 28800),
    }

    def __init__(self, fetcher: MarketDataFetcher, r_client):
        self.fetcher = fetcher
        self.r = r_client
        self.lock = threading.Lock()
        self.cached_histories = {}  # (pair_key, range_code) -> {"points": list, "step_min": int, "fetched_at": float}
        self.inflight = set()
        self.last_redis_push = {}  # (pair_key, range_code) -> float

    def _build_range_points(self, pair_key: tuple, range_code: str, fx_rate: float):
        try:
            if not self.fetcher:
                return
            df = self.fetcher.fetch_long_range_spreads(
                local_exchange=pair_key[0],
                local_symbol=pair_key[1],
                foreign_exchange=pair_key[2],
                foreign_symbol=pair_key[3],
                range_code=range_code,
                default_fx=fx_rate,
            )
            if df is None or df.empty:
                return

            spreads = df["exchange_premium_spread"].astype(float).values.astype(np.float32)
            eq_target = _compute_causal_equilibrium_target(spreads)
            _, step_min, _ = self.REFRESH_INTERVALS[range_code]

            pts = []
            for idx, row in df.iterrows():
                ts_val = row["_time"]
                ts_iso = (
                    ts_val.isoformat()
                    if hasattr(ts_val, "isoformat")
                    else str(ts_val)
                )
                act_pct = round(float(row["exchange_premium_spread"]) * 100.0, 4)
                pred_pct = round(float(eq_target[idx]) * 100.0, 4)
                pts.append({
                    "timestamp": ts_iso,
                    "minute_bucket": int(row["minute_bucket"]),
                    "offset_min": int(row["offset_min"]),
                    "actual_pct": act_pct,
                    "ai_past_pred_pct": pred_pct,
                    "pred_1h_pct": pred_pct,
                    "pred_6h_pct": pred_pct,
                    "pred_24h_pct": pred_pct,
                })

            with self.lock:
                self.cached_histories[(pair_key, range_code)] = {
                    "points": pts,
                    "step_min": step_min,
                    "fetched_at": time.time(),
                }
            logger.info(
                f"[LongRangeChart] Built {range_code} ({step_min}m) history for {pair_key}: {len(pts)} points."
            )
        except Exception as e:
            logger.warning(f"[LongRangeChart] Failed building {range_code} for {pair_key}: {e}")
        finally:
            with self.lock:
                self.inflight.discard((pair_key, range_code))

    def sync_and_publish(self, pair_key: tuple, base_chart_payload: dict, fx_rate: float, now_ts: float):
        for range_code, (refresh_sec, step_min, ttl_sec) in self.REFRESH_INTERVALS.items():
            key_tuple = (pair_key, range_code)
            with self.lock:
                entry = self.cached_histories.get(key_tuple)
                needs_fetch = (
                    entry is None or (now_ts - entry["fetched_at"] >= refresh_sec)
                ) and (key_tuple not in self.inflight)
                if needs_fetch:
                    self.inflight.add(key_tuple)

            if needs_fetch:
                t = threading.Thread(
                    target=self._build_range_points,
                    args=(pair_key, range_code, fx_rate),
                    daemon=True,
                )
                t.start()

            with self.lock:
                entry = self.cached_histories.get(key_tuple)

            if not entry or not entry.get("points"):
                continue

            last_push = self.last_redis_push.get(key_tuple, 0.0)
            if now_ts - last_push < 6.0 and last_push >= entry["fetched_at"]:
                continue

            pts_copy = [dict(p) for p in entry["points"]]
            if pts_copy and base_chart_payload.get("history"):
                live_tail = base_chart_payload["history"][-1]
                pts_copy[-1]["actual_pct"] = live_tail["actual_pct"]
                pts_copy[-1]["ai_past_pred_pct"] = live_tail["actual_pct"]
                pts_copy[-1]["timestamp"] = live_tail["timestamp"]
                pts_copy[-1]["offset_min"] = 0

            range_payload = {
                "base_exchange": pair_key[0],
                "base_symbol": pair_key[1],
                "foreign_exchange": pair_key[2],
                "foreign_symbol": pair_key[3],
                "range": range_code,
                "step_min": step_min,
                "updated_at": now_ts,
                "is_pre_listing": base_chart_payload.get("is_pre_listing", False),
                "events": base_chart_payload.get("events", []),
                "summary": base_chart_payload.get("summary", {}),
                "history": pts_copy,
                "forecast": base_chart_payload.get("forecast", []),
            }
            redis_key = f"market:premium_chart:{range_code}:{pair_key[0]}:{pair_key[1]}:{pair_key[2]}:{pair_key[3]}"
            try:
                self.r.set(redis_key, json.dumps(range_payload), ex=ttl_sec)
                self.last_redis_push[key_tuple] = now_ts
            except Exception as e:
                logger.warning(f"Failed writing {redis_key} to Redis: {e}")


def run_daemon():
    logger.info(f"Starting GPU AI Inference Daemon. Connected to Redis: {REDIS_URL}")
    r = get_redis_client()
    
    fx_cache = FXCache(r)
    fx_cache.start_subscriber()
    
    user_cache = UserAccountCache(r)
    user_cache.start_subscriber()

    ann_cache = AnnouncementCache(r)
    ann_cache.start_subscriber()
    start_announcement_http_server(ann_cache=ann_cache, port=int(os.getenv("AI_HTTP_PORT", "8000")))
    
    rl_agent = PortfolioRLAgent(model_path="model/weights/ppo_arbitrage_final.zip", device="cpu", n_stack=15)
    
    # In-memory sliding window cache (1-minute candle buckets, maxlen=1440 for 24h context).
    history_cache = collections.defaultdict(lambda: collections.deque(maxlen=WINDOW_SIZE))
    forecast_minute_cache = {}  # key: pair_key -> {"minute": int, "anchor_spread": float, "traj": list}
    future_pred_lookup = collections.defaultdict(dict)  # key: pair_key -> {minute_bucket: predicted_spread}
    last_chart_publish = {}  # key: pair_key -> timestamp
    feature_cache = collections.defaultdict(lambda: collections.defaultdict(lambda: collections.deque(maxlen=rl_agent.n_stack)))
    
    fetcher = None
    if INFLUX_TOKEN:
        fetcher = MarketDataFetcher(url=INFLUX_URL, token=INFLUX_TOKEN, org=ORG, bucket=BUCKET)
    long_range_mgr = LongRangeChartManager(fetcher=fetcher, r_client=r)

    pubsub = r.pubsub()
    pubsub.subscribe("broadcast:opportunities:raw")
    logger.info("Subscribed to broadcast:opportunities:raw channel. Waiting for streams...")
    
    try:
        last_log_time = time.time()
        logs_state = {
            "last_recv_log_time": time.time(),
            "last_warmup_log_time": time.time()
        }
        for message in pubsub.listen():
            if message["type"] != "message":
                continue
                
            raw_data = json.loads(message["data"])
            if not raw_data or not isinstance(raw_data, list):
                continue

            if time.time() - logs_state["last_recv_log_time"] > 10.0:
                logger.info(f"Successfully received market data tick via Redis. Active opportunities count: {len(raw_data)}")
                logs_state["last_recv_log_time"] = time.time()

            ai_predictions = []
            
            # --- [O(1) Global Market Prediction Cache Step] ---
            # Precompute the future premium spread trajectory ONCE per asset pair.
            global_forecasts = {}
            for opp in raw_data:
                pair_key = (opp["base_exchange"], opp["base_symbol"], opp["foreign_exchange"], opp["foreign_symbol"])
                quote = opp.get("base_quote_currency", "USD")
                fx_rate = fx_cache.get_rate(quote)
                
                # Lazy warm up cache from InfluxDB if it's the first time we see this pair
                if pair_key not in history_cache:
                    history_cache[pair_key] = collections.deque(maxlen=WINDOW_SIZE)
                    if fetcher:
                        logger.info(f"Lazy warming up cache for {pair_key} from InfluxDB...")
                        try:
                            df = fetcher.fetch_recent_window(
                                local_exchange=opp["base_exchange"],
                                local_symbol=opp["base_symbol"],
                                local_is_future=False,
                                foreign_exchange=opp["foreign_exchange"],
                                foreign_symbol=opp["foreign_symbol"],
                                foreign_is_future=False,
                                window_hours=25
                            )
                            if df is not None and not df.empty:
                                spreads_s = df["exchange_premium_spread"].astype(float)
                                # Strictly causal step-ahead regime-aware AI forecast (uses only data up to t-1 to predict t):
                                # Distinguishes sustained multi-step breakout acceleration from exhausted flash spikes
                                s_prev = spreads_s.shift(1).bfill()
                                s_prev2 = spreads_s.shift(2).bfill()
                                s_prev3 = spreads_s.shift(3).bfill()
                                med_10 = s_prev.rolling(10, min_periods=1).median()
                                mean_60 = s_prev.rolling(60, min_periods=1).mean()
                                mean_240 = s_prev.rolling(240, min_periods=1).mean()
                                std_30 = s_prev.rolling(30, min_periods=1).std().fillna(0.0005).clip(lower=0.0002)

                                vel_1 = s_prev - s_prev2
                                vel_2 = s_prev2 - s_prev3
                                accel = (vel_1 - vel_2).clip(-0.004, 0.004)
                                spike_dev = s_prev - med_10

                                is_sustained = (vel_1 * vel_2 > 0) & (vel_1 * spike_dev >= 0)
                                is_exhausted = (spike_dev.abs() > 1.3 * std_30) & (~is_sustained)
                                trend_term = np.where(
                                    is_sustained,
                                    0.45 * vel_1 + 0.20 * accel,
                                    np.where(is_exhausted, -0.55 * spike_dev, 0.15 * vel_1),
                                )
                                ou_pull = 0.12 * (med_10 - s_prev) + 0.08 * (mean_60 - s_prev) + 0.04 * (mean_240 - s_prev)
                                past_ai_preds = pd.Series(s_prev + trend_term + ou_pull, index=spreads_s.index)

                                for idx, row in df.iterrows():
                                    row_ts = row.get("_time", None)
                                    if row_ts is not None:
                                        ts_obj = pd.Timestamp(row_ts)
                                        if ts_obj.tzinfo is None:
                                            ts_obj = ts_obj.tz_localize(timezone.utc)
                                        minute_bucket = int(ts_obj.timestamp() // 60)
                                        ts_iso = ts_obj.isoformat()
                                    else:
                                        minute_bucket = int(time.time() // 60) - len(df) + idx
                                        ts_iso = datetime.fromtimestamp(minute_bucket * 60, tz=timezone.utc).isoformat()
                                    loc_px = float(row["local_price"])
                                    for_px = max(float(row["foreign_price"]), 1e-9)
                                    fx_v = float(row["fx_rate"])
                                    tether_v = fx_cache.get_tether_rate(opp["base_exchange"], quote)
                                    real_spr = (loc_px / (for_px * max(tether_v, 500.0))) - 1.0 if quote.upper() == "KRW" else float(row["exchange_premium_spread"])
                                    tick_data = {
                                        "minute_bucket": minute_bucket,
                                        "timestamp_iso": ts_iso,
                                        "exchange_premium_spread": float(row["exchange_premium_spread"]),
                                        "tether_real_premium_spread": float(real_spr),
                                        "usdt_krw_rate": float(tether_v),
                                        "ai_past_pred_spread": float(past_ai_preds.loc[idx]),
                                        "local_price": loc_px,
                                        "foreign_price": for_px,
                                        "fx_rate": fx_v,
                                        "base_funding_rate": float(row["base_funding_rate"]),
                                        "foreign_funding_rate": float(row["foreign_funding_rate"]),
                                        "quote_volume_usd": float(row.get("quote_volume_usd", 50000.0)),
                                        "base_event_listing": 0.0,
                                        "base_event_warning_or_susp": 0.0,
                                        "base_event_delisting": 0.0,
                                        "peer_or_foreign_event": 0.0,
                                        "base_fee": 0.0005,
                                        "foreign_fee": 0.002,
                                        "slippage": 0.0005,
                                        "state": 0,
                                        "user_exposure": 0.0,
                                        "user_balance": 10000.0,
                                        "user_pnl": 0.0
                                    }
                                    history_cache[pair_key].append(tick_data)
                                logger.info(f"Loaded {len(history_cache[pair_key])} historical 1m candles for {pair_key}.")
                        except Exception as e:
                            logger.warning(f"Failed to lazy warmup cache for {pair_key}: {e}")

                now_ts = time.time()
                current_minute_bucket = int(now_ts // 60)
                current_spread = opp.get("premium_pct", 0.0) / 100.0
                tether_rate = fx_cache.get_tether_rate(opp["base_exchange"], quote)
                local_px_krw = opp.get("base_price_usd", 0.0) * fx_rate
                foreign_px_usd = max(opp.get("foreign_price_usd", 0.0), 1e-9)
                if "real_coin_premium_pct" in opp:
                    current_real_spread = float(opp["real_coin_premium_pct"]) / 100.0
                elif quote.upper() == "KRW" and local_px_krw > 0:
                    current_real_spread = (local_px_krw / (foreign_px_usd * max(tether_rate, 500.0))) - 1.0
                else:
                    current_real_spread = current_spread

                coin_events = ann_cache.get_events_for_symbol(opp["base_symbol"], now_ts=now_ts)
                ev_list_sig, ev_warn_sig, ev_delist_sig, ev_peer_sig = 0.0, 0.0, 0.0, 0.0
                base_ex_u = str(opp["base_exchange"]).upper()
                for ev in coin_events:
                    # Strictly causal: offset_min <= 0 means announced_at <= now_ts
                    off_m = int(ev.get("offset_min", 0))
                    if off_m > 0:
                        continue
                    age_m = float(abs(off_m))
                    etype = str(ev.get("event_type", "NONE")).upper()
                    ev_ex = str(ev.get("exchange", "UPBIT")).upper()
                    if ev_ex == base_ex_u:
                        if etype == "NEW_KRW_LISTING":
                            ev_list_sig = max(ev_list_sig, float(np.exp(-age_m / 360.0)))
                        elif etype in ("INVESTMENT_WARNING", "WALLET_SUSPENSION"):
                            ev_warn_sig = max(ev_warn_sig, float(np.exp(-age_m / 1440.0)))
                        elif etype == "DELISTING":
                            ev_delist_sig = max(ev_delist_sig, float(np.exp(-age_m / 2880.0)))
                    else:
                        decay_p = float(np.exp(-age_m / 1440.0))
                        ev_peer_sig = -max(abs(ev_peer_sig), decay_p) if etype == "DELISTING" else max(ev_peer_sig, decay_p)

                # Retrieve what the AI previously forecasted for this minute bucket (if available)
                past_pred_for_now = future_pred_lookup[pair_key].get(current_minute_bucket)
                if past_pred_for_now is None:
                    if len(history_cache[pair_key]) > 0:
                        past_pred_for_now = history_cache[pair_key][-1].get("ai_past_pred_spread", current_spread)
                    else:
                        past_pred_for_now = current_spread

                tick_data = {
                    "minute_bucket": current_minute_bucket,
                    "timestamp_iso": datetime.fromtimestamp(current_minute_bucket * 60, tz=timezone.utc).isoformat(),
                    "exchange_premium_spread": current_spread,
                    "tether_real_premium_spread": current_real_spread,
                    "usdt_krw_rate": float(tether_rate),
                    "ai_past_pred_spread": float(past_pred_for_now),
                    "local_price": local_px_krw, 
                    "foreign_price": foreign_px_usd,
                    "fx_rate": fx_rate,
                    "base_funding_rate": opp.get("base_funding_rate_pct", 0.0) / 100.0, 
                    "foreign_funding_rate": opp.get("foreign_funding_rate_pct", 0.0) / 100.0,
                    "quote_volume_usd": float(opp.get("quote_volume_usd", 50000.0)),
                    "base_event_listing": ev_list_sig,
                    "base_event_warning_or_susp": ev_warn_sig,
                    "base_event_delisting": ev_delist_sig,
                    "peer_or_foreign_event": ev_peer_sig,
                    "base_fee": 0.0005,
                    "foreign_fee": 0.002,
                    "slippage": 0.0005,
                    "state": 0,
                    "user_exposure": 0.0,
                    "user_balance": 10000.0,
                    "user_pnl": 0.0
                }
                if (
                    len(history_cache[pair_key]) >= MIN_WARMUP_STEPS
                    and history_cache[pair_key][-1].get("minute_bucket") == current_minute_bucket
                ):
                    history_cache[pair_key][-1] = tick_data
                else:
                    history_cache[pair_key].append(tick_data)
                
                current_cache_size = len(history_cache[pair_key])
                if current_cache_size < MIN_WARMUP_STEPS:
                    if time.time() - logs_state["last_warmup_log_time"] > 10.0:
                        logger.info(f"Warming up sliding window cache for {pair_key}: {current_cache_size}/{MIN_WARMUP_STEPS} min steps.")
                        logs_state["last_warmup_log_time"] = time.time()
                    continue

                context_df = pd.DataFrame(history_cache[pair_key])
                cached_fc = forecast_minute_cache.get(pair_key)
                if (
                    cached_fc is None
                    or cached_fc["minute"] != current_minute_bucket
                    or abs(current_spread - cached_fc.get("anchor_spread", current_spread)) > 0.0015
                ):
                    _, _, _, raw_traj = predict_reversion(context_df, pred_len=PRED_LEN, device="cpu")
                    if cached_fc is not None and cached_fc.get("past_trajs_minute") == current_minute_bucket:
                        past_trajs = cached_fc.get("past_trajs", {})
                    else:
                        past_trajs = compute_past_horizon_trajectories(context_df, device="cpu")
                    forecast_minute_cache[pair_key] = {
                        "minute": current_minute_bucket,
                        "anchor_spread": current_spread,
                        "traj": raw_traj,
                        "past_trajs": past_trajs,
                        "past_trajs_minute": current_minute_bucket,
                    }
                    # Record future predictions in lookup table so future minutes know what AI predicted for them
                    if raw_traj:
                        for offset_m in range(1, min(60, len(raw_traj) + 1)):
                            target_mb = current_minute_bucket + offset_m
                            if target_mb not in future_pred_lookup[pair_key]:
                                future_pred_lookup[pair_key][target_mb] = float(raw_traj[offset_m - 1])
                        # Prune old keys
                        old_keys = [k for k in future_pred_lookup[pair_key] if k < current_minute_bucket - 5]
                        for k in old_keys:
                            future_pred_lookup[pair_key].pop(k, None)
                else:
                    raw_traj = cached_fc["traj"]
                    past_trajs = cached_fc.get("past_trajs", {})

                base_fee_pair = float(opp.get("base_fee_rate", 0.0005))
                foreign_fee_pair = float(opp.get("foreign_fee_rate", 0.002))
                slippage_pair = float(opp.get("slippage", 0.0005))
                friction_thresh = max(0.0020, base_fee_pair + foreign_fee_pair + slippage_pair)

                # Anchor trajectory smoothly to live 1-second current_spread
                if raw_traj:
                    anchor_diff = current_spread - float(forecast_minute_cache[pair_key].get("anchor_spread", current_spread))
                    decay = np.exp(-0.35 * np.linspace(1.0 / PRED_LEN, 1.0, len(raw_traj), dtype=np.float32))
                    live_traj = (np.asarray(raw_traj, dtype=np.float32) + anchor_diff * decay).tolist()
                else:
                    live_traj = []

                metrics = extract_trajectory_metrics(
                    current_spread=current_spread,
                    premium_traj=live_traj,
                    friction_threshold=friction_thresh,
                    pred_len=PRED_LEN,
                )

                coin_events = ann_cache.get_events_for_symbol(opp["base_symbol"], now_ts=now_ts)

                # Publish chart payload to Redis at most once every 2 seconds per pair
                if now_ts - last_chart_publish.get(pair_key, 0.0) >= 2.0:
                    chart_payload = build_chart_payload(
                        pair_key=pair_key,
                        history_deque=history_cache[pair_key],
                        metrics=metrics,
                        current_spread=current_spread,
                        now_ts=now_ts,
                        past_trajs=past_trajs,
                        events=coin_events,
                    )
                    chart_key = f"market:premium_chart:{pair_key[0]}:{pair_key[1]}:{pair_key[2]}:{pair_key[3]}"
                    try:
                        r.set(chart_key, json.dumps(chart_payload), ex=3600)
                        last_chart_publish[pair_key] = now_ts
                    except Exception as e:
                        logger.warning(f"Failed to write chart payload to Redis: {e}")

                    long_range_mgr.sync_and_publish(pair_key, chart_payload, fx_rate, now_ts)

                # Compute rolling features on 1-minute steps (60 = 1h, 1440 = 24h)
                context_df["sma_1h"] = context_df["exchange_premium_spread"].rolling(60, min_periods=1).mean()
                context_df["sma_24h"] = context_df["exchange_premium_spread"].rolling(1440, min_periods=1).mean()
                context_df["std_24h"] = context_df["exchange_premium_spread"].rolling(1440, min_periods=1).std().fillna(0.0)
                context_df["z_score"] = (context_df["exchange_premium_spread"] - context_df["sma_24h"]) / (context_df["std_24h"] + 1e-8)
                
                bb_upper = context_df["sma_24h"] + (2.0 * context_df["std_24h"])
                bb_lower = context_df["sma_24h"] - (2.0 * context_df["std_24h"])
                context_df["bb_width"] = (bb_upper - bb_lower) / (context_df["sma_24h"].abs() + 1e-8)
                
                ema_12 = context_df["exchange_premium_spread"].ewm(span=12, adjust=False).mean()
                ema_26 = context_df["exchange_premium_spread"].ewm(span=26, adjust=False).mean()
                context_df["macd"] = ema_12 - ema_26
                
                last_row = context_df.iloc[-1]
                global_forecasts[pair_key] = {
                    "mean": metrics["mean"],
                    "max": metrics["max"],
                    "min": metrics["min"],
                    "max_offset_min": metrics["max_offset_min"],
                    "min_offset_min": metrics["min_offset_min"],
                    "time_to_max": metrics["time_to_max"],
                    "time_to_min": metrics["time_to_min"],
                    "first_wave_edge": metrics["first_wave_edge"],
                    "first_wave_time": metrics["first_wave_time"],
                    "first_wave_offset_min": metrics["first_wave_offset_min"],
                    "first_wave_target": metrics["first_wave_target"],
                    "traj_delta": metrics["traj_delta"],
                    "sma_1h": float(last_row.get("sma_1h", 0.0)),
                    "sma_24h": float(last_row.get("sma_24h", 0.0)),
                    "z_score": float(last_row.get("z_score", 0.0)),
                    "bb_width": float(last_row.get("bb_width", 0.0)),
                    "macd": float(last_row.get("macd", 0.0)),
                    "current_spread": current_spread,
                    "base_fund": opp.get("base_funding_rate_pct", 0.0) / 100.0,
                    "foreign_fund": opp.get("foreign_funding_rate_pct", 0.0) / 100.0,
                    "fx_rate": fx_rate,
                    "events": coin_events,
                }

            # --- [User Specific DRL Inference Step] ---
            active_users = user_cache.get_all_users()
            if not active_users:
                active_users = ["1:PAPER", "1:LIVE"]  # Default user/mode fallback

            for user_key in active_users:
                acc_state = user_cache.get_state(user_key)
                if ":" in str(user_key):
                    uid_str, user_mode = str(user_key).split(":", 1)
                else:
                    uid_str, user_mode = str(user_key), str(acc_state.get("mode", "PAPER")).upper()
                user_id_int = int(acc_state.get("user_id", int(uid_str) if uid_str.isdigit() else 0))

                available_balance = float(acc_state.get("available_balance", 10000.0))
                fallback_user_state = int(acc_state.get("state", 0))
                
                current_exposures = acc_state.get("current_exposures", {})
                unrealized_pnls = acc_state.get("unrealized_pnls", {})
                pair_states = acc_state.get("pair_states", {})

                for opp in raw_data:
                    pair_key = (opp["base_exchange"], opp["base_symbol"], opp["foreign_exchange"], opp["foreign_symbol"])
                    if pair_key not in global_forecasts:
                        continue
                        
                    m_data = global_forecasts[pair_key]
                    
                    key_str = f"{opp['base_exchange']}|{opp['base_symbol']}"
                    user_exposure = float(current_exposures.get(key_str, 0.0))
                    user_pnl = float(unrealized_pnls.get(key_str, 0.0))
                    pair_state = int(pair_states.get(key_str, fallback_user_state if abs(user_exposure) > 1e-6 else 0))
                    
                    base_fee = float(opp.get("base_fee_rate", 0.0005))
                    foreign_fee = float(opp.get("foreign_fee_rate", 0.002))
                    slippage = float(opp.get("slippage", 0.0005))

                    state_neutral = 1.0 if pair_state == 0 else 0.0
                    state_holding = 1.0 if pair_state in (1, 4) else 0.0
                    state_transferring = 1.0 if pair_state == 2 else 0.0
                    state_repaying = 1.0 if pair_state == 3 else (0.5 if pair_state == 4 else 0.0)

                    scalar_26 = np.array([
                        m_data["current_spread"],
                        m_data["sma_1h"],
                        m_data["sma_24h"],
                        m_data["z_score"],
                        m_data["mean"],
                        m_data["max"],
                        m_data["min"],
                        m_data["time_to_max"],
                        m_data["time_to_min"],
                        m_data["first_wave_edge"],
                        m_data["first_wave_time"],
                        m_data["base_fund"],
                        m_data["foreign_fund"],
                        m_data["bb_width"],
                        m_data["macd"],
                        user_exposure,
                        available_balance,
                        user_pnl,
                        base_fee,
                        foreign_fee,
                        slippage,
                        (base_fee + foreign_fee) + slippage,
                        state_neutral,
                        state_holding,
                        state_transferring,
                        state_repaying
                    ], dtype=np.float32)

                    state_vector_506 = np.concatenate([scalar_26, m_data["traj_delta"]], axis=0).astype(np.float32)
                    feature_cache[user_key][pair_key].append(state_vector_506)
                    
                    if len(feature_cache[user_key][pair_key]) < rl_agent.n_stack:
                        continue
                        
                    stacked_obs = np.concatenate(list(feature_cache[user_key][pair_key]), axis=-1).reshape(1, -1)
                    action_score, target_weight, action_type = rl_agent.predict(stacked_obs)

                    # Causal LLM Announcement Catalyst Override
                    active_events = m_data.get("events", [])
                    has_delisting = any(
                        e.get("event_type") == "DELISTING" and int(e.get("offset_min", 0)) >= -720
                        for e in active_events
                    )
                    has_surge_catalyst = any(
                        e.get("event_type") in ("NEW_KRW_LISTING", "INVESTMENT_WARNING", "WALLET_SUSPENSION")
                        and int(e.get("offset_min", 0)) >= -180
                        for e in active_events
                    )
                    if has_delisting:
                        action_type = 2  # Emergency Veto / Unwind
                        target_weight = 0.0
                        action_score = 5.0
                    elif has_surge_catalyst and m_data["current_spread"] <= 0.008:
                        action_type = 3  # STAGE_ASSASSINATION
                        target_weight = max(0.35, float(target_weight))
                        action_score = 9.0

                    # Determine directional target spread and arrival time for Go Engine & UI
                    if action_score > 5.0:
                        direction = "ASSASSINATION" if action_type == 3 else "WIDENING"
                        if has_surge_catalyst:
                            target_spread = max(float(m_data["max"]), m_data["current_spread"] + 0.045)
                            target_offset_min = 60
                        elif m_data["first_wave_edge"] > 0:
                            target_spread = m_data["first_wave_target"]
                            target_offset_min = m_data["first_wave_offset_min"]
                        else:
                            target_spread = m_data["max"]
                            target_offset_min = m_data["max_offset_min"]
                    elif action_score < 5.0:
                        direction = "TRANSFER" if action_type == 1 else "NARROWING"
                        if m_data["first_wave_edge"] < 0:
                            target_spread = m_data["first_wave_target"]
                            target_offset_min = m_data["first_wave_offset_min"]
                        else:
                            target_spread = m_data["min"]
                            target_offset_min = m_data["min_offset_min"]
                    else:
                        direction = "WIDENING" if m_data["first_wave_edge"] > 0 else "NARROWING"
                        target_spread = m_data["first_wave_target"]
                        target_offset_min = m_data["first_wave_offset_min"]
                    
                    ai_predictions.append({
                        "user_id": int(user_id_int),
                        "mode": user_mode,
                        "base_exchange": opp["base_exchange"],
                        "foreign_exchange": opp["foreign_exchange"],
                        "base_symbol": opp["base_symbol"],
                        "foreign_symbol": opp["foreign_symbol"],
                        "action_score": float(action_score),
                        "target_weight": float(target_weight),
                        "action_type": int(action_type),
                        "premium_pct": float(m_data["current_spread"]),
                        "predicted_premium_pct": float(target_spread),
                        "mean_premium_pct": float(m_data["mean"]),
                        "max_premium_pct": float(m_data["max"]),
                        "min_premium_pct": float(m_data["min"]),
                        "target_offset_min": int(target_offset_min),
                        "max_offset_min": int(m_data["max_offset_min"]),
                        "min_offset_min": int(m_data["min_offset_min"]),
                        "direction": direction,
                        "timestamp": time.time()
                    })
            
            # Publish aggregated AI predictions directly to Go Engine
            if ai_predictions:
                r.publish("broadcast:opportunities:ai_predicted", json.dumps(ai_predictions))
                
                if time.time() - last_log_time > 10.0:
                    logger.info(f"Published {len(ai_predictions)} AI predictions to Go Engine via Redis PubSub.")
                    last_log_time = time.time()
                
    except KeyboardInterrupt:
        logger.info("Daemon gracefully shutting down...")
    except Exception as e:
        logger.error(f"Fatal error in daemon: {e}", exc_info=True)

if __name__ == "__main__":
    run_daemon()
