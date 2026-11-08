/*
Package strategy
This file contains math and business logic utility functions used by strategies when calculating ROI or premium gaps.
It decouples complex reusable calculations such as handling fees between Base and Foreign currencies, exchange rate conversions, and deriving profit/loss ratios based on short/long position directions.
*/
package strategy

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/magnolia/backend/internal/exchange"
)

var (
	fxRatesMu sync.RWMutex
	fxRates   = map[string]float64{
		"KRW":  1385.0,
		"USD":  1.0,
		"USDT": 1.0,
	}
	tetherRates = map[string]float64{
		"UPBIT:KRW":   1400.0,
		"BITHUMB:KRW": 1400.0,
	}
)

// SetFXRate updates the shared FX rate cache for a quote currency (e.g. "KRW").
func SetFXRate(quote string, rate float64) {
	if rate <= 0 || quote == "" {
		return
	}
	fxRatesMu.Lock()
	fxRates[quote] = rate
	fxRatesMu.Unlock()
}

// GetFXRate retrieves the latest FX rate for converting USD to the given quote currency.
func GetFXRate(quote string) float64 {
	fxRatesMu.RLock()
	rate, ok := fxRates[quote]
	fxRatesMu.RUnlock()
	if !ok || rate <= 0 {
		return 1.0
	}
	return rate
}

// SetExchangeTetherRate updates the real-time USDT quote rate for a specific domestic exchange (e.g. "UPBIT", "KRW", 1425.0).
func SetExchangeTetherRate(exchangeName, quote string, rate float64) {
	if rate <= 0 || exchangeName == "" || quote == "" {
		return
	}
	key := fmt.Sprintf("%s:%s", exchangeName, quote)
	fxRatesMu.Lock()
	tetherRates[key] = rate
	fxRatesMu.Unlock()
}

// GetExchangeTetherRate retrieves the latest USDT price on a specific domestic exchange (falls back to fiat FX rate if unavailable).
func GetExchangeTetherRate(exchangeName, quote string) float64 {
	key := fmt.Sprintf("%s:%s", exchangeName, quote)
	fxRatesMu.RLock()
	rate, ok := tetherRates[key]
	fxRatesMu.RUnlock()
	if ok && rate > 0 {
		return rate
	}
	return GetFXRate(quote)
}

// GetAllExchangeTetherRatesSnapshot returns a copy of all cached domestic exchange USDT rates.
func GetAllExchangeTetherRatesSnapshot() map[string]float64 {
	fxRatesMu.RLock()
	defer fxRatesMu.RUnlock()
	out := make(map[string]float64, len(tetherRates))
	for k, v := range tetherRates {
		out[k] = v
	}
	return out
}

// GetVASPTier returns the Korean Travel Rule VASP classification for transfers between baseExchange and foreignExchange.
// Upbit vs Bithumb asymmetry (Part 02 [14강]):
// - Upbit classifies Coinbase/Kraken as Tier 3 ("위험평가 통과 해외 VASP"), blocking direct deposits >= 1M KRW and requiring a 2-hop personal wallet relay.
// - Bithumb supports direct 1-hop Travel Rule interoperability (Tier 2) with Coinbase and Kraken once the user's exchange address is whitelisted.
func GetVASPTier(baseExchange, foreignExchange string) exchange.VASPTier {
	baseEx := strings.ToUpper(strings.TrimSpace(baseExchange))
	forEx := strings.ToUpper(strings.TrimSpace(foreignExchange))

	switch forEx {
	case "BITHUMB", "COINONE", "KORBIT", "GOPAX", "UPBIT":
		return exchange.VASPTier1Solution
	case "BINANCE", "BYBIT", "OKX", "GATEIO", "BITGET", "HTX", "MEXC", "KUCOIN", "BINGX":
		return exchange.VASPTier2OwnerLinked
	case "COINBASE", "KRAKEN", "BITSTAMP":
		if baseEx == "BITHUMB" {
			return exchange.VASPTier2OwnerLinked
		}
		return exchange.VASPTier3RiskAssessed
	default:
		return exchange.VASPTier3RiskAssessed
	}
}

