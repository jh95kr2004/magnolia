/*
Package strategy
This file is the Core Engine of the arbitrage trading system running on each node.
It acts as the heart of the system by subscribing to real-time ticks via Redis or WebSocket, continuously monitoring liquidation conditions for held positions (RunPositionMonitor), and scanning for new entry points (via Ranker) to trigger the most favorable ArbitrageStrategy when no positions are held.
*/
package strategy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/magnolia/backend/internal/db/postgres"
	"github.com/magnolia/backend/internal/db/redis"
	"github.com/magnolia/backend/internal/exchange"
	"github.com/magnolia/backend/internal/execution"
	"github.com/magnolia/backend/internal/notification"
	"github.com/magnolia/backend/internal/wallet"
)

// Config maps directly from the Postgres database schema
type Config struct {
	MinTargetProfit        float64 // e.g. 0.015 (1.5%)
	MaxStopLoss            float64 // e.g. 0.05 (5%)
	MaxAcceptableSpread    float64
	CapitalLimitUsd        float64
	MaxRiskScore           float64 // Maximum allowed risk score (e.g. 50.0)
	RiskMultiplier         float64 // Configurable multiplier weight for transfer time
	RetryOnFailure         bool    // Attempt automated retries if dual-leg errors happen
	DynamicSwitchThreshold float64 // The minimum edge required to pivot between strategies
	RebalanceTriggerSkew   float64 // Trigger capital rebalancing when Foreign USD share drops <= this (e.g. 0.20)
	RebalanceTargetSkew    float64 // Stop rebalancing once Foreign USD share reaches this target (e.g. 0.50)
	RebalanceMaxPremium    float64 // Maximum premium allowed to execute reverse rebalancing (e.g. 0.0 for <= 0% discount)
}

// Engine struct remains as the core orchestrator that will pipe PriceUpdate streams into the Simulator.
type Engine struct {
	nodeID     string
	userID     uint
	db         *postgres.DB
	config     Config
	posManager *PositionManager
	executor   *execution.Executor
	clients    []exchange.Client

	// Handles continuous evaluation and ranking of strategies via data polling
	filters []*PersonalFilter

	strategies []ArbitrageStrategy

	// Redis PubSub client for subscribing to global opportunities
	redisClient *redis.PubSubClient

	// Draining State
	isDraining bool

	// Wallet Proxy
	walletProxy wallet.Client

	// Mutex to serialize position evaluations and opportunity processing
	mu sync.Mutex

	// Live unrealized PnL USD cache per "Exchange|Symbol" updated by RunPositionMonitor
	liveUnrealizedPnLs map[string]float64

	// Error logging throttle
	lastErrorLogTime   map[string]time.Time
	lastErrorLogTimeMu sync.Mutex
}

func (e *Engine) StartDraining() {
	e.isDraining = true
}

func NewEngine(
	nodeID string,
	userID uint,
	db *postgres.DB,
	rClient *redis.PubSubClient,
	strategyName string,
	clients []exchange.Client,
	walletProxy wallet.Client,
) *Engine {
	// Attempt to load strategy config dynamically from Database
	threshold, err := db.LoadThresholds(userID, strategyName)
	var cfg Config
	if err != nil {
		log.Printf("[WARNING] Could not load strategy configs from DB for User %d: %v. Falling back to defaults.",
			userID, err)
		cfg = Config{
			MinTargetProfit:        1.5,
			MaxStopLoss:            -5.0,
			MaxAcceptableSpread:    0.1,
			CapitalLimitUsd:        50000.0,
			MaxRiskScore:           80.0,
			RiskMultiplier:         1.0,
			RetryOnFailure:         true,
			DynamicSwitchThreshold: 0.001,
			RebalanceTriggerSkew:   0.20,
			RebalanceTargetSkew:    0.50,
			RebalanceMaxPremium:    0.0,
		}
	} else {
		cfg = Config{
			MinTargetProfit:        threshold.MinTargetProfit,
			MaxStopLoss:            threshold.MaxStopLoss,
			MaxAcceptableSpread:    threshold.MaxAcceptableSpread,
			CapitalLimitUsd:        threshold.CapitalLimitUsd,
			MaxRiskScore:           threshold.MaxRiskScore,
			RiskMultiplier:         threshold.RiskMultiplier,
			RetryOnFailure:         threshold.RetryOnFailure,
			DynamicSwitchThreshold: 0.001,
			RebalanceTriggerSkew:   0.20,
			RebalanceTargetSkew:    0.50,
			RebalanceMaxPremium:    0.0,
		}
	}

	pm := NewPositionManager()
	pm.AddCondition(&TakeProfitCondition{TargetROI: cfg.MinTargetProfit / 100.0})
	pm.AddCondition(&StopLossCondition{MaxLossROI: cfg.MaxStopLoss / 100.0})
	pm.AddCondition(&TimeExpirationCondition{MaxHoldDuration: 8 * time.Hour})
	pm.AddCondition(&TrailingStopCondition{ActivationROI: 0.02, CallbackRate: 0.005})
	pm.AddCondition(&LiquidityGapCondition{MaxAcceptableSpread: cfg.MaxAcceptableSpread})

	// Instantiate all available Arbitrage Strategies
	strats := []ArbitrageStrategy{
		&MarginSpotStrategy{},
		&MarginFutureStrategy{},
		&ForeignTransferStrategy{},
		&FutureSpotStrategy{},
		&FutureFutureStrategy{},
	}

	e := &Engine{
		nodeID:             nodeID,
		userID:             userID,
		db:                 db,
		config:             cfg,
		posManager:         pm,
		executor:           execution.NewExecutor(cfg.RetryOnFailure),
		clients:            clients,
		redisClient:        rClient,
		walletProxy:        walletProxy,
		liveUnrealizedPnLs: make(map[string]float64),
		lastErrorLogTime:   make(map[string]time.Time),
	}
	e.strategies = strats

	// Build directed pairs in both directions (i != j) to support bidirectional arbitrage and rebalancing
	for i := range clients {
		baseClient := clients[i]
		for j := range clients {
			if i == j {
				continue
			}
			foreignClient := clients[j]
			ranker := NewGlobalRawRanker(rClient)
			pairs, err := ranker.FindIntersections(context.Background(), baseClient, foreignClient)
			if err != nil {
				log.Printf("[Engine] Warning: Failed to calculate intersections between %s and %s: %v", baseClient.GetName(), foreignClient.GetName(), err)
				pairs = []CrossMarketPair{}
			}
			e.filters = append(
				e.filters,
				NewPersonalFilter(userID, db, baseClient, foreignClient, strats, pairs),
			)
		}
	}

	return e
}

// StartWebLendingWatchdogs spawns child goroutines bound to the per-user context (runUserEngine)
// for any exchange client that uses browser-based coin lending (e.g., Upbit, Bithumb).
func (e *Engine) StartWebLendingWatchdogs(ctx context.Context) {
	for _, client := range e.clients {
		wc, ok := client.(exchange.WebLendingCapable)
		if !ok || wc.LendingManager() == nil {
			continue
		}
		lendingMgr := wc.LendingManager()
		lendingMgr.SetCallbacks(exchange.SessionCallbacks{
			OnSessionLost: func(exchangeName string) {
				e.handleWebSessionLost(ctx, exchangeName)
			},
			OnSessionRestored: func(exchangeName string) {
				e.handleWebSessionRestored(ctx, exchangeName)
			},
		})
		go lendingMgr.StartWatchdog(ctx, e.userID)
	}
}

