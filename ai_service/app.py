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
from dotenv import load_dotenv

from model.predictor import predict_reversion, extract_trajectory_metrics
from model.rl_agent import PortfolioRLAgent
from data.influx_client import MarketDataFetcher

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
                        quote = channel.split("-")[-1]
                        with self.lock:
                            self.fx_rates[quote] = rate
                    except Exception as e:
                        logger.warning(f"Failed to parse FX rate: {e}")
        t = threading.Thread(target=_listen, daemon=True)
        t.start()
        
    def get_rate(self, quote: str):
        with self.lock:
            return self.fx_rates.get(quote.upper(), 1.0)


class UserAccountCache:
    def __init__(self, r):
        self.r = r
        self.users = {}  # key: user_id (str) -> value: account_state dict
        self.lock = threading.Lock()
        
    def start_subscriber(self):
        def _listen():
            pubsub = self.r.pubsub()
            pubsub.psubscribe("user:*:state")
            for msg in pubsub.listen():
                if msg["type"] == "pmessage":
                    try:
                        channel = msg["channel"]
                        user_id = channel.split(":")[1]
                        data = json.loads(msg["data"])
                        with self.lock:
                            self.users[user_id] = data
                    except Exception as e:
                        logger.warning(f"Failed to parse User state: {e}")
        t = threading.Thread(target=_listen, daemon=True)
        t.start()
        
    def get_state(self, user_id: str):
        with self.lock:
            return self.users.get(str(user_id), {
                "available_balance": 10000.0,
                "current_exposures": {},
                "unrealized_pnls": {},
                "pair_states": {},
                "state": 0
            })
        
    def get_all_users(self):
        with self.lock:
            return list(self.users.keys())


