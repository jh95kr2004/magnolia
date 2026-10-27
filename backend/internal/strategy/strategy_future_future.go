/*
Package strategy
This file implements 'Strategy E: Future-Future Strategy'.
- Condition: The price at Base will become relatively lower.
- Position: Base Future Short, Foreign Future Long
- Profit points: Premium, Funding rate
- Risk points: Liquidation, Trading fees
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

type FutureFutureStrategy struct {
	lastEval StrategyEvaluation
}

func (s *FutureFutureStrategy) ID() StrategyType {
	return StrategyFutureFuture
}

func (s *FutureFutureStrategy) Evaluate(
	baseSpot, foreignSpot exchange.PriceUpdate,
	params PremiumEvalParams,
) StrategyEvaluation {
	baseFutureBid := baseSpot.BidFutureUSD
	if baseFutureBid <= 0 {
		baseFutureBid = baseSpot.BidUSD
	}
	foreignFutureAsk := foreignSpot.AskFutureUSD
	if foreignFutureAsk <= 0 {
		foreignFutureAsk = foreignSpot.AskUSD
	}

	entryPremium := (baseFutureBid - foreignFutureAsk) / foreignFutureAsk
	basisReturn := entryPremium - params.GetExpectedPremium()

	roundTripTradingFees := params.BaseFutureSellFeeRate + params.BaseFutureBuyFeeRate +
		params.ForeignFutureBuyFeeRate + params.ForeignFutureSellFeeRate

	expectedFunding := 0.0
	if len(params.PredictedSellFundingRates) > 0 {
		expectedFunding += params.PredictedSellFundingRates[0] * 3.0 // roughly 24 hours (3 8h cycles)
	}
	if len(params.PredictedBuyFundingRates) > 0 {
		expectedFunding -= params.PredictedBuyFundingRates[0] * 3.0
	}

	netROI := basisReturn - roundTripTradingFees + expectedFunding

	riskScore := computeLiquidationRisk(params.InvestmentUsd, params.ForeignBalanceUsd)
	riskScore += (1.0 - params.PredictionConfidence) * 30.0
	riskScore = math.Min(100.0, riskScore)

	s.lastEval = StrategyEvaluation{
		Type:            s.ID(),
		EntryPremium:    entryPremium,
		BasisReturn:     basisReturn,
		TradingFees:     roundTripTradingFees,
		TransferFees:    0,
		BorrowInterest:  0, // Futures don't have borrow interest
		ExpectedFunding: expectedFunding,
		NetROI:          netROI,
		RiskScore:       riskScore,
		FinalScore:      computeFinalScore(netROI, riskScore),
		InvestmentUSD:   params.InvestmentUsd,
	}

	return s.lastEval
}

func (s *FutureFutureStrategy) GetLastEvaluation() StrategyEvaluation {
	return s.lastEval
}

func (s *FutureFutureStrategy) SetEvaluation(eval StrategyEvaluation) {
	s.lastEval = eval
}

func (s *FutureFutureStrategy) ExecuteEntry(
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
		Side:   exchange.SellOrder,
		Type:   exchange.FuturesShort,
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

	baseRes, foreignRes, err := exec.ExecuteAtomicDualTrade(ctx, baseReq, foreignReq, pair.BaseClient, pair.ForeignClient)
	if err != nil {
		return nil, fmt.Errorf("failed to atomic entry %s: %w", s.ID(), err)
	}

	return &postgres.ActivePosition{
		InvestmentUSD: s.lastEval.InvestmentUSD,
		Type:          string(s.ID()),
		BaseAmount:    baseRes.Amount,
		ForeignAmount: foreignRes.Amount,
	}, nil
}

func (s *FutureFutureStrategy) ExecuteExit(
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
		Side:   exchange.BuyOrder,
		Type:   exchange.FuturesShort,
		Price:  0,
		Amount: exitBaseAmt,
	}

	foreignReq := exchange.OrderRequest{
		Symbol: pair.ForeignMarketInfo.Symbol,
		Side:   exchange.SellOrder,
		Type:   exchange.FuturesLong,
		Price:  0,
		Amount: exitForeignAmt,
	}

	_, _, err := exec.ExecuteAtomicDualTrade(ctx, baseReq, foreignReq, pair.BaseClient, pair.ForeignClient)
	return err
}

func (s *FutureFutureStrategy) EvaluateScaling(
	pos postgres.ActivePosition,
	baseQuote, foreignQuote exchange.PriceUpdate,
	params PremiumEvalParams,
) bool {
	return false
}

func (s *FutureFutureStrategy) ExecuteScale(
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
		Type:   exchange.FuturesShort,
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

	baseRes, foreignRes, err := exec.ExecuteAtomicDualTrade(ctx, baseReq, foreignReq, pair.BaseClient, pair.ForeignClient)
	if err != nil {
		return fmt.Errorf("failed scale entry %s: %w", s.ID(), err)
	}

	pos.InvestmentUSD += scaleUsd
	pos.BaseAmount += baseRes.Amount
	pos.ForeignAmount += foreignRes.Amount
	return nil
}
