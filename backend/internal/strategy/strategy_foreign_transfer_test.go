package strategy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/magnolia/backend/internal/exchange"
	"github.com/magnolia/backend/internal/execution"
	"github.com/magnolia/backend/internal/wallet"
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
	exec.WithdrawPollInterval = 10 * time.Millisecond

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

func TestDualKimpAndPerExchangeTetherRates(t *testing.T) {
	SetFXRate("KRW", 1400.0)
	SetExchangeTetherRate("UPBIT", "KRW", 1442.0)   // +3.0% Tether Kimp on Upbit
	SetExchangeTetherRate("BITHUMB", "KRW", 1449.0) // +3.5% Tether Kimp on Bithumb

	if got := GetExchangeTetherRate("UPBIT", "KRW"); got != 1442.0 {
		t.Fatalf("expected Upbit USDT-KRW 1442.0, got %.2f", got)
	}
	if got := GetExchangeTetherRate("BITHUMB", "KRW"); got != 1449.0 {
		t.Fatalf("expected Bithumb USDT-KRW 1449.0, got %.2f", got)
	}

	strat := &ForeignTransferStrategy{}
	// Foreign BTC = $100,000 USD, Base BTC on Upbit = 148,526,000 KRW ($106,090 USD at 1400 FX -> +6.09% Nominal Kimp, +3.0% Real Tether Kimp)
	baseSpot := exchange.PriceUpdate{
		Exchange: "UPBIT",
		BidUSD:   148526000.0 / 1400.0,
		AskUSD:   148526000.0 / 1400.0,
		RawBid:   148526000.0,
		RawAsk:   148526000.0,
	}
	foreignSpot := exchange.PriceUpdate{
		Exchange:     "BINANCE",
		BidUSD:       100000.0,
		AskUSD:       100000.0,
		BidFutureUSD: 99950.0,
		AskFutureUSD: 99950.0,
	}
	params := PremiumEvalParams{
		BaseExchange:           "UPBIT",
		ForeignExchange:        "BINANCE",
		BaseSupportsMargin:     false,
		ForeignSupportsFutures: true,
		BaseBalanceUsd:         25000.0,
		ForeignBalanceUsd:      25000.0,
		BalanceSkewThreshold:   0.15,
		InvestmentUsd:          5000.0,
		EstTransferTime:        3 * time.Minute,
		FXRateKRW:              1400.0,
		TetherRateKRW:          1442.0,
		VASPTier:               exchange.VASPTier1Solution,
	}

	eval := strat.Evaluate(baseSpot, foreignSpot, params)
	if eval.ErrorReason != "" {
		t.Fatalf("unexpected evaluation error: %s", eval.ErrorReason)
	}
	if eval.HedgeMode != "FOREIGN_FUTURES_HEDGE" {
		t.Fatalf("expected HedgeMode FOREIGN_FUTURES_HEDGE when BaseSupportsMargin=false, got %s", eval.HedgeMode)
	}
	if eval.FiatPremiumPct <= eval.TetherRealPremiumPct {
		t.Fatalf("expected FiatPremiumPct (%.4f) > TetherRealPremiumPct (%.4f) when Tether is at +3%% premium",
			eval.FiatPremiumPct, eval.TetherRealPremiumPct)
	}
}

type mockWalletProxy struct {
	gasBalances map[string]float64
}

func (m *mockWalletProxy) GetName() string {
	return "MOCK_WALLET"
}

func (m *mockWalletProxy) GetSupportedNetworks(ctx context.Context, currency string) ([]exchange.NetworkInfo, error) {
	return []exchange.NetworkInfo{{Network: "BTC", WithdrawFeeFixed: 0.0001}}, nil
}

func (m *mockWalletProxy) GetReceiveAddress(ctx context.Context, currency string, network string) (string, error) {
	return "0xMockPersonalWalletAddr", nil
}