func (e *Engine) handleWebSessionLost(ctx context.Context, exchangeName string) {
	if e.redisClient != nil {
		e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(fmt.Sprintf(
			`{"action":"WEB_LENDING_SESSION_LOST","exchange":"%s"}`, exchangeName,
		)))
	}

	if e.db == nil {
		return
	}
	positions, err := e.db.GetActivePositionsByUser(e.userID)
	if err != nil {
		return
	}

	var borrowedPosIDs []string
	for _, pos := range positions {
		if pos.BaseExchange == exchangeName &&
			(pos.Type == string(StrategyMarginSpot) ||
				pos.Type == string(StrategyMarginFuture) ||
				pos.Type == string(StrategyForeignTransfer)) {
			borrowedPosIDs = append(borrowedPosIDs, pos.PositionID)
		}
	}

	if len(borrowedPosIDs) > 0 {
		log.Printf("🚨 [CRITICAL][User-%d] %s web lending session lost while holding active borrowed positions: %v",
			e.userID, exchangeName, borrowedPosIDs)
		e.sendNotification(ctx, notification.AlertLiquidation, fmt.Sprintf(
			"🚨 **[CRITICAL] %s 웹 코인 대여 세션 끊김 감지 (대여 포지션 보유 중!)**\n- **보유 대여 포지션**: `%s`\n- **조치 필요**: 상환(Repay)이 지연되지 않도록 즉시 대시보드 설정(noVNC)에서 %s 웹 로그인을 복구해 주세요! (신규 대여 진입은 즉시 자동 차단되었습니다.)",
			exchangeName, strings.Join(borrowedPosIDs, ", "), exchangeName,
		))
	} else {
		log.Printf("⚠️ [WebLending][User-%d] %s web session disconnected. Margin borrow strategies paused.", e.userID, exchangeName)
		e.sendNotification(ctx, notification.AlertSystemError, fmt.Sprintf(
			"⚠️ **%s 웹 대여 세션 만료 안내**\n- %s 웹 로그인 세션이 만료되어 코인 대여가 필요한 전략(A, B, C)의 신규 진입이 자동 차단되었습니다.\n- 설정 페이지(noVNC 브라우저)에서 재로그인하시면 자동 재개됩니다.",
			exchangeName, exchangeName,
		))
	}
}

func (e *Engine) handleWebSessionRestored(ctx context.Context, exchangeName string) {
	if e.redisClient != nil {
		e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(fmt.Sprintf(
			`{"action":"WEB_LENDING_SESSION_RESTORED","exchange":"%s"}`, exchangeName,
		)))
	}
	if e.db == nil {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	positions, err := e.db.GetActivePositionsByUser(e.userID)
	if err != nil {
		return
	}

	for _, pos := range positions {
		if pos.BaseExchange == exchangeName && pos.Status == "AWAITING_REPAY" {
			log.Printf("🔄 [Session Restored] Retrying pending repayment for Position %s on %s...", pos.PositionID, exchangeName)
			targetPair := e.findPair(pos.BaseExchange, pos.ForeignExchange, pos.BaseSymbol)
			targetStrategy := e.findStrategy(StrategyType(pos.Type))
			if targetPair != nil && targetStrategy != nil {
				if err := targetStrategy.ExecuteExit(ctx, e.executor, *targetPair, pos, 1.0, e.walletProxy); err == nil {
					_ = e.db.UpdatePositionStatus(pos.PositionID, "CLOSED")
					e.notifyPositionClosed(ctx, pos.PositionID)
				}
			}
		}
	}
}

func (e *Engine) findStrategy(id StrategyType) ArbitrageStrategy {
	for _, strat := range e.strategies {
		if strat.ID() == id {
			return strat
		}
	}
	return nil
}

// GetActivePositions loads all previously open positions assigned to THIS specific nodeID.
func (e *Engine) GetActivePositions() ([]postgres.ActivePosition, error) {
	return e.db.GetActivePositions(e.nodeID)
}

// ExecuteStrategy executes the specific entry workflow for the selected strategy.
func (e *Engine) ExecuteStrategy(
	ctx context.Context,
	strat ArbitrageStrategy,
	pair CrossMarketPair,
) (*postgres.ActivePosition, error) {
	eval := strat.GetLastEvaluation()
	posID := fmt.Sprintf("POS-%s-%s-%s-%06d",
		string(pair.BaseCurrency),
		string(eval.Type),
		time.Now().Format("20060102150405"),
		time.Now().Nanosecond() / 1000,
	)

	pos, err := strat.ExecuteEntry(ctx, e.executor, pair, e.walletProxy)
	if err != nil {
		return nil, fmt.Errorf("strategy execution failed: %w", err)
	}

	// Augment the returned position with overarching tracking data
	pos.PositionID = string(posID)
	pos.UserID = e.userID
	pos.MonitorNodeID = e.nodeID
	pos.Status = "OPEN"
	pos.BaseExchange = pair.BaseClient.GetName()
	pos.ForeignExchange = pair.ForeignClient.GetName()
	pos.BaseSymbol = string(pair.BaseMarketInfo.Symbol)
	pos.ForeignSymbol = string(pair.ForeignMarketInfo.Symbol)
	pos.EntryPremium = eval.EntryPremium
	pos.ExpectedFuturePrem = eval.EntryPremium - eval.BasisReturn
	pos.MaxUnrealizedROI = 0.0
	pos.OpenedAt = time.Now()

	// 2. The trades succeeded (or were retried successfully). Record to DB.
	if err := e.db.SavePosition(pos); err != nil {
		return nil, err
	}

	// 3. Dispatch WebSocket event and send external notification
	e.notifyPositionOpened(ctx, pos)

	// 4. Auto-repay for Strategy C (Transfer Loop) in SPOT mode
	if pos.Type == string(StrategyForeignTransfer) && pos.HedgeMarket == "SPOT" {
		log.Printf("⚡ [Auto-Repay] Strategy C SPOT transfer completed. Automatically triggering repayment...")
		// Mark it as TRANSFERRING so the state machine reflects the transition
		_ = e.db.UpdatePositionStatus(pos.PositionID, "TRANSFERRING")
		e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(fmt.Sprintf(
			`{"action":"POSITION_TRANSFERRING", "position_id":"%s"}`, pos.PositionID,
		)))
		e.sendNotification(ctx, notification.AlertPositionOpened, fmt.Sprintf(
			"📦 **Coin Transfer Completed (Auto-Repay Started)**\n- **ID**: `%s`\n- **Exchanges**: `%s` -> `%s`",
			pos.PositionID, pos.ForeignExchange, pos.BaseExchange,
		))

		// Execute Exit (which for SPOT does the Repay and closes the debt)
		err := strat.ExecuteExit(ctx, e.executor, pair, *pos, 1.0, e.walletProxy)
		if err == nil {
			pos.Status = "CLOSED"
			_ = e.db.UpdatePositionStatus(pos.PositionID, "CLOSED")
			e.notifyPositionClosed(ctx, pos.PositionID)
		} else {
			if strings.Contains(err.Error(), "ERR_SESSION_EXPIRED") {
				pos.Status = "AWAITING_REPAY"
				_ = e.db.UpdatePositionStatus(pos.PositionID, "AWAITING_REPAY")
			}
			log.Printf("❌ [Auto-Repay Error] Failed to execute auto-repayment for %s: %v", pos.PositionID, err)
			e.sendNotification(ctx, notification.AlertLiquidation, fmt.Sprintf(
				"🚨 **[CRITICAL] Auto-Repay Error: Failed to repay debt for Position %s**\n- **Error**: %v\n- 웹 대여 세션이 만료된 경우 즉시 noVNC에서 재로그인하면 자동 상환됩니다.",
				pos.PositionID, err,
			))
		}
	}

	return pos, nil
}