// GetRequiredNativeGasToken returns the native gas token currency and minimum safe balance required
// to relay an asset out of a personal wallet (walletProxy) on the given blockchain network.
func GetRequiredNativeGasToken(network exchange.Network) (exchange.Currency, float64) {
	netStr := string(network)
	switch netStr {
	case "ERC20", "ETH", "ETHEREUM":
		return "ETH", 0.0015
	case "ARB", "ARBITRUM", "BASE", "OP", "OPTIMISM":
		return "ETH", 0.0003
	case "TRC20", "TRX", "TRON":
		return "TRX", 25.0
	case "SOL", "SOLANA", "SPL":
		return "SOL", 0.01
	case "MATIC", "POLYGON":
		return "MATIC", 0.5
	case "BSC", "BEP20", "BNB":
		return "BNB", 0.002
	case "XRP", "RIPPLE":
		return "XRP", 1.0
	case "BTC", "BITCOIN":
		return "BTC", 0.00005
	default:
		return exchange.Currency(netStr), 0.001
	}
}

// PredictPremiumAfterTransfer estimates the expected premium ratio after `transferTime` has elapsed
// based on the AI's 1-minute step forecast trajectory.
func PredictPremiumAfterTransfer(currentPrem float64, trajectory []float64, transferTime time.Duration) float64 {
	if len(trajectory) == 0 || transferTime <= 0 {
		return currentPrem
	}
	mins := int(math.Round(transferTime.Minutes()))
	if mins <= 0 {
		mins = 1
	}
	idx := mins - 1
	if idx >= len(trajectory) {
		idx = len(trajectory) - 1
	}
	return trajectory[idx]
}

// AssassinationRationale captures the structured reason why a position entered or transitioned into
// STAGED_ASSASSINATION or PRE_STAGED_LISTING mode, surfaced directly on the Dashboard and Opportunities UI.
type AssassinationRationale struct {
	TriggerType        string  `json:"trigger_type"`          // LLM_EVENT_NEW_LISTING, LLM_EVENT_WARNING_GADURI, VOLUME_SURGE_BASIS_CHEAP, IN_TRANSIT_KIMP_DROP_DEFENSE, USER_MANUAL_STAGE
	ReasonKO           string  `json:"reason_ko"`             // Human-readable Korean explanation
	ReasonEN           string  `json:"reason_en"`             // Human-readable English explanation
	EventTitle         string  `json:"event_title,omitempty"` // Original exchange notice title if event-driven
	EntryRealKimpPct   float64 `json:"entry_real_kimp_pct"`   // Real Tether-adjusted Kimp % at staging
	EntryBasisPct      float64 `json:"entry_basis_pct"`       // Foreign Spot-Futures basis % at staging
	TargetExitKimpPct  float64 `json:"target_exit_kimp_pct"`  // Target Kimp % for instant 0-latency exit
	MaxHoldHours       float64 `json:"max_hold_hours"`        // Timeout hours before in-place unwind
	TimeoutRemainingMin int    `json:"timeout_remaining_min"` // Remaining minutes before timeout
}

// GetAssetAmount calculates the asset size equivalent to a USD investment,
// taking into account ticker price and quote currency conversion.
func GetAssetAmount(ctx context.Context, client exchange.Client, info exchange.MarketInfo, investmentUSD float64) (float64, error) {
	var price float64
	var err error
	if info.MarketType == "FUTURE" {
		price, err = client.GetFutureTicker(ctx, info.Symbol)
	} else {
		price, err = client.GetTicker(ctx, info.Symbol)
	}
	if err != nil {
		return 0, err
	}
	if price <= 0 {
		return 0, fmt.Errorf("ticker price is zero or negative: %f", price)
	}

	_, quoteCur := exchange.ParseSymbol(info.Symbol)
	fxRate := GetFXRate(string(quoteCur))

	// Amount = (InvestmentUSD * FXRate) / Price
	amount := (investmentUSD * fxRate) / price
	return amount, nil
}