func (m *mockWalletProxy) GetBalance(ctx context.Context) ([]exchange.Balance, error) {
	var out []exchange.Balance
	for k, v := range m.gasBalances {
		out = append(out, exchange.Balance{Currency: exchange.Currency(k), Available: v})
	}
	return out, nil
}

func (m *mockWalletProxy) CreateTransfer(ctx context.Context, req wallet.TransferRequest) (string, error) {
	exchange.CreditPaperDepositByAddress(req.ToAddress, req.Currency, req.Amount)
	return "mock-wallet-tx-001", nil
}

var _ wallet.Client = (*mockWalletProxy)(nil)

func TestVASPTier3TwoHopNetROIAndPreflightGasGuard(t *testing.T) {
	if tier := GetVASPTier("UPBIT", "COINBASE"); tier != exchange.VASPTier3RiskAssessed {
		t.Fatalf("expected COINBASE to be VASPTier3RiskAssessed on UPBIT, got %s", tier)
	}

	// 1. Test CheckPreflightWalletGas blocks when ETH gas is insufficient
	lowGasWallet := &mockWalletProxy{gasBalances: map[string]float64{"ETH": 0.0001}}
	if err := execution.CheckPreflightWalletGas(context.Background(), lowGasWallet, "ERC20", "ETH", 0.003); err == nil {
		t.Fatalf("expected CheckPreflightWalletGas to fail when ETH balance (0.0001) < minRequired (0.003)")
	}

	// 2. Test CheckPreflightWalletGas succeeds when ETH gas is sufficient
	okGasWallet := &mockWalletProxy{gasBalances: map[string]float64{"ETH": 0.05}}
	if err := execution.CheckPreflightWalletGas(context.Background(), okGasWallet, "ERC20", "ETH", 0.003); err != nil {
		t.Fatalf("expected CheckPreflightWalletGas to succeed when ETH balance is 0.05, got: %v", err)
	}
}

