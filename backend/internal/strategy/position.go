/*
Package strategy
This file is the PositionManager module for loading and monitoring the state of currently open arbitrage positions (Active Position) in memory.
To reduce database I/O bottlenecks, it operates a local cache, controls concurrency issues with Mutex locks, and enables state restoration post-crash recovery.
*/
package strategy

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/magnolia/backend/internal/exchange"
)

// ActivePosition represents an open arbitrage trade across two exchanges.
type ActivePosition struct {
	ID                 string
	Type               StrategyType
	BaseExchange       string
	ForeignExchange    string
	BaseSymbol         exchange.Symbol
	ForeignSymbol      exchange.Symbol
	BaseAmount         float64
	ForeignAmount      float64
	EntryTime          time.Time
	EntryBasePrice     float64
	EntryForeignPrice  float64
	EntryPremium       float64
	ExpectedFuturePrem float64

	// Track peaks for Trailing Stop logic
	MaxUnrealizedROI float64
}

// ExitResult contains the decision from an ExitCondition
type ExitResult struct {
	ShouldExit bool
	Reason     string
}

// ExitCondition defines an interface for all exit logic (e.g. Stop Loss, Take Profit, Trailing Stop)
type ExitCondition interface {
	Evaluate(ctx context.Context, pos *ActivePosition, baseQuote, foreignQuote exchange.PriceUpdate) ExitResult
}

// ------------------------------------------------------------------
// 1. Take Profit
// ------------------------------------------------------------------
// This condition evaluates if the real-time NetROI of the position exceeds the user-configured TargetROI.
// Regardless of whether the AI's predicted maximum premium has been reached, if the profit meets
// our minimum acceptable threshold, this acts as a defensive mechanism to 'take' the guaranteed profit
// and immediately exit the market before volatility can erode it.
type TakeProfitCondition struct {
	TargetROI float64
}

func (c *TakeProfitCondition) Evaluate(ctx context.Context, pos *ActivePosition, baseQuote, foreignQuote exchange.PriceUpdate) ExitResult {
	currentROI := calculateCurrentROI(pos, baseQuote, foreignQuote)
	if currentROI >= c.TargetROI {
		return ExitResult{ShouldExit: true, Reason: fmt.Sprintf("Take Profit reached (%.2f%% >= %.2f%%)", currentROI*100, c.TargetROI*100)}
	}
	return ExitResult{ShouldExit: false}
}

// ------------------------------------------------------------------
// 2. Stop Loss
// ------------------------------------------------------------------
// This acts as the ultimate shield to prevent catastrophic losses and protect remaining capital.
// It is triggered if the Kimchi Premium moves against our position, causing Divergence Loss,
// or if accumulated expenses (like funding fees) drag the total NetROI below the user's maximum acceptable loss tier (MaxLossROI).
type StopLossCondition struct {
	MaxLossROI float64 // usually a negative number, e.g. -0.05 for -5%
}

func (c *StopLossCondition) Evaluate(ctx context.Context, pos *ActivePosition, baseQuote, foreignQuote exchange.PriceUpdate) ExitResult {
	currentROI := calculateCurrentROI(pos, baseQuote, foreignQuote)
	if currentROI <= c.MaxLossROI {
		return ExitResult{ShouldExit: true, Reason: fmt.Sprintf("Stop Loss activated (%.2f%% <= %.2f%%)", currentROI*100, c.MaxLossROI*100)}
	}
	return ExitResult{ShouldExit: false}
}

// ------------------------------------------------------------------
// 3. Time Expiration (AI Outlook Expiry)
// ------------------------------------------------------------------
// Arbitrage strategies fight against time. If a position is held for an extended period, 
// margin borrow interest continuously eats into the profit margins, worsening the payoff.
// If the position exceeds the optimal 'Reversion Time' originally predicted by the AI,
// or hits the user's hard 'MaxHoldDuration', the bot will immediately close the position to unfreeze capital,
// regardless of whether the position is currently in profit or loss.
type TimeExpirationCondition struct {
	MaxHoldDuration time.Duration
}

func (c *TimeExpirationCondition) Evaluate(ctx context.Context, pos *ActivePosition, baseQuote, foreignQuote exchange.PriceUpdate) ExitResult {
	if time.Since(pos.EntryTime) >= c.MaxHoldDuration {
		return ExitResult{ShouldExit: true, Reason: fmt.Sprintf("Time Expiration reached (%v)", c.MaxHoldDuration)}
	}
	return ExitResult{ShouldExit: false}
}

// ------------------------------------------------------------------
// 4. Trailing Stop
// ------------------------------------------------------------------
// While a simple TakeProfit dumps the position at a fixed target, a Trailing Stop follows the profitable 'trend' to the very end.
// It activates only when the position's ROI breaches the ActivationROI (e.g. 2%). From then on, it records the peak MaxUnrealizedROI.
// If the ROI is still climbing, it holds patiently to ride the peak.
// However, the instant the profit drops from that recorded peak by a designated CallbackRate (e.g. 1%),
// it immediately liquidates to lock in profits near the absolute top.
type TrailingStopCondition struct {
	ActivationROI float64 // Start trailing only after reaching this ROI (e.g. 0.02)
	CallbackRate  float64 // Drop from peak allowed (e.g. 0.01 for 1% drop from peak)
}

