/*
Package strategy
This file implements 'Strategy B: Margin-Future Strategy'.
- Condition: The price at Base will become relatively lower.
- Position: Base Margin Short, Foreign Future Long
- Profit points: Premium, Funding rate
- Risk points: Liquidation, Borrow rate increase, Trading fees
*/
package strategy

import (
	"context"
	"fmt"
	"math"

	"github.com/magnolia/backend/internal/db/postgres"
	"github.com/magnolia/backend/internal/exchange"
	"github.com/magnolia/backend/internal/execution"
	"github.com/magnolia/backend/internal/wallet"
)

type MarginFutureStrategy struct {
	lastEval StrategyEvaluation
}

func (s *MarginFutureStrategy) ID() StrategyType {
	return StrategyMarginFuture
}

func (s *MarginFutureStrategy) Evaluate(
	baseSpot, foreignSpot exchange.PriceUpdate,
	params PremiumEvalParams,
) StrategyEvaluation {
	entryPremium := (baseSpot.BidUSD - foreignSpot.AskFutureUSD) / foreignSpot.AskFutureUSD
	basisReturn := entryPremium - params.GetExpectedPremium()

	holdTimeHours := 24.0

	if !params.BaseSupportsMargin {
		s.lastEval = StrategyEvaluation{
			Type:        s.ID(),
			ErrorReason: "API does not support margin borrowing",
		}
		return s.lastEval
	}

	roundTripTradingFees := params.BaseSpotSellFeeRate + params.BaseSpotBuyFeeRate +
		params.ForeignFutureBuyFeeRate + params.ForeignFutureSellFeeRate
	borrowInterest := baseSpot.BorrowRatePerHour * holdTimeHours

	// Dynamic funding rate heuristic
	expectedFunding := 0.0
	if len(params.PredictedBuyFundingRates) > 0 {
		expectedFunding = params.PredictedBuyFundingRates[0] * 3.0 // roughly 24 hours (3 8h cycles)
	}

	netROI := basisReturn - roundTripTradingFees - borrowInterest + expectedFunding

	riskScore := computeLiquidationRisk(params.InvestmentUsd, params.BaseBalanceUsd)
	riskScore += (1.0 - params.PredictionConfidence) * 30.0
	riskScore = math.Min(100.0, riskScore)

	s.lastEval = StrategyEvaluation{
		Type:            s.ID(),
		EntryPremium:    entryPremium,
		BasisReturn:     basisReturn,
		TradingFees:     roundTripTradingFees,
		TransferFees:    0,
		BorrowInterest:  borrowInterest,
		ExpectedFunding: expectedFunding,
		NetROI:          netROI,
		RiskScore:       riskScore,
		FinalScore:      computeFinalScore(netROI, riskScore),
		InvestmentUSD:   params.InvestmentUsd,
	}

	return s.lastEval
}

func (s *MarginFutureStrategy) GetLastEvaluation() StrategyEvaluation {
	return s.lastEval
}

func (s *MarginFutureStrategy) SetEvaluation(eval StrategyEvaluation) {
	s.lastEval = eval
}

func (s *MarginFutureStrategy) ExecuteEntry(
	ctx context.Context,
	exec *execution.Executor,
	pair CrossMarketPair,
	walletProxy wallet.Client,
) (*postgres.ActivePosition, error) {
	baseAmount, err := GetAssetAmount(ctx, pair.BaseClient, pair.BaseMarketInfo, s.lastEval.InvestmentUSD)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate base amount: %w", err)
	}
	foreignAmount := baseAmount

	baseReq := exchange.OrderRequest{
		Symbol: pair.BaseMarketInfo.Symbol,
		Side:   exchange.SellOrder, // Borrowed, now short sell
		Type:   exchange.MarketOrder,
		Price:  0,
		Amount: baseAmount,
	}

	foreignReq := exchange.OrderRequest{
		Symbol: pair.ForeignMarketInfo.Symbol,
		Side:   exchange.BuyOrder,
		Type:   exchange.FuturesLong, // Future Long
		Price:  0,
		Amount: foreignAmount,
	}

	if err := exec.CheckPreflightBalance(ctx, pair.BaseClient, baseReq); err != nil {
		return nil, fmt.Errorf("pre-flight base balance check failed: %w", err)
	}
	if err := exec.CheckPreflightBalance(ctx, pair.ForeignClient, foreignReq); err != nil {
		return nil, fmt.Errorf("pre-flight foreign balance check failed: %w", err)
	}

	err = pair.BaseClient.Borrow(ctx, pair.BaseCurrency, baseAmount)
	if err != nil {
		return nil, fmt.Errorf("failed to borrow base asset %s: %w", s.ID(), err)
	}

	baseRes, foreignRes, err := exec.ExecuteAtomicDualTrade(ctx, baseReq, foreignReq, pair.BaseClient, pair.ForeignClient)
	if err != nil {
		// Rollback: Repay borrowed asset to avoid leaving debt on Base exchange
		repayAction := &execution.TransferAction{
			Type:           execution.StepTypeRepay,
			Description:    "Rollback Borrow on Base Exchange due to trade failure",
			ExchangeClient: pair.BaseClient,
			Currency:       pair.BaseCurrency,
			Amount:         baseAmount,
		}
		_ = exec.ExecutePipeline(ctx, []*execution.TransferAction{repayAction})
		return nil, fmt.Errorf("failed to atomic entry %s: %w", s.ID(), err)
	}

	return &postgres.ActivePosition{
		InvestmentUSD:     s.lastEval.InvestmentUSD,
		Type:              string(s.ID()),
		BaseAmount:        baseRes.Amount,
		ForeignAmount:     foreignRes.Amount,
		EntryBasePrice:    baseRes.Price,
		EntryForeignPrice: foreignRes.Price,
	}, nil
}