// RunPositionMonitor starts a background loop that listens to real-time price ticks
// and periodic timer ticks to evaluate if any open positions should be closed.
func (e *Engine) RunPositionMonitor(ctx context.Context, priceCh <-chan exchange.PriceUpdate) {
	log.Printf("[Node-%s] Starting Dual-Trigger Position Monitor...", e.nodeID)

	// Start per-user web lending session watchdogs bound to this user's context
	e.StartWebLendingWatchdogs(ctx)

	// Cache to hold the latest seen prices per symbol, since positions need both Base and Foreign
	// quotes
	latestQuotes := make(map[string]exchange.PriceUpdate)

	ticker := time.NewTicker(2 * time.Second) // Fallback timer for Expiration checks
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("RunPositionMonitor gracefully shutting down.")
			return
		case quote := <-priceCh:
			if quote.FXRate > 0 && quote.QuoteCurrency != "" {
				SetFXRate(string(quote.QuoteCurrency), quote.FXRate)
			}
			latestQuotes[string(quote.Symbol)] = quote

			e.mu.Lock()
			e.evaluateAllOpenPositions(ctx, latestQuotes)
			if !e.isDraining {
				e.evaluateRebalancing(ctx, latestQuotes)
			}
			e.publishAccountState(ctx)
			e.mu.Unlock()
		case <-ticker.C:
			e.mu.Lock()
			e.evaluateAllOpenPositions(ctx, latestQuotes)
			if !e.isDraining {
				e.evaluateRebalancing(ctx, latestQuotes)
			}
			e.publishAccountState(ctx)
			e.mu.Unlock()
		}
	}
}

// evaluateAllOpenPositions fetches the active DB positions assigned to this user.
func (e *Engine) evaluateAllOpenPositions(
	ctx context.Context, quotes map[string]exchange.PriceUpdate) {
	positions, err := e.db.GetActivePositionsByUser(e.userID)
	if err != nil {
		log.Printf("[Monitor Error] Could not fetch positions for User %d: %v", e.userID, err)
		return
	}

	for _, pos := range positions {
		baseQuote, hasBase := quotes[pos.BaseSymbol]
		foreignQuote, hasForeign := quotes[pos.ForeignSymbol]

		// Skip evaluation until we have fresh data for both legs
		if !hasBase || !hasForeign {
			continue
		}

		// Because ActivePosition model differs slightly from memory struct, we map it temporarily
		// to the format PositionManager expects.
		memPos := &ActivePosition{
			ID:                 pos.PositionID,
			Type:               StrategyType(pos.Type),
			BaseExchange:       pos.BaseExchange,
			ForeignExchange:    pos.ForeignExchange,
			BaseSymbol:         exchange.Symbol(pos.BaseSymbol),
			ForeignSymbol:      exchange.Symbol(pos.ForeignSymbol),
			BaseAmount:         pos.BaseAmount,
			ForeignAmount:      pos.ForeignAmount,
			EntryTime:          pos.OpenedAt,
			EntryBasePrice:     pos.EntryBasePrice,
			EntryForeignPrice:  pos.EntryForeignPrice,
			EntryPremium:       pos.EntryPremium,
			ExpectedFuturePrem: pos.ExpectedFuturePrem,
			MaxUnrealizedROI:   pos.MaxUnrealizedROI,
		}

		res := e.posManager.CheckExit(ctx, memPos, baseQuote, foreignQuote)

		// Compute and cache real-time unrealized PnL (USD) for AI observation feedback
		currentROI := calculateCurrentROI(memPos, baseQuote, foreignQuote)
		livePnLUSD := currentROI * pos.InvestmentUSD
		if e.liveUnrealizedPnLs == nil {
			e.liveUnrealizedPnLs = make(map[string]float64)
		}
		e.liveUnrealizedPnLs[pos.BaseExchange+"|"+pos.BaseSymbol] = livePnLUSD
		e.liveUnrealizedPnLs[pos.ForeignExchange+"|"+pos.ForeignSymbol] = livePnLUSD

		// Update trailing peak ROI back to DB if it climbed higher
		if memPos.MaxUnrealizedROI > pos.MaxUnrealizedROI {
			_ = e.db.UpdatePositionMaxROI(pos.PositionID, memPos.MaxUnrealizedROI)
		}

		if res.ShouldExit {
			log.Printf("🚨 [EXIT TRIGGERED] %s | Reason: %s", pos.PositionID, res.Reason)

			// Find the strategy object associated with this position type
			var targetStrategy ArbitrageStrategy
			for _, strat := range e.strategies {
				if string(strat.ID()) == pos.Type {
					targetStrategy = strat
					break
				}
			}

			if targetStrategy == nil {
				log.Printf("CRITICAL [Monitor Error] Unrecognized strategy type: %s", pos.Type)
				continue
			}

			targetPair := e.findPair(pos.BaseExchange, pos.ForeignExchange, pos.BaseSymbol)
			if targetPair == nil {
				log.Printf("CRITICAL [Monitor Error] Could not find cross market pair for exit: %s/%s %s", pos.BaseExchange, pos.ForeignExchange, pos.BaseSymbol)
				continue
			}

			exitRatio := 1.0 // Default 100% full exit

			// Delegate the complex multi-step exit sequence directly to the Strategy OOP Instance
			execErr := targetStrategy.ExecuteExit(
				ctx,
				e.executor,
				*targetPair,
				pos,
				exitRatio,
				e.walletProxy,
			)

			if execErr != nil {
				if strings.Contains(execErr.Error(), "ERR_SESSION_EXPIRED") {
					_ = e.db.UpdatePositionStatus(pos.PositionID, "AWAITING_REPAY")
				}
				log.Printf("CRITICAL [Monitor Error] Failed to execute Exit Trades! %v", execErr)
				// We do NOT mark it closed, so it will try again on the next tick or when session restores
				continue
			}

			if err := e.db.UpdatePositionStatus(pos.PositionID, "CLOSED"); err != nil {
				log.Printf("[Monitor Error] Failed to mark position CLOSED in DB: %v", err)
				e.sendNotification(ctx, notification.AlertSystemError, fmt.Sprintf(
					"⚠️ **CRITICAL: Failed to execute Exit Trades/Repay for Position %s**\n- **Error**: %v",
					pos.PositionID, err,
				))
			} else {
				log.Printf("✅ Position %s successfully CLOSED.", pos.PositionID)
				e.notifyPositionClosed(ctx, pos.PositionID)
			}
		} else {
			// If we are not exiting, check if an intra-strategy pivot (Dynamic Switching) is strictly better
			e.evaluateDynamicSwitch(ctx, pos, memPos, baseQuote, foreignQuote)
		}
	}
}