func TestThreeStageHedgeFallbackAndInTransitKimpDropDefense(t *testing.T) {
	SetFXRate("KRW", 1400.0)
	SetExchangeTetherRate("UPBIT", "KRW", 1400.0)

	realUpbit := exchange.NewUpbitClient("public", "public")
	// Note: realUpbit.SupportsMargin() is FALSE because web session is not logged in
	realCb, err := exchange.NewCoinbaseClient("public", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", 0)
	if err != nil {
		t.Fatalf("failed to create coinbase client: %v", err)
	}

	// Simulate in-transit Kimp drop: Upbit BTC drops to 139,500,000 KRW (-0.35% below $100,000 * 1400) when transfer arrives!
	paperUpbit := &mockTickerClient{
		PaperClient: exchange.NewPaperClient(realUpbit, map[string]float64{
			"KRW": 50000000.0,
			"BTC": 0.0,
		}),
		spotPrice:   139500000.0,
		futurePrice: 139500000.0,
	}
	paperCoinbase := &mockTickerClient{
		PaperClient: exchange.NewPaperClient(realCb, map[string]float64{
			"USD": 50000.0,
			"BTC": 0.0,
		}),
		spotPrice:   100000.0,
		futurePrice: 100000.0,
	}

	paperUpbit.SetPriceHint("BTC-KRW", 139500000.0, 139500000.0)
	paperCoinbase.SetPriceHint("BTC-USD", 100000.0, 100000.0)

	pair := CrossMarketPair{
		BaseCurrency: "BTC",
		BaseClient:   paperUpbit,
		BaseMarketInfo: exchange.MarketInfo{
			Symbol:           "BTC-KRW",
			BaseCurrency:     "BTC",
			QuoteCurrency:    "KRW",
			MarketType:       "SPOT",
			IsDepositEnabled: true,
		},
		BaseNetwork:   exchange.NetworkInfo{Network: "BTC", WithdrawFeeFixed: 0.0001},
		ForeignClient: paperCoinbase,
		ForeignMarketInfo: exchange.MarketInfo{
			Symbol:            "BTC-USD",
			BaseCurrency:      "BTC",
			QuoteCurrency:     "USD",
			MarketType:        "FUTURE",
			IsWithdrawEnabled: true,
		},
		ForeignNetwork:     exchange.NetworkInfo{Network: "BTC", WithdrawFeeFixed: 0.0001},
		IsValidForTransfer: true,
	}

	strat := &ForeignTransferStrategy{}
	strat.SetEvaluation(StrategyEvaluation{
		Type:                 StrategyForeignTransfer,
		HedgeMarket:          "SPOT",
		HedgeMode:            "FOREIGN_FUTURES_HEDGE",
		EntryPremium:         0.035,
		TetherRealPremiumPct: 0.035,
		InvestmentUSD:        2000.0,
		VASPTier:             exchange.VASPTier3RiskAssessed,
		NeedsWalletProxy:     true,
	})

	exec := execution.NewExecutor(false)
	exec.WithdrawPollInterval = 10 * time.Millisecond
	walletProxy := &mockWalletProxy{gasBalances: map[string]float64{"BTC": 0.05, "ETH": 0.05}}

	pos, err := strat.ExecuteEntry(context.Background(), exec, pair, walletProxy)
	if err != nil {
		t.Fatalf("ExecuteEntry failed: %v", err)
	}
	if pos.Status != "STAGED_ASSASSINATION" {
		t.Fatalf("expected position status to auto-switch to STAGED_ASSASSINATION on in-transit Kimp drop, got %s", pos.Status)
	}
	if pos.AssassinationRationaleJSON == "" {
		t.Fatalf("expected non-empty AssassinationRationaleJSON explaining IN_TRANSIT_KIMP_DROP_DEFENSE")
	}

	var rat AssassinationRationale
	if err := json.Unmarshal([]byte(pos.AssassinationRationaleJSON), &rat); err != nil {
		t.Fatalf("failed to unmarshal AssassinationRationaleJSON: %v", err)
	}
	if rat.TriggerType != "IN_TRANSIT_KIMP_DROP_DEFENSE" {
		t.Fatalf("expected TriggerType IN_TRANSIT_KIMP_DROP_DEFENSE, got %s", rat.TriggerType)
	}

	// Now simulate Kimp rebounding to +5% (147,000,000 KRW) and execute assassination exit
	paperUpbit.spotPrice = 147000000.0
	paperUpbit.SetPriceHint("BTC-KRW", 147000000.0, 147000000.0)
	if _, _, err := strat.ExecuteAssassinationExit(context.Background(), exec, pair, *pos); err != nil {
		t.Fatalf("ExecuteAssassinationExit failed: %v", err)
	}
}

func TestPreStagedListingTwoStageAssassinationFlow(t *testing.T) {
	SetFXRate("KRW", 1400.0)
	SetExchangeTetherRate("UPBIT", "KRW", 1400.0)

	realUpbit := exchange.NewUpbitClient("public", "public")
	realCb, _ := exchange.NewCoinbaseClient("public", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", 0)

	paperUpbit := &mockTickerClient{
		PaperClient: exchange.NewPaperClient(realUpbit, map[string]float64{"KRW": 50000000.0, "BTC": 0.0}),
		spotPrice:   148000000.0,
		futurePrice: 148000000.0,
	}
	paperCoinbase := &mockTickerClient{
		PaperClient: exchange.NewPaperClient(realCb, map[string]float64{"USD": 50000.0, "BTC": 0.0}),
		spotPrice:   100000.0,
		futurePrice: 100000.0,
	}

	paperUpbit.SetPriceHint("BTC-KRW", 139500000.0, 139500000.0)
	paperCoinbase.SetPriceHint("BTC-USD", 100000.0, 100000.0)

	pair := CrossMarketPair{
		BaseCurrency: "BTC",
		BaseClient:   paperUpbit,
		BaseMarketInfo: exchange.MarketInfo{
			Symbol:           "BTC-KRW",
			BaseCurrency:     "BTC",
			QuoteCurrency:    "KRW",
			MarketType:       "SPOT",
			IsDepositEnabled: false, // Pre-listing: deposit not yet open
		},
		BaseNetwork:   exchange.NetworkInfo{Network: "BTC", WithdrawFeeFixed: 0.0001},
		ForeignClient: paperCoinbase,
		ForeignMarketInfo: exchange.MarketInfo{
			Symbol:            "BTC-USD",
			BaseCurrency:      "BTC",
			QuoteCurrency:     "USD",
			MarketType:        "FUTURE",
			IsWithdrawEnabled: true,
		},
		ForeignNetwork:     exchange.NetworkInfo{Network: "BTC", WithdrawFeeFixed: 0.0001},
		IsValidForTransfer: false,
	}

	strat := &ForeignTransferStrategy{}
	strat.SetEvaluation(StrategyEvaluation{
		Type:            StrategyForeignTransfer,
		HedgeMode:       "FOREIGN_FUTURES_HEDGE",
		InvestmentUSD:   2000.0,
		PreStageListing: true,
		EventType:       "NEW_KRW_LISTING",
		EventTitle:      "[거래] 원화 마켓 신규 상장 안내 (BTC)",
	})

	exec := execution.NewExecutor(false)
	exec.WithdrawPollInterval = 10 * time.Millisecond
	walletProxy := &mockWalletProxy{gasBalances: map[string]float64{"BTC": 0.05, "ETH": 0.05}}

	// Stage 1: Buy + 1x Short on Foreign & withdraw to personal wallet
	pos, err := strat.ExecuteEntry(context.Background(), exec, pair, walletProxy)
	if err != nil {
		t.Fatalf("Stage-1 PreStageListing ExecuteEntry failed: %v", err)
	}
	if pos.Status != "PRE_STAGED_LISTING" {
		t.Fatalf("expected status PRE_STAGED_LISTING after Stage-1, got %s", pos.Status)
	}

	// Stage 2: Base deposit opens -> CompletePreStagedListingToBase transfers from personal wallet to Base
	pair.BaseMarketInfo.IsDepositEnabled = true
	pair.IsValidForTransfer = true
	if err := strat.CompletePreStagedListingToBase(context.Background(), exec, pair, pos, walletProxy); err != nil {
		t.Fatalf("Stage-2 CompletePreStagedListingToBase failed: %v", err)
	}
	if pos.Status != "STAGED_ASSASSINATION" {
		t.Fatalf("expected status STAGED_ASSASSINATION after Stage-2, got %s", pos.Status)
	}

	// Stage 3: Listing surge -> ExecuteAssassinationExit
	if _, _, err := strat.ExecuteAssassinationExit(context.Background(), exec, pair, *pos); err != nil {
		t.Fatalf("Stage-3 ExecuteAssassinationExit failed: %v", err)
	}
}

func TestUpbitVerifyTravelRuleDepositMockHTTP(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/deposits", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"uuid":"dep-uuid-123","currency":"BTC","txid":"tx-abc","state":"TRAVEL_RULE_SUSPECTED","amount":"0.05"}]`))
	})
	mux.HandleFunc("/v1/travel_rule/vasps", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"vasp_name":"Binance","vasp_uuid":"vasp-binance-001","depositable":true,"withdrawable":true}]`))
	})
	mux.HandleFunc("/v1/travel_rule/deposit/uuid", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deposit_uuid":"dep-uuid-123","verification_result":"VERIFIED","deposit_state":"PROCESSING"}`))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	t.Setenv("UPBIT_REST_URL_OVERRIDE", server.URL+"/v1")
	upbit := exchange.NewUpbitClient("test-access", "test-secret")

	status, err := upbit.VerifyTravelRuleDeposit(context.Background(), "BTC", "tx-abc", "BINANCE")
	if err != nil {
		t.Fatalf("VerifyTravelRuleDeposit failed: %v", err)
	}
	if !status.Verified || status.DepositUUID != "dep-uuid-123" {
		t.Fatalf("unexpected TravelRuleDepositStatus: %+v", status)
	}
}

