package strategy

import (
	"context"
	"testing"
	"time"

	"github.com/magnolia/backend/internal/exchange"
	"github.com/magnolia/backend/internal/execution"
)

func TestForeignTransferStrategySkewCheck(t *testing.T) {
	strat := &ForeignTransferStrategy{}

	baseSpot := exchange.PriceUpdate{
		BidUSD:            100.0,
		AskUSD:            100.0,
		BorrowRatePerHour: 0.001,
	}
	foreignSpot := exchange.PriceUpdate{
		BidUSD:            95.0,
		AskUSD:            95.0,
		BorrowRatePerHour: 0.001,
	}

	// 1. Skew check passes: foreign balance has 50% of total
	paramsPass := PremiumEvalParams{
		BaseSupportsMargin:   true,
		BaseBalanceUsd:       1000.0,
		ForeignBalanceUsd:    1000.0,
		BalanceSkewThreshold: 0.15,
		InvestmentUsd:        100.0,
		EstTransferTime:      5 * time.Minute,
	}

	evalPass := strat.Evaluate(baseSpot, foreignSpot, paramsPass)
	if evalPass.ErrorReason != "" {
		t.Errorf("expected no evaluation error, got: %s", evalPass.ErrorReason)
	}

	// 2. Skew check fails: foreign balance is 10% of total (100 / 1000 < 0.15)
	paramsFail := PremiumEvalParams{
		BaseSupportsMargin:   true,
		BaseBalanceUsd:       900.0,
		ForeignBalanceUsd:    100.0,
		BalanceSkewThreshold: 0.15,
		InvestmentUsd:        100.0,
		EstTransferTime:      5 * time.Minute,
	}

	evalFail := strat.Evaluate(baseSpot, foreignSpot, paramsFail)
	if evalFail.ErrorReason == "" {
		t.Errorf("expected evaluation error due to skew protection, but got none")
	} else {
		t.Logf("Got expected error reason: %s", evalFail.ErrorReason)
	}
}

func TestFutureSpotAndFutureFutureDirectionality(t *testing.T) {
	stratD := &FutureSpotStrategy{}
	stratE := &FutureFutureStrategy{}

	// Base is at $105,000 (5% premium over Foreign $100,000), expected to narrow to 1%
	baseQuote := exchange.PriceUpdate{
		BidUSD:       105000.0,
		AskUSD:       105100.0,
		BidFutureUSD: 105000.0,
		AskFutureUSD: 105100.0,
	}
	foreignQuote := exchange.PriceUpdate{
		BidUSD:       99900.0,
		AskUSD:       100000.0,
		BidFutureUSD: 99900.0,
		AskFutureUSD: 100000.0,
	}
	params := PremiumEvalParams{
		BaseFutureSellFeeRate:      0.0004,
		BaseFutureBuyFeeRate:       0.0004,
		ForeignSpotBuyFeeRate:      0.001,
		ForeignSpotSellFeeRate:     0.001,
		ForeignFutureBuyFeeRate:    0.0004,
		ForeignFutureSellFeeRate:   0.0004,
		PredictedPremiumTrajectory: []float64{0.01}, // Expected to narrow to 1%
		PredictionConfidence:       0.9,
		InvestmentUsd:              10000.0,
		BaseBalanceUsd:             50000.0,
		ForeignBalanceUsd:          50000.0,
	}

	evalD := stratD.Evaluate(baseQuote, foreignQuote, params)
	if evalD.BasisReturn <= 0 {
		t.Fatalf("expected positive BasisReturn for Strategy D when premium narrows from 5%% to 1%%, got %.4f", evalD.BasisReturn)
	}
	if evalD.NetROI <= 0 {
		t.Fatalf("expected positive NetROI for Strategy D, got %.4f", evalD.NetROI)
	}

	evalE := stratE.Evaluate(baseQuote, foreignQuote, params)
	if evalE.BasisReturn <= 0 {
		t.Fatalf("expected positive BasisReturn for Strategy E when premium narrows from 5%% to 1%%, got %.4f", evalE.BasisReturn)
	}
	if evalE.NetROI <= 0 {
		t.Fatalf("expected positive NetROI for Strategy E, got %.4f", evalE.NetROI)
	}
}