def build_chart_payload(
    pair_key: tuple,
    history_deque,
    metrics: dict,
    current_spread: float,
    now_ts: float = None,
) -> dict:
    """
    Constructs the unified historical (actual + past AI forecast) and future 8-hour
    forecast trajectory payload for Redis storage and Frontend chart rendering.
    """
    if now_ts is None:
        now_ts = time.time()
    now_dt = datetime.fromtimestamp(now_ts, tz=timezone.utc)

    history_points = []
    for item in history_deque:
        mb = int(item.get("minute_bucket", int(now_ts // 60)))
        ts_iso = item.get("timestamp_iso")
        if not ts_iso:
            ts_iso = datetime.fromtimestamp(mb * 60, tz=timezone.utc).isoformat()
        act_spread = float(item.get("exchange_premium_spread", 0.0))
        past_pred_spread = float(item.get("ai_past_pred_spread", act_spread))
        history_points.append({
            "timestamp": ts_iso,
            "minute_bucket": mb,
            "actual_pct": round(act_spread * 100.0, 4),
            "ai_past_pred_pct": round(past_pred_spread * 100.0, 4),
        })

    # Ensure the final history point matches the live current spread at NOW
    if history_points:
        history_points[-1]["actual_pct"] = round(current_spread * 100.0, 4)
        history_points[-1]["timestamp"] = now_dt.isoformat()

    traj = metrics.get("traj", [])
    pred_len = len(traj)
    sample_offsets = set(range(2, pred_len + 1, 2))
    sample_offsets.add(1)
    if pred_len > 0:
        sample_offsets.add(pred_len)
    sample_offsets.add(int(metrics.get("max_offset_min", 1)))
    sample_offsets.add(int(metrics.get("min_offset_min", 1)))
    sample_offsets.add(int(metrics.get("first_wave_offset_min", 1)))

    forecast_points = [
        {
            "timestamp": now_dt.isoformat(),
            "offset_min": 0,
            "predicted_pct": round(current_spread * 100.0, 4),
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
    direction = "WIDENING" if fw_edge > 0 else "NARROWING"
    target_spread = float(metrics.get("first_wave_target", metrics.get("mean", current_spread)))
    target_offset_min = int(metrics.get("first_wave_offset_min", 60))

    return {
        "base_exchange": pair_key[0],
        "base_symbol": pair_key[1],
        "foreign_exchange": pair_key[2],
        "foreign_symbol": pair_key[3],
        "updated_at": now_ts,
        "summary": {
            "current_pct": round(current_spread * 100.0, 4),
            "target_pct": round(target_spread * 100.0, 4),
            "target_offset_min": target_offset_min,
            "mean_8h_pct": round(float(metrics.get("mean", current_spread)) * 100.0, 4),
            "max_8h_pct": round(float(metrics.get("max", current_spread)) * 100.0, 4),
            "max_offset_min": int(metrics.get("max_offset_min", 60)),
            "min_8h_pct": round(float(metrics.get("min", current_spread)) * 100.0, 4),
            "min_offset_min": int(metrics.get("min_offset_min", 60)),
            "first_wave_pct": round(float(metrics.get("first_wave_target", current_spread)) * 100.0, 4),
            "first_wave_offset_min": int(metrics.get("first_wave_offset_min", 60)),
            "direction": direction,
        },
        "history": history_points,
        "forecast": forecast_points,
    }


def run_daemon():
    logger.info(f"Starting GPU AI Inference Daemon. Connected to Redis: {REDIS_URL}")
    r = get_redis_client()
    
    fx_cache = FXCache(r)
    fx_cache.start_subscriber()
    
    user_cache = UserAccountCache(r)
    user_cache.start_subscriber()
    
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
                                # Compute historical 15-minute ahead OU + momentum forecast for past AI overlay line
                                past_origin = spreads_s.shift(15).bfill()
                                past_mean = spreads_s.rolling(240, min_periods=1).mean().shift(15).bfill()
                                past_mom = (spreads_s.shift(15) - spreads_s.shift(30)).fillna(0.0) * 0.25
                                past_ai_preds = past_origin + (1.0 - np.exp(-0.35)) * (past_mean - past_origin) + past_mom

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
                                    tick_data = {
                                        "minute_bucket": minute_bucket,
                                        "timestamp_iso": ts_iso,
                                        "exchange_premium_spread": float(row["exchange_premium_spread"]),
                                        "ai_past_pred_spread": float(past_ai_preds.loc[idx]),
                                        "local_price": float(row["local_price"]),
                                        "foreign_price": float(row["foreign_price"]),
                                        "fx_rate": float(row["fx_rate"]),
                                        "base_funding_rate": float(row["base_funding_rate"]),
                                        "foreign_funding_rate": float(row["foreign_funding_rate"]),
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
                    "ai_past_pred_spread": float(past_pred_for_now),
                    "local_price": opp.get("base_price_usd", 0.0) * fx_rate, 
                    "foreign_price": opp.get("foreign_price_usd", 0.0),
                    "fx_rate": fx_rate,
                    "base_funding_rate": opp.get("base_funding_rate_pct", 0.0) / 100.0, 
                    "foreign_funding_rate": opp.get("foreign_funding_rate_pct", 0.0) / 100.0,
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
                    forecast_minute_cache[pair_key] = {
                        "minute": current_minute_bucket,
                        "anchor_spread": current_spread,
                        "traj": raw_traj,
                    }
                    # Record future predictions in lookup table so future minutes know what AI predicted for them
                    if raw_traj:
                        for offset_m in range(1, min(60, len(raw_traj) + 1)):
                            target_mb = current_minute_bucket + offset_m
                            # Keep 15-minute ahead prediction as primary reference or set if not yet populated
                            if offset_m == 15 or target_mb not in future_pred_lookup[pair_key]:
                                future_pred_lookup[pair_key][target_mb] = float(raw_traj[offset_m - 1])
                        # Prune old keys
                        old_keys = [k for k in future_pred_lookup[pair_key] if k < current_minute_bucket - 5]
                        for k in old_keys:
                            future_pred_lookup[pair_key].pop(k, None)
                else:
                    raw_traj = cached_fc["traj"]

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

                # Publish chart payload to Redis at most once every 2 seconds per pair
                if now_ts - last_chart_publish.get(pair_key, 0.0) >= 2.0:
                    chart_payload = build_chart_payload(
                        pair_key=pair_key,
                        history_deque=history_cache[pair_key],
                        metrics=metrics,
                        current_spread=current_spread,
                        now_ts=now_ts,
                    )
                    chart_key = f"market:premium_chart:{pair_key[0]}:{pair_key[1]}:{pair_key[2]}:{pair_key[3]}"
                    try:
                        r.set(chart_key, json.dumps(chart_payload), ex=3600)
                        last_chart_publish[pair_key] = now_ts
                    except Exception as e:
                        logger.warning(f"Failed to write chart payload to Redis: {e}")

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
                    "fx_rate": fx_rate
                }

            # --- [User Specific DRL Inference Step] ---
            active_users = user_cache.get_all_users()
            if not active_users:
                active_users = ["0"]  # Default system user fallback

            for user_id in active_users:
                acc_state = user_cache.get_state(user_id)
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
                    state_holding = 1.0 if pair_state == 1 else 0.0
                    state_transferring = 1.0 if pair_state == 2 else 0.0
                    state_repaying = 1.0 if pair_state == 3 else 0.0

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
                    feature_cache[user_id][pair_key].append(state_vector_506)
                    
                    if len(feature_cache[user_id][pair_key]) < rl_agent.n_stack:
                        continue
                        
                    stacked_obs = np.concatenate(list(feature_cache[user_id][pair_key]), axis=-1).reshape(1, -1)
                    action_score, target_weight, action_type = rl_agent.predict(stacked_obs)

                    # Determine directional target spread and arrival time for Go Engine & UI
                    if action_score > 5.0:
                        direction = "WIDENING"
                        if m_data["first_wave_edge"] > 0:
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
                        "user_id": int(user_id),
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
