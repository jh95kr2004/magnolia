/*
Package strategy
This file implements 'Strategy C: Foreign Transfer Arbitrage'.
- Condition: There is existing premium spread and optionally the future price will be maintained lower than spot price at the foreign exchange.
- Position: Base Margin Short (Borrow and sell on Base), Foreign Spot Long or Future Long
- Profit points: Deterministic premium spread, Funding rate (when future price is lower than spot price in the foreign exchange)
- Risk points: Liquidation, Borrow rate increase, Trading fees, Transfer fees
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

type ForeignTransferStrategy struct {
	lastEval StrategyEvaluation
}

func (s *ForeignTransferStrategy) ID() StrategyType {
	return StrategyForeignTransfer
}

func (s *ForeignTransferStrategy) Evaluate(baseSpot, foreignSpot exchange.PriceUpdate,
	params PremiumEvalParams) StrategyEvaluation {
	// Strategy C relies on borrowing from Base. Reject if unsupported.
	if !params.BaseSupportsMargin {
		s.lastEval = StrategyEvaluation{
			Type:        s.ID(),
			ErrorReason: "API does not support margin borrowing on base exchange",
		}
		return s.lastEval
	}

	totalBalance := params.BaseBalanceUsd + params.ForeignBalanceUsd
	if totalBalance > 0 && params.BalanceSkewThreshold > 0 {
		if params.ForeignBalanceUsd/totalBalance < params.BalanceSkewThreshold {
			s.lastEval = StrategyEvaluation{
				Type:        s.ID(),
				ErrorReason: fmt.Sprintf("foreign balance too low; skew protection active (foreign: %.2f, total: %.2f, skew: %.4f < threshold: %.2f)", params.ForeignBalanceUsd, totalBalance, params.ForeignBalanceUsd/totalBalance, params.BalanceSkewThreshold),
			}
			return s.lastEval
		}
	}

	entryPremium := (baseSpot.BidUSD - foreignSpot.AskUSD) / foreignSpot.AskUSD
	basisReturn := entryPremium - params.GetExpectedPremium()

	holdTimeHours := 24.0
	// Base exchange borrow interest
	borrowInterest := baseSpot.BorrowRatePerHour * holdTimeHours

	// Hedge Market Selection (SPOT vs FUTURE)
	hedgeMarket := "SPOT"
	spotHedgeFee := params.ForeignSpotBuyFeeRate + params.ForeignSpotSellFeeRate
	futureHedgeFee := params.ForeignFutureBuyFeeRate + params.ForeignFutureSellFeeRate
	expectedFundingCost := 0.0

	// Positive expectedFundingCost means we pay funding
	if len(params.PredictedBuyFundingRates) > 0 {
		expectedFundingCost = params.PredictedBuyFundingRates[0] * 3.0 // roughly 3 cycles
	}

	if (futureHedgeFee + expectedFundingCost) < spotHedgeFee {
		hedgeMarket = "FUTURE"
	}

	// Entry fees calculation based on hedge choice
	tradingFees := params.BaseSpotSellFeeRate
	if hedgeMarket == "FUTURE" {
		tradingFees += params.ForeignFutureBuyFeeRate
	} else {
		tradingFees += params.ForeignSpotBuyFeeRate
	}

	// Add Wallet Proxy Networking Fees if required
	networkFeeFixed := params.TransferFeeFixedUsd
	totalTransferTime := float64(params.EstTransferTime.Minutes())
	if params.NeedsWalletProxy {
		networkFeeFixed += params.WalletTransferFeeFixedUsd
		totalTransferTime += float64(params.WalletEstTransferTime.Minutes())
	}

	networkFeePercent := params.TransferFeeRate
	if params.InvestmentUsd > 0 {
		networkFeePercent += networkFeeFixed / params.InvestmentUsd
	}

	netROI := basisReturn - tradingFees - networkFeePercent - borrowInterest

	transferRisk := totalTransferTime
	liquidationRisk := computeLiquidationRisk(params.InvestmentUsd, params.BaseBalanceUsd)
	riskScore := transferRisk + liquidationRisk
	riskScore += (1.0 - params.PredictionConfidence) * 30.0
	riskScore = math.Min(100.0, riskScore)

	s.lastEval = StrategyEvaluation{
		Type:            s.ID(),
		EntryPremium:    entryPremium,
		BasisReturn:     basisReturn,
		TradingFees:     tradingFees,
		TransferFees:    networkFeePercent,
		BorrowInterest:  borrowInterest,
		ExpectedFunding: expectedFundingCost,
		EstTransferTime: params.EstTransferTime,
		NetROI:          netROI,
		RiskScore:       riskScore,
		FinalScore:      computeFinalScore(netROI, riskScore),
		InvestmentUSD:        params.InvestmentUsd,
		HedgeMarket:          hedgeMarket,
		BalanceSkewThreshold: params.BalanceSkewThreshold,
	}

	return s.lastEval
}

func (s *ForeignTransferStrategy) GetLastEvaluation() StrategyEvaluation {
	return s.lastEval
}

func (s *ForeignTransferStrategy) SetEvaluation(eval StrategyEvaluation) {
	s.lastEval = eval
}

func (s *ForeignTransferStrategy) ExecuteEntry(
	ctx context.Context,
	exec *execution.Executor,
	pair CrossMarketPair,
	walletProxy wallet.Client,
) (*postgres.ActivePosition, error) {

	// Live balance skew check to protect against AI bypass or race conditions
	baseBalances, err := pair.BaseClient.GetBalances(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch base balances for skew check: %w", err)
	}
	foreignBalances, err := pair.ForeignClient.GetBalances(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch foreign balances for skew check: %w", err)
	}

	baseQuote := pair.BaseMarketInfo.QuoteCurrency
	foreignQuote := pair.ForeignMarketInfo.QuoteCurrency

	var baseBal, foreignBal float64
	for _, b := range baseBalances {
		if b.Currency == baseQuote {
			baseBal = b.Available
			break
		}
	}
	for _, b := range foreignBalances {
		if b.Currency == foreignQuote {
			foreignBal = b.Available
			break
		}
	}

	baseBalanceUsd := baseBal / GetFXRate(string(baseQuote))
	foreignBalanceUsd := foreignBal / GetFXRate(string(foreignQuote))

	totalBalance := baseBalanceUsd + foreignBalanceUsd
	if totalBalance > 0 {
		skewThreshold := s.lastEval.BalanceSkewThreshold
		if skewThreshold <= 0 {
			skewThreshold = 0.15
		}
		if foreignBalanceUsd/totalBalance < skewThreshold {
			return nil, fmt.Errorf("foreign balance too low; skew protection active (foreign: %.2f, total: %.2f, skew: %.4f < threshold: %.2f)", foreignBalanceUsd, totalBalance, foreignBalanceUsd/totalBalance, skewThreshold)
		}
	}

	foreignAmount, err := GetAssetAmount(ctx, pair.ForeignClient, pair.ForeignMarketInfo, s.lastEval.InvestmentUSD)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate foreign amount: %w", err)
	}
	baseAmount := foreignAmount * (1.0 - s.lastEval.TransferFees)

	// Pre-flight balance checks BEFORE borrowing anything
	baseReq := exchange.OrderRequest{
		Symbol: pair.BaseMarketInfo.Symbol,
		Side:   exchange.SellOrder,
		Type:   exchange.MarketOrder,
		Price:  0,
		Amount: baseAmount,
	}

	hedgeType := exchange.MarketOrder
	if s.lastEval.HedgeMarket == "FUTURE" {
		hedgeType = exchange.FuturesLong
	}

	foreignReq := exchange.OrderRequest{
		Symbol: pair.ForeignMarketInfo.Symbol,
		Side:   exchange.BuyOrder,
		Type:   hedgeType,
		Price:  0,
		Amount: foreignAmount,
	}

	if err := exec.CheckPreflightBalance(ctx, pair.BaseClient, baseReq); err != nil {
		return nil, fmt.Errorf("pre-flight base balance check failed: %w", err)
	}
	if err := exec.CheckPreflightBalance(ctx, pair.ForeignClient, foreignReq); err != nil {
		return nil, fmt.Errorf("pre-flight foreign balance check failed: %w", err)
	}

	// 1. Borrow Asset on Base Exchange (Debt created here)
	borrowAction := &execution.TransferAction{
		Type:           execution.StepTypeBorrow,
		Description:    "Borrow Asset on Base Exchange",
		ExchangeClient: pair.BaseClient,
		Currency:       pair.BaseCurrency,
		Amount:         baseAmount,
	}

	err = exec.ExecutePipeline(ctx, []*execution.TransferAction{borrowAction})
	if err != nil {
		return nil, fmt.Errorf("failed to borrow on base: %w", err)
	}

	// 2. DualTrade: Sell on Base immediately (Hedge Short), Buy on Foreign immediately (Hedge Long)

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
		return nil, fmt.Errorf("dual trade entry failed (prices locked incorrectly): %w", err)
	}

	arrivingAmount := foreignRes.Amount * (1.0 - s.lastEval.TransferFees)

	// 3. If SPOT, Transfer to Base immediately. If FUTURE, just hold the position for funding.
	if s.lastEval.HedgeMarket == "SPOT" {
		var actions []*execution.TransferAction
		var destAddress string

		if walletProxy != nil {
			destAddress, _ = walletProxy.GetReceiveAddress(ctx, string(pair.BaseCurrency),
				string(pair.ForeignNetwork.Network))

			withdrawReq := exchange.WithdrawRequest{
				Currency: pair.BaseCurrency,
				Network:  exchange.Network(pair.ForeignNetwork.Network),
				Amount:   foreignRes.Amount,
				Address:  destAddress,
			}

			actions = append(actions, &execution.TransferAction{
				Type:           execution.StepTypeWithdraw,
				Description:    fmt.Sprintf("Withdraw to Personal Wallet %s", walletProxy.GetName()),
				ExchangeClient: pair.ForeignClient,
				WithdrawReq:    &withdrawReq,
			})

			baseDep, _ := pair.BaseClient.GetDepositAddress(ctx, pair.BaseCurrency,
				exchange.Network(pair.BaseNetwork.Network))
			walletReq := &wallet.TransferRequest{
				Currency:  string(pair.BaseCurrency),
				Amount:    arrivingAmount,
				Network:   string(pair.BaseNetwork.Network),
				ToAddress: baseDep.Address,
			}
			actions = append(actions, &execution.TransferAction{
				Type:         execution.StepTypeWalletTransfer,
				Description:  "Proxy Hop Wallet -> Base Exchange",
				WalletClient: walletProxy,
				WalletReq:    walletReq,
			})
		} else {
			baseDep, _ := pair.BaseClient.GetDepositAddress(ctx, pair.BaseCurrency,
				exchange.Network(pair.BaseNetwork.Network))
			destAddress = baseDep.Address

			withdrawReq := exchange.WithdrawRequest{
				Currency: pair.BaseCurrency,
				Network:  exchange.Network(pair.ForeignNetwork.Network),
				Amount:   foreignRes.Amount,
				Address:  destAddress,
			}

			actions = append(actions, &execution.TransferAction{
				Type:           execution.StepTypeWithdraw,
				Description:    "Direct Withdraw Foreign -> Base",
				ExchangeClient: pair.ForeignClient,
				WithdrawReq:    &withdrawReq,
			})
		}

		err = exec.ExecutePipeline(ctx, actions)
		if err != nil {
			return nil, fmt.Errorf("failed to complete transfer to base: %w", err)
		}
	}

	// At this point, positions are delta-neutral.
	// We've successfully transferred Spot to Base, or held Future on Foreign.
	// Debt is on Base.
	return &postgres.ActivePosition{
		InvestmentUSD:     s.lastEval.InvestmentUSD,
		Type:              string(s.ID()),
		HedgeMarket:       s.lastEval.HedgeMarket,
		BaseAmount:        arrivingAmount,
		ForeignAmount:     foreignRes.Amount,
		EntryBasePrice:    baseRes.Price,
		EntryForeignPrice: foreignRes.Price,
	}, nil
}

func (s *ForeignTransferStrategy) ExecuteExit(
	ctx context.Context,
	exec *execution.Executor,
	pair CrossMarketPair,
	pos postgres.ActivePosition,
	exitRatio float64,
	walletProxy wallet.Client,
) error {
	var actions []*execution.TransferAction
	exitAmt := pos.ForeignAmount * exitRatio
	arrivingAmount := pos.BaseAmount * exitRatio

	// Strategy C Exit Flow:
	// If HedgeMarket is FUTURE, we swap Future Long to Spot Long via DualTrade, then Transfer to Base
	if pos.HedgeMarket == "FUTURE" {
		closeFutureReq := exchange.OrderRequest{
			Symbol: pair.ForeignMarketInfo.Symbol,
			Side:   exchange.SellOrder, // Close Future Long
			Type:   exchange.FuturesLong,
			Price:  0,
			Amount: exitAmt,
		}

		buySpotReq := exchange.OrderRequest{
			Symbol: pair.ForeignMarketInfo.Symbol,
			Side:   exchange.BuyOrder,
			Type:   exchange.MarketOrder,
			Price:  0,
			Amount: exitAmt, // Assuming same symbol maps properly internally
		}

		// Execute DualTrade entirely inside Foreign Exchange to perfectly hedge during transition
		_, _, err := exec.ExecuteAtomicDualTrade(ctx, closeFutureReq, buySpotReq, pair.ForeignClient, pair.ForeignClient)
		if err != nil {
			return fmt.Errorf("failed dual trade to swap future to spot on foreign: %w", err)
		}

		var destAddress string
		if walletProxy != nil {
			destAddress, _ = walletProxy.GetReceiveAddress(ctx, string(pair.BaseCurrency),
				string(pair.ForeignNetwork.Network))

			withdrawReq := exchange.WithdrawRequest{
				Currency: pair.BaseCurrency,
				Network:  exchange.Network(pair.ForeignNetwork.Network),
				Amount:   exitAmt,
				Address:  destAddress,
			}

			actions = append(actions, &execution.TransferAction{
				Type:           execution.StepTypeWithdraw,
				Description:    fmt.Sprintf("Withdraw to Personal Wallet %s", walletProxy.GetName()),
				ExchangeClient: pair.ForeignClient,
				WithdrawReq:    &withdrawReq,
			})

			baseDep, _ := pair.BaseClient.GetDepositAddress(ctx, pair.BaseCurrency,
				exchange.Network(pair.BaseNetwork.Network))
			walletReq := &wallet.TransferRequest{
				Currency:  string(pair.BaseCurrency),
				Amount:    arrivingAmount,
				Network:   string(pair.BaseNetwork.Network),
				ToAddress: baseDep.Address,
			}
			actions = append(actions, &execution.TransferAction{
				Type:         execution.StepTypeWalletTransfer,
				Description:  "Proxy Hop Wallet -> Base Exchange",
				WalletClient: walletProxy,
				WalletReq:    walletReq,
			})
		} else {
			baseDep, _ := pair.BaseClient.GetDepositAddress(ctx, pair.BaseCurrency,
				exchange.Network(pair.BaseNetwork.Network))
			destAddress = baseDep.Address

			withdrawReq := exchange.WithdrawRequest{
				Currency: pair.BaseCurrency,
				Network:  exchange.Network(pair.ForeignNetwork.Network),
				Amount:   exitAmt,
				Address:  destAddress,
			}

			actions = append(actions, &execution.TransferAction{
				Type:           execution.StepTypeWithdraw,
				Description:    "Direct Withdraw Foreign -> Base",
				ExchangeClient: pair.ForeignClient,
				WithdrawReq:    &withdrawReq,
			})
		}

		// Pull the Spot across network to Base
		err = exec.ExecutePipeline(ctx, actions)
		if err != nil {
			return fmt.Errorf("failed pulling foreign spot back to base on exit: %w", err)
		}

		// Reset actions because we just executed the transfer pipeline
		actions = []*execution.TransferAction{}
	}

	// Always Repay Base Debt
	// Base has the coins (either from Entry SPOT transfer, or from Exit FUTURE transfer)
	actions = append(actions, &execution.TransferAction{
		Type:           execution.StepTypeRepay,
		Description:    "Repay Crypto Debt on Base",
		ExchangeClient: pair.BaseClient,
		Currency:       pair.BaseCurrency,
		Amount:         arrivingAmount,
	})

	err := exec.ExecutePipeline(ctx, actions)
	if err != nil {
		return fmt.Errorf("failed to process base debt repay exit: %w", err)
	}

	return nil
}

func (s *ForeignTransferStrategy) EvaluateScaling(
	pos postgres.ActivePosition,
	baseQuote, foreignQuote exchange.PriceUpdate,
	params PremiumEvalParams,
) bool {
	eval := s.Evaluate(baseQuote, foreignQuote, params)

	// If the premium gap continues to exist comfortably and risk isn't overwhelming,
	// We can loop the transfer (ONLY if Spot, as we must have coins physically arrived).
	if eval.NetROI > 0.02 && eval.RiskScore < 80.0 {
		if pos.HedgeMarket == "SPOT" {
			return true
		}
	}
	return false
}

func (s *ForeignTransferStrategy) ExecuteScale(
	ctx context.Context,
	exec *execution.Executor,
	pair CrossMarketPair,
	pos *postgres.ActivePosition,
	scaleUsd float64,
	walletProxy wallet.Client,
) error {
	foreignAmount, err := GetAssetAmount(ctx, pair.ForeignClient, pair.ForeignMarketInfo, scaleUsd)
	if err != nil {
		return fmt.Errorf("failed to calculate scale foreign amount: %w", err)
	}

	arrivingAmount := foreignAmount * (1.0 - s.lastEval.TransferFees)

	baseReq := exchange.OrderRequest{
		Symbol: pair.BaseMarketInfo.Symbol,
		Side:   exchange.SellOrder,
		Type:   exchange.MarketOrder,
		Price:  0,
		Amount: arrivingAmount,
	}

	foreignReq := exchange.OrderRequest{
		Symbol: pair.ForeignMarketInfo.Symbol,
		Side:   exchange.BuyOrder,
		Type:   exchange.MarketOrder,
		Price:  0,
		Amount: foreignAmount,
	}

	_, _, err = exec.ExecuteAtomicDualTrade(ctx, baseReq, foreignReq, pair.BaseClient, pair.ForeignClient)
	if err != nil {
		return fmt.Errorf("failed scale up dual trade (prices locked incorrectly): %w", err)
	}

	// 2. Transfer newly bought Spot on Foreign back to Base
	var actions []*execution.TransferAction
	var destAddress string

	if walletProxy != nil {
		destAddress, _ = walletProxy.GetReceiveAddress(ctx, string(pair.BaseCurrency),
			string(pair.ForeignNetwork.Network))

		withdrawReq := exchange.WithdrawRequest{
			Currency: pair.BaseCurrency,
			Network:  exchange.Network(pair.ForeignNetwork.Network),
			Amount:   foreignAmount,
			Address:  destAddress,
		}

		actions = append(actions, &execution.TransferAction{
			Type:           execution.StepTypeWithdraw,
			Description:    fmt.Sprintf("Withdraw Scale to %s", walletProxy.GetName()),
			ExchangeClient: pair.ForeignClient,
			WithdrawReq:    &withdrawReq,
		})

		baseDep, _ := pair.BaseClient.GetDepositAddress(ctx, pair.BaseCurrency,
			exchange.Network(pair.BaseNetwork.Network))
		walletReq := &wallet.TransferRequest{
			Currency:  string(pair.BaseCurrency),
			Amount:    arrivingAmount,
			Network:   string(pair.BaseNetwork.Network),
			ToAddress: baseDep.Address,
		}
		actions = append(actions, &execution.TransferAction{
			Type:         execution.StepTypeWalletTransfer,
			Description:  "Proxy Hop Scale to Base",
			WalletClient: walletProxy,
			WalletReq:    walletReq,
		})

	} else {
		baseDep, _ := pair.BaseClient.GetDepositAddress(ctx, pair.BaseCurrency,
			exchange.Network(pair.BaseNetwork.Network))
		destAddress = baseDep.Address

		withdrawReq := exchange.WithdrawRequest{
			Currency: pair.BaseCurrency,
			Network:  exchange.Network(pair.ForeignNetwork.Network),
			Amount:   foreignAmount,
			Address:  destAddress,
		}

		actions = append(actions, &execution.TransferAction{
			Type:           execution.StepTypeWithdraw,
			Description:    "Direct Withdraw Scale",
			ExchangeClient: pair.ForeignClient,
			WithdrawReq:    &withdrawReq,
		})
	}

	err = exec.ExecutePipeline(ctx, actions)
	if err != nil {
		return fmt.Errorf("failed to scale up via separate Spot loop transfer: %w", err)
	}

	// Note: We use DB specific AddPositionAmount in Engine loop, but update struct for local use
	pos.InvestmentUSD += scaleUsd
	pos.BaseAmount += arrivingAmount
	pos.ForeignAmount += foreignAmount

	return nil
}

// ExecuteHedgedRebalance performs a 100% Delta-Neutral capital rebalancing transfer
// from sourcePair.ForeignClient (e.g. Upbit KRW) to sourcePair.BaseClient (e.g. Coinbase USD)
// when foreign USD liquidity falls below the trigger threshold and premium is <= breakeven.
// - Case A (Destination supports Margin Borrow): Borrow & Sell on Dest + Spot Buy on Source -> Transfer -> Repay on Dest.
// - Case B (Destination supports Futures Short only): Futures Short on Dest + Spot Buy on Source -> Transfer -> Atomic(Spot Sell + Futures Short Cover) on Dest.
func (s *ForeignTransferStrategy) ExecuteHedgedRebalance(
	ctx context.Context,
	exec *execution.Executor,
	reversePair CrossMarketPair,
	rebalanceUSD float64,
	walletProxy wallet.Client,
) error {
	// In reversePair:
	// - BaseClient is the USD destination exchange (e.g. Coinbase) where we hedge Short and receive coins.
	// - ForeignClient is the KRW source exchange (e.g. Upbit) where we buy Spot and withdraw coins.
	sourceAmount, err := GetAssetAmount(ctx, reversePair.ForeignClient, reversePair.ForeignMarketInfo, rebalanceUSD)
	if err != nil {
		return fmt.Errorf("rebalance: failed to calculate source asset amount: %w", err)
	}
	arrivingAmount := sourceAmount * (1.0 - s.lastEval.TransferFees)

	useMarginBorrow := reversePair.BaseClient.SupportsMargin()

	var destEntryReq exchange.OrderRequest
	if useMarginBorrow {
		destEntryReq = exchange.OrderRequest{
			Symbol: reversePair.BaseMarketInfo.Symbol,
			Side:   exchange.SellOrder,
			Type:   exchange.MarketOrder,
			Price:  0,
			Amount: arrivingAmount,
		}
	} else {
		destEntryReq = exchange.OrderRequest{
			Symbol: reversePair.BaseMarketInfo.Symbol,
			Side:   exchange.SellOrder,
			Type:   exchange.FuturesShort,
			Price:  0,
			Amount: arrivingAmount,
		}
	}

	sourceBuyReq := exchange.OrderRequest{
		Symbol: reversePair.ForeignMarketInfo.Symbol,
		Side:   exchange.BuyOrder,
		Type:   exchange.MarketOrder,
		Price:  0,
		Amount: sourceAmount,
	}

	if useMarginBorrow {
		if err := reversePair.BaseClient.Borrow(ctx, reversePair.BaseCurrency, arrivingAmount); err != nil {
			return fmt.Errorf("rebalance: failed to borrow on destination exchange: %w", err)
		}
	}

	// Step 1: Atomic Dual Trade (Short on Destination + Spot Buy on Source) -> 100% Delta-Neutral locked
	_, sourceRes, err := exec.ExecuteAtomicDualTrade(ctx, destEntryReq, sourceBuyReq, reversePair.BaseClient, reversePair.ForeignClient)
	if err != nil {
		if useMarginBorrow {
			_ = reversePair.BaseClient.Repay(ctx, reversePair.BaseCurrency, arrivingAmount)
		}
		return fmt.Errorf("rebalance: failed delta-neutral entry dual trade: %w", err)
	}

	// Step 2: Transfer bought Spot from Source (Upbit) -> Destination (Coinbase)
	destDep, err := reversePair.BaseClient.GetDepositAddress(ctx, reversePair.BaseCurrency, exchange.Network(reversePair.BaseNetwork.Network))
	if err != nil {
		return fmt.Errorf("rebalance: failed to fetch destination deposit address: %w", err)
	}

	var transferActions []*execution.TransferAction
	if walletProxy != nil {
		proxyAddr, _ := walletProxy.GetReceiveAddress(ctx, string(reversePair.BaseCurrency), string(reversePair.ForeignNetwork.Network))
		transferActions = append(transferActions,
			&execution.TransferAction{
				Type:           execution.StepTypeWithdraw,
				Description:    "Rebalance Withdraw Source -> Wallet Proxy",
				ExchangeClient: reversePair.ForeignClient,
				WithdrawReq: &exchange.WithdrawRequest{
					Currency: reversePair.BaseCurrency,
					Network:  exchange.Network(reversePair.ForeignNetwork.Network),
					Amount:   sourceRes.Amount,
					Address:  proxyAddr,
				},
			},
			&execution.TransferAction{
				Type:         execution.StepTypeWalletTransfer,
				Description:  "Rebalance Hop Wallet Proxy -> Destination Exchange",
				WalletClient: walletProxy,
				WalletReq: &wallet.TransferRequest{
					Currency:  string(reversePair.BaseCurrency),
					Amount:    arrivingAmount,
					Network:   string(reversePair.BaseNetwork.Network),
					ToAddress: destDep.Address,
					ToTag:     destDep.Tag,
				},
			},
		)
	} else {
		transferActions = append(transferActions, &execution.TransferAction{
			Type:           execution.StepTypeWithdraw,
			Description:    "Rebalance Direct Withdraw Source -> Destination",
			ExchangeClient: reversePair.ForeignClient,
			WithdrawReq: &exchange.WithdrawRequest{
				Currency: reversePair.BaseCurrency,
				Network:  exchange.Network(reversePair.ForeignNetwork.Network),
				Amount:   sourceRes.Amount,
				Address:  destDep.Address,
				Tag:      destDep.Tag,
			},
		})
	}

	if err := exec.ExecutePipeline(ctx, transferActions); err != nil {
		return fmt.Errorf("rebalance: transfer pipeline failed: %w", err)
	}

	// Step 3: Settle on Destination Exchange
	if useMarginBorrow {
		// Simply repay the borrowed coin; USD proceeds were already credited in Step 1!
		repayAction := &execution.TransferAction{
			Type:           execution.StepTypeRepay,
			Description:    "Rebalance Repay Borrowed Coin on Destination",
			ExchangeClient: reversePair.BaseClient,
			Currency:       reversePair.BaseCurrency,
			Amount:         arrivingAmount,
		}
		if err := exec.ExecutePipeline(ctx, []*execution.TransferAction{repayAction}); err != nil {
			return fmt.Errorf("rebalance: failed to repay on destination: %w", err)
		}
	} else {
		// Futures Short Hedge mode: Simultaneously Sell arrived Spot for USD + Close Futures Short
		sellSpotReq := exchange.OrderRequest{
			Symbol: reversePair.BaseMarketInfo.Symbol,
			Side:   exchange.SellOrder,
			Type:   exchange.MarketOrder,
			Price:  0,
			Amount: arrivingAmount,
		}
		coverShortReq := exchange.OrderRequest{
			Symbol: reversePair.BaseMarketInfo.Symbol,
			Side:   exchange.BuyOrder,
			Type:   exchange.FuturesShort,
			Price:  0,
			Amount: arrivingAmount,
		}
		if _, _, err := exec.ExecuteAtomicDualTrade(ctx, sellSpotReq, coverShortReq, reversePair.BaseClient, reversePair.BaseClient); err != nil {
			return fmt.Errorf("rebalance: failed atomic spot sell + futures short cover on destination: %w", err)
		}
	}

	return nil
}

// ExecuteTransferAndRepay handles AI ActionType=1 (TRANSFER) for an existing open margin-borrowed position
// (Strategy A MarginSpot, Strategy B MarginFuture, or Strategy C ForeignTransfer in FUTURE hold mode).
// It converts Foreign Futures to Spot if necessary, withdraws Spot from Foreign to Base on-chain,
// and immediately repays the borrowed debt on Base upon arrival.
func (s *ForeignTransferStrategy) ExecuteTransferAndRepay(
	ctx context.Context,
	exec *execution.Executor,
	pair CrossMarketPair,
	pos postgres.ActivePosition,
	walletProxy wallet.Client,
) error {
	if !pair.IsValidForTransfer {
		return fmt.Errorf("pair %s/%s is not valid for on-chain transfer", pair.BaseClient.GetName(), pair.ForeignClient.GetName())
	}

	transferAmt := pos.ForeignAmount
	repayAmt := pos.BaseAmount
	if transferAmt <= 0 {
		transferAmt = repayAmt
	}

	// 1. If holding Futures Long on Foreign (Strategy B or Strategy C FUTURE mode), atomically swap to Spot Long first
	if pos.HedgeMarket == "FUTURE" || pos.Type == string(StrategyMarginFuture) {
		closeFutureReq := exchange.OrderRequest{
			Symbol: pair.ForeignMarketInfo.Symbol,
			Side:   exchange.SellOrder,
			Type:   exchange.FuturesLong,
			Price:  0,
			Amount: transferAmt,
		}
		buySpotReq := exchange.OrderRequest{
			Symbol: pair.ForeignMarketInfo.Symbol,
			Side:   exchange.BuyOrder,
			Type:   exchange.MarketOrder,
			Price:  0,
			Amount: transferAmt,
		}
		if _, _, err := exec.ExecuteAtomicDualTrade(ctx, closeFutureReq, buySpotReq, pair.ForeignClient, pair.ForeignClient); err != nil {
			return fmt.Errorf("transfer-and-repay: failed to swap foreign futures to spot: %w", err)
		}
	}

	// 2. Withdraw Spot from Foreign -> Base (unless already transferred in Strategy C SPOT mode)
	if !(pos.Type == string(StrategyForeignTransfer) && pos.HedgeMarket == "SPOT") {
		baseDep, err := pair.BaseClient.GetDepositAddress(ctx, pair.BaseCurrency, exchange.Network(pair.BaseNetwork.Network))
		if err != nil {
			return fmt.Errorf("transfer-and-repay: failed to fetch base deposit address: %w", err)
		}

		var transferActions []*execution.TransferAction
		if walletProxy != nil {
			destAddr, _ := walletProxy.GetReceiveAddress(ctx, string(pair.BaseCurrency), string(pair.ForeignNetwork.Network))
			transferActions = append(transferActions,
				&execution.TransferAction{
					Type:           execution.StepTypeWithdraw,
					Description:    fmt.Sprintf("Withdraw Foreign Spot to Wallet Proxy %s", walletProxy.GetName()),
					ExchangeClient: pair.ForeignClient,
					WithdrawReq: &exchange.WithdrawRequest{
						Currency: pair.BaseCurrency,
						Network:  exchange.Network(pair.ForeignNetwork.Network),
						Amount:   transferAmt,
						Address:  destAddr,
					},
				},
				&execution.TransferAction{
					Type:         execution.StepTypeWalletTransfer,
					Description:  "Hop Wallet Proxy -> Base Exchange",
					WalletClient: walletProxy,
					WalletReq: &wallet.TransferRequest{
						Currency:  string(pair.BaseCurrency),
						Amount:    repayAmt,
						Network:   string(pair.BaseNetwork.Network),
						ToAddress: baseDep.Address,
						ToTag:     baseDep.Tag,
					},
				},
			)
		} else {
			transferActions = append(transferActions, &execution.TransferAction{
				Type:           execution.StepTypeWithdraw,
				Description:    "Direct Withdraw Foreign -> Base",
				ExchangeClient: pair.ForeignClient,
				WithdrawReq: &exchange.WithdrawRequest{
					Currency: pair.BaseCurrency,
					Network:  exchange.Network(pair.ForeignNetwork.Network),
					Amount:   transferAmt,
					Address:  baseDep.Address,
					Tag:      baseDep.Tag,
				},
			})
		}

		if err := exec.ExecutePipeline(ctx, transferActions); err != nil {
			return fmt.Errorf("transfer-and-repay: on-chain transfer failed: %w", err)
		}
	}

	// 3. Repay borrowed debt on Base
	repayAction := &execution.TransferAction{
		Type:           execution.StepTypeRepay,
		Description:    "Repay Crypto Debt on Base after Transfer",
		ExchangeClient: pair.BaseClient,
		Currency:       pair.BaseCurrency,
		Amount:         repayAmt,
	}
	if err := exec.ExecutePipeline(ctx, []*execution.TransferAction{repayAction}); err != nil {
		return fmt.Errorf("transfer-and-repay: base debt repayment failed: %w", err)
	}

	return nil
}