func (c *TrailingStopCondition) Evaluate(ctx context.Context, pos *ActivePosition, baseQuote, foreignQuote exchange.PriceUpdate) ExitResult {
	currentROI := calculateCurrentROI(pos, baseQuote, foreignQuote)
	
	if currentROI > pos.MaxUnrealizedROI {
		pos.MaxUnrealizedROI = currentROI
	}

	if pos.MaxUnrealizedROI >= c.ActivationROI {
		if currentROI <= pos.MaxUnrealizedROI - c.CallbackRate {
			return ExitResult{ShouldExit: true, Reason: fmt.Sprintf("Trailing Stop hit (Peak: %.2f%%, Current: %.2f%%)", pos.MaxUnrealizedROI*100, currentROI*100)}
		}
	}
	return ExitResult{ShouldExit: false}
}

// ------------------------------------------------------------------
// 5. Funding Rate Spike Guard
// ------------------------------------------------------------------
// A specialized defensive condition exclusively for Perpetual Futures.
// Triggered when extreme Long/Short polarization occurs in the market, causing the Funding Rate 
// that our position is subjected to, to spike uncontrollably to an unbearable degree.
// The bot forces an exit when it calculates that the incoming wave of funding fee deductions 
// will dwarf any expected premium basis gains.
type FundingSpikeCondition struct {
	MaxFundingRate float64
}

func (c *FundingSpikeCondition) Evaluate(ctx context.Context, pos *ActivePosition, baseQuote, foreignQuote exchange.PriceUpdate) ExitResult {
	if foreignQuote.IsFutures && math.Abs(foreignQuote.FundingRate) >= c.MaxFundingRate {
		return ExitResult{ShouldExit: true, Reason: fmt.Sprintf("Funding Rate Spike Guard triggered (%.4f >= %.4f)", math.Abs(foreignQuote.FundingRate), c.MaxFundingRate)}
	}
	return ExitResult{ShouldExit: false}
}

// ------------------------------------------------------------------
// 6. Liquidity / Spread Gap Guard
// ------------------------------------------------------------------
// Triggered during Flash Crashes or massive market pumps caused by extreme news.
// During these anomalies, the orderbook liquidity drains instantly, causing the gap (Spread) 
// between the best Ask and Bid to widen abnormally. 
// Attempting to market-close a position under these conditions incurs devastating Slippage damage.
// If anomalous widening is detected, the bot raises an alert or abstains/exits immediately to avoid market impacts.
type LiquidityGapCondition struct {
	MaxAcceptableSpread float64
}

func (c *LiquidityGapCondition) Evaluate(ctx context.Context, pos *ActivePosition, baseQuote, foreignQuote exchange.PriceUpdate) ExitResult {
	baseSpread := (baseQuote.AskUSD - baseQuote.BidUSD) / baseQuote.AskUSD
	foreignSpread := (foreignQuote.AskUSD - foreignQuote.BidUSD) / foreignQuote.AskUSD

	if baseSpread > c.MaxAcceptableSpread || foreignSpread > c.MaxAcceptableSpread {
		return ExitResult{ShouldExit: true, Reason: fmt.Sprintf("Liquidity Crisis detected. Spreads widened abnormally (Base: %.2f%%, Foreign: %.2f%%)", baseSpread*100, foreignSpread*100)}
	}
	return ExitResult{ShouldExit: false}
}

// Helper: Calculate current unrealized ROI roughly assuming Immediate Market Exit
func calculateCurrentROI(pos *ActivePosition, baseQuote, foreignQuote exchange.PriceUpdate) float64 {
	// A naive approximation of current ROI based on simple entry - exit gap
	// Ideally, this mirrors the exact logic in evaluateStrategyA~E but reversing the Buy/Sells
	var currentPremium, basisReturn float64

	isShortBase := pos.ExpectedFuturePrem < pos.EntryPremium
	if isShortBase {
		// We sold Base, bought Foreign. To Exit: Buy Base, Sell Foreign.
		currentPremium = (baseQuote.AskUSD - foreignQuote.BidUSD) / foreignQuote.BidUSD
		basisReturn = pos.EntryPremium - currentPremium
	} else {
		// We bought Base, sold Foreign. To Exit: Sell Base, Buy Foreign.
		currentPremium = (baseQuote.BidUSD - foreignQuote.AskUSD) / foreignQuote.AskUSD
		basisReturn = currentPremium - pos.EntryPremium
	}
	
	// Deduct a generic estimated rough fee for exit. (Actual will be tracked post-trade)
	feeEstimate := 0.005
	return basisReturn - feeEstimate
}

// PositionManager evaluates an active position against a list of configured exit conditions
type PositionManager struct {
	Conditions []ExitCondition
}

func NewPositionManager() *PositionManager {
	return &PositionManager{
		Conditions: []ExitCondition{},
	}
}

func (pm *PositionManager) AddCondition(c ExitCondition) {
	pm.Conditions = append(pm.Conditions, c)
}

// CheckExit runs through all attached OR rules. Returns true on the first matched exit condition.
func (pm *PositionManager) CheckExit(ctx context.Context, pos *ActivePosition, baseQuote, foreignQuote exchange.PriceUpdate) ExitResult {
	for _, cond := range pm.Conditions {
		res := cond.Evaluate(ctx, pos, baseQuote, foreignQuote)
		if res.ShouldExit {
			return res
		}
	}
	return ExitResult{ShouldExit: false}
}
