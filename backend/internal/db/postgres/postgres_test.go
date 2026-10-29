package postgres

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func connectTestDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = "host=127.0.0.1 user=magnolia_test password=magnolia_test dbname=magnolia_test port=15432 sslmode=disable"
	}
	db, err := NewDB(dsn)
	if err != nil {
		t.Skipf("Skipping Postgres integration test (TEST_POSTGRES_DSN not reachable: %v)", err)
	}
	return db
}

func TestUserSettingsAndDefaultThresholdsAutoSeed(t *testing.T) {
	db := connectTestDB(t)

	email := fmt.Sprintf("unit_%d@magnolia.test", time.Now().UnixNano())
	user := &User{
		Email:        email,
		PasswordHash: "hashed_pw",
		Region:       "kr",
	}
	if err := db.CreateUser(user); err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}
	if user.ID == 0 {
		t.Fatalf("expected non-zero User.ID")
	}

	// Verify LoadThresholds auto-seeds non-zero default values when no row exists yet
	th, err := db.LoadThresholds(user.ID, "PremiumStrategy")
	if err != nil {
		t.Fatalf("LoadThresholds failed: %v", err)
	}
	if th.MinTargetProfit != 1.5 || th.MaxStopLoss != -5.0 || th.CapitalLimitUsd != 50000.0 {
		t.Fatalf("expected non-zero seeded thresholds (1.5 / -5.0 / 50000), got: %+v", th)
	}
	if th.RebalanceTriggerSkew != 0.20 || th.RebalanceTargetSkew != 0.50 {
		t.Fatalf("expected rebalance skew 0.20 -> 0.50, got: %+v", th)
	}

	// Verify UserSettings defaults and persistence
	settings, _ := db.GetUserSettings(user.ID)
	if settings.ActiveViewMode != "PAPER" || settings.PaperUpbitKRW != 50000000.0 || settings.PaperCoinbaseUSD != 35000.0 {
		t.Fatalf("unexpected default UserSettings: %+v", settings)
	}
}

func TestActivePositionPaperVsLiveIsolation(t *testing.T) {
	db := connectTestDB(t)

	email := fmt.Sprintf("pos_%d@magnolia.test", time.Now().UnixNano())
	u := &User{Email: email, PasswordHash: "pw", Region: "kr"}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}

	paperPos := &ActivePosition{
		UserID:          u.ID,
		PositionID:      fmt.Sprintf("paper-pos-%d", time.Now().UnixNano()),
		IsPaper:         true,
		Status:          "OPEN",
		Type:            "MARGIN_SPOT",
		BaseExchange:    "UPBIT",
		ForeignExchange: "COINBASE",
		BaseSymbol:      "BTC-KRW",
		ForeignSymbol:   "BTC-USD",
		BaseAmount:      0.05,
		ForeignAmount:   0.05,
		InvestmentUSD:   3500.0,
		EntryPremium:    0.032,
		OpenedAt:        time.Now(),
	}
	livePos := &ActivePosition{
		UserID:          u.ID,
		PositionID:      fmt.Sprintf("live-pos-%d", time.Now().UnixNano()),
		IsPaper:         false,
		Status:          "OPEN",
		Type:            "FOREIGN_TRANSFER",
		BaseExchange:    "UPBIT",
		ForeignExchange: "COINBASE",
		BaseSymbol:      "ETH-KRW",
		ForeignSymbol:   "ETH-USD",
		BaseAmount:      1.0,
		ForeignAmount:   1.0,
		InvestmentUSD:   2500.0,
		EntryPremium:    0.028,
		OpenedAt:        time.Now(),
	}

	if err := db.SavePosition(paperPos); err != nil {
		t.Fatalf("SavePosition paper failed: %v", err)
	}
	if err := db.SavePosition(livePos); err != nil {
		t.Fatalf("SavePosition live failed: %v", err)
	}

	paperList, err := db.GetActivePositionsByUserAndMode(u.ID, true)
	if err != nil || len(paperList) != 1 || paperList[0].PositionID != paperPos.PositionID {
		t.Fatalf("expected 1 paper position, got %+v (err=%v)", paperList, err)
	}

	liveList, err := db.GetActivePositionsByUserAndMode(u.ID, false)
	if err != nil || len(liveList) != 1 || liveList[0].PositionID != livePos.PositionID {
		t.Fatalf("expected 1 live position, got %+v (err=%v)", liveList, err)
	}
}