type StrategyType string

const (
	StrategyMarginSpot      StrategyType = "MARGIN_SPOT"      // Strategy A
	StrategyMarginFuture    StrategyType = "MARGIN_FUTURE"    // Strategy B
	StrategyForeignTransfer StrategyType = "FOREIGN_TRANSFER" // Strategy C
	StrategyFutureSpot      StrategyType = "FUTURE_SPOT"      // Strategy D
	StrategyFutureFuture    StrategyType = "FUTURE_FUTURE"    // Strategy E
)

// PremiumEvalParams bundles parameters needed to evaluate an arbitrage opportunity.
type PremiumEvalParams struct {
	BaseSpotBuyFeeRate         float64       // Fee rate for buying spot on the base exchange
	BaseSpotSellFeeRate        float64       // Fee rate for selling spot on the base exchange
	BaseFutureBuyFeeRate       float64       // Fee rate for buying futures on the base exchange
	BaseFutureSellFeeRate      float64       // Fee rate for selling futures on the base exchange
	ForeignSpotBuyFeeRate      float64       // Fee rate for buying spot on the foreign exchange
	ForeignSpotSellFeeRate     float64       // Fee rate for selling spot on the foreign exchange
	ForeignFutureBuyFeeRate    float64       // Fee rate for buying futures on the foreign exchange
	ForeignFutureSellFeeRate   float64       // Fee rate for selling futures on the foreign exchange
	TransferFeeFixedUsd        float64       // Fixed network fee for crypto transfer in USD
	TransferFeeRate            float64       // Percentage fee for crypto transfer (if applicable)
	EstTransferTime            time.Duration // Estimated time required for network transfer
	NeedsWalletProxy           bool          // If true, transfer must bounce through a personal wallet (Travel Rule)
	WalletTransferFeeFixedUsd  float64       // Gas fee for personal wallet -> Base exchange
	WalletEstTransferTime      time.Duration // Time for personal wallet -> Base exchange
	PredictedSellFundingRates  []float64     // Array of predicted funding rates for short positions
	PredictedBuyFundingRates   []float64     // Array of predicted funding rates for long positions
	InvestmentUsd              float64       // The intended size of the arbitrage trade in USD
	BaseBalanceUsd             float64       // Available USD-equivalent balance on the base exchange
	ForeignBalanceUsd          float64       // Available USD-equivalent balance on the foreign exchange
	PredictedPremiumTrajectory []float64     // AI-predicted premium values over the next N periods
	PredictionConfidence       float64       // AI model's confidence in the prediction (0.0 to 1.0)
	AISignal                   string        // Phase 3: "HOLD", "SCALE_IN", "SCALE_OUT", "EXIT"
	AIWeight                   float64       // Phase 3: Suggested investment sizing scalar (0.0 to 1.0)
	TargetExposure             float64       // AI's designated structural direction (-1.0 to 1.0)
	BalanceSkewThreshold       float64            // Custom threshold to deter transfer looping (e.g. 0.15)
	BaseExchange               string             // Base exchange name (e.g. "UPBIT")
	ForeignExchange            string             // Foreign exchange name (e.g. "COINBASE")
	BaseSupportsMargin         bool               // Whether the base exchange supports margin borrowing/shorting
	ForeignSupportsMargin      bool               // Whether the foreign exchange supports margin borrowing/shorting
	ForeignSupportsFutures     bool               // Whether the foreign exchange supports futures hedging
	FXRateKRW                  float64            // Fiat USD-KRW exchange rate
	TetherRateKRW              float64            // Exchange-specific USDT-KRW rate
	TetherRealPremiumPct       float64            // Real Tether-adjusted Kimp ratio (e.g., 0.035 for +3.5%), used by Strategy C
	FiatPremiumPct             float64            // Nominal Fiat FX Kimp ratio (e.g., 0.042 for +4.2%), used by Strategy A/B/D/E
	ForeignBasisPct            float64            // Foreign Spot-Futures basis ratio ((SpotAsk - FutureBid) / SpotAsk)
	VASPTier                   exchange.VASPTier  // Korean Travel Rule VASP classification for the pair
	StageAssassination         bool               // If true, stage coin on Base exchange with 100% hedge and wait for Kimp spike
	PreStageListing            bool               // If true, pre-stage coin into personal wallet before domestic deposit opens
	EventType                  string             // NEW_KRW_LISTING, INVESTMENT_WARNING, DELISTING, WALLET_SUSPENSION, VOLUME_SURGE
	EventTitle                 string             // Announcement title if triggered by an event
	TargetAssassinationKimpPct float64            // Target Kimp % to trigger instant assassination sell (default 4.5%)
}