func (e *Engine) evaluateDynamicSwitch(
	ctx context.Context,
	dbPos postgres.ActivePosition,
	memPos *ActivePosition,
	baseQuote, foreignQuote exchange.PriceUpdate,
) {
	switchingEdgeThreshold := e.config.DynamicSwitchThreshold
	targetPair := e.findPair(dbPos.BaseExchange, dbPos.ForeignExchange, dbPos.BaseSymbol)
	if targetPair == nil {
		return
	}

	// Switching between Strategy A (Spot Long on Foreign) <-> Strategy B (Futures Long on Foreign)
	if memPos.Type == StrategyMarginSpot && foreignQuote.IsFutures &&
		foreignQuote.FundingRate < -switchingEdgeThreshold {
		log.Printf(
			"🔄 [Dynamic Switch] Strategy A -> B identified for %s! FundingRate: %.4f",
			memPos.ID,
			foreignQuote.FundingRate,
		)
		sellSpotReq := exchange.OrderRequest{
			Symbol: targetPair.ForeignMarketInfo.Symbol,
			Side:   exchange.SellOrder,
			Type:   exchange.MarketOrder,
			Amount: dbPos.ForeignAmount,
		}
		buyFutureReq := exchange.OrderRequest{
			Symbol: targetPair.ForeignMarketInfo.Symbol,
			Side:   exchange.BuyOrder,
			Type:   exchange.FuturesLong,
			Amount: dbPos.ForeignAmount,
		}
		if _, _, err := e.executor.ExecuteAtomicDualTrade(ctx, sellSpotReq, buyFutureReq, targetPair.ForeignClient, targetPair.ForeignClient); err == nil {
			_ = e.db.UpdatePositionType(dbPos.PositionID, string(StrategyMarginFuture), "FUTURE")
		}
	} else if memPos.Type == StrategyMarginFuture &&
		foreignQuote.FundingRate > switchingEdgeThreshold {
		log.Printf(
			"🔄 [Dynamic Switch] Strategy B -> A identified for %s! FundingRate: %.4f",
			memPos.ID,
			foreignQuote.FundingRate,
		)
		closeFutureReq := exchange.OrderRequest{
			Symbol: targetPair.ForeignMarketInfo.Symbol,
			Side:   exchange.SellOrder,
			Type:   exchange.FuturesLong,
			Amount: dbPos.ForeignAmount,
		}
		buySpotReq := exchange.OrderRequest{
			Symbol: targetPair.ForeignMarketInfo.Symbol,
			Side:   exchange.BuyOrder,
			Type:   exchange.MarketOrder,
			Amount: dbPos.ForeignAmount,
		}
		if _, _, err := e.executor.ExecuteAtomicDualTrade(ctx, closeFutureReq, buySpotReq, targetPair.ForeignClient, targetPair.ForeignClient); err == nil {
			_ = e.db.UpdatePositionType(dbPos.PositionID, string(StrategyMarginSpot), "SPOT")
		}
	}
}

// evaluateRebalancing checks the Dual-Trigger criteria for Reverse Transfer Rebalancing:
// 1. Foreign exchange USD share drops <= RebalanceTriggerSkew (e.g. 20%), AND
// 2. Current premium is <= RebalanceMaxPremium (e.g. <= 0% discount / breakeven).
// It executes 100% delta-neutral reverse transfers until the 50:50 target ratio (RebalanceTargetSkew) is restored
// or the premium rises back above the threshold.
func (e *Engine) evaluateRebalancing(ctx context.Context, quotes map[string]exchange.PriceUpdate) {
	var transferStrat *ForeignTransferStrategy
	for _, s := range e.strategies {
		if ft, ok := s.(*ForeignTransferStrategy); ok {
			transferStrat = ft
			break
		}
	}
	if transferStrat == nil {
		return
	}

	for _, filter := range e.filters {
		for _, pair := range filter.Pairs {
			// Look for primary pair where Base is KRW (e.g. Upbit) and Foreign is USD (e.g. Coinbase)
			if pair.BaseMarketInfo.QuoteCurrency != "KRW" || pair.ForeignMarketInfo.QuoteCurrency != "USD" {
				continue
			}
			if !pair.IsValidForTransfer {
				continue
			}

			baseQuote, hasBase := quotes[string(pair.BaseMarketInfo.Symbol)]
			foreignQuote, hasForeign := quotes[string(pair.ForeignMarketInfo.Symbol)]
			if !hasBase || !hasForeign || foreignQuote.AskUSD <= 0 {
				continue
			}

			currentPremium := (baseQuote.AskUSD - foreignQuote.BidUSD) / foreignQuote.BidUSD
			if currentPremium > e.config.RebalanceMaxPremium {
				// Stop Criterion 1: Premium rose above threshold
				continue
			}

			baseBalances, err1 := pair.BaseClient.GetBalances(ctx)
			foreignBalances, err2 := pair.ForeignClient.GetBalances(ctx)
			if err1 != nil || err2 != nil {
				continue
			}

			var krwAvail, usdAvail float64
			for _, b := range baseBalances {
				if b.Currency == "KRW" {
					krwAvail = b.Available
					break
				}
			}
			for _, b := range foreignBalances {
				if b.Currency == "USD" {
					usdAvail = b.Available
					break
				}
			}

			baseUSD := krwAvail / GetFXRate("KRW")
			totalUSD := baseUSD + usdAvail
			if totalUSD <= 0 {
				continue
			}

			foreignShare := usdAvail / totalUSD
			// Stop Criterion 2: Already at or above target ratio (50:50), or above trigger skew (20%)
			if foreignShare >= e.config.RebalanceTargetSkew || foreignShare > e.config.RebalanceTriggerSkew {
				continue
			}

			// Find the reverse pair (Base = Coinbase USD, Foreign = Upbit KRW)
			reversePair := e.findPair(pair.ForeignClient.GetName(), pair.BaseClient.GetName(), string(pair.ForeignMarketInfo.Symbol))
			if reversePair == nil {
				continue
			}

			targetForeignUSD := totalUSD * e.config.RebalanceTargetSkew
			neededUSD := targetForeignUSD - usdAvail
			if neededUSD <= 0 {
				continue
			}
			rebalanceChunkUSD := math.Min(e.config.CapitalLimitUsd, neededUSD)

			log.Printf("⚖️ [Rebalancing Triggered] Foreign USD share %.1f%% <= %.1f%% & Premium %.2f%% <= %.2f%%. Rebalancing $%.2f (%s -> %s)...",
				foreignShare*100, e.config.RebalanceTriggerSkew*100,
				currentPremium*100, e.config.RebalanceMaxPremium*100,
				rebalanceChunkUSD, pair.BaseClient.GetName(), pair.ForeignClient.GetName())

			if err := transferStrat.ExecuteHedgedRebalance(ctx, e.executor, *reversePair, rebalanceChunkUSD, e.walletProxy); err != nil {
				log.Printf("⚠️ [Rebalancing Error] Failed hedged rebalance for %s: %v", pair.BaseCurrency, err)
			} else {
				log.Printf("✅ [Rebalancing Complete] Restored $%.2f to %s via 100%% delta-neutral transfer.",
					rebalanceChunkUSD, pair.ForeignClient.GetName())
				e.sendNotification(ctx, notification.AlertPositionClosed, fmt.Sprintf(
					"⚖️ **자본 자동 리밸런싱 완료 (100%% 델타 중립)**\n- **경로**: `%s` (KRW) -> `%s` (USD)\n- **코인**: `%s`\n- **환류 규모**: `$%.2f`",
					pair.BaseClient.GetName(), pair.ForeignClient.GetName(), pair.BaseCurrency, rebalanceChunkUSD,
				))
			}
			return
		}
	}
}

