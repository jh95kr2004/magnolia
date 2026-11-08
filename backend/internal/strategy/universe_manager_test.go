package strategy

import (
	"context"
	"testing"

	"github.com/magnolia/backend/internal/exchange"
)

type mockUniverseExchange struct {
	exchange.Client
	name     string
	markets  []exchange.MarketInfo
	networks []exchange.NetworkInfo
}

func (m *mockUniverseExchange) GetName() string { return m.name }
func (m *mockUniverseExchange) GetMarkets(ctx context.Context) ([]exchange.MarketInfo, error) {
	return m.markets, nil
}
func (m *mockUniverseExchange) GetSupportedNetworks(ctx context.Context, currency exchange.Currency) ([]exchange.NetworkInfo, error) {
	if currency == "" {
		return m.networks, nil
	}
	var out []exchange.NetworkInfo
	for _, n := range m.networks {
		if string(n.Network) == string(currency) {
			out = append(out, n)
		}
	}
	return out, nil
}

func TestDynamicUniverseManager_AllAltcoinsAndPreListingInjection(t *testing.T) {
	upbitMock := &mockUniverseExchange{
		name: "UPBIT",
		markets: []exchange.MarketInfo{
			{Symbol: "BTC-KRW", BaseCurrency: "BTC", QuoteCurrency: "KRW", MarketType: "SPOT", IsTradingEnabled: true, IsDepositEnabled: true, IsWithdrawEnabled: true},
			{Symbol: "ETH-KRW", BaseCurrency: "ETH", QuoteCurrency: "KRW", MarketType: "SPOT", IsTradingEnabled: true, IsDepositEnabled: true, IsWithdrawEnabled: true},
			{Symbol: "SOL-KRW", BaseCurrency: "SOL", QuoteCurrency: "KRW", MarketType: "SPOT", IsTradingEnabled: true, IsDepositEnabled: true, IsWithdrawEnabled: true},
			{Symbol: "SEI-KRW", BaseCurrency: "SEI", QuoteCurrency: "KRW", MarketType: "SPOT", IsTradingEnabled: true, IsDepositEnabled: true, IsWithdrawEnabled: true},
		},
	}

	coinbaseMock := &mockUniverseExchange{
		name: "COINBASE",
		markets: []exchange.MarketInfo{
			{Symbol: "BTC-USD", BaseCurrency: "BTC", QuoteCurrency: "USD", MarketType: "SPOT", IsTradingEnabled: true, IsDepositEnabled: true, IsWithdrawEnabled: true},
			{Symbol: "ETH-USD", BaseCurrency: "ETH", QuoteCurrency: "USD", MarketType: "SPOT", IsTradingEnabled: true, IsDepositEnabled: true, IsWithdrawEnabled: true},
			{Symbol: "SOL-USD", BaseCurrency: "SOL", QuoteCurrency: "USD", MarketType: "SPOT", IsTradingEnabled: true, IsDepositEnabled: true, IsWithdrawEnabled: true},
			{Symbol: "SEI-USD", BaseCurrency: "SEI", QuoteCurrency: "USD", MarketType: "SPOT", IsTradingEnabled: true, IsDepositEnabled: true, IsWithdrawEnabled: true},
			// Newly listed coin on Foreign exchange not yet in Upbit /v1/market/all
			{Symbol: "ZRO-USD", BaseCurrency: "ZRO", QuoteCurrency: "USD", MarketType: "SPOT", IsTradingEnabled: true, IsDepositEnabled: true, IsWithdrawEnabled: true},
		},
		networks: []exchange.NetworkInfo{
			{Network: "ZRO", ExchangeNetID: "ARB", IsDepositEnable: true, IsWithdrawEnable: true, ContractAddress: "0x6985884c4392d348587b19cb9eaaf157f13271cd", EstimatedTime: 60},
		},
	}

	ranker := NewGlobalRawRanker(nil)
	mgr := NewDynamicUniverseManager(
		ranker,
		nil,
		[]exchange.Client{upbitMock, coinbaseMock},
		"ALL",
		"KRW",
		"USD",
		30,
	)

	ctx := context.Background()
	pairs, coins := mgr.RefreshUniverse(ctx)
	if len(coins) != 4 {
		t.Fatalf("expected 4 overlapping coins initially (BTC, ETH, SOL, SEI), got %d (%v)", len(coins), coins)
	}
	if len(pairs) < 4 {
		t.Fatalf("expected at least 4 cross-market pairs, got %d", len(pairs))
	}

	// Now simulate a NEW_KRW_LISTING announcement for "ZRO" before Upbit adds it to /v1/market/all
	updatedPairs, isNew := mgr.InjectPreListingCoin(ctx, "ZRO")
	if !isNew {
		t.Fatalf("expected ZRO injection to return isNew=true")
	}
	updatedCoins := mgr.GetActiveCoins()
	if len(updatedCoins) != 5 {
		t.Fatalf("expected 5 coins after ZRO pre-listing injection, got %d (%v)", len(updatedCoins), updatedCoins)
	}
	if updatedCoins[0] != "ZRO" {
		t.Fatalf("expected newly injected catalyst coin ZRO to be prioritized first, got %s", updatedCoins[0])
	}
	foundZRO := false
	for _, p := range updatedPairs {
		if string(p.BaseCurrency) == "ZRO" && p.BaseClient.GetName() == "UPBIT" && p.ForeignClient.GetName() == "COINBASE" {
			foundZRO = true
			if p.BaseNetwork.ContractAddress != "0x6985884c4392d348587b19cb9eaaf157f13271cd" {
				t.Errorf("expected synthesized ZRO pair to inherit foreign contract address, got %s", p.BaseNetwork.ContractAddress)
			}
		}
	}
	if !foundZRO {
		t.Fatalf("expected synthesized ZRO UPBIT<->COINBASE pre-listing pair in updatedPairs")
	}
}
