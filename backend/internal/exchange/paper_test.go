package exchange

import (
	"context"
	"math"
	"testing"
)

func TestPaperClientUserIsolationAndSpotOrders(t *testing.T) {
	ctx := context.Background()
	realUpbit := NewUpbitClient("public", "public")

	pUser1 := NewPaperClientForUser(101, realUpbit, map[string]float64{"KRW": 50000000.0})
	pUser2 := NewPaperClientForUser(102, realUpbit, map[string]float64{"KRW": 10000000.0})

	pUser1.SetPriceHint("BTC-KRW", 100000000.0, 100000000.0)
	pUser2.SetPriceHint("BTC-KRW", 100000000.0, 100000000.0)

	// User 101 buys 0.1 BTC (10,000,000 KRW + fee)
	resp, err := pUser1.PlaceOrder(ctx, OrderRequest{
		Symbol: "BTC-KRW",
		Side:   BuyOrder,
		Type:   MarketOrder,
		Amount: 0.1,
	})
	if err != nil {
		t.Fatalf("expected spot buy to succeed, got: %v", err)
	}
	if resp.Amount != 0.1 {
		t.Fatalf("expected 0.1 BTC filled, got %f", resp.Amount)
	}

	bals1, _ := pUser1.GetBalances(ctx)
	bals2, _ := pUser2.GetBalances(ctx)

	var u1BTC, u1KRW, u2KRW float64
	for _, b := range bals1 {
		if b.Currency == "BTC" {
			u1BTC = b.Available
		}
		if b.Currency == "KRW" {
			u1KRW = b.Available
		}
	}
	for _, b := range bals2 {
		if b.Currency == "KRW" {
			u2KRW = b.Available
		}
	}

	if math.Abs(u1BTC-0.1) > 1e-8 {
		t.Fatalf("expected User 101 to hold 0.1 BTC, got %f", u1BTC)
	}
	if u1KRW >= 40000000.0 {
		t.Fatalf("expected User 101 KRW balance < 40,000,000 after fee, got %f", u1KRW)
	}
	if math.Abs(u2KRW-10000000.0) > 1e-6 {
		t.Fatalf("expected User 102 KRW balance untouched at 10,000,000, got %f", u2KRW)
	}
}

func TestPaperClientMarginBorrowRepayAndFuturesPnL(t *testing.T) {
	ctx := context.Background()
	realCb, _ := NewCoinbaseClient("public", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", 0)
	pc := NewPaperClientForUser(103, realCb, map[string]float64{"USD": 20000.0})
	pc.SetPriceHint("ETH-USD", 2000.0, 2000.0)

	// 1. Margin Borrow 2 ETH -> Sell 2 ETH -> Repay 2 ETH
	if err := pc.Borrow(ctx, "ETH", 2.0); err != nil {
		t.Fatalf("borrow failed: %v", err)
	}
	snapAfterBorrow := pc.ExportSnapshot()
	if snapAfterBorrow.Borrowed["ETH"] != 2.0 || snapAfterBorrow.Balances["ETH"] != 2.0 {
		t.Fatalf("unexpected borrow state: %+v", snapAfterBorrow)
	}

	if err := pc.Repay(ctx, "ETH", 2.0); err != nil {
		t.Fatalf("repay failed: %v", err)
	}
	snapAfterRepay := pc.ExportSnapshot()
	if snapAfterRepay.Borrowed["ETH"] != 0 || snapAfterRepay.Balances["ETH"] != 0 {
		t.Fatalf("expected zero ETH debt and balance after repay, got: %+v", snapAfterRepay)
	}

	// 2. Futures Long 1 ETH at $2,000 -> Price rises to $2,200 -> Close with SellOrder
	_, err := pc.PlaceOrder(ctx, OrderRequest{
		Symbol: "ETH-USD",
		Side:   BuyOrder,
		Type:   FuturesLong,
		Amount: 1.0,
	})
	if err != nil {
		t.Fatalf("futures long open failed: %v", err)
	}

	// Price rises +$200
	pc.SetPriceHint("ETH-USD", 2200.0, 2200.0)
	_, err = pc.PlaceOrder(ctx, OrderRequest{
		Symbol: "ETH-USD",
		Side:   SellOrder,
		Type:   FuturesShort,
		Amount: 1.0,
	})
	if err != nil {
		t.Fatalf("futures long close failed: %v", err)
	}

	bals, _ := pc.GetBalances(ctx)
	var finalUSD float64
	for _, b := range bals {
		if b.Currency == "USD" {
			finalUSD = b.Available
		}
	}
	// Initial $20,000 + $200 realized profit - fees (~$4-16) > $20,150
	if finalUSD <= 20150.0 {
		t.Fatalf("expected realized futures profit to lift USD above $20,150, got %.2f", finalUSD)
	}

	// 3. Export / Reset / Import state round-trip
	exportedJSON := ExportAllUserPaperState(103)
	if exportedJSON == "" {
		t.Fatalf("expected non-empty exported JSON state")
	}

	pc.ResetBalances(map[string]float64{"USD": 10000.0})
	balsAfterReset, _ := pc.GetBalances(ctx)
	for _, b := range balsAfterReset {
		if b.Currency == "USD" && math.Abs(b.Available-10000.0) > 1e-6 {
			t.Fatalf("expected reset USD balance 10000, got %.2f", b.Available)
		}
	}

	ImportAllUserPaperState(103, exportedJSON)
	balsRestored, _ := pc.GetBalances(ctx)
	for _, b := range balsRestored {
		if b.Currency == "USD" && math.Abs(b.Available-finalUSD) > 1e-6 {
			t.Fatalf("expected restored USD balance %.2f, got %.2f", finalUSD, b.Available)
		}
	}
}