// AIPredictedOpportunity matches the JSON payload published by Python AI app.py
type AIPredictedOpportunity struct {
	UserID              int     `json:"user_id"`
	BaseExchange        string  `json:"base_exchange"`
	ForeignExchange     string  `json:"foreign_exchange"`
	BaseSymbol          string  `json:"base_symbol"`
	ForeignSymbol       string  `json:"foreign_symbol"`
	ActionScore         float64 `json:"action_score"`
	TargetWeight        float64 `json:"target_weight"`
	ActionType          int     `json:"action_type"`
	PremiumPct          float64 `json:"premium_pct"`
	PredictedPremiumPct float64 `json:"predicted_premium_pct"`
	Timestamp           float64 `json:"timestamp"`
}

// ListenOpportunities subscribes to AI inferred opportunities and triggers Portfolio Rebalancing.
func (e *Engine) ListenOpportunities(ctx context.Context) {
	aiCh := make(chan []byte)
	go e.redisClient.SubscribeAIPredictedOpportunities(ctx, aiCh)

	lastLogTime := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case payload := <-aiCh:
			if e.isDraining {
				continue // Do not enter new trades during graceful shutdown
			}

			var aiOps []AIPredictedOpportunity
			if err := json.Unmarshal(payload, &aiOps); err != nil {
				log.Printf("[Engine-%d] Failed to parse AI opportunities: %v", e.userID, err)
				continue
			}

			// 1. Filter by UserID
			var filteredOps []AIPredictedOpportunity
			for _, opp := range aiOps {
				if opp.UserID != 0 && uint(opp.UserID) != e.userID {
					continue
				}
				filteredOps = append(filteredOps, opp)
			}

			logDetails := time.Since(lastLogTime) > 10*time.Second
			if logDetails {
				log.Printf("[PaperEngine] Received %d AI predictions from Redis. Filtered for user %d: %d.", len(aiOps), e.userID, len(filteredOps))
				lastLogTime = time.Now()
			}

			if len(filteredOps) == 0 {
				continue
			}

			e.mu.Lock()

			// 2. Clamp individual weights and only renormalize (scale down) when totalWeight > 1.0
			// so a single partial recommendation (e.g. 0.20) is never forced into a 100% full-in allocation.
			const maxSinglePairWeight = 0.50
			totalWeight := 0.0
			for i := range filteredOps {
				if filteredOps[i].TargetWeight < 0 {
					filteredOps[i].TargetWeight = 0
				} else if filteredOps[i].TargetWeight > maxSinglePairWeight {
					filteredOps[i].TargetWeight = maxSinglePairWeight
				}
				totalWeight += filteredOps[i].TargetWeight
			}

			if totalWeight > 1.0 {
				for i := range filteredOps {
					filteredOps[i].TargetWeight = filteredOps[i].TargetWeight / totalWeight
				}
			}

			// 3. Execution Pipeline and ActionType Handling
			activePos, _ := e.db.GetActivePositionsByUser(e.userID)

			for _, opp := range filteredOps {
				targetDollarVolume := e.config.CapitalLimitUsd * opp.TargetWeight

				// Find current exposure (matching either canonical direction or reverse direction)
				var matchingPos *postgres.ActivePosition
				currentExposure := 0.0
				for i, pos := range activePos {
					if pos.Status == "CLOSED" {
						continue
					}
					isCanonicalMatch := pos.BaseExchange == opp.BaseExchange &&
						pos.ForeignExchange == opp.ForeignExchange &&
						pos.BaseSymbol == opp.BaseSymbol
					isReverseMatch := pos.BaseExchange == opp.ForeignExchange &&
						pos.ForeignExchange == opp.BaseExchange &&
						pos.BaseSymbol == opp.ForeignSymbol
					if isCanonicalMatch || isReverseMatch {
						currentExposure += pos.InvestmentUSD
						matchingPos = &activePos[i]
						break
					}
				}

				// --- ActionType = 2 (REPAY / 청산) ---
				if opp.ActionType == 2 {
					if matchingPos != nil && matchingPos.Status != "CLOSED" {
						log.Printf("🚨 [AI REPAY TRIGGERED] User %d %s, closing position %s", e.userID, opp.BaseSymbol, matchingPos.PositionID)
						
						var targetStrategy ArbitrageStrategy
						for _, strat := range e.strategies {
							if string(strat.ID()) == matchingPos.Type {
								targetStrategy = strat
								break
							}
						}
						
						if targetStrategy != nil {
							targetPair := e.findPair(matchingPos.BaseExchange, matchingPos.ForeignExchange, matchingPos.BaseSymbol)
							if targetPair == nil {
								log.Printf("CRITICAL [AI REPAY] Could not find cross market pair for exit: %s/%s %s", matchingPos.BaseExchange, matchingPos.ForeignExchange, matchingPos.BaseSymbol)
								continue
							}
							exitRatio := 1.0
							err := targetStrategy.ExecuteExit(ctx, e.executor, *targetPair, *matchingPos, exitRatio, e.walletProxy)
							if err == nil {
								e.db.UpdatePositionStatus(matchingPos.PositionID, "CLOSED")
								e.notifyPositionClosed(ctx, matchingPos.PositionID)
							} else {
								if strings.Contains(err.Error(), "ERR_SESSION_EXPIRED") {
									_ = e.db.UpdatePositionStatus(matchingPos.PositionID, "AWAITING_REPAY")
								}
								e.sendNotification(ctx, notification.AlertSystemError, fmt.Sprintf(
									"⚠️ **AI REPAY Error: Failed to execute Exit Trades for Position %s**\n- **Error**: %v",
									matchingPos.PositionID, err,
								))
							}
						}
					} else {
						if logDetails {
							log.Printf("[PaperEngine] AI suggested REPAY for %s but no matching open position was found.", opp.BaseSymbol)
						}
					}
					continue
				}

				// --- ActionType = 1 (TRANSFER / 전송 및 상환) ---
				if opp.ActionType == 1 {
					if matchingPos != nil && matchingPos.Status == "OPEN" {
						// For pure futures strategies (D/E), on-chain transfer does not apply; treat ActionType=1 as Hold.
						if matchingPos.Type == string(StrategyFutureSpot) || matchingPos.Type == string(StrategyFutureFuture) {
							continue
						}

						posPair := e.findPair(matchingPos.BaseExchange, matchingPos.ForeignExchange, matchingPos.BaseSymbol)
						var transferStrat *ForeignTransferStrategy
						for _, s := range e.strategies {
							if ft, ok := s.(*ForeignTransferStrategy); ok {
								transferStrat = ft
								break
							}
						}

						if posPair != nil && posPair.IsValidForTransfer && transferStrat != nil {
							log.Printf("🔄 [AI TRANSFER TRIGGERED] User %d %s, executing on-chain transfer and auto-repay for %s",
								e.userID, matchingPos.BaseSymbol, matchingPos.PositionID)
							_ = e.db.UpdatePositionStatus(matchingPos.PositionID, "TRANSFERRING")
							e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(fmt.Sprintf(
								`{"action":"POSITION_TRANSFERRING", "position_id":"%s"}`, matchingPos.PositionID,
							)))
							e.publishAccountState(ctx)
							e.sendNotification(ctx, notification.AlertPositionOpened, fmt.Sprintf(
								"📦 **Coin Transfer Started (AI Triggered)**\n- **ID**: `%s`\n- **From**: `%s` to `%s`\n- **Symbol**: `%s`",
								matchingPos.PositionID, matchingPos.ForeignExchange, matchingPos.BaseExchange, matchingPos.BaseSymbol,
							))

							if err := transferStrat.ExecuteTransferAndRepay(ctx, e.executor, *posPair, *matchingPos, e.walletProxy); err == nil {
								_ = e.db.UpdatePositionStatus(matchingPos.PositionID, "CLOSED")
								e.notifyPositionClosed(ctx, matchingPos.PositionID)
							} else {
								if strings.Contains(err.Error(), "ERR_SESSION_EXPIRED") {
									_ = e.db.UpdatePositionStatus(matchingPos.PositionID, "AWAITING_REPAY")
								} else {
									_ = e.db.UpdatePositionStatus(matchingPos.PositionID, "OPEN")
								}
								log.Printf("❌ [AI TRANSFER ERROR] Failed transfer-and-repay for %s: %v", matchingPos.PositionID, err)
							}
						}
					} else {
						if logDetails {
							log.Printf("[PaperEngine] AI suggested TRANSFER for %s but no matching open position was found.", opp.BaseSymbol)
						}
					}
					continue
				}

				// --- ActionType = 0 (TRADE / 진입 및 리밸런싱) ---
				// Resolve directional exchange pair based on AI ActionScore:
				// ActionScore <= 5.0: Short BaseExchange / Long ForeignExchange
				// ActionScore > 5.0:  Long BaseExchange / Short ForeignExchange (flip Base <-> Foreign)
				entryBaseEx := opp.BaseExchange
				entryForeignEx := opp.ForeignExchange
				entryBaseSym := opp.BaseSymbol
				entrySpread := opp.PremiumPct
				entryPredictedSpread := opp.PredictedPremiumPct
				if opp.ActionScore > 5.0 {
					entryBaseEx = opp.ForeignExchange
					entryForeignEx = opp.BaseExchange
					entryBaseSym = opp.ForeignSymbol
					entrySpread = -opp.PremiumPct
					entryPredictedSpread = -opp.PredictedPremiumPct
				}

				delta := targetDollarVolume - currentExposure
				toleranceAmount := e.config.CapitalLimitUsd * 0.02

				if delta > toleranceAmount {
					targetPair := e.findPair(entryBaseEx, entryForeignEx, entryBaseSym)
					if targetPair == nil {
						targetPair = e.findPair(opp.BaseExchange, opp.ForeignExchange, opp.BaseSymbol)
					}

					if matchingPos == nil {
						if targetPair != nil {
							// Dynamic Strategy Router: compare expected Net ROI across eligible strategies (A ~ E)
							var selectedStrategy StrategyType = StrategyMarginSpot
							isBaseFuture := targetPair.BaseMarketInfo.MarketType == "FUTURE"
							isForeignFuture := targetPair.ForeignMarketInfo.MarketType == "FUTURE"
							if isBaseFuture && isForeignFuture {
								selectedStrategy = StrategyFutureFuture
							} else if isBaseFuture {
								selectedStrategy = StrategyFutureSpot
							} else {
								// Base is Spot (Margin): compare convergence ROI (A/B) vs instant transfer ROI (C)
								reversionNetROI := (entrySpread - entryPredictedSpread) - 0.0013
								transferNetROI := entrySpread - 0.0022

								if targetPair.IsValidForTransfer && transferNetROI >= reversionNetROI && entrySpread > 0.0025 {
									selectedStrategy = StrategyForeignTransfer
								} else if isForeignFuture {
									selectedStrategy = StrategyMarginFuture
								} else {
									selectedStrategy = StrategyMarginSpot
								}
							}

							targetStrategy := e.findStrategy(selectedStrategy)
							if targetStrategy != nil {
								eval := targetStrategy.GetLastEvaluation()
								eval.Type = selectedStrategy
								eval.EntryPremium = entrySpread
								eval.BasisReturn = entrySpread - entryPredictedSpread
								eval.InvestmentUSD = targetDollarVolume
								eval.HedgeMarket = "SPOT"
								if isForeignFuture {
									eval.HedgeMarket = "FUTURE"
								}
								targetStrategy.SetEvaluation(eval)
								_, err := e.ExecuteStrategy(ctx, targetStrategy, *targetPair)
								if err != nil {
									errMsg := err.Error()
									isInsuff := strings.Contains(strings.ToLower(errMsg), "insufficient balance") ||
										strings.Contains(strings.ToLower(errMsg), "insufficient paper funds") ||
										strings.Contains(strings.ToLower(errMsg), "insufficient funds") ||
										strings.Contains(strings.ToLower(errMsg), "foreign balance too low") ||
										strings.Contains(strings.ToLower(errMsg), "skew protection active")

									shouldLog := true
									if isInsuff {
										e.lastErrorLogTimeMu.Lock()
										key := fmt.Sprintf("%s-%s", entryBaseSym, selectedStrategy)
										lastLog, exists := e.lastErrorLogTime[key]
										if exists && time.Since(lastLog) < 1*time.Minute {
											shouldLog = false
										} else {
											e.lastErrorLogTime[key] = time.Now()
										}
										e.lastErrorLogTimeMu.Unlock()
									}

									if shouldLog {
										log.Printf("❌ [AI ENTRY FAILED] (Throttled) User %d %s via %s: %v", e.userID, entryBaseSym, selectedStrategy, err)
									}
								} else {
									log.Printf("🚀 [AI ENTRY TRIGGERED] User %d %s via %s, Target Vol: $%.2f", e.userID, entryBaseSym, selectedStrategy, targetDollarVolume)
								}
							}
						}
					} else {
						// SCALE IN: Execute pyramiding on the existing position
						log.Printf("📈 [AI SCALE-IN TRIGGERED] User %d %s, adding $%.2f", e.userID, matchingPos.BaseSymbol, delta)
						targetStrategy := e.findStrategy(StrategyType(matchingPos.Type))
						posPair := e.findPair(matchingPos.BaseExchange, matchingPos.ForeignExchange, matchingPos.BaseSymbol)
						if targetStrategy != nil && posPair != nil {
							prevBase := matchingPos.BaseAmount
							prevForeign := matchingPos.ForeignAmount
							if err := targetStrategy.ExecuteScale(ctx, e.executor, *posPair, matchingPos, delta, e.walletProxy); err == nil {
								addedBase := matchingPos.BaseAmount - prevBase
								addedForeign := matchingPos.ForeignAmount - prevForeign
								_ = e.db.AddPositionAmount(matchingPos.PositionID, delta, addedBase, addedForeign)
							} else {
								log.Printf("⚠️ [AI SCALE-IN ERROR] Failed for %s: %v", matchingPos.PositionID, err)
							}
						}
					}

				} else if delta < -toleranceAmount {
					// SCALE OUT: Partial or full position reduction
					if matchingPos != nil && currentExposure > 0 {
						log.Printf("📉 [AI SCALE-OUT TRIGGERED] User %d %s, removing $%.2f", e.userID, matchingPos.BaseSymbol, -delta)
						exitRatio := math.Min(1.0, (-delta)/currentExposure)
						targetStrategy := e.findStrategy(StrategyType(matchingPos.Type))
						posPair := e.findPair(matchingPos.BaseExchange, matchingPos.ForeignExchange, matchingPos.BaseSymbol)
						if targetStrategy != nil && posPair != nil {
							if err := targetStrategy.ExecuteExit(ctx, e.executor, *posPair, *matchingPos, exitRatio, e.walletProxy); err == nil {
								if exitRatio >= 0.99 {
									_ = e.db.UpdatePositionStatus(matchingPos.PositionID, "CLOSED")
									e.notifyPositionClosed(ctx, matchingPos.PositionID)
								} else {
									reducedUsd := matchingPos.InvestmentUSD * exitRatio
									reducedBase := matchingPos.BaseAmount * exitRatio
									reducedForeign := matchingPos.ForeignAmount * exitRatio
									_ = e.db.ReducePositionAmount(matchingPos.PositionID, reducedUsd, reducedBase, reducedForeign)
								}
							} else {
								log.Printf("⚠️ [AI SCALE-OUT ERROR] Failed for %s: %v", matchingPos.PositionID, err)
							}
						}
					} else if logDetails {
						log.Printf("[PaperEngine] AI suggested SCALE-OUT for %s but no matching active position exists.", opp.BaseSymbol)
					}
				} else {
					if logDetails {
						log.Printf("[PaperEngine] AI suggested TRADE for %s with target weight %.4f ($%.2f exposure), but current exposure is $%.2f. Delta ($%.2f) is within tolerance ($%.2f). No trade executed.",
							opp.BaseSymbol, opp.TargetWeight, targetDollarVolume, currentExposure, delta, toleranceAmount)
					}
				}
			}

			// 4. Publish Filtered Opportunities back to user's dashboard Tab
			type FilteredOppDTO struct {
				BaseExchange    string  `json:"base_exchange"`
				ForeignExchange string  `json:"foreign_exchange"`
				BaseSymbol      string  `json:"base_symbol"`
				ForeignSymbol   string  `json:"foreign_symbol"`
				Strategy        string  `json:"strategy"`
				ExpectedRoi     float64 `json:"expected_roi"`
				RiskScore       float64 `json:"risk_score"`
				InvestmentUsd   float64 `json:"investment_usd"`
				IsValid         bool    `json:"is_valid"`
			}

			var dtoList []FilteredOppDTO
			for _, opp := range filteredOps {
				targetPair := e.findPair(opp.BaseExchange, opp.ForeignExchange, opp.BaseSymbol)
				if targetPair != nil {
					isBaseFuture := targetPair.BaseMarketInfo.MarketType == "FUTURE"
					isForeignFuture := targetPair.ForeignMarketInfo.MarketType == "FUTURE"

					if isBaseFuture && isForeignFuture {
						// Strategy E: Future-Future
						roi := opp.PremiumPct - opp.PredictedPremiumPct
						dtoList = append(dtoList, FilteredOppDTO{
							BaseExchange:    opp.BaseExchange,
							ForeignExchange: opp.ForeignExchange,
							BaseSymbol:      opp.BaseSymbol,
							ForeignSymbol:   opp.ForeignSymbol,
							Strategy:        "Future-Future (E)",
							ExpectedRoi:     roi - 0.0005, // minus estimated trading fee rate (0.05%)
							RiskScore:       15.0,         // low risk, no transfer time exposure
							InvestmentUsd:   e.config.CapitalLimitUsd * opp.TargetWeight,
							IsValid:         true,
						})
					} else if isBaseFuture {
						// Strategy D: Future-Spot
						roi := opp.PremiumPct - opp.PredictedPremiumPct
						dtoList = append(dtoList, FilteredOppDTO{
							BaseExchange:    opp.BaseExchange,
							ForeignExchange: opp.ForeignExchange,
							BaseSymbol:      opp.BaseSymbol,
							ForeignSymbol:   opp.ForeignSymbol,
							Strategy:        "Future-Spot (D)",
							ExpectedRoi:     roi - 0.0006, // minus estimated fee rate (0.06%)
							RiskScore:       18.0,
							InvestmentUsd:   e.config.CapitalLimitUsd * opp.TargetWeight,
							IsValid:         true,
						})
					} else {
						// Base is Spot (Margin) - requires margin support on base exchange
						if targetPair.BaseClient.SupportsMargin() {
							// 1. Holding Strategy: Margin-Spot (A) or Margin-Future (B)
							roiHolding := opp.PremiumPct - opp.PredictedPremiumPct
							strategyHoldingName := "Margin-Spot (A)"
							if isForeignFuture {
								strategyHoldingName = "Margin-Future (B)"
							}
							dtoList = append(dtoList, FilteredOppDTO{
								BaseExchange:    opp.BaseExchange,
								ForeignExchange: opp.ForeignExchange,
								BaseSymbol:      opp.BaseSymbol,
								ForeignSymbol:   opp.ForeignSymbol,
								Strategy:        strategyHoldingName,
								ExpectedRoi:     roiHolding - 0.0013, // accounting for spot trading fees + estimated borrow interest over hold (0.13%)
								RiskScore:       18.0,                // lower risk, no block confirmation delay
								InvestmentUsd:   e.config.CapitalLimitUsd * opp.TargetWeight,
								IsValid:         true,
							})

							// 2. Transfer Loop Strategy: Transfer Loop (C)
							if targetPair.IsValidForTransfer {
								roiTransfer := opp.PremiumPct - opp.PredictedPremiumPct
								
								// Compute realistic risk based on block confirmation time and congestion
								networkRisk := 20.0 // Default baseline for fast confirmation chains (ETH, etc.)
								if strings.HasPrefix(opp.BaseSymbol, "BTC") {
									networkRisk = 35.0 // Higher baseline block confirmation risk for BTC
								}
								
								// Add premium volatility correlation factor
								absPremium := opp.PremiumPct
								if absPremium < 0 {
									absPremium = -absPremium
								}
								networkRisk += absPremium * 1.5
								if networkRisk > 75.0 {
									networkRisk = 75.0
								}
								totalRisk := networkRisk + 15.0 // network risk + capital utilization margin liquidation risk

								dtoList = append(dtoList, FilteredOppDTO{
									BaseExchange:    opp.BaseExchange,
									ForeignExchange: opp.ForeignExchange,
									BaseSymbol:      opp.BaseSymbol,
									ForeignSymbol:   opp.ForeignSymbol,
									Strategy:        "Transfer Loop (C)",
									ExpectedRoi:     roiTransfer - 0.0022, // accounting for trading fees + network gas/withdrawal fees (0.22%)
									RiskScore:       totalRisk,
									InvestmentUsd:   e.config.CapitalLimitUsd * opp.TargetWeight,
									IsValid:         true,
								})
							}
						}
					}
				}
			}

			filteredPayload, err := json.Marshal(dtoList)
			if err == nil {
				e.redisClient.PublishUserOpportunities(ctx, fmt.Sprint(e.userID), filteredPayload)
			}
			e.mu.Unlock()
		}
	}
}

