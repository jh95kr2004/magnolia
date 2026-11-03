package strategy

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/magnolia/backend/internal/db/postgres"
	"github.com/magnolia/backend/internal/exchange"
)

// PersonalFilter applies user-specific rules (Wallet balances, API permissions, Risk limits, Max Capital) to raw market opportunities.
type PersonalFilter struct {
	userID          uint
	db              *postgres.DB
	baseExchange    exchange.Client
	foreignExchange exchange.Client
	strategies      []ArbitrageStrategy
	Pairs           []CrossMarketPair
}

func NewPersonalFilter(
	userID uint,
	db *postgres.DB,
	base, foreign exchange.Client,
	strats []ArbitrageStrategy,
	pairs []CrossMarketPair,
) *PersonalFilter {
	return &PersonalFilter{
		userID:          userID,
		db:              db,
		baseExchange:    base,
		foreignExchange: foreign,
		strategies:      strats,
		Pairs:           pairs,
	}
}

// EvaluateAndFilter takes raw opportunities and applies user-specific contextual constraints.
func (p *PersonalFilter) EvaluateAndFilter(
	ctx context.Context,
	rawOps []RawOpportunity,
) ([]RankedOpportunity, error) {
	// 1. Fetch User Threshold Configurations (Mock Strategy Name for prototype)
	threshold, err := p.db.LoadThresholds(p.userID, "PremiumStrategy")
	if err != nil {
		// Use Safe Fallbacks if none exists
		threshold = &postgres.UserThreshold{
			MinTargetProfit:        10.0,
			MinTargetProfitPercent: 1.0,
			CapitalLimitUsd:        10000.0,
			PerPositionLimitUsd:    2000.0,
			MaxRiskScore:           50.0,
			BalanceSkewThreshold:   0.15,
		}
	}

	skewThreshold := threshold.BalanceSkewThreshold
	if skewThreshold <= 0 {
		skewThreshold = 0.15
	}

	// 2. Fetch User's currently active positions to prevent Duplicate self-competition
	activePositions, err := p.db.GetActivePositionsByUser(p.userID)
	if err != nil {
		log.Printf(
			"[PersonalFilter:%d] Warning: failed to fetch active positions: %v",
			p.userID,
			err,
		)
		activePositions = []postgres.ActivePosition{}
	}

	// Track total capital in use, and sets of currently active traded coins
	currentUsedUsd := 0.0
	activeCoinMap := make(map[string]bool)
	for _, pos := range activePositions {
		currentUsedUsd += pos.InvestmentUSD
		activeCoinMap[pos.BaseSymbol] = true
	}

	// Map pairs locally for fast lookup
	pairMap := make(map[string]CrossMarketPair)
	for _, pair := range p.Pairs {
		key := fmt.Sprintf("%s_%s", pair.BaseMarketInfo.Symbol, pair.ForeignMarketInfo.Symbol)
		pairMap[key] = pair
	}

	basePerms, _ := p.baseExchange.GetAPIKeyPermissions(ctx)

	baseBalances, err := p.baseExchange.GetBalances(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch base balances: %w", err)
	}
	foreignBalances, err := p.foreignExchange.GetBalances(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch foreign balances: %w", err)
	}

	baseBalMap := make(map[exchange.Currency]float64)
	for _, bal := range baseBalances {
		baseBalMap[bal.Currency] = bal.Available
	}

	foreignBalMap := make(map[exchange.Currency]float64)
	for _, bal := range foreignBalances {
		foreignBalMap[bal.Currency] = bal.Available
	}

	var results []RankedOpportunity

	for _, raw := range rawOps {
		// Never open a new position on a coin announced for DELISTING (거래지원 종료)
		if raw.EventType == "DELISTING" {
			continue
		}

		// Allow assassination staging on catalyst events even if current spot spread is still low
		isCatalystEvent := raw.EventType == "NEW_KRW_LISTING" || raw.EventType == "INVESTMENT_WARNING" || raw.EventType == "WALLET_SUSPENSION"

		// Check Profit Threshold early drop (unless catalyst event is present for staging evaluation)
		if !isCatalystEvent && raw.ExpectedMargin < threshold.MinTargetProfitPercent {
			continue // Immediately drop below-threshold margins
		}

		key := fmt.Sprintf("%s_%s", raw.BaseSymbol, raw.ForeignSymbol)
		pair, exists := pairMap[key]
		if !exists {
			continue // No valid transfer route for this pair
		}

		// Prevent Self-Competition (Duplicate entry)
		if activeCoinMap[string(pair.BaseMarketInfo.Symbol)] {
			continue // Already trading this coin, skip
		}

		// Calculate available investment
		remainingCapital := threshold.CapitalLimitUsd - currentUsedUsd
		if remainingCapital <= 0 {
			break // System completely out of allocated capital, stop testing
		}

		aiSignal := "SCALE_IN"
		aiWeight := 1.0

		// Determine dynamic sizing based on AI confidence
		investmentUsd := remainingCapital * aiWeight
		if investmentUsd > threshold.PerPositionLimitUsd {
			investmentUsd = threshold.PerPositionLimitUsd
		}

		// Fetch quote currency balances and convert to USD using live FX rate
		baseQuote := pair.BaseMarketInfo.QuoteCurrency
		foreignQuote := pair.ForeignMarketInfo.QuoteCurrency

		baseBal := baseBalMap[baseQuote]
		foreignBal := foreignBalMap[foreignQuote]

		liveFX := raw.FXRateKRW
		if liveFX <= 500 {
			liveFX = GetFXRate("KRW")
		}
		if liveFX <= 500 {
			liveFX = 1385.0
		}

		baseBalanceUsd := baseBal
		if baseQuote == "KRW" {
			baseBalanceUsd = baseBal / liveFX
		}

		foreignBalanceUsd := foreignBal
		if foreignQuote == "KRW" {
			foreignBalanceUsd = foreignBal / liveFX
		}

		// Recreate precise spot updates for strict Strategy Evaluation
		baseSpotUpdate := exchange.PriceUpdate{
			Exchange:      p.baseExchange.GetName(),
			Symbol:        exchange.Symbol(raw.BaseSymbol),
			QuoteCurrency: baseQuote,
			RawBid:        raw.BasePriceUSD * liveFX,
			RawAsk:        raw.BasePriceUSD * liveFX,
			BidUSD:        raw.BasePriceUSD,
			BidFutureUSD:  raw.BaseFuturePriceUSD,
			AskUSD:        raw.BasePriceUSD,
			AskFutureUSD:  raw.BaseFuturePriceUSD,
			FXRate:        liveFX,
		}

		foreignSpotUpdate := exchange.PriceUpdate{
			Exchange:      p.foreignExchange.GetName(),
			Symbol:        exchange.Symbol(raw.ForeignSymbol),
			QuoteCurrency: foreignQuote,
			RawBid:        raw.ForeignPriceUSD,
			RawAsk:        raw.ForeignPriceUSD,
			BidUSD:        raw.ForeignPriceUSD,
			BidFutureUSD:  raw.ForeignFuturePriceUSD,
			AskUSD:        raw.ForeignPriceUSD,
			AskFutureUSD:  raw.ForeignFuturePriceUSD,
			FXRate:        1.0,
		}

		vaspTier := GetVASPTier(p.baseExchange.GetName(), p.foreignExchange.GetName())
		needsWalletProxy := (vaspTier == exchange.VASPTier3RiskAssessed) && (investmentUsd*liveFX >= 1000000.0)

		// Evaluation Parameters
		params := PremiumEvalParams{
			InvestmentUsd:              investmentUsd,
			BaseBalanceUsd:             baseBalanceUsd,
			ForeignBalanceUsd:          foreignBalanceUsd,
			BaseSupportsMargin:         p.baseExchange.SupportsMargin(),
			ForeignSupportsMargin:      p.foreignExchange.SupportsMargin(),
			PredictionConfidence:       0.8,
			AISignal:                   aiSignal,
			AIWeight:                   aiWeight,
			TransferFeeFixedUsd:        5.0,
			EstTransferTime:            5 * time.Minute,
			NeedsWalletProxy:           needsWalletProxy,
			WalletTransferFeeFixedUsd:  2.5,
			WalletEstTransferTime:      3 * time.Minute,
			BalanceSkewThreshold:       skewThreshold,
			TetherRealPremiumPct:       raw.RealCoinPremiumPct / 100.0,
			FiatPremiumPct:             raw.PremiumPct / 100.0,
			ForeignBasisPct:            raw.ForeignBasisPct / 100.0,
			VASPTier:                   vaspTier,
			StageAssassination:         isCatalystEvent && raw.RealCoinPremiumPct < 1.5,
			PreStageListing:            raw.EventType == "NEW_KRW_LISTING" && !raw.IsWalletOpen,
			EventType:                  raw.EventType,
			EventTitle:                 raw.EventTitle,
			TargetAssassinationKimpPct: 4.5,
		}

		var evals []StrategyEvaluation
		for _, strat := range p.strategies {
			eval := strat.Evaluate(baseSpotUpdate, foreignSpotUpdate, params)
			if eval.ErrorReason == "" {
				// Final Risk Score Validation
				if eval.RiskScore > threshold.MaxRiskScore {
					continue // Risk Discard
				}

				// Strict Profit Minimum Drop
				expectedProfit := eval.NetROI * eval.InvestmentUSD
				if expectedProfit < threshold.MinTargetProfit {
					continue
				}

				evals = append(evals, eval)
			}
		}

		if len(evals) > 0 {
			best := evals[0]
			for _, e := range evals {
				if e.FinalScore > best.FinalScore {
					best = e
				}
			}

			// Add API Withdrawal permission strict validations
			if !basePerms.CanWithdraw && best.Type == StrategyForeignTransfer {
				// Cannot withdraw, drop transfer-based strategies
				continue
			}

			results = append(results, RankedOpportunity{
				Pair:        pair,
				Evaluations: evals,
				BestEval:    best,
			})
		}
	}

	return results, nil
}