func (s *MarginFutureStrategy) ExecuteExit(
	ctx context.Context,
	exec *execution.Executor,
	pair CrossMarketPair,
	pos postgres.ActivePosition,
	exitRatio float64,
	walletProxy wallet.Client,
) error {
	exitBaseAmt := pos.BaseAmount * exitRatio
	exitForeignAmt := pos.ForeignAmount * exitRatio

	baseReq := exchange.OrderRequest{
		Symbol: pair.BaseMarketInfo.Symbol,
		Side:   exchange.BuyOrder, // Buy back to repay debt
		Type:   exchange.MarketOrder,
		Price:  0,
		Amount: exitBaseAmt,
	}

	foreignReq := exchange.OrderRequest{
		Symbol: pair.ForeignMarketInfo.Symbol,
		Side:   exchange.SellOrder, // Close Future Long
		Type:   exchange.FuturesLong,
		Price:  0,
		Amount: exitForeignAmt,
	}

	_, _, err := exec.ExecuteAtomicDualTrade(ctx, baseReq, foreignReq, pair.BaseClient, pair.ForeignClient)
	if err == nil {
		_ = pair.BaseClient.Repay(ctx, pair.BaseCurrency, exitBaseAmt)
	}
	return err
}

func (s *MarginFutureStrategy) EvaluateScaling(
	pos postgres.ActivePosition,
	baseQuote, foreignQuote exchange.PriceUpdate,
	params PremiumEvalParams,
) bool {
	return false
}

func (s *MarginFutureStrategy) ExecuteScale(
	ctx context.Context,
	exec *execution.Executor,
	pair CrossMarketPair,
	pos *postgres.ActivePosition,
	scaleUsd float64,
	walletProxy wallet.Client,
) error {
	baseAmount, err := GetAssetAmount(ctx, pair.BaseClient, pair.BaseMarketInfo, scaleUsd)
	if err != nil {
		return fmt.Errorf("failed to calculate scale base amount: %w", err)
	}
	foreignAmount := baseAmount

	baseReq := exchange.OrderRequest{
		Symbol: pair.BaseMarketInfo.Symbol,
		Side:   exchange.SellOrder,
		Type:   exchange.MarketOrder,
		Price:  0,
		Amount: baseAmount,
	}
	foreignReq := exchange.OrderRequest{
		Symbol: pair.ForeignMarketInfo.Symbol,
		Side:   exchange.BuyOrder,
		Type:   exchange.FuturesLong,
		Price:  0,
		Amount: foreignAmount,
	}

	if err := exec.CheckPreflightBalance(ctx, pair.BaseClient, baseReq); err != nil {
		return err
	}
	if err := exec.CheckPreflightBalance(ctx, pair.ForeignClient, foreignReq); err != nil {
		return err
	}
	if err := pair.BaseClient.Borrow(ctx, pair.BaseCurrency, baseAmount); err != nil {
		return fmt.Errorf("failed to borrow base asset for scale %s: %w", s.ID(), err)
	}

	baseRes, foreignRes, err := exec.ExecuteAtomicDualTrade(ctx, baseReq, foreignReq, pair.BaseClient, pair.ForeignClient)
	if err != nil {
		_ = pair.BaseClient.Repay(ctx, pair.BaseCurrency, baseAmount)
		return fmt.Errorf("failed scale entry %s: %w", s.ID(), err)
	}

	pos.InvestmentUSD += scaleUsd
	pos.BaseAmount += baseRes.Amount
	pos.ForeignAmount += foreignRes.Amount
	return nil
}