// AIAccountState maps the user's balance and positions for python DRL observation feed
type AIAccountState struct {
	UserID           uint               `json:"user_id"`
	AvailableBalance float64            `json:"available_balance"`
	CurrentExposures map[string]float64 `json:"current_exposures"`
	UnrealizedPnLs   map[string]float64 `json:"unrealized_pnls"`
	PairStates       map[string]int     `json:"pair_states"`
	State            int                `json:"state"`
}

func (e *Engine) publishAccountState(ctx context.Context) {
	positions, err := e.db.GetActivePositionsByUser(e.userID)
	if err != nil {
		log.Printf("[Engine-%d] Failed to load user active positions: %v", e.userID, err)
		return
	}

	exposures := make(map[string]float64)
	pnls := make(map[string]float64)
	pairStates := make(map[string]int)
	state := 0 // Global fallback: 0: Neutral, 1: Holding, 2: Transferring, 3: Repaying

	totalInvestedUSD := 0.0
	for _, pos := range positions {
		if pos.Status == "CLOSED" {
			continue
		}
		totalInvestedUSD += pos.InvestmentUSD

		key := pos.BaseExchange + "|" + pos.BaseSymbol
		reverseKey := pos.ForeignExchange + "|" + pos.ForeignSymbol

		normExposure := 0.0
		if e.config.CapitalLimitUsd > 0 {
			normExposure = math.Min(1.0, pos.InvestmentUSD/e.config.CapitalLimitUsd)
		}
		// Strategies A~E short BaseExchange (-normExposure) and long ForeignExchange (+normExposure)
		exposures[key] = -normExposure
		exposures[reverseKey] = normExposure

		posPnL := pos.MaxUnrealizedROI * pos.InvestmentUSD
		if e.liveUnrealizedPnLs != nil {
			if liveVal, ok := e.liveUnrealizedPnLs[key]; ok {
				posPnL = liveVal
			}
		}
		pnls[key] = posPnL
		pnls[reverseKey] = posPnL

		posState := 1
		if pos.Status == "TRANSFERRING" {
			posState = 2
		} else if pos.Status == "REPAYING" || pos.Status == "AWAITING_REPAY" {
			posState = 3
		}
		pairStates[key] = posState
		pairStates[reverseKey] = posState
		if posState > state {
			state = posState
		}
	}

	balance := math.Max(0.0, e.config.CapitalLimitUsd-totalInvestedUSD)

	payload, err := json.Marshal(AIAccountState{
		UserID:           e.userID,
		AvailableBalance: balance,
		CurrentExposures: exposures,
		UnrealizedPnLs:   pnls,
		PairStates:       pairStates,
		State:            state,
	})
	if err == nil {
		e.redisClient.PublishUserState(ctx, fmt.Sprint(e.userID), payload)
	}
}