func TestWebLendingSessionGuardOnUpbitClient(t *testing.T) {
	upbit := exchange.NewUpbitClient("public", "public")

	// Initially, without an active browser login session, SupportsMargin must be false
	if upbit.SupportsMargin() {
		t.Fatalf("expected UpbitClient.SupportsMargin() to be false when web session is not logged in")
	}

	// Simulate active web lending session
	upbit.LendingManager().SetSessionValidForTest(true)
	if !upbit.SupportsMargin() {
		t.Fatalf("expected UpbitClient.SupportsMargin() to be true when web session is valid")
	}

	// Simulate session expiration
	upbit.LendingManager().SetSessionValidForTest(false)
	if upbit.SupportsMargin() {
		t.Fatalf("expected UpbitClient.SupportsMargin() to immediately become false upon session loss")
	}
}

// mockTickerClient wraps PaperClient with deterministic offline tickers for unit testing.
type mockTickerClient struct {
	*exchange.PaperClient
	spotPrice   float64
	futurePrice float64
}

func (m *mockTickerClient) GetTicker(ctx context.Context, symbol exchange.Symbol) (float64, error) {
	return m.spotPrice, nil
}

func (m *mockTickerClient) GetFutureTicker(ctx context.Context, symbol exchange.Symbol) (float64, error) {
	return m.futurePrice, nil
}

func TestExecuteHedgedRebalanceDeltaNeutral(t *testing.T) {
	realUpbit := exchange.NewUpbitClient("public", "public")
	realCb, err := exchange.NewCoinbaseClient("public", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", 0)
	if err != nil {
		t.Fatalf("failed to create coinbase client: %v", err)
	}

	paperUpbit := &mockTickerClient{
		PaperClient: exchange.NewPaperClient(realUpbit, map[string]float64{
			"KRW": 70000000.0, // ~$50,000 in KRW
			"BTC": 0.0,
		}),
		spotPrice:   140000000.0, // 1 BTC = 140,000,000 KRW ($100,000 at 1400 FX)
		futurePrice: 140000000.0,
	}
	paperCoinbase := &mockTickerClient{
		PaperClient: exchange.NewPaperClient(realCb, map[string]float64{
			"USD": 5000.0, // Low USD balance (skewed)
			"BTC": 0.0,
		}),
		spotPrice:   100000.0, // 1 BTC = $100,000 USD
		futurePrice: 100000.0,
	}

	reversePair := CrossMarketPair{
		BaseCurrency:  "BTC",
		BaseClient:    paperCoinbase,
		BaseMarketInfo: exchange.MarketInfo{
			Symbol:        "BTC-USD",
			BaseCurrency:  "BTC",
			QuoteCurrency: "USD",
			MarketType:    "SPOT",
		},
		BaseNetwork: exchange.NetworkInfo{Network: "BTC"},
		ForeignClient: paperUpbit,
		ForeignMarketInfo: exchange.MarketInfo{
			Symbol:        "BTC-KRW",
			BaseCurrency:  "BTC",
			QuoteCurrency: "KRW",
			MarketType:    "SPOT",
		},
		ForeignNetwork:     exchange.NetworkInfo{Network: "BTC"},
		IsValidForTransfer: true,
	}

	strat := &ForeignTransferStrategy{}
	exec := execution.NewExecutor(false)

	rebalanceUSD := 10000.0
	if err := strat.ExecuteHedgedRebalance(context.Background(), exec, reversePair, rebalanceUSD, nil); err != nil {
		t.Fatalf("ExecuteHedgedRebalance failed: %v", err)
	}

	cbBalances, _ := paperCoinbase.GetBalances(context.Background())
	var usdAfter float64
	for _, b := range cbBalances {
		if b.Currency == "USD" {
			usdAfter = b.Available
		}
	}
	if usdAfter <= 5000.0 {
		t.Fatalf("expected Coinbase USD balance to increase after rebalancing $10,000 from Upbit, got %.2f", usdAfter)
	}
}
