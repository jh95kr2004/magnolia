import pytest
from datetime import datetime, timezone
from data.announcement_classifier import AnnouncementClassifier, classify_announcement


GOLDEN_ANNOUNCEMENTS = [
    # 1. NEW_KRW_LISTING (Single & Multi-ticker KRW Market Listings)
    (
        "[거래] 원화(KRW) 마켓 디지털 자산 추가 (AXL) (18시 거래지원 개시 예정)",
        "NEW_KRW_LISTING",
        ["AXL"],
        True,
    ),
    (
        "[거래] 신규 거래지원 안내 (ZRO, ONDO) (KRW, BTC, USDT 마켓)",
        "NEW_KRW_LISTING",
        ["ZRO", "ONDO"],
        True,
    ),
    (
        "레이어제로(ZRO) 원화 마켓 신규 상장 안내 (2026-11-03 16:00 예정)",
        "NEW_KRW_LISTING",
        ["ZRO"],
        True,
    ),
    (
        "[거래] KRW 마켓 디지털 자산 추가 (PEPE)",
        "NEW_KRW_LISTING",
        ["PEPE"],
        True,
    ),
    # 2. Non-KRW market listing (BTC/USDT only -> should NOT trigger NEW_KRW_LISTING)
    (
        "[거래] BTC, USDT 마켓 디지털 자산 추가 (TURBO) (BTC, USDT 마켓)",
        "NONE",
        ["TURBO"],
        False,
    ),
    # 3. INVESTMENT_WARNING (거래유의 종목 지정)
    (
        "[유의] 거래유의 종목 지정 안내 (WAVES)",
        "INVESTMENT_WARNING",
        ["WAVES"],
        True,
    ),
    (
        "[투자유의] 디지털 자산 투자유의 종목 지정 안내 (HIFI, LOOM)",
        "INVESTMENT_WARNING",
        ["HIFI", "LOOM"],
        True,
    ),
    (
        "[유의] 거래유의 종목 지정 연장 안내 (XEM)",
        "INVESTMENT_WARNING",
        ["XEM"],
        True,
    ),
    # 4. INVESTMENT_WARNING_RELEASE (유의 종목 해제 -> must be NONE, never INVESTMENT_WARNING!)
    (
        "[유의] 거래유의 종목 지정 해제 안내 (WAVES)",
        "NONE",
        ["WAVES"],
        False,
    ),
    (
        "[투자유의] 투자유의 종목 지정 해제 안내 (SEI)",
        "NONE",
        ["SEI"],
        False,
    ),
    # 5. DELISTING (거래지원 종료 / 상장폐지 -> 100% Recall & is_catalyst=False)
    (
        "[거래] 디지털 자산 거래지원 종료 안내 (WAVES)",
        "DELISTING",
        ["WAVES"],
        False,
    ),
    (
        "[상장폐지] 플레이댑(PLA) 거래지원 종료 안내",
        "DELISTING",
        ["PLA"],
        False,
    ),
    (
        "[거래] 거래지원 종료 안내 (BTG, OMG)",
        "DELISTING",
        ["BTG", "OMG"],
        False,
    ),
    (
        "Notice of Termination of Trading Support (WEMIX)",
        "DELISTING",
        ["WEMIX"],
        False,
    ),
    # 6. WALLET_SUSPENSION (입출금 일시 중단 / 하드포크 가두리 예고)
    (
        "[입출금] 네트워크 업그레이드에 따른 입출금 일시 중단 안내 (ETH, ARB) (18:00 중단 예정)",
        "WALLET_SUSPENSION",
        ["ETH", "ARB"],
        True,
    ),
    (
        "[입출금] 메인넷 하드포크에 따른 입출금 일시 중단 안내 (SOL)",
        "WALLET_SUSPENSION",
        ["SOL"],
        True,
    ),
    (
        "[입출금] 리브랜딩 및 토큰 스왑에 따른 입출금 중단 안내 (MATIC)",
        "WALLET_SUSPENSION",
        ["MATIC"],
        True,
    ),
    # 7. WALLET_RESUMPTION (입출금 재개 -> must be NONE!)
    (
        "[입출금] 네트워크 업그레이드 완료에 따른 입출금 재개 안내 (ETH)",
        "NONE",
        ["ETH"],
        False,
    ),
    (
        "[입출금] 솔라나(SOL) 입출금 정상화 및 재개 안내",
        "NONE",
        ["SOL"],
        False,
    ),
    # 8. General notices (이벤트 / 서버 점검 -> NONE)
    (
        "[점검] 업비트 시스템 정기 점검 안내",
        "NONE",
        [],
        False,
    ),
    (
        "[이벤트] 비트코인(BTC) 에어드랍 이벤트 당첨자 안내",
        "NONE",
        ["BTC"],
        False,
    ),
]


def test_golden_announcement_classification_precision_and_recall():
    clf = AnnouncementClassifier(use_local_llm=False)
    base_dt = datetime(2026, 11, 3, 6, 0, 0, tzinfo=timezone.utc)  # 15:00 KST

    for title, expected_type, expected_symbols, expected_catalyst in GOLDEN_ANNOUNCEMENTS:
        res = clf.classify(title=title, exchange="UPBIT", announced_at=base_dt)
        assert res["event_type"] == expected_type, f"Failed event_type for '{title}': got {res['event_type']}, expected {expected_type}"
        assert res["symbols"] == expected_symbols, f"Failed symbols for '{title}': got {res['symbols']}, expected {expected_symbols}"
        assert res["is_catalyst"] == expected_catalyst, f"Failed is_catalyst for '{title}'"
        assert res["latency_ms"] < 50.0, f"Classifier latency too high: {res['latency_ms']}ms"
        assert res["summary_ko"] and res["summary_en"]