func (e *Engine) findPair(baseExchange, foreignExchange, baseSymbol string) *CrossMarketPair {
	for _, filter := range e.filters {
		for _, pair := range filter.Pairs {
			if pair.BaseClient.GetName() == baseExchange &&
				pair.ForeignClient.GetName() == foreignExchange &&
				string(pair.BaseMarketInfo.Symbol) == baseSymbol {
				return &pair
			}
		}
	}
	return nil
}

func (e *Engine) notifyPositionOpened(ctx context.Context, pos *postgres.ActivePosition) {
	e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(fmt.Sprintf(
		`{"action":"POSITION_OPENED", "position_id":"%s"}`, pos.PositionID,
	)))
	e.publishAccountState(ctx)

	e.sendNotification(ctx, notification.AlertPositionOpened, fmt.Sprintf(
		"🚀 **Position Opened**\n- **ID**: `%s`\n- **Type**: `%s`\n- **Exchanges**: `%s` (Base) / `%s` (Foreign)\n- **Symbol**: `%s`\n- **Size**: `$%.2f`\n- **Premium**: `%.4f` (Target Profit: `%.2f%%`)",
		pos.PositionID, pos.Type, pos.BaseExchange, pos.ForeignExchange, pos.BaseSymbol, pos.InvestmentUSD, pos.EntryPremium, e.config.MinTargetProfit,
	))
}