// GetExpectedPremium computes the mean expected premium from the AI trajectory array.
func (p *PremiumEvalParams) GetExpectedPremium() float64 {
	if len(p.PredictedPremiumTrajectory) == 0 {
		return 0.0 // Fallback if AI is offline
	}
	var sum float64
	for _, val := range p.PredictedPremiumTrajectory {
		sum += val
	}
	return sum / float64(len(p.PredictedPremiumTrajectory))
}

// StrategyEvaluation represents the result of calling Evaluate() on a Strategy.
type StrategyEvaluation struct {
	Type                   StrategyType
	EntryPremium           float64
	FiatPremiumPct         float64
	TetherRealPremium      float64
	TetherRealPremiumPct   float64
	ForeignBasisCost       float64
	TransferDelayDecay     float64
	BasisReturn            float64
	TradingFees            float64
	TransferFees           float64
	BorrowInterest         float64
	ExpectedFunding        float64
	NetROI                 float64
	RiskScore              float64
	FinalScore             float64
	EstTransferTime        time.Duration
	InvestmentUSD          float64
	AISignal               string                  // Phase 3 AI Decision Signal
	AIWeight               float64                 // Phase 3 Dynamic Sizing Weight
	HedgeMarket            string                  // "SPOT" or "FUTURE"
	HedgeMode              string                  // "BASE_BORROW", "FOREIGN_BORROW", or "FOREIGN_FUTURES_HEDGE"
	StageAssassination     bool                    // True if evaluated for staged assassination mode
	PreStageListing        bool                    // True if evaluated for Stage-1 pre-listing personal wallet staging
	EventType              string                  // Event type if catalyst-driven
	EventTitle             string                  // Event announcement title
	VASPTier               exchange.VASPTier       // Korean Travel Rule VASP classification
	NeedsWalletProxy       bool                    // True if 2-hop personal wallet relay is required
	AssassinationRationale *AssassinationRationale // Structured rationale for UI display
	BalanceSkewThreshold   float64                 // Configured skew threshold (e.g. 0.15)
	ErrorReason            string                  // Reason for rejection, if any
}

// RankedOpportunity represents an evaluated multi-coin strategy scenario
type RankedOpportunity struct {
	Pair        CrossMarketPair
	Evaluations []StrategyEvaluation
	BestEval    StrategyEvaluation
}

// computeLiquidationRisk calculates a sliding-scale risk score based on percentage utilization.
func computeLiquidationRisk(investmentUsd, availableBalanceUsd float64) float64 {
	if availableBalanceUsd <= 0.0 || investmentUsd <= 0.0 {
		return 50.0 // Extreme risk if lacking funds
	}
	utilizationRate := investmentUsd / availableBalanceUsd
	risk := 5.0 + (utilizationRate * 45.0)
	return math.Min(100.0, risk)
}

// computeFinalScore evaluates ROI versus Risk to produce a 1-100 standardized metric.
func computeFinalScore(roi, risk float64) float64 {
	roiFactor := math.Min(100.0, roi*1000.0)
	riskFactor := math.Min(100.0, risk) / 2.0
	final := 50.0 + roiFactor - riskFactor
	return math.Max(1.0, math.Min(100.0, final))
}