def test_scheduled_timestamp_extraction_for_future_chart_markers():
    base_dt = datetime(2026, 11, 3, 6, 0, 0, tzinfo=timezone.utc)  # 15:00 KST
    # "18시 거래지원 개시 예정" -> 18:00 KST = 09:00 UTC (+180 minutes from 15:00 KST)
    res = classify_announcement(
        title="[거래] 원화(KRW) 마켓 디지털 자산 추가 (AXL) (18시 거래지원 개시 예정)",
        exchange="UPBIT",
        announced_at=base_dt,
    )
    assert res["event_type"] == "NEW_KRW_LISTING"
    assert res["scheduled_offset_min"] == 180
    assert res["scheduled_at"].startswith("2026-11-03T09:00:00")

    # Default lead time when no clock is in title: +95m for NEW_KRW_LISTING, +120m for WALLET_SUSPENSION
    res_default = classify_announcement(
        title="[거래] KRW 마켓 디지털 자산 추가 (PEPE)",
        exchange="UPBIT",
        announced_at=base_dt,
    )
    assert res_default["scheduled_offset_min"] == 95


def test_four_scenario_assassination_and_event_stress_suite():
    from evaluate_agent import evaluate_assassination_scenarios

    suite = evaluate_assassination_scenarios()
    # Scenario A: Successful Listing / Gaduri Surge
    assert suite["scenario_a_listing_surge"]["sniped"] is True
    assert suite["scenario_a_listing_surge"]["net_roi_pct"] >= 1.5
    # Scenario B: False Signal / Fizzle Timeout Unwind with zero reverse gas fee
    assert suite["scenario_b_fizzle_timeout"]["timeout_unwound_in_place"] is True
    assert suite["scenario_b_fizzle_timeout"]["reverse_gas_fee_usd"] == 0.0
    # Scenario C: Delisting Trap Defense -> 100% entry veto
    assert suite["scenario_c_delisting_defense"]["delisting_entry_block_rate_pct"] == 100.0
    # Scenario D: In-Transit Kimp Drop Defense -> auto-switched to STATE_STAGED and sniped on rebound
    assert suite["scenario_d_in_transit_kimp_drop_defense"]["auto_switched_to_staged"] is True
    assert suite["scenario_d_in_transit_kimp_drop_defense"]["sniped_on_rebound"] is True
    assert suite["scenario_d_in_transit_kimp_drop_defense"]["net_roi_pct"] > 0.5


def test_pre_listing_chart_payload_and_causal_dataset_episodes():
    import numpy as np
    import pandas as pd
    from app import build_chart_payload
    from bootstrap_dataset import synthesize_causal_event_episodes

    # 1. Verify Pre-Listing chart payload synthesis (0.0% parity waiting zone -> surge curve after scheduled_offset_min)
    now_ts = 1762149600.0
    history_deque = [
        {"minute_bucket": int(now_ts // 60) - 2, "exchange_premium_spread": 0.01},
        {"minute_bucket": int(now_ts // 60) - 1, "exchange_premium_spread": 0.01},
        {"minute_bucket": int(now_ts // 60), "exchange_premium_spread": 0.01},
    ]
    metrics = {"traj": [0.01] * 480, "mean": 0.01, "max": 0.015, "min": 0.005, "first_wave_edge": 0.005}
    events = [
        {
            "exchange": "UPBIT",
            "symbol": "ZRO",
            "event_type": "NEW_KRW_LISTING",
            "offset_min": -5,
            "scheduled_offset_min": 60,
            "summary_ko": "원화 마켓 신규 상장 예고: ZRO",
        }
    ]
    payload = build_chart_payload(
        pair_key=("UPBIT", "ZRO-KRW", "COINBASE", "ZRO-USD"),
        history_deque=history_deque,
        metrics=metrics,
        current_spread=0.0,
        now_ts=now_ts,
        events=events,
    )
    assert payload["is_pre_listing"] is True
    assert len(payload["events"]) == 1
    assert payload["history"][-1]["actual_pct"] == 0.0
    # Before scheduled_offset_min (60m), forecast is 0.0%; after 60m, forecast surges > +3.5%
    pre_pts = [p for p in payload["forecast"] if 0 <= p["offset_min"] < 60]
    post_pts = [p for p in payload["forecast"] if p["offset_min"] >= 62]
    assert all(p["predicted_pct"] == 0.0 for p in pre_pts)
    assert max(p["predicted_pct"] for p in post_pts) >= 4.0

    # 2. Verify Causal Event Dataset synthesis (zero look-ahead before announcement + delisting episodes included)
    base_df = pd.DataFrame({
        "exchange_premium_spread": np.full(1500, 0.005, dtype=np.float32),
        "local_price": np.full(1500, 140000.0, dtype=np.float32),
        "foreign_price": np.full(1500, 100.0, dtype=np.float32),
        "fx_rate": np.full(1500, 1400.0, dtype=np.float32),
        "base_funding_rate": np.zeros(1500, dtype=np.float32),
        "foreign_funding_rate": np.zeros(1500, dtype=np.float32),
    })
    out_df = synthesize_causal_event_episodes(base_df)
    assert "event_new_listing" in out_df.columns
    assert "event_delisting" in out_df.columns
    assert out_df["event_new_listing"].sum() > 0
    assert out_df["event_delisting"].sum() > 0
