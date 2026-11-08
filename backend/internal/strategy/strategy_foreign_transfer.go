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
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

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

func buildAssassinationRationale(
	params PremiumEvalParams,
	realKimpRatio float64,
	basisRatio float64,
	triggerOverride string,
) *AssassinationRationale {
	triggerType := triggerOverride
	if triggerType == "" {
		switch params.EventType {
		case "NEW_KRW_LISTING":
			triggerType = "LLM_EVENT_NEW_LISTING"
		case "INVESTMENT_WARNING", "WALLET_SUSPENSION":
			triggerType = "LLM_EVENT_WARNING_GADURI"
		case "VOLUME_SURGE":
			triggerType = "VOLUME_SURGE_BASIS_CHEAP"
		default:
			if params.StageAssassination {
				triggerType = "USER_MANUAL_STAGE"
			}
		}
	}
	if triggerType == "" {
		return nil
	}

	targetExitPct := params.TargetAssassinationKimpPct
	if targetExitPct <= 0 {
		targetExitPct = 4.5
	}
	realKimpPct := realKimpRatio * 100.0
	basisPct := basisRatio * 100.0

	var reasonKO, reasonEN string
	switch triggerType {
	case "LLM_EVENT_NEW_LISTING":
		titleInfo := params.EventTitle
		if titleInfo == "" {
			titleInfo = "신규 원화 마켓 상장 공지"
		}
		if params.PreStageListing {
			reasonKO = fmt.Sprintf("📢 [LLM 원상 공지 1차 선점] %s 포착 — 국내 입금 지갑 오픈 전 해외 100%% 헷징 + 개인지갑 물량 선점 (현선갭 %.2f%%, 목표 매도 김프 +%.1f%%)", titleInfo, basisPct, targetExitPct)
			reasonEN = fmt.Sprintf("📢 [LLM New Listing Pre-Stage] %s — Hedged 100%% overseas & pre-staged in personal wallet before domestic deposit opens (Basis %.2f%%, Target +%.1f%%)", titleInfo, basisPct, targetExitPct)
		} else {
			reasonKO = fmt.Sprintf("📢 [LLM 원상 공지 암살 대기] %s 포착 — 상장 빔 분출 대비 100%% 헷징 국내 선입금 완료 (현선갭 %.2f%%, 목표 매도 김프 +%.1f%%)", titleInfo, basisPct, targetExitPct)
			reasonEN = fmt.Sprintf("📢 [LLM New Listing Assassination] %s — Staged on domestic exchange with 100%% hedge awaiting listing surge (Basis %.2f%%, Target +%.1f%%)", titleInfo, basisPct, targetExitPct)
		}
	case "LLM_EVENT_WARNING_GADURI":
		titleInfo := params.EventTitle
		if titleInfo == "" {
			titleInfo = "투자유의/입출금 중단 공지"
		}
		reasonKO = fmt.Sprintf("⚠️ [LLM 가두리 공지 암살 대기] %s 포착 — 입금 차단 가두리 펌핑 선점 대기 (진입 실질김프 %+.2f%%, 현선갭 %.2f%%, 목표 +%.1f%%)", titleInfo, realKimpPct, basisPct, targetExitPct)
		reasonEN = fmt.Sprintf("⚠️ [LLM Gaduri Catalyst Stage] %s — Staged with 100%% hedge ahead of deposit suspension pump (Entry Kimp %+.2f%%, Basis %.2f%%, Target +%.1f%%)", titleInfo, realKimpPct, basisPct, targetExitPct)
	case "VOLUME_SURGE_BASIS_CHEAP":
		reasonKO = fmt.Sprintf("🔥 [거래량 급증 + 저비용 헷징 선점] 국내 거래량 급등 및 해외 현선갭(%.2f%%) 안정 구간 포착 — 100%% 헷징 선제 입금 대기 (목표 +%.1f%%)", basisPct, targetExitPct)
		reasonEN = fmt.Sprintf("🔥 [Volume Surge + Low Basis Stage] Domestic volume surge with tight foreign basis (%.2f%%) — Staged for instant exit at +%.1f%%", basisPct, targetExitPct)
	case "IN_TRANSIT_KIMP_DROP_DEFENSE":
		reasonKO = fmt.Sprintf("🛡️ [전송 중 김프 급락 방어] 보따리 전송 도중 실질 김프가 %+.2f%%로 급락하여 헐값 손절 대신 100%% 헷징 유지 암살 대기로 자동 전환 (반등 목표 +%.1f%%)", realKimpPct, targetExitPct)
		reasonEN = fmt.Sprintf("🛡️ [In-Transit Kimp Drop Defense] Real Kimp dropped to %+.2f%% during transfer; auto-switched to 100%% hedged Staged Assassination instead of selling at a loss (Target +%.1f%%)", realKimpPct, targetExitPct)
	default:
		reasonKO = fmt.Sprintf("🎯 [수동 암살 대기] 사용자 명령으로 100%% 델타 중립 헷징 선제 입금 대기 중 (진입 실질김프 %+.2f%%, 현선갭 %.2f%%, 목표 +%.1f%%)", realKimpPct, basisPct, targetExitPct)
		reasonEN = fmt.Sprintf("🎯 [Manual Staged Assassination] User-initiated 100%% delta-neutral staged position (Entry Kimp %+.2f%%, Basis %.2f%%, Target +%.1f%%)", realKimpPct, basisPct, targetExitPct)
	}

	return &AssassinationRationale{
		TriggerType:         triggerType,
		ReasonKO:            reasonKO,
		ReasonEN:            reasonEN,
		EventTitle:          params.EventTitle,
		EntryRealKimpPct:    realKimpPct,
		EntryBasisPct:       basisPct,
		TargetExitKimpPct:   targetExitPct,
		MaxHoldHours:        12.0,
		TimeoutRemainingMin: 720,
	}
}

