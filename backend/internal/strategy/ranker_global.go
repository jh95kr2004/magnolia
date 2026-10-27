package strategy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/magnolia/backend/internal/db/redis"
	"github.com/magnolia/backend/internal/exchange"
)

// CrossMarketPair represents an overlap of the same coin between two exchanges
type CrossMarketPair struct {
	BaseCurrency      exchange.Currency
	BaseClient        exchange.Client
	BaseMarketInfo    exchange.MarketInfo
	BaseNetwork       exchange.NetworkInfo
	ForeignClient     exchange.Client
	ForeignMarketInfo exchange.MarketInfo
	ForeignNetwork    exchange.NetworkInfo

	// Pre-calculated validity
	IsValidForTransfer bool
}

// RawOpportunity represents the 1st tier global calculation.
type RawOpportunity struct {
	BaseExchange          string    `json:"base_exchange"`
	ForeignExchange       string    `json:"foreign_exchange"`
	BaseSymbol            string    `json:"base_symbol"`
	ForeignSymbol         string    `json:"foreign_symbol"`
	BasePriceUSD          float64   `json:"base_price_usd"`
	BaseFuturePriceUSD    float64   `json:"base_future_price_usd"`
	ForeignPriceUSD       float64   `json:"foreign_price_usd"`
	ForeignFuturePriceUSD float64   `json:"foreign_future_price_usd"`
	BaseQuoteCurrency     string    `json:"base_quote_currency"`
	PremiumPct            float64   `json:"premium_pct"`
	BaseFundingRatePct    float64   `json:"base_funding_rate_pct"`
	ForeignFundingRatePct float64   `json:"foreign_funding_rate_pct"`
	BorrowRatePct         float64   `json:"borrow_rate_pct"`
	ExpectedMargin        float64   `json:"expected_margin"` // PremiumPct + FundingRate - BorrowRatePct
	Timestamp             time.Time `json:"timestamp"`
}

// GlobalRawRanker evaluates market intersections using cached O(1) reads
type GlobalRawRanker struct {
	redisClient    *redis.PubSubClient
	currentFXRates *sync.Map
}

// NewGlobalRawRanker constructs a singleton ranker that evaluates any given pairs
func NewGlobalRawRanker(rClient *redis.PubSubClient) *GlobalRawRanker {
	r := &GlobalRawRanker{
		redisClient:    rClient,
		currentFXRates: &sync.Map{},
	}

	r.currentFXRates.Store("KRW", 1400.0) // Default fallback

	if rClient != nil {
		ctx := context.Background()
		go func() {
			fxCh := make(chan float64)
			go rClient.SubscribeFXRate(ctx, "USD", "KRW", fxCh)
			for rate := range fxCh {
				r.currentFXRates.Store("KRW", rate)
			}
		}()
	}

	return r
}

// FindIntersections evaluates a pair of exchanges and returns valid overlaps
func (r *GlobalRawRanker) FindIntersections(ctx context.Context, baseClient, foreignClient exchange.Client) ([]CrossMarketPair, error) {
	baseMarkets, err := baseClient.GetMarkets(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get base markets: %w", err)
	}

	foreignMarkets, err := foreignClient.GetMarkets(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get foreign markets: %w", err)
	}

	baseNetworksMap := make(map[exchange.Currency]exchange.NetworkInfo)
	if baseNets, err := baseClient.GetSupportedNetworks(ctx, ""); err == nil {
		for _, n := range baseNets {
			baseNetworksMap[exchange.Currency(n.Network)] = n
		}
	}

	foreignNetworksMap := make(map[exchange.Currency]exchange.NetworkInfo)
	if forNets, err := foreignClient.GetSupportedNetworks(ctx, ""); err == nil {
		for _, n := range forNets {
			foreignNetworksMap[exchange.Currency(n.Network)] = n
		}
	}

	baseMap := make(map[exchange.Currency][]exchange.MarketInfo)
	for _, m := range baseMarkets {
		if m.IsTradingEnabled {
			baseMap[m.BaseCurrency] = append(baseMap[m.BaseCurrency], m)
		}
	}

	foreignMap := make(map[exchange.Currency][]exchange.MarketInfo)
	for _, fm := range foreignMarkets {
		if fm.IsTradingEnabled {
			foreignMap[fm.BaseCurrency] = append(foreignMap[fm.BaseCurrency], fm)
		}
	}

	var intersections []CrossMarketPair

	for baseKey, fms := range foreignMap {
		bms, ok := baseMap[baseKey]
		if ok {
			baseNet := baseNetworksMap[baseKey]
			foreignNet := foreignNetworksMap[baseKey]

			for _, fm := range fms {
				for _, bm := range bms {
					isValidForTransfer := bm.IsWithdrawEnabled && bm.IsDepositEnabled &&
						fm.IsWithdrawEnabled && fm.IsDepositEnabled
					if !isValidForTransfer {
						isValidForTransfer = bm.IsWithdrawEnabled && fm.IsDepositEnabled
					}

					intersections = append(intersections, CrossMarketPair{
						BaseCurrency:       baseKey,
						BaseClient:         baseClient,
						BaseMarketInfo:     bm,
						BaseNetwork:        baseNet,
						ForeignClient:      foreignClient,
						ForeignMarketInfo:  fm,
						ForeignNetwork:     foreignNet,
						IsValidForTransfer: isValidForTransfer,
					})
				}
			}
		}
	}

	return intersections, nil
}

