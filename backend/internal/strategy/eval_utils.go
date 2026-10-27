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
	"sync"
	"time"

	"github.com/magnolia/backend/internal/exchange"
)

var (
	fxRatesMu sync.RWMutex
	fxRates   = map[string]float64{
		"KRW":  1400.0,
		"USD":  1.0,
		"USDT": 1.0,
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
	BalanceSkewThreshold       float64       // Custom threshold to deter transfer looping (e.g. 0.15)
	BaseSupportsMargin         bool          // Whether the base exchange supports margin borrowing/shorting
	ForeignSupportsMargin      bool          // Whether the foreign exchange supports margin borrowing/shorting
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
	Type            StrategyType
	EntryPremium    float64
	BasisReturn     float64
	TradingFees     float64
	TransferFees    float64
	BorrowInterest  float64
	ExpectedFunding float64
	NetROI          float64
	RiskScore       float64
	FinalScore      float64
	EstTransferTime time.Duration
	InvestmentUSD   float64
	AISignal             string  // Phase 3 AI Decision Signal
	AIWeight             float64 // Phase 3 Dynamic Sizing Weight
	HedgeMarket          string  // "SPOT" or "FUTURE"
	BalanceSkewThreshold float64 // Configured skew threshold (e.g. 0.15)
	ErrorReason          string  // Reason for rejection, if any
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