func (e *Engine) notifyPositionClosed(ctx context.Context, positionID string) {
	e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(fmt.Sprintf(
		`{"action":"POSITION_CLOSED", "position_id":"%s"}`, positionID,
	)))
	e.publishAccountState(ctx)

	profitStr := "N/A"
	pos, err := e.db.GetPositionByID(positionID)
	if err == nil && pos.EntryBasePrice > 0 && pos.EntryForeignPrice > 0 {
		upbitSellUSD := (pos.BaseAmount * pos.EntryBasePrice) / GetFXRate("KRW")
		coinbaseBuyUSD := pos.ForeignAmount * pos.EntryForeignPrice

		upbitFeeUSD := upbitSellUSD * 0.0005     // 0.05% taker fee
		coinbaseFeeUSD := coinbaseBuyUSD * 0.006 // 0.6% taker fee

		netProfitUSD := upbitSellUSD - coinbaseBuyUSD - upbitFeeUSD - coinbaseFeeUSD
		profitStr = fmt.Sprintf("$%.2f (Est.)", netProfitUSD)
		log.Printf("💰 [REALIZED PROFIT] Position %s closed. Estimated Net Profit: %s (Base Sell: $%.2f, Foreign Buy: $%.2f, Fees: $%.2f)",
			positionID, profitStr, upbitSellUSD, coinbaseBuyUSD, upbitFeeUSD+coinbaseFeeUSD)
	}

	e.sendNotification(ctx, notification.AlertPositionClosed, fmt.Sprintf(
		"✅ **Position Closed**\n- **ID**: `%s`\n- **Estimated Profit**: `%s`\n- **Timestamp**: `%s`",
		positionID, profitStr, time.Now().Format("2006-01-02 15:04:05 MST"),
	))
}

func (e *Engine) sendNotification(ctx context.Context, alertType notification.AlertType, message string) {
	// 1. Get UserSettings from DB
	settings, err := e.db.GetUserSettings(e.userID)
	
	var enabled bool = true
	var discordURL string
	var telegramChatID string

	if err == nil {
		// Check if notification is enabled for this type in DB settings
		switch alertType {
		case notification.AlertPositionOpened:
			enabled = settings.NotifyOnEntry
		case notification.AlertPositionClosed:
			enabled = settings.NotifyOnExit
		case notification.AlertSystemError, notification.AlertTransferFailed, notification.AlertLiquidation:
			enabled = settings.NotifyOnException
		}
		discordURL = settings.DiscordWebhookURL
		telegramChatID = settings.TelegramChatID
	}

	if !enabled {
		return
	}

	// 3. Resolve Discord Webhook URL (fallback to env variable if empty)
	if discordURL == "" {
		discordURL = os.Getenv("DISCORD_WEBHOOK_URL")
	}

	if discordURL == "" && telegramChatID == "" {
		return // No webhook/chat ID configured
	}

	// 4. Send alert asynchronously via Notifier
	notifier := notification.NewNotifier(discordURL, telegramChatID)
	_ = notifier.SendAlert(ctx, alertType, message)
}
