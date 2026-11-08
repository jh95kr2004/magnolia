/*
Package strategy
This file implements 'Strategy D: Future-Spot Strategy'.
- Condition: The price at Base will become relatively lower.
- Position: Base Future Short, Foreign Spot Long
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

type FutureSpotStrategy struct {
	lastEval StrategyEvaluation
}

func (s *FutureSpotStrategy) ID() StrategyType {
	return StrategyFutureSpot
}

func (s *FutureSpotStrategy) Evaluate(
	baseSpot, foreignSpot exchange.PriceUpdate,
	params PremiumEvalParams,
) StrategyEvaluation {
	baseFutureBid := baseSpot.BidFutureUSD
	if baseFutureBid <= 0 {
		baseFutureBid = baseSpot.BidUSD
	}
	entryPremium := (baseFutureBid - foreignSpot.AskUSD) / foreignSpot.AskUSD
	basisReturn := entryPremium - params.GetExpectedPremium()

	roundTripTradingFees := params.BaseFutureSellFeeRate + params.BaseFutureBuyFeeRate +
		params.ForeignSpotBuyFeeRate + params.ForeignSpotSellFeeRate
	borrowInterest := 0.0 // No borrow interest since Base uses futures short and Foreign buys spot long

	expectedFunding := 0.0
	if len(params.PredictedSellFundingRates) > 0 {
		expectedFunding = params.PredictedSellFundingRates[0] * 3.0 // roughly 24 hours (3 8h cycles)
	}

	// Part 02 [07~09강] Guardrail: If entering primarily to harvest funding rates (basisReturn <= 0.005 and expectedFunding > 0),
	// enforce strict 2.0% Spot-Futures Basis cap so entry basis loss does not wipe out funding income.
	spotFutureGap := 0.0
	if baseSpot.AskUSD > 0 && baseFutureBid > 0 && baseSpot.AskUSD > baseFutureBid {
		spotFutureGap = (baseSpot.AskUSD - baseFutureBid) / baseSpot.AskUSD
	}
	if expectedFunding > 0 && basisReturn < 0.005 && spotFutureGap > MaxFundingHarvestBasisRatio {
		s.lastEval = StrategyEvaluation{
			Type:        s.ID(),
			ErrorReason: fmt.Sprintf("spot-futures basis (%.2f%%) exceeds 2.0%% funding harvest cap", spotFutureGap*100.0),
		}
		return s.lastEval
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

func (s *FutureSpotStrategy) GetLastEvaluation() StrategyEvaluation {
	return s.lastEval
}

func (s *FutureSpotStrategy) SetEvaluation(eval StrategyEvaluation) {
	s.lastEval = eval
}

func (s *FutureSpotStrategy) ExecuteEntry(
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
		Side:   exchange.BuyOrder, // Spot buy
		Type:   exchange.MarketOrder,
		Price:  0,
		Amount: foreignAmount,
	}

	baseRes, foreignRes, err := exec.ExecuteAtomicDualTrade(ctx, baseReq, foreignReq, pair.BaseClient, pair.ForeignClient)
	if err != nil {
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

func (s *FutureSpotStrategy) ExecuteExit(
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
		Side:   exchange.BuyOrder, // Buy back to cover futures short
		Type:   exchange.FuturesShort,
		Price:  0,
		Amount: exitBaseAmt,
	}

	foreignReq := exchange.OrderRequest{
		Symbol: pair.ForeignMarketInfo.Symbol,
		Side:   exchange.SellOrder, // Sell spot
		Type:   exchange.MarketOrder,
		Price:  0,
		Amount: exitForeignAmt,
	}

	_, _, err := exec.ExecuteAtomicDualTrade(ctx, baseReq, foreignReq, pair.BaseClient, pair.ForeignClient)
	return err
}

func (s *FutureSpotStrategy) EvaluateScaling(
	pos postgres.ActivePosition,
	baseQuote, foreignQuote exchange.PriceUpdate,
	params PremiumEvalParams,
) bool {
	return false
}

func (s *FutureSpotStrategy) ExecuteScale(
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
		Type:   exchange.MarketOrder,
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
