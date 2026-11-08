"""
Real-time Global Crypto News & Exchange Catalyst Ingestor for Magnolia AI Service.
Polls global crypto news feeds (Tree of Alpha News API & exchange announcement feeds)
and classifies headlines via `classify_announcement` to detect:
  - POSITIVE_NEWS_CATALYST (Part 02 [09강]: Investment rounds, token buybacks/burns, mainnet/stablecoin launches)
  - NEW_KRW_LISTING, INVESTMENT_WARNING, WALLET_SUSPENSION, DELISTING
"""

import json
import time
import logging
import threading
import urllib.request
from typing import Callable, Optional

from data.announcement_classifier import classify_announcement

logger = logging.getLogger("NewsIngestor")


class NewsIngestorDaemon:
    def __init__(
        self,
        redis_client=None,
        on_event_callback: Optional[Callable[[dict], None]] = None,
        poll_interval_sec: int = 30,
    ):
        self.r = redis_client
        self.on_event_callback = on_event_callback
        self.poll_interval_sec = max(10, int(poll_interval_sec))
        self._seen_ids = set()
        self._stop_event = threading.Event()

    def process_raw_headline(self, headline_id: str, title: str, source: str = "GLOBAL_NEWS", body: str = "") -> Optional[dict]:
        if not title or headline_id in self._seen_ids:
            return None
        self._seen_ids.add(headline_id)
        if len(self._seen_ids) > 1000:
            # Keep memory bounded
            self._seen_ids = set(list(self._seen_ids)[-500:])

        res = classify_announcement(title=title, body=body, exchange=source)
        if res.get("event_type", "NONE") == "NONE" or not res.get("symbols"):
            return None

        if self.on_event_callback is not None:
            for sym in res.get("symbols", []):
                ev_copy = dict(res)
                ev_copy["symbol"] = sym
                self.on_event_callback(ev_copy)

        if self.r is not None:
            try:
                payload = json.dumps(res, ensure_ascii=False)
                self.r.publish("event:announcement", payload)
            except Exception as e:
                logger.debug(f"Failed to publish news catalyst to Redis: {e}")

        return res

    def poll_tree_news_once(self) -> int:
        """
        Fetches recent crypto news items from Tree of Alpha public REST feed.
        """
        count = 0
        try:
            req = urllib.request.Request(
                "https://news.treeofalpha.com/api/news?limit=15",
                headers={"User-Agent": "MagnoliaNewsIngestor/1.0"},
            )
            with urllib.request.urlopen(req, timeout=6) as resp:
                if resp.status != 200:
                    return 0
                items = json.loads(resp.read().decode("utf-8"))
                if not isinstance(items, list):
                    return 0
                for item in items:
                    nid = str(item.get("_id") or item.get("id") or item.get("title", ""))[:64]
                    title = str(item.get("title") or item.get("en") or "")
                    source = str(item.get("source") or "GLOBAL_NEWS").upper()
                    if self.process_raw_headline(nid, title, source=source):
                        count += 1
        except Exception as e:
            logger.debug(f"Tree News poll skipped: {e}")
        return count

    def start(self):
        def _loop():
            while not self._stop_event.is_set():
                self.poll_tree_news_once()
                self._stop_event.wait(self.poll_interval_sec)

        t = threading.Thread(target=_loop, daemon=True)
        t.start()
        return t