func (s *ForeignTransferStrategy) Evaluate(baseSpot, foreignSpot exchange.PriceUpdate,
	params PremiumEvalParams) StrategyEvaluation {
	// Never enter on DELISTING (거래지원 종료)
	if params.EventType == "DELISTING" {
		s.lastEval = StrategyEvaluation{
			Type:        s.ID(),
			ErrorReason: "delisting announcement detected: entry vetoed to protect capital",
		}
		return s.lastEval
	}

	// 4-Stage Hedge Mode Selection (Part 02 [02~11강]):
	// 1st priority: BASE_BORROW (Borrow & Sell on Base + Buy on Foreign)
	// 2nd priority: FOREIGN_BORROW (Borrow & Sell on Foreign + Spot Buy on Foreign -> Transfer -> Sell on Base & Repay on Foreign)
	// 3rd priority: FOREIGN_FUTURES_HEDGE (Spot Buy on Foreign + Futures Short on Foreign -> Transfer -> Sell on Base & Cover Short on Foreign)
	// 4th priority: ONCHAIN_DEFI_BORROW (Borrow on Aave V3 / Venus / Morpho / Kamino with HF >= 1.30 -> Transfer -> Sell on Base)
	hasForeignFutures := params.ForeignSupportsFutures || foreignSpot.BidFutureUSD > 0 || foreignSpot.AskFutureUSD > 0
	hasDeFiBorrow := params.SupportsDeFiBorrow
	defiHourlyRate := params.DeFiBorrowRatePerHour
	if !hasDeFiBorrow && baseSpot.Symbol != "" {
		scanner := wallet.NewDeFiLendingScanner()
		if pools, err := scanner.DiscoverPools(context.Background(), string(baseSpot.Symbol), math.Max(1000.0, params.InvestmentUsd)); err == nil && len(pools) > 0 {
			hasDeFiBorrow = true
			defiHourlyRate = pools[0].BorrowRatePerHour
		}
	}

	hedgeMode := ""
	if params.BaseSupportsMargin && !params.StageAssassination && !params.PreStageListing {
		hedgeMode = "BASE_BORROW"
	} else if params.ForeignSupportsMargin && !params.StageAssassination && !params.PreStageListing {
		hedgeMode = "FOREIGN_BORROW"
	} else if hasForeignFutures {
		hedgeMode = "FOREIGN_FUTURES_HEDGE"
	} else if hasDeFiBorrow {
		hedgeMode = "ONCHAIN_DEFI_BORROW"
	} else if params.BaseSupportsMargin {
		hedgeMode = "BASE_BORROW"
	} else if params.ForeignSupportsMargin {
		hedgeMode = "FOREIGN_BORROW"
	} else {
		s.lastEval = StrategyEvaluation{
			Type:        s.ID(),
			ErrorReason: "API does not support margin borrowing on base/foreign exchange, no foreign futures market, and no on-chain DeFi lending pool available",
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

	if foreignSpot.AskUSD <= 0 {
		s.lastEval = StrategyEvaluation{
			Type:        s.ID(),
			ErrorReason: "foreign spot ask price is zero or negative",
		}
		return s.lastEval
	}

	entryPremium := (baseSpot.BidUSD - foreignSpot.AskUSD) / foreignSpot.AskUSD

	// Real Tether-adjusted Kimp (정품 실질 김프) for Strategy C transfer margin
	realPremium := entryPremium
	if math.Abs(params.TetherRealPremiumPct) > 1e-9 {
		realPremium = params.TetherRealPremiumPct
	} else {
		exName := baseSpot.Exchange
		if exName == "" {
			exName = params.BaseExchange
		}
		tetherKRW := params.TetherRateKRW
		if tetherKRW <= 0 && exName != "" {
			tetherKRW = GetExchangeTetherRate(exName, "KRW")
		}
		fiatKRW := params.FXRateKRW
		if fiatKRW <= 0 {
			fiatKRW = baseSpot.FXRate
		}
		if fiatKRW <= 0 {
			fiatKRW = GetFXRate("KRW")
		}
		if tetherKRW > 500 && fiatKRW > 500 && (baseSpot.QuoteCurrency == "KRW" || exName == "UPBIT" || exName == "BITHUMB" || params.TetherRateKRW > 500) {
			rawKRW := baseSpot.RawBid
			if rawKRW <= 0 {
				rawKRW = baseSpot.BidUSD * fiatKRW
			}
			realPremium = ((rawKRW / tetherKRW) - foreignSpot.AskUSD) / foreignSpot.AskUSD
		}
	}

	// Foreign Spot-Futures basis cost ((SpotAsk - FutureBid) / SpotAsk)
	foreignBasisRatio := params.ForeignBasisPct
	if math.Abs(foreignBasisRatio) < 1e-9 && foreignSpot.BidFutureUSD > 0 {
		foreignBasisRatio = (foreignSpot.AskUSD - foreignSpot.BidFutureUSD) / foreignSpot.AskUSD
	}
	foreignBasisCost := 0.0
	if hedgeMode == "FOREIGN_FUTURES_HEDGE" && foreignBasisRatio > 0 {
		foreignBasisCost = foreignBasisRatio
		// Hard guardrail from Part 02 [07강]: Never enter futures-hedged arbitrage if Spot-Futures backwardation basis exceeds 5.0%
		if foreignBasisCost > MaxArbitrageBasisRatio {
			s.lastEval = StrategyEvaluation{
				Type:        s.ID(),
				ErrorReason: fmt.Sprintf("foreign spot-futures basis exceeds 5.0%% arbitrage cap (%.2f%% > %.2f%%)", foreignBasisCost*100.0, MaxArbitrageBasisRatio*100.0),
			}
			return s.lastEval
		}
	}

	// Guardrail for Assassination / Pre-Staging: reject if foreign backwardation basis is too expensive (> 0.35%)
	if (params.StageAssassination || params.PreStageListing) && foreignBasisCost > 0.0035 {
		s.lastEval = StrategyEvaluation{
			Type:        s.ID(),
			ErrorReason: fmt.Sprintf("foreign futures backwardation basis too wide for assassination staging (%.2f%% > 0.35%%)", foreignBasisCost*100.0),
		}
		return s.lastEval
	}

	// Total transfer time (including 2-hop personal wallet relay if required by VASP Tier 3)
	totalTransferDuration := params.EstTransferTime
	networkFeeFixed := params.TransferFeeFixedUsd
	if params.NeedsWalletProxy {
		networkFeeFixed += params.WalletTransferFeeFixedUsd
		totalTransferDuration += params.WalletEstTransferTime
	}
	totalTransferMinutes := totalTransferDuration.Minutes()

	// Transfer delay premium decay (for FOREIGN_BORROW and FOREIGN_FUTURES_HEDGE where domestic sale happens AFTER arrival)
	transferDelayDecay := 0.0
	if (hedgeMode == "FOREIGN_BORROW" || hedgeMode == "FOREIGN_FUTURES_HEDGE") && !params.StageAssassination && !params.PreStageListing {
		predAfterArrival := PredictPremiumAfterTransfer(realPremium, params.PredictedPremiumTrajectory, totalTransferDuration)
		if predAfterArrival < realPremium {
			transferDelayDecay = realPremium - predAfterArrival
		}
	}

	var basisReturn float64
	if params.StageAssassination || params.PreStageListing {
		targetKimp := params.TargetAssassinationKimpPct / 100.0
		if targetKimp <= 0 {
			targetKimp = 0.045
		}
		basisReturn = targetKimp - math.Max(0, realPremium)
	} else if hedgeMode == "BASE_BORROW" {
		basisReturn = realPremium - params.GetExpectedPremium()
	} else {
		// Direct spread capture on arrival after deducting expected decay during transfer
		basisReturn = realPremium - transferDelayDecay
	}

	holdTimeHours := 24.0
	if hedgeMode != "BASE_BORROW" && !params.StageAssassination && !params.PreStageListing {
		holdTimeHours = math.Max(0.25, totalTransferMinutes/60.0)
	}
	borrowInterest := baseSpot.BorrowRatePerHour * holdTimeHours
	if hedgeMode == "FOREIGN_FUTURES_HEDGE" {
		borrowInterest = 0.0
	} else if hedgeMode == "ONCHAIN_DEFI_BORROW" {
		if defiHourlyRate <= 0 {
			defiHourlyRate = 0.05 / 8760.0
		}
		borrowInterest = defiHourlyRate * holdTimeHours
	}

	// Hedge Market Selection (SPOT vs FUTURE)
	hedgeMarket := "SPOT"
	spotHedgeFee := params.ForeignSpotBuyFeeRate + params.ForeignSpotSellFeeRate
	futureHedgeFee := params.ForeignFutureBuyFeeRate + params.ForeignFutureSellFeeRate
	expectedFundingCost := 0.0

	if len(params.PredictedBuyFundingRates) > 0 {
		expectedFundingCost = params.PredictedBuyFundingRates[0] * 3.0
	}

	if hedgeMode == "BASE_BORROW" && (futureHedgeFee+expectedFundingCost) < spotHedgeFee {
		hedgeMarket = "FUTURE"
	}

	// Entry & Exit trading fees calculation based on hedge mode
	tradingFees := params.BaseSpotSellFeeRate
	if hedgeMode == "FOREIGN_FUTURES_HEDGE" {
		// Foreign Spot Buy + Foreign Futures Short Entry + Foreign Futures Short Cover + Base Spot Sell
		tradingFees += params.ForeignSpotBuyFeeRate + params.ForeignFutureSellFeeRate + params.ForeignFutureBuyFeeRate
	} else if hedgeMarket == "FUTURE" {
		tradingFees += params.ForeignFutureBuyFeeRate
	} else {
		tradingFees += params.ForeignSpotBuyFeeRate
	}

	networkFeePercent := params.TransferFeeRate
	if params.InvestmentUsd > 0 {
		networkFeePercent += networkFeeFixed / params.InvestmentUsd
	}

	netROI := basisReturn - tradingFees - networkFeePercent - borrowInterest - foreignBasisCost

	transferRisk := totalTransferMinutes
	liquidationRisk := computeLiquidationRisk(params.InvestmentUsd, params.BaseBalanceUsd)
	riskScore := transferRisk + liquidationRisk
	riskScore += (1.0 - params.PredictionConfidence) * 30.0
	riskScore = math.Min(100.0, riskScore)

	rationale := buildAssassinationRationale(params, realPremium, foreignBasisRatio, "")

	s.lastEval = StrategyEvaluation{
		Type:                   s.ID(),
		EntryPremium:           entryPremium,
		FiatPremiumPct:         entryPremium,
		TetherRealPremium:      realPremium,
		TetherRealPremiumPct:   realPremium,
		ForeignBasisCost:       foreignBasisCost,
		TransferDelayDecay:     transferDelayDecay,
		BasisReturn:            basisReturn,
		TradingFees:            tradingFees,
		TransferFees:           networkFeePercent,
		BorrowInterest:         borrowInterest,
		ExpectedFunding:        expectedFundingCost,
		EstTransferTime:        totalTransferDuration,
		NetROI:                 netROI,
		RiskScore:              riskScore,
		FinalScore:             computeFinalScore(netROI, riskScore),
		InvestmentUSD:          params.InvestmentUsd,
		HedgeMarket:            hedgeMarket,
		HedgeMode:              hedgeMode,
		StageAssassination:     params.StageAssassination,
		PreStageListing:        params.PreStageListing,
		EventType:              params.EventType,
		EventTitle:             params.EventTitle,
		VASPTier:               params.VASPTier,
		NeedsWalletProxy:       params.NeedsWalletProxy,
		AssassinationRationale: rationale,
		BalanceSkewThreshold:   params.BalanceSkewThreshold,
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

	// 0. Pre-flight Personal Wallet Native Gas Check (when 2-hop walletProxy is used)
	// Prevents tokens from getting stuck in a personal wallet that lacks native gas (ETH/SOL/TRX).
	if walletProxy != nil {
		netToCheck := pair.BaseNetwork.Network
		if netToCheck == "" {
			netToCheck = pair.ForeignNetwork.Network
		}
		if netToCheck == "" {
			netToCheck = exchange.Network(pair.BaseCurrency)
		}
		gasToken, minGas := GetRequiredNativeGasToken(netToCheck)
		if err := exec.CheckPreflightWalletGas(ctx, walletProxy, netToCheck, gasToken, minGas); err != nil {
			return nil, fmt.Errorf("wallet proxy pre-flight gas check failed: %w", err)
		}
	}

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

	hedgeMode := s.lastEval.HedgeMode
	if hedgeMode == "" {
		if pair.BaseClient.SupportsMargin() && !s.lastEval.StageAssassination && !s.lastEval.PreStageListing {
			hedgeMode = "BASE_BORROW"
		} else {
			hedgeMode = "FOREIGN_FUTURES_HEDGE"
		}
	}

	// =========================================================================
	// CASE 1: FOREIGN_FUTURES_HEDGE or FOREIGN_BORROW (Including Assassination & Pre-Stage Listing)
	// =========================================================================
	if hedgeMode == "FOREIGN_FUTURES_HEDGE" || hedgeMode == "FOREIGN_BORROW" {
		buySpotReq := exchange.OrderRequest{
			Symbol: pair.ForeignMarketInfo.Symbol,
			Side:   exchange.BuyOrder,
			Type:   exchange.MarketOrder,
			Price:  0,
			Amount: foreignAmount,
		}
		shortType := exchange.FuturesShort
		if hedgeMode == "FOREIGN_BORROW" {
			shortType = exchange.MarketOrder
			if err := pair.ForeignClient.Borrow(ctx, pair.BaseCurrency, foreignAmount); err != nil {
				return nil, fmt.Errorf("failed to borrow on foreign exchange: %w", err)
			}
		}
		shortHedgeReq := exchange.OrderRequest{
			Symbol: pair.ForeignMarketInfo.Symbol,
			Side:   exchange.SellOrder,
			Type:   shortType,
			Price:  0,
			Amount: foreignAmount,
		}

		// Step 1: Atomic Dual Trade on Foreign Exchange (Spot Buy + 1x Short Hedge) -> 100% Delta Neutral!
		spotRes, shortRes, err := exec.ExecuteAtomicDualTrade(ctx, buySpotReq, shortHedgeReq, pair.ForeignClient, pair.ForeignClient)
		if err != nil {
			if hedgeMode == "FOREIGN_BORROW" {
				_ = pair.ForeignClient.Repay(ctx, pair.BaseCurrency, foreignAmount)
			}
			return nil, fmt.Errorf("foreign spot+short hedge entry failed: %w", err)
		}

		arrivingAmount := spotRes.Amount * (1.0 - s.lastEval.TransferFees)
		rationale := s.lastEval.AssassinationRationale
		if (s.lastEval.StageAssassination || s.lastEval.PreStageListing) && rationale == nil {
			rationale = buildAssassinationRationale(
				PremiumEvalParams{
					StageAssassination:         s.lastEval.StageAssassination,
					PreStageListing:            s.lastEval.PreStageListing,
					TargetAssassinationKimpPct: 4.5,
				},
				s.lastEval.TetherRealPremium,
				s.lastEval.ForeignBasisCost,
				"",
			)
		}

		// Stage-1 Pre-Listing Mode: Domestic deposit wallet is NOT open yet -> withdraw to personal wallet (walletProxy) and hold!
		if s.lastEval.PreStageListing && walletProxy != nil {
			destAddress, _ := walletProxy.GetReceiveAddress(ctx, string(pair.BaseCurrency), string(pair.ForeignNetwork.Network))
			withdrawAction := &execution.TransferAction{
				Type:           execution.StepTypeWithdraw,
				Description:    fmt.Sprintf("Stage-1 Pre-Listing Withdraw to Personal Wallet %s", walletProxy.GetName()),
				ExchangeClient: pair.ForeignClient,
				WithdrawReq: &exchange.WithdrawRequest{
					Currency: pair.BaseCurrency,
					Network:  exchange.Network(pair.ForeignNetwork.Network),
					Amount:   spotRes.Amount,
					Address:  destAddress,
				},
			}
			if err := exec.ExecutePipeline(ctx, []*execution.TransferAction{withdrawAction}); err != nil {
				return nil, fmt.Errorf("pre-staged listing withdraw to personal wallet failed: %w", err)
			}
			rationaleJSON := ""
			if rationale != nil {
				if b, err := json.Marshal(rationale); err == nil {
					rationaleJSON = string(b)
				}
			}
			return &postgres.ActivePosition{
				Status:                     "PRE_STAGED_LISTING",
				InvestmentUSD:              s.lastEval.InvestmentUSD,
				Type:                       string(s.ID()),
				HedgeMarket:                "FUTURE",
				HedgeMode:                  hedgeMode,
				BaseAmount:                 arrivingAmount,
				ForeignAmount:              spotRes.Amount,
				EntryBasePrice:             shortRes.Price,
				EntryForeignPrice:          spotRes.Price,
				EntryPremium:               s.lastEval.EntryPremium,
				AssassinationRationaleJSON: rationaleJSON,
			}, nil
		}

		// Step 2: Transfer Spot from Foreign -> Base (+ Await Deposit & Auto-Verify Travel Rule)
		var actions []*execution.TransferAction
		baseDep, _ := pair.BaseClient.GetDepositAddress(ctx, pair.BaseCurrency, exchange.Network(pair.BaseNetwork.Network))
		if walletProxy != nil {
			destAddress, _ := walletProxy.GetReceiveAddress(ctx, string(pair.BaseCurrency), string(pair.ForeignNetwork.Network))
			actions = append(actions,
				&execution.TransferAction{
					Type:           execution.StepTypeWithdraw,
					Description:    fmt.Sprintf("Withdraw to Personal Wallet %s", walletProxy.GetName()),
					ExchangeClient: pair.ForeignClient,
					WithdrawReq: &exchange.WithdrawRequest{
						Currency: pair.BaseCurrency,
						Network:  exchange.Network(pair.ForeignNetwork.Network),
						Amount:   spotRes.Amount,
						Address:  destAddress,
					},
				},
				&execution.TransferAction{
					Type:         execution.StepTypeWalletTransfer,
					Description:  "Proxy Hop Wallet -> Base Exchange",
					WalletClient: walletProxy,
					WalletReq: &wallet.TransferRequest{
						Currency:  string(pair.BaseCurrency),
						Amount:    arrivingAmount,
						Network:   string(pair.BaseNetwork.Network),
						ToAddress: baseDep.Address,
						ToTag:     baseDep.Tag,
					},
				},
			)
		} else {
			actions = append(actions, &execution.TransferAction{
				Type:           execution.StepTypeWithdraw,
				Description:    "Direct Withdraw Foreign -> Base",
				ExchangeClient: pair.ForeignClient,
				WithdrawReq: &exchange.WithdrawRequest{
					Currency: pair.BaseCurrency,
					Network:  exchange.Network(pair.ForeignNetwork.Network),
					Amount:   spotRes.Amount,
					Address:  baseDep.Address,
					Tag:      baseDep.Tag,
				},
			})
		}

		actions = append(actions, &execution.TransferAction{
			Type:           execution.StepTypeAwaitDeposit,
			Description:    "Await Base Exchange Deposit & Auto-Verify Travel Rule",
			ExchangeClient: pair.BaseClient,
			Currency:       pair.BaseCurrency,
			ExpectedAmount: arrivingAmount,
			FromExchange:   pair.ForeignClient.GetName(),
			DepositTimeout: 30 * time.Second,
		})

		err = exec.ExecutePipeline(ctx, actions)
		if err != nil {
			if errors.Is(err, execution.ErrTravelRuleManualRequired) {
				// Keep the 100% delta-neutral hedge intact and transition to AWAITING_TRAVEL_RULE
				return &postgres.ActivePosition{
					Status:            "AWAITING_TRAVEL_RULE",
					InvestmentUSD:     s.lastEval.InvestmentUSD,
					Type:              string(s.ID()),
					HedgeMarket:       "FUTURE",
					HedgeMode:         hedgeMode,
					BaseAmount:        arrivingAmount,
					ForeignAmount:     spotRes.Amount,
					EntryBasePrice:    shortRes.Price,
					EntryForeignPrice: spotRes.Price,
					EntryPremium:      s.lastEval.EntryPremium,
				}, nil
			}
			return nil, fmt.Errorf("failed transfer pipeline to base: %w", err)
		}

		// Step 3: If Staged Assassination Mode was requested, hold the arrived coin on Base with 100% hedge intact!
		if s.lastEval.StageAssassination {
			rationaleJSON := ""
			if rationale != nil {
				if b, err := json.Marshal(rationale); err == nil {
					rationaleJSON = string(b)
				}
			}
			return &postgres.ActivePosition{
				Status:                     "STAGED_ASSASSINATION",
				InvestmentUSD:              s.lastEval.InvestmentUSD,
				Type:                       string(s.ID()),
				HedgeMarket:                "FUTURE",
				HedgeMode:                  hedgeMode,
				BaseAmount:                 arrivingAmount,
				ForeignAmount:              spotRes.Amount,
				EntryBasePrice:             shortRes.Price,
				EntryForeignPrice:          spotRes.Price,
				EntryPremium:               s.lastEval.EntryPremium,
				AssassinationRationaleJSON: rationaleJSON,
			}, nil
		}

		// Step 4: Post-Arrival Kimp Drop Defense!
		// Check live Base price upon arrival. If Kimp dropped below breakeven during the 5~15m transfer,
		// do NOT market-sell at a loss; automatically transition to STAGED_ASSASSINATION!
		liveBasePrice, bErr := pair.BaseClient.GetTicker(ctx, pair.BaseMarketInfo.Symbol)
		liveForeignPrice, fErr := pair.ForeignClient.GetTicker(ctx, pair.ForeignMarketInfo.Symbol)
		if bErr == nil && fErr == nil && liveBasePrice > 0 && liveForeignPrice > 0 {
			tetherKRW := GetExchangeTetherRate(pair.BaseClient.GetName(), string(pair.BaseMarketInfo.QuoteCurrency))
			if tetherKRW <= 0 {
				tetherKRW = GetFXRate(string(pair.BaseMarketInfo.QuoteCurrency))
			}
			basePriceInForeign := liveBasePrice
			if pair.BaseMarketInfo.QuoteCurrency == "KRW" && tetherKRW > 0 {
				basePriceInForeign = liveBasePrice / tetherKRW
			}
			arrivalRealKimp := (basePriceInForeign - liveForeignPrice) / liveForeignPrice
			breakevenKimp := s.lastEval.TradingFees + s.lastEval.TransferFees + s.lastEval.ForeignBasisCost
			if arrivalRealKimp < breakevenKimp {
				defenseRationale := buildAssassinationRationale(
					PremiumEvalParams{
						TargetAssassinationKimpPct: math.Max(2.5, (breakevenKimp+0.015)*100.0),
					},
					arrivalRealKimp,
					s.lastEval.ForeignBasisCost,
					"IN_TRANSIT_KIMP_DROP_DEFENSE",
				)
				rationaleJSON := ""
				if defenseRationale != nil {
					if b, err := json.Marshal(defenseRationale); err == nil {
						rationaleJSON = string(b)
					}
				}
				return &postgres.ActivePosition{
					Status:                     "STAGED_ASSASSINATION",
					InvestmentUSD:              s.lastEval.InvestmentUSD,
					Type:                       string(s.ID()),
					HedgeMarket:                "FUTURE",
					HedgeMode:                  hedgeMode,
					BaseAmount:                 arrivingAmount,
					ForeignAmount:              spotRes.Amount,
					EntryBasePrice:             shortRes.Price,
					EntryForeignPrice:          spotRes.Price,
					EntryPremium:               arrivalRealKimp,
					AssassinationRationaleJSON: rationaleJSON,
				}, nil
			}
		}

		// Step 5: Kimp is still profitable upon arrival -> Immediately sell arrived Spot on Base + cover Short on Foreign!
		sellBaseReq := exchange.OrderRequest{
			Symbol: pair.BaseMarketInfo.Symbol,
			Side:   exchange.SellOrder,
			Type:   exchange.MarketOrder,
			Price:  0,
			Amount: arrivingAmount,
		}
		coverForeignReq := exchange.OrderRequest{
			Symbol: pair.ForeignMarketInfo.Symbol,
			Side:   exchange.BuyOrder,
			Type:   shortType,
			Price:  0,
			Amount: spotRes.Amount,
		}
		baseSaleRes, _, err := exec.ExecuteAtomicDualTrade(ctx, sellBaseReq, coverForeignReq, pair.BaseClient, pair.ForeignClient)
		if err != nil {
			return nil, fmt.Errorf("failed post-arrival atomic settlement on base/foreign: %w", err)
		}
		if hedgeMode == "FOREIGN_BORROW" {
			_ = pair.ForeignClient.Repay(ctx, pair.BaseCurrency, spotRes.Amount)
		}

		return &postgres.ActivePosition{
			Status:            "OPEN",
			InvestmentUSD:     s.lastEval.InvestmentUSD,
			Type:              string(s.ID()),
			HedgeMarket:       "SPOT",
			HedgeMode:         hedgeMode,
			BaseAmount:        arrivingAmount,
			ForeignAmount:     spotRes.Amount,
			EntryBasePrice:    baseSaleRes.Price,
			EntryForeignPrice: spotRes.Price,
			EntryPremium:      s.lastEval.EntryPremium,
		}, nil
	}

	// =========================================================================
	// CASE 2: BASE_BORROW (Classic Borrow & Sell on Base + Buy on Foreign -> Transfer -> Repay)
	// =========================================================================
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

	return &postgres.ActivePosition{
		InvestmentUSD:     s.lastEval.InvestmentUSD,
		Type:              string(s.ID()),
		HedgeMarket:       s.lastEval.HedgeMarket,
		HedgeMode:         "BASE_BORROW",
		BaseAmount:        arrivingAmount,
		ForeignAmount:     foreignRes.Amount,
		EntryBasePrice:    baseRes.Price,
		EntryForeignPrice: foreignRes.Price,
	}, nil
}

// CompletePreStagedListingToBase executes Stage-2 of a NEW_KRW_LISTING pre-staged position (`PRE_STAGED_LISTING`):
// once the domestic exchange opens deposits for the newly listed coin, it transfers the pre-staged tokens
// from the personal wallet (`walletProxy`) to the Base exchange and transitions the position to `STAGED_ASSASSINATION`.
func (s *ForeignTransferStrategy) CompletePreStagedListingToBase(
	ctx context.Context,
	exec *execution.Executor,
	pair CrossMarketPair,
	pos *postgres.ActivePosition,
	walletProxy wallet.Client,
) error {
	if walletProxy == nil {
		return fmt.Errorf("walletProxy is required to complete Stage-2 pre-staged listing transfer")
	}
	baseDep, err := pair.BaseClient.GetDepositAddress(ctx, pair.BaseCurrency, exchange.Network(pair.BaseNetwork.Network))
	if err != nil {
		return fmt.Errorf("failed to fetch base deposit address for Stage-2 listing transfer: %w", err)
	}
	actions := []*execution.TransferAction{
		{
			Type:         execution.StepTypeWalletTransfer,
			Description:  "Stage-2 Listing Hop: Personal Wallet -> Base Exchange",
			WalletClient: walletProxy,
			WalletReq: &wallet.TransferRequest{
				Currency:  string(pair.BaseCurrency),
				Amount:    pos.BaseAmount,
				Network:   string(pair.BaseNetwork.Network),
				ToAddress: baseDep.Address,
				ToTag:     baseDep.Tag,
			},
		},
		{
			Type:           execution.StepTypeAwaitDeposit,
			Description:    "Await Stage-2 Listing Deposit on Base Exchange",
			ExchangeClient: pair.BaseClient,
			Currency:       pair.BaseCurrency,
			ExpectedAmount: pos.BaseAmount,
			FromExchange:   walletProxy.GetName(),
			DepositTimeout: 30 * time.Second,
		},
	}
	if err := exec.ExecutePipeline(ctx, actions); err != nil {
		return fmt.Errorf("stage-2 pre-staged listing transfer failed: %w", err)
	}
	pos.Status = "STAGED_ASSASSINATION"
	return nil
}

// ExecuteAssassinationExit executes the 0-second instant exit (or in-place timeout unwind) for a
// `STAGED_ASSASSINATION` position: simultaneously sells the pre-deposited Spot on Base AND covers
// the Short hedge on Foreign without paying any additional blockchain transfer gas fees.
func (s *ForeignTransferStrategy) ExecuteAssassinationExit(
	ctx context.Context,
	exec *execution.Executor,
	pair CrossMarketPair,
	pos postgres.ActivePosition,
) (*exchange.OrderResponse, *exchange.OrderResponse, error) {
	sellBaseReq := exchange.OrderRequest{
		Symbol: pair.BaseMarketInfo.Symbol,
		Side:   exchange.SellOrder,
		Type:   exchange.MarketOrder,
		Price:  0,
		Amount: pos.BaseAmount,
	}

	coverType := exchange.FuturesShort
	if pos.HedgeMode == "FOREIGN_BORROW" {
		coverType = exchange.MarketOrder
	}
	coverForeignReq := exchange.OrderRequest{
		Symbol: pair.ForeignMarketInfo.Symbol,
		Side:   exchange.BuyOrder,
		Type:   coverType,
		Price:  0,
		Amount: pos.ForeignAmount,
	}

	baseRes, foreignRes, err := exec.ExecuteAtomicDualTrade(ctx, sellBaseReq, coverForeignReq, pair.BaseClient, pair.ForeignClient)
	if err != nil {
		return nil, nil, fmt.Errorf("assassination dual-leg exit failed: %w", err)
	}
	if pos.HedgeMode == "FOREIGN_BORROW" {
		_ = pair.ForeignClient.Repay(ctx, pair.BaseCurrency, pos.ForeignAmount)
	}
	return baseRes, foreignRes, nil
}

func (s *ForeignTransferStrategy) ExecuteExit(
	ctx context.Context,
	exec *execution.Executor,
	pair CrossMarketPair,
	pos postgres.ActivePosition,
	exitRatio float64,
	walletProxy wallet.Client,
) error {
	// If position is in STAGED_ASSASSINATION or uses FOREIGN_FUTURES_HEDGE / FOREIGN_BORROW,
	// unwind both legs in place (Domestic Spot Sell + Foreign Short Cover) without paying re-transfer gas fees!
	if pos.Status == "STAGED_ASSASSINATION" || pos.HedgeMode == "FOREIGN_FUTURES_HEDGE" || pos.HedgeMode == "FOREIGN_BORROW" {
		if pos.Status == "OPEN" && (pos.HedgeMode == "FOREIGN_FUTURES_HEDGE" || pos.HedgeMode == "FOREIGN_BORROW") && pos.HedgeMarket == "SPOT" {
			// Already settled on arrival
			return nil
		}
		scaledPos := pos
		if exitRatio > 0 && exitRatio < 1.0 {
			scaledPos.BaseAmount = pos.BaseAmount * exitRatio
			scaledPos.ForeignAmount = pos.ForeignAmount * exitRatio
		}
		_, _, err := s.ExecuteAssassinationExit(ctx, exec, pair, scaledPos)
		return err
	}

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
