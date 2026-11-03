package strategy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
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
	FXRateKRW             float64   `json:"fx_rate_krw"`
	TetherKRW             float64   `json:"tether_krw"`
	TetherPremiumPct      float64   `json:"tether_premium_pct"`
	PremiumPct            float64   `json:"premium_pct"`           // Nominal Fiat FX Kimp %
	RealCoinPremiumPct    float64   `json:"real_coin_premium_pct"` // Real Tether-adjusted Kimp %
	ForeignBasisPct       float64   `json:"foreign_basis_pct"`     // Foreign Spot-Futures basis %
	VASPTier              string    `json:"vasp_tier"`             // TIER1_SOLUTION, TIER2_OWNER_LINKED, TIER3_RISK_ASSESSED
	IsWalletOpen          bool      `json:"is_wallet_open"`
	MatchedNetwork        string    `json:"matched_network"`
	IsWarning             bool      `json:"is_warning"`
	IsCaution             bool      `json:"is_caution"`
	EventType             string    `json:"event_type,omitempty"`
	EventTitle            string    `json:"event_title,omitempty"`
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

	r.currentFXRates.Store("KRW", GetFXRate("KRW"))

	if rClient != nil {
		ctx := context.Background()
		if cachedFX, err := rClient.GetCachedFXRate(ctx, "USD", "KRW"); err == nil && cachedFX > 500 {
			r.currentFXRates.Store("KRW", cachedFX)
			SetFXRate("KRW", cachedFX)
		}
		go func() {
			fxCh := make(chan float64)
			go rClient.SubscribeFXRate(ctx, "USD", "KRW", fxCh)
			for rate := range fxCh {
				if rate > 500 {
					r.currentFXRates.Store("KRW", rate)
					SetFXRate("KRW", rate)
				}
			}
		}()
		go rClient.SubscribeExchangeTetherRates(ctx, func(exchangeName string, rate float64) {
			if rate > 500 {
				SetExchangeTetherRate(exchangeName, "KRW", rate)
			}
		})
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
					// Strategy C transfers Foreign -> Base (Foreign Withdraw + Base Deposit),
					// while reverse rebalancing transfers Base -> Foreign (Base Withdraw + Foreign Deposit).
					// Primary transfer readiness requires Foreign withdraw enabled AND Base deposit enabled,
					// plus matching network if network metadata is populated.
					isValidForTransfer := fm.IsWithdrawEnabled && bm.IsDepositEnabled
					if baseNet.Network != "" && foreignNet.Network != "" {
						isValidForTransfer = isValidForTransfer &&
							foreignNet.IsWithdrawEnable &&
							baseNet.IsDepositEnable &&
							baseNet.Network == foreignNet.Network
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

	krwRate := GetFXRate("KRW")
	if rate, ok := r.currentFXRates.Load("KRW"); ok {
		if rf, okCast := rate.(float64); okCast && rf > 500 {
			krwRate = rf
			SetFXRate("KRW", krwRate)
		}
	}

	// Sync live USDT-KRW ticks from priceCache if present
	for _, domEx := range []string{"UPBIT", "BITHUMB"} {
		if usdtVal, ok := priceCache.Load(domEx + "|USDT-KRW"); ok {
			if usdtUpd, okCast := usdtVal.(*exchange.PriceUpdate); okCast && usdtUpd.RawBid > 500 {
				SetExchangeTetherRate(domEx, "KRW", usdtUpd.RawBid)
			}
		}
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

		baseFX := 1.0
		if p.BaseMarketInfo.QuoteCurrency == "KRW" {
			baseFX = krwRate
		}
		foreignFX := 1.0
		if p.ForeignMarketInfo.QuoteCurrency == "KRW" {
			foreignFX = krwRate
		}

		// Retrieve normalized USD Prices. Preference for Bid/Ask order.
		baseUSD := baseUpdate.BidUSD
		if baseUpdate.RawBid > 0 && baseFX > 0 {
			baseUSD = baseUpdate.RawBid / baseFX
		}

		foreignUSD := foreignUpdate.AskUSD
		if foreignUpdate.RawAsk > 0 && foreignFX > 0 {
			foreignUSD = foreignUpdate.RawAsk / foreignFX
		}

		// Calculate future prices
		baseFutureUSD := baseUpdate.BidFutureUSD
		if baseFutureUSD == 0 && baseUpdate.RawFutureBid > 0 {
			baseFutureUSD = baseUpdate.RawFutureBid / baseFX
		}

		foreignFutureUSD := foreignUpdate.AskFutureUSD
		if foreignFutureUSD == 0 && foreignUpdate.RawFutureAsk > 0 {
			foreignFutureUSD = foreignUpdate.RawFutureAsk / foreignFX
		}

		if baseUSD <= 0 || foreignUSD <= 0 {
			continue
		}

		// Emit a single canonical direction per exchange pair so that both positive (+) and negative (-)
		// spreads stream continuously into the AI time-series buffer and allow exit/repay signals during reverse premium.
		// Canonical rule: Local fiat quote (e.g. KRW on UPBIT) is Base and USD quote (e.g. COINBASE) is Foreign;
		// if both share the same quote currency type, break ties lexicographically by exchange name.
		baseQuoteCur := string(p.BaseMarketInfo.QuoteCurrency)
		foreignQuoteCur := string(p.ForeignMarketInfo.QuoteCurrency)
		isCanonicalDirection := false
		if baseQuoteCur != "USD" && foreignQuoteCur == "USD" {
			isCanonicalDirection = true
		} else if baseQuoteCur == "USD" && foreignQuoteCur != "USD" {
			isCanonicalDirection = false
		} else {
			isCanonicalDirection = p.BaseClient.GetName() < p.ForeignClient.GetName()
		}
		if !isCanonicalDirection {
			continue
		}

		// 1. 명목 김치 프리미엄 (Fiat FX-based Premium %) & 정품 실질 김프 (Tether-based Real Premium %)
		premiumPct := ((baseUSD - foreignUSD) / foreignUSD) * 100.0

		tetherKRW := GetExchangeTetherRate(p.BaseClient.GetName(), "KRW")
		if tetherKRW <= 0 {
			tetherKRW = krwRate
		}
		tetherPremiumPct := ((tetherKRW - krwRate) / krwRate) * 100.0

		realCoinPremiumPct := premiumPct
		if baseQuoteCur == "KRW" && tetherKRW > 0 {
			rawKRW := baseUpdate.RawBid
			if rawKRW <= 0 {
				rawKRW = baseUSD * krwRate
			}
			baseInTether := rawKRW / tetherKRW
			realCoinPremiumPct = ((baseInTether - foreignUSD) / foreignUSD) * 100.0
		}

		// 해외 현선갭 (Foreign Spot-Futures Basis %: positive when Futures < Spot backwardation)
		foreignBasisPct := 0.0
		if foreignFutureUSD > 0 && foreignUSD > 0 {
			foreignBasisPct = ((foreignUSD - foreignFutureUSD) / foreignUSD) * 100.0
		}

		// 2. 펀딩비
		baseFundingRatePct := baseUpdate.FundingRate * 100.0
		foreignFundingRatePct := foreignUpdate.FundingRate * 100.0

		// 3. 대여 이자
		borrowRatePct := baseUpdate.BorrowRatePerHour * 24.0 * 100.0

		margin := math.Abs(premiumPct) + foreignFundingRatePct - baseFundingRatePct - borrowRatePct

		matchedNet := string(p.BaseNetwork.Network)
		if matchedNet == "" {
			matchedNet = string(p.BaseCurrency)
		}

		eventType := ""
		eventTitle := ""
		if p.BaseMarketInfo.IsWarning {
			eventType = "INVESTMENT_WARNING"
			eventTitle = fmt.Sprintf("[%s] 거래유의 종목 지정", p.BaseCurrency)
		} else if p.BaseMarketInfo.IsCaution {
			eventType = "VOLUME_SURGE"
			eventTitle = fmt.Sprintf("[%s] 주의 종목 (거래량/입금량 급등)", p.BaseCurrency)
		}
		if r.redisClient != nil {
			if evts := r.redisClient.GetCoinAnnouncementEvents(ctx, string(p.BaseCurrency)); len(evts) > 0 {
				latest := evts[0]
				if latest.EventType != "" && latest.EventType != "NONE" {
					eventType = latest.EventType
					eventTitle = latest.Title
				}
			}
		}

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
			FXRateKRW:             krwRate,
			TetherKRW:             tetherKRW,
			TetherPremiumPct:      tetherPremiumPct,
			PremiumPct:            premiumPct,
			RealCoinPremiumPct:    realCoinPremiumPct,
			ForeignBasisPct:       foreignBasisPct,
			VASPTier:              string(GetVASPTier(p.BaseClient.GetName(), p.ForeignClient.GetName())),
			IsWalletOpen:          p.IsValidForTransfer,
			MatchedNetwork:        matchedNet,
			IsWarning:             p.BaseMarketInfo.IsWarning,
			IsCaution:             p.BaseMarketInfo.IsCaution,
			EventType:             eventType,
			EventTitle:            eventTitle,
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
