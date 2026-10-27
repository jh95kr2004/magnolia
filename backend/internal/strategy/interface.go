/*
Package strategy
This file defines the polymorphic interface (ArbitrageStrategy) that all arbitrage trading strategies running in the engine must adhere to.
By applying the Strategy Pattern, it standardizes the Evaluate and Execute (Entry/Exit) pipelines identically across all strategy types, whether they are spot, margin, futures, or transfer-based.
*/
package strategy

import (
	"context"

	"github.com/magnolia/backend/internal/db/postgres"
	"github.com/magnolia/backend/internal/exchange"
	"github.com/magnolia/backend/internal/execution"
	"github.com/magnolia/backend/internal/wallet"
)

// ArbitrageStrategy represents a unified blueprint for a specific arbitrage scenario.
// Implementations of this interface (e.g. MarginSpotStrategy, ForeignTransferStrategy)
// encapsulate both their mathematical valuation logic and their exact API sequence for entry/exit.
type ArbitrageStrategy interface {
	// ID returns the unique identifier identifying this specific strategy instance.
	ID() StrategyType

	// Evaluate analyzes the given price quotes and parameters to determine expected ROI and Risk.
	// It stores the computed StrategyEvaluation internally as state for immediate execution later.
	Evaluate(
		baseQuote, foreignQuote exchange.PriceUpdate,
		params PremiumEvalParams,
	) StrategyEvaluation

	// GetLastEvaluation returns the internal cache of the most recent Evaluate() call.
	GetLastEvaluation() StrategyEvaluation

	// SetEvaluation manually injects an evaluation state
	SetEvaluation(eval StrategyEvaluation)

	// ExecuteEntry sequences the precise steps needed to open this specific arbitrage position.
	// It relies on the internal StrategyEvaluation state rather than having it passed in.
	ExecuteEntry(
		ctx context.Context,
		exec *execution.Executor,
		pair CrossMarketPair,
		walletProxy wallet.Client,
	) (*postgres.ActivePosition, error)

	// ExecuteExit sequences the precise steps needed to close and neutralize this arbitrage position.
	ExecuteExit(
		ctx context.Context,
		exec *execution.Executor,
		pair CrossMarketPair,
		pos postgres.ActivePosition,
		exitRatio float64,
		walletProxy wallet.Client,
	) error

	// EvaluateScaling checks if an existing position can be profitably expanded (scaled up).
	// This is particularly useful for Strategy C where persistent premiums allow repeated cycles without liquidation risks.
	EvaluateScaling(
		pos postgres.ActivePosition,
		baseQuote, foreignQuote exchange.PriceUpdate,
		params PremiumEvalParams,
	) bool

	// ExecuteScale safely increases the position size.
	ExecuteScale(
		ctx context.Context,
		exec *execution.Executor,
		pair CrossMarketPair,
		pos *postgres.ActivePosition,
		scaleUsd float64,
		walletProxy wallet.Client,
	) error
}