// EvaluateAllCache uses the local memory cache of prices replacing EvaluateAllREST for 1-second ticks
func (r *GlobalRawRanker) EvaluateAllCache(ctx context.Context, pairs []CrossMarketPair, priceCache *sync.Map) ([]RawOpportunity, error) {
	var results []RawOpportunity

	krwRate := 1400.0
	if rate, ok := r.currentFXRates.Load("KRW"); ok {
		krwRate = rate.(float64)
	}

	for _, p := range pairs {
		baseSym := string(p.BaseMarketInfo.Symbol)
		foreignSym := string(p.ForeignMarketInfo.Symbol)

		baseKey := p.BaseClient.GetName() + "|" + baseSym
		foreignKey := p.ForeignClient.GetName() + "|" + foreignSym

		baseVal, baseOk := priceCache.Load(baseKey)
		foreignVal, foreignOk := priceCache.Load(foreignKey)

		if !baseOk || !foreignOk {
			continue
		}

		baseUpdate := baseVal.(*exchange.PriceUpdate)
		foreignUpdate := foreignVal.(*exchange.PriceUpdate)

		fxRate := 1.0
		if p.BaseMarketInfo.QuoteCurrency != "USD" {
			if p.BaseMarketInfo.QuoteCurrency == "KRW" {
				fxRate = krwRate
			} else {
				fxRate = krwRate // Default fallback for MVP
			}
		}

		// Retrieve normalized USD Prices. Preference for Bid/Ask order.
		baseUSD := baseUpdate.BidUSD
		if baseUSD == 0 && baseUpdate.RawBid > 0 {
			baseUSD = baseUpdate.RawBid / fxRate
		}

		foreignUSD := foreignUpdate.AskUSD
		if foreignUSD == 0 && foreignUpdate.RawAsk > 0 {
			foreignUSD = foreignUpdate.RawAsk
		}

		// Calculate future prices
		baseFutureUSD := baseUpdate.BidFutureUSD
		if baseFutureUSD == 0 && baseUpdate.RawFutureBid > 0 {
			baseFutureUSD = baseUpdate.RawFutureBid / fxRate
		}

		foreignFutureUSD := foreignUpdate.AskFutureUSD
		if foreignFutureUSD == 0 && foreignUpdate.RawFutureAsk > 0 {
			foreignFutureUSD = foreignUpdate.RawFutureAsk
		}

		if baseUSD <= 0 || foreignUSD <= 0 {
			continue
		}

		// Prevent redundant calculations for opposite directions.
		// Since Upbit-Coinbase and Coinbase-Upbit are mirror images of each other,
		// we only evaluate the direction where the base price is greater than or equal to
		// the foreign price. The opposite direction will have a negative premium/profit and is skipped.
		if baseUSD < foreignUSD {
			continue
		}

		// 1. 김치 프리미엄
		premiumPct := ((baseUSD - foreignUSD) / foreignUSD) * 100.0

		// 2. 펀딩비 
		baseFundingRatePct := baseUpdate.FundingRate * 100.0
		foreignFundingRatePct := foreignUpdate.FundingRate * 100.0

		// 3. 대여 이자
		borrowRatePct := baseUpdate.BorrowRatePerHour * 24.0 * 100.0

		margin := premiumPct + foreignFundingRatePct - baseFundingRatePct - borrowRatePct

		raw := RawOpportunity{
			BaseExchange:          p.BaseClient.GetName(),
			ForeignExchange:       p.ForeignClient.GetName(),
			BaseSymbol:            baseSym,
			ForeignSymbol:         foreignSym,
			BasePriceUSD:          baseUSD,
			BaseFuturePriceUSD:    baseFutureUSD,
			ForeignPriceUSD:       foreignUSD,
			ForeignFuturePriceUSD: foreignFutureUSD,
			BaseQuoteCurrency:     string(p.BaseMarketInfo.QuoteCurrency),
			PremiumPct:            premiumPct,
			BaseFundingRatePct:    baseFundingRatePct,
			ForeignFundingRatePct: foreignFundingRatePct,
			BorrowRatePct:         borrowRatePct,
			ExpectedMargin:        margin,
			Timestamp:             time.Now(),
		}

		results = append(results, raw)
	}

	// Sort globally by ExpectedMargin Descending
	sort.Slice(results, func(i, j int) bool {
		return results[i].ExpectedMargin > results[j].ExpectedMargin
	})

	// Publish to Redis
	if r.redisClient != nil {
		payload, err := json.Marshal(results)
		if err == nil {
			r.redisClient.PublishRawOpportunities(ctx, payload)
		} else {
			log.Printf("[GlobalRanker] Failed to marshal raw ops: %v", err)
		}
	}

	return results, nil
}
