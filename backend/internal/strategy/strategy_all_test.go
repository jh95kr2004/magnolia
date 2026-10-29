package strategy

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/magnolia/backend/internal/exchange"
	"github.com/magnolia/backend/internal/execution"
)

func TestAllFiveStrategiesEvaluateAndExecute(t *testing.T) {
	ctx := context.Background()
	realUpbit := exchange.NewUpbitClient("public", "public")
	realUpbit.LendingManager().SetSessionValidForTest(true)
	realCb, _ := exchange.NewCoinbaseClient("public", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", 0)

	paperUpbit := &mockTickerClient{
		PaperClient: exchange.NewPaperClientForUser(201, realUpbit, map[string]float64{
			"KRW": 140000000.0,
			"BTC": 2.0,
		}),
		spotPrice:   147000000.0, // $105,000 at 1400 KRW/USD (5% premium)
		futurePrice: 147000000.0,
	}
	paperCb := &mockTickerClient{
		PaperClient: exchange.NewPaperClientForUser(201, realCb, map[string]float64{
			"USD": 100000.0,
			"BTC": 2.0,
		}),
		spotPrice:   100000.0,
		futurePrice: 100000.0,
	}

	pair := CrossMarketPair{
		BaseCurrency: "BTC",
		BaseClient:   paperUpbit,
		BaseMarketInfo: exchange.MarketInfo{
			Symbol:        "BTC-KRW",
			BaseCurrency:  "BTC",
			QuoteCurrency: "KRW",
			MarketType:    "SPOT",
		},
		BaseNetwork:   exchange.NetworkInfo{Network: "BTC"},
		ForeignClient: paperCb,
		ForeignMarketInfo: exchange.MarketInfo{
			Symbol:        "BTC-USD",
			BaseCurrency:  "BTC",
			QuoteCurrency: "USD",
			MarketType:    "SPOT",
		},
		ForeignNetwork:     exchange.NetworkInfo{Network: "BTC"},
		IsValidForTransfer: true,
	}

	baseQuote := exchange.PriceUpdate{
		Exchange:          "UPBIT",
		Symbol:            "BTC-KRW",
		QuoteCurrency:     "KRW",
		RawBid:            147000000.0,
		RawAsk:            147000000.0,
		BidUSD:            105000.0,
		AskUSD:            105000.0,
		BidFutureUSD:      105000.0,
		AskFutureUSD:      105000.0,
		BorrowRatePerHour: 0.0001,
		FXRate:            1400.0,
	}
	foreignQuote := exchange.PriceUpdate{
		Exchange:          "COINBASE",
		Symbol:            "BTC-USD",
		QuoteCurrency:     "USD",
		RawBid:            100000.0,
		RawAsk:            100000.0,
		BidUSD:            100000.0,
		AskUSD:            100000.0,
		BidFutureUSD:      100000.0,
		AskFutureUSD:      100000.0,
		BorrowRatePerHour: 0.0001,
		FXRate:            1.0,
	}

	params := PremiumEvalParams{
		BaseSpotBuyFeeRate:         0.0005,
		BaseSpotSellFeeRate:        0.0005,
		BaseFutureBuyFeeRate:       0.0004,
		BaseFutureSellFeeRate:      0.0004,
		ForeignSpotBuyFeeRate:      0.001,
		ForeignSpotSellFeeRate:     0.001,
		ForeignFutureBuyFeeRate:    0.0004,
		ForeignFutureSellFeeRate:   0.0004,
		BaseSupportsMargin:         true,
		ForeignSupportsMargin:      true,
		PredictedPremiumTrajectory: []float64{0.005},
		PredictionConfidence:       0.9,
		InvestmentUsd:              2000.0,
		BaseBalanceUsd:             50000.0,
		ForeignBalanceUsd:          50000.0,
		BalanceSkewThreshold:       0.15,
		EstTransferTime:            3 * time.Minute,
		TransferFeeFixedUsd:        2.5,
	}

	strats := []ArbitrageStrategy{
		&MarginSpotStrategy{},
		&MarginFutureStrategy{},
		&ForeignTransferStrategy{},
		&FutureSpotStrategy{},
		&FutureFutureStrategy{},
	}

	exec := execution.NewExecutor(false)
	exec.WithdrawPollInterval = 10 * time.Millisecond
	for _, s := range strats {
		eval := s.Evaluate(baseQuote, foreignQuote, params)
		if eval.ErrorReason != "" {
			t.Fatalf("strategy %s evaluate failed: %s", s.ID(), eval.ErrorReason)
		}
		if eval.NetROI <= 0 {
			t.Fatalf("expected positive NetROI for %s on 5%% spread, got %.4f", s.ID(), eval.NetROI)
		}

		s.SetEvaluation(eval)
		pos, err := s.ExecuteEntry(ctx, exec, pair, nil)
		if err != nil {
			t.Fatalf("strategy %s ExecuteEntry failed: %v", s.ID(), err)
		}
		if pos == nil || pos.BaseAmount <= 0 {
			t.Fatalf("strategy %s returned invalid position: %+v", s.ID(), pos)
		}

		if err := s.ExecuteExit(ctx, exec, pair, *pos, 1.0, nil); err != nil {
			t.Fatalf("strategy %s ExecuteExit failed: %v", s.ID(), err)
		}
	}
}

func TestGlobalRawRankerCanonicalDirectionAndFX(t *testing.T) {
	ctx := context.Background()
	ranker := NewGlobalRawRanker(nil)
	ranker.currentFXRates.Store("KRW", 1400.0)

	upbit := exchange.NewUpbitClient("public", "public")
	cb, _ := exchange.NewCoinbaseClient("public", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", 0)

	pairs := []CrossMarketPair{
		{
			BaseCurrency:       "BTC",
			BaseClient:         upbit,
			BaseMarketInfo:     exchange.MarketInfo{Symbol: "BTC-KRW", BaseCurrency: "BTC", QuoteCurrency: "KRW"},
			ForeignClient:      cb,
			ForeignMarketInfo:  exchange.MarketInfo{Symbol: "BTC-USD", BaseCurrency: "BTC", QuoteCurrency: "USD"},
			IsValidForTransfer: true,
		},
		// Reverse non-canonical direction should be filtered out by EvaluateAllCache
		{
			BaseCurrency:       "BTC",
			BaseClient:         cb,
			BaseMarketInfo:     exchange.MarketInfo{Symbol: "BTC-USD", BaseCurrency: "BTC", QuoteCurrency: "USD"},
			ForeignClient:      upbit,
			ForeignMarketInfo:  exchange.MarketInfo{Symbol: "BTC-KRW", BaseCurrency: "BTC", QuoteCurrency: "KRW"},
			IsValidForTransfer: true,
		},
	}

	priceCache := &sync.Map{}
	priceCache.Store("UPBIT|BTC-KRW", &exchange.PriceUpdate{
		Exchange:      "UPBIT",
		Symbol:        "BTC-KRW",
		QuoteCurrency: "KRW",
		RawBid:        144200000.0, // $103,000 at 1400 FX -> +3% premium
		RawAsk:        144200000.0,
	})
	priceCache.Store("COINBASE|BTC-USD", &exchange.PriceUpdate{
		Exchange:      "COINBASE",
		Symbol:        "BTC-USD",
		QuoteCurrency: "USD",
		RawBid:        100000.0,
		RawAsk:        100000.0,
	})

	opps, err := ranker.EvaluateAllCache(ctx, pairs, priceCache)
	if err != nil {
		t.Fatalf("EvaluateAllCache failed: %v", err)
	}
	if len(opps) != 1 {
		t.Fatalf("expected exactly 1 canonical opportunity, got %d", len(opps))
	}
	if opps[0].BaseExchange != "UPBIT" || opps[0].ForeignExchange != "COINBASE" {
		t.Fatalf("unexpected direction: %+v", opps[0])
	}
	if math.Abs(opps[0].PremiumPct-3.0) > 1e-4 {
		t.Fatalf("expected +3.0%% premium, got %.4f%%", opps[0].PremiumPct)
	}
}

func TestEngineGetSimStatusCosineSCurve(t *testing.T) {
	now := time.Now().Unix()
	e := &Engine{
		isPaper:            true,
		simExchange:        "UPBIT",
		simStartOffsetPct:  0.0,
		simTargetOffsetPct: 4.0,
		simRampMinutes:     2.0,          // 120 seconds total
		simStartedAt:       now - 60,     // 50% elapsed (60s / 120s)
	}

	ex, curr, target, ramp, prog, rem := e.GetSimStatus()
	if ex != "UPBIT" || target != 4.0 || ramp != 2.0 {
		t.Fatalf("unexpected sim metadata: ex=%s target=%f ramp=%f", ex, target, ramp)
	}
	// At 50% elapsed, cosine S-curve weight = (1 - cos(pi * 0.5)) / 2 = 0.5 -> curr ~ 2.0%
	if math.Abs(curr-2.0) > 0.2 {
		t.Fatalf("expected ~2.0%% offset at midpoint, got %.2f%%", curr)
	}
	if prog < 45.0 || prog > 55.0 || rem <= 0 {
		t.Fatalf("unexpected progress=%.1f rem=%d", prog, rem)
	}
}
