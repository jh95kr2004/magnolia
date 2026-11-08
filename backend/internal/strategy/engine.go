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

	// Mode & Bot Lifecycle State
	isPaper   bool
	botStatus string // "RUNNING", "DRAINING", "STOPPED"

	// Gradual Price Offset Simulator State (Paper Mode only)
	simExchange        string
	simStartOffsetPct  float64
	simTargetOffsetPct float64
	simRampMinutes     float64
	simStartedAt       int64

	// Draining State
	isDraining bool

	// Wallet Proxy
	walletProxy wallet.Client

	// Mutex to serialize position evaluations and opportunity processing
	mu sync.Mutex

	// Live unrealized PnL USD cache per "Exchange|Symbol" updated by RunPositionMonitor
	liveUnrealizedPnLs map[string]float64
	rawQuotes          map[string]exchange.PriceUpdate
	latestQuotes       map[string]exchange.PriceUpdate

	// Error logging throttle
	lastErrorLogTime   map[string]time.Time
	lastErrorLogTimeMu sync.Mutex

	// Equity history snapshot cadence (every 60s or on key portfolio events)
	lastSnapshotTime     time.Time
	forceNextSnapshot    bool
	resetSnapshotPending bool
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
		triggerSkew := threshold.RebalanceTriggerSkew
		if triggerSkew <= 0 {
			triggerSkew = 0.20
		}
		targetSkew := threshold.RebalanceTargetSkew
		if targetSkew <= 0 {
			targetSkew = 0.50
		}
		cfg = Config{
			MinTargetProfit:        threshold.MinTargetProfit,
			MaxStopLoss:            threshold.MaxStopLoss,
			MaxAcceptableSpread:    threshold.MaxAcceptableSpread,
			CapitalLimitUsd:        threshold.CapitalLimitUsd,
			MaxRiskScore:           threshold.MaxRiskScore,
			RiskMultiplier:         threshold.RiskMultiplier,
			RetryOnFailure:         threshold.RetryOnFailure,
			DynamicSwitchThreshold: 0.001,
			RebalanceTriggerSkew:   triggerSkew,
			RebalanceTargetSkew:    targetSkew,
			RebalanceMaxPremium:    threshold.RebalanceMaxPremium,
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

	isPaper := false
	for _, c := range clients {
		if _, ok := c.(*exchange.PaperClient); ok {
			isPaper = true
			break
		}
	}

	botStatus := "STOPPED"
	if isPaper {
		botStatus = "RUNNING"
	}
	simEx := "UPBIT"
	var simStart, simTarget, simRamp float64
	var simStartedAt int64

	if db != nil {
		if snaps, snapErr := db.GetEquitySnapshots(userID, true, time.Now().Add(-24*time.Hour), 10); snapErr == nil && len(snaps) > 0 {
			lastFX := snaps[len(snaps)-1].FXRateKRW
			if lastFX > 100 {
				SetFXRate("KRW", lastFX)
			}
		}
		if settings, sErr := db.GetUserSettings(userID); sErr == nil && settings != nil {
			if isPaper {
				if settings.PaperBotStatus != "" {
					botStatus = settings.PaperBotStatus
				}
				if settings.SimExchange != "" {
					simEx = settings.SimExchange
				}
				simStart = settings.SimStartOffsetPct
				simTarget = settings.SimTargetOffsetPct
				simRamp = settings.SimRampMinutes
				simStartedAt = settings.SimStartedAt
				if settings.PaperBalancesJSON != "" {
					exchange.ImportAllUserPaperState(userID, settings.PaperBalancesJSON)
				}
			} else {
				if settings.LiveBotStatus != "" {
					botStatus = settings.LiveBotStatus
				}
			}
		}
		if isPaper {
			if openPos, pErr := db.GetActivePositionsByUserAndMode(userID, true); pErr == nil {
				for _, pos := range openPos {
					for _, c := range clients {
						if pc, ok := c.(*exchange.PaperClient); ok {
							if strings.EqualFold(pc.GetName(), pos.BaseExchange) && pos.EntryBasePrice > 0 {
								pc.SetPriceHint(exchange.Symbol(pos.BaseSymbol), pos.EntryBasePrice, pos.EntryBasePrice)
							}
							if strings.EqualFold(pc.GetName(), pos.ForeignExchange) && pos.EntryForeignPrice > 0 {
								pc.SetPriceHint(exchange.Symbol(pos.ForeignSymbol), pos.EntryForeignPrice, pos.EntryForeignPrice)
							}
						}
					}
				}
			}
		}
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
		isPaper:            isPaper,
		botStatus:          botStatus,
		simExchange:        simEx,
		simStartOffsetPct:  simStart,
		simTargetOffsetPct: simTarget,
		simRampMinutes:     simRamp,
		simStartedAt:       simStartedAt,
		isDraining:         botStatus != "RUNNING",
		walletProxy:        walletProxy,
		liveUnrealizedPnLs: make(map[string]float64),
		rawQuotes:          make(map[string]exchange.PriceUpdate),
		latestQuotes:       make(map[string]exchange.PriceUpdate),
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

func (e *Engine) ModeString() string {
	if e.isPaper {
		return "PAPER"
	}
	return "LIVE"
}

// GetSimStatus computes the current S-curve interpolated price offset (%) for Paper Mode scenario simulation.
func (e *Engine) GetSimStatus() (exchangeName string, currentOffsetPct, targetOffsetPct, rampMinutes, progressPct float64, remainingSec int64) {
	ex := e.simExchange
	if ex == "" {
		ex = "UPBIT"
	}
	if !e.isPaper || e.simStartedAt <= 0 {
		return ex, e.simTargetOffsetPct, e.simTargetOffsetPct, e.simRampMinutes, 100.0, 0
	}
	if e.simRampMinutes <= 0 {
		return ex, e.simTargetOffsetPct, e.simTargetOffsetPct, 0, 100.0, 0
	}
	totalSec := e.simRampMinutes * 60.0
	elapsed := float64(time.Now().Unix() - e.simStartedAt)
	if elapsed >= totalSec {
		return ex, e.simTargetOffsetPct, e.simTargetOffsetPct, e.simRampMinutes, 100.0, 0
	}
	alpha := math.Max(0.0, math.Min(1.0, elapsed/totalSec))
	// Smooth cosine S-curve interpolation
	weight := (1.0 - math.Cos(math.Pi*alpha)) / 2.0
	curr := e.simStartOffsetPct + (e.simTargetOffsetPct-e.simStartOffsetPct)*weight
	rem := int64(math.Ceil(totalSec - elapsed))
	return ex, curr, e.simTargetOffsetPct, e.simRampMinutes, alpha * 100.0, rem
}

func (e *Engine) applySimOffsetToQuote(q exchange.PriceUpdate) exchange.PriceUpdate {
	if !e.isPaper {
		return q
	}
	simEx, currPct, _, _, _, _ := e.GetSimStatus()
	if strings.EqualFold(q.Exchange, simEx) && math.Abs(currPct) > 1e-6 {
		mult := 1.0 + (currPct / 100.0)
		q.RawBid *= mult
		q.RawAsk *= mult
		q.BidUSD *= mult
		q.AskUSD *= mult
		q.RawFutureBid *= mult
		q.RawFutureAsk *= mult
		q.BidFutureUSD *= mult
		q.AskFutureUSD *= mult
	}

	// Inject the latest price into the corresponding PaperClient so virtual market orders fill accurately
	for _, c := range e.clients {
		if pc, ok := c.(*exchange.PaperClient); ok && strings.EqualFold(pc.GetName(), q.Exchange) {
			spotPrice := q.RawAsk
			if spotPrice <= 0 {
				if q.QuoteCurrency == "KRW" || strings.HasSuffix(string(q.Symbol), "-KRW") {
					spotPrice = q.AskUSD * GetFXRate("KRW")
				} else {
					spotPrice = q.AskUSD
				}
			}
			futPrice := q.RawFutureAsk
			if futPrice <= 0 {
				futPrice = spotPrice
			}
			pc.SetPriceHint(q.Symbol, spotPrice, futPrice)
		}
	}
	return q
}

func (e *Engine) persistPaperState() {
	if !e.isPaper || e.db == nil {
		return
	}
	snapJSON := exchange.ExportAllUserPaperState(e.userID)
	if snapJSON == "" {
		return
	}
	if settings, err := e.db.GetUserSettings(e.userID); err == nil && settings != nil {
		settings.PaperBalancesJSON = snapJSON
		_ = e.db.SaveUserSettings(settings)
	}
}

// StartWebLendingWatchdogs spawns child goroutines bound to the per-user context (runUserEngine)
// for any exchange client that uses browser-based coin lending (e.g., Upbit, Bithumb).
func (e *Engine) StartWebLendingWatchdogs(ctx context.Context) {
	if e.isPaper {
		return // Paper trading simulates coin borrowing without requiring a live noVNC browser session
	}
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
	pos.IsPaper = e.isPaper
	pos.MonitorNodeID = e.nodeID
	if pos.Status == "" {
		pos.Status = "OPEN"
	}
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
	e.persistPaperState()

	// 3. Dispatch WebSocket event and send external notification
	e.notifyPositionOpened(ctx, pos)

	// 4. Post-Entry Settlement for Strategy C (Transfer Loop)
	if pos.Type == string(StrategyForeignTransfer) {
		switch pos.Status {
		case "STAGED_ASSASSINATION", "PRE_STAGED_LISTING", "AWAITING_TRAVEL_RULE":
			// Remain in staged state; RunPositionMonitor will track deposit opening / target surge / timeout
			log.Printf("🎯 [Staged Position] %s entered state %s (HedgeMode=%s)", pos.PositionID, pos.Status, pos.HedgeMode)
		case "OPEN":
			if pos.HedgeMode == "FOREIGN_FUTURES_HEDGE" || pos.HedgeMode == "FOREIGN_BORROW" {
				// ExecuteEntry already sold the arrived coin on Base and covered the Foreign short leg atomically
				pos.Status = "CLOSED"
				_ = e.db.UpdatePositionStatus(pos.PositionID, "CLOSED")
				e.persistPaperState()
				e.notifyPositionClosed(ctx, pos.PositionID)
			} else if pos.HedgeMarket == "SPOT" {
				log.Printf("⚡ [Auto-Repay] Strategy C SPOT transfer completed. Automatically triggering repayment...")
				_ = e.db.UpdatePositionStatus(pos.PositionID, "TRANSFERRING")
				e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(fmt.Sprintf(
					`{"action":"POSITION_TRANSFERRING", "position_id":"%s"}`, pos.PositionID,
				)))
				e.sendNotification(ctx, notification.AlertPositionOpened, fmt.Sprintf(
					"📦 **Coin Transfer Completed (Auto-Repay Started)**\n- **ID**: `%s`\n- **Exchanges**: `%s` -> `%s`",
					pos.PositionID, pos.ForeignExchange, pos.BaseExchange,
				))

				// Execute Exit (which for BASE_BORROW SPOT does the Repay and closes the debt)
				err := strat.ExecuteExit(ctx, e.executor, pair, *pos, 1.0, e.walletProxy)
				if err == nil {
					pos.Status = "CLOSED"
					_ = e.db.UpdatePositionStatus(pos.PositionID, "CLOSED")
					e.persistPaperState()
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
		}
	}

	return pos, nil
}

// RunPositionMonitor starts a background loop that listens to real-time price ticks
// and periodic timer ticks to evaluate if any open positions should be closed.
func (e *Engine) RunPositionMonitor(ctx context.Context, priceCh <-chan exchange.PriceUpdate) {
	log.Printf("[Node-%s] Starting Dual-Trigger Position Monitor...", e.nodeID)

	// Start per-user web lending session watchdogs and manual command bus listener bound to this user's context
	e.StartWebLendingWatchdogs(ctx)
	go e.ListenUserCommands(ctx)
	e.publishAccountState(ctx)

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
			if strings.EqualFold(string(quote.Symbol), "USDT-KRW") && quote.RawAsk > 0 {
				mid := quote.RawAsk
				if quote.RawBid > 0 {
					mid = (quote.RawBid + quote.RawAsk) / 2.0
				}
				SetExchangeTetherRate(quote.Exchange, "KRW", mid)
			}

			e.mu.Lock()
			if e.rawQuotes == nil {
				e.rawQuotes = make(map[string]exchange.PriceUpdate)
			}
			if e.latestQuotes == nil {
				e.latestQuotes = make(map[string]exchange.PriceUpdate)
			}
			e.rawQuotes[string(quote.Symbol)] = quote
			adjQuote := e.applySimOffsetToQuote(quote)
			e.latestQuotes[string(adjQuote.Symbol)] = adjQuote

			e.evaluateAllOpenPositions(ctx, e.latestQuotes)
			if !e.isDraining && e.botStatus == "RUNNING" {
				e.evaluateRebalancing(ctx, e.latestQuotes)
			}
			e.publishAccountState(ctx)
			e.mu.Unlock()
		case <-ticker.C:
			e.mu.Lock()
			// Re-apply gradual offset interpolation on raw quotes (never compounding!) as ramp time progresses
			if e.rawQuotes != nil {
				for sym, rawQ := range e.rawQuotes {
					e.latestQuotes[sym] = e.applySimOffsetToQuote(rawQ)
				}
			}
			e.evaluateAllOpenPositions(ctx, e.latestQuotes)
			if !e.isDraining && e.botStatus == "RUNNING" {
				e.evaluateRebalancing(ctx, e.latestQuotes)
			}
			e.publishAccountState(ctx)
			e.mu.Unlock()
		}
	}
}

// evaluateAllOpenPositions fetches the active DB positions assigned to this user and mode.
func (e *Engine) evaluateAllOpenPositions(
	ctx context.Context, quotes map[string]exchange.PriceUpdate) {
	positions, err := e.db.GetActivePositionsByUserAndMode(e.userID, e.isPaper)
	if err != nil {
		log.Printf("[Monitor Error] Could not fetch positions for User %d (%s): %v", e.userID, e.ModeString(), err)
		return
	}

	if e.botStatus == "DRAINING" && len(positions) == 0 {
		e.botStatus = "STOPPED"
		e.isDraining = true
		if settings, sErr := e.db.GetUserSettings(e.userID); sErr == nil && settings != nil {
			if e.isPaper {
				settings.PaperBotStatus = "STOPPED"
			} else {
				settings.LiveBotStatus = "STOPPED"
			}
			_ = e.db.SaveUserSettings(settings)
		}
		e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(fmt.Sprintf(
			`{"action":"BOT_DRAIN_COMPLETED","mode":"%s","message":"All open %s positions have been safely closed. Bot is now STOPPED."}`,
			e.ModeString(), e.ModeString(),
		)))
		return
	}

	for _, pos := range positions {
		targetPair := e.findPair(pos.BaseExchange, pos.ForeignExchange, pos.BaseSymbol)

		// 1. Handle PRE_STAGED_LISTING (Stage-1 coin in personal wallet awaiting Base deposit opening)
		if pos.Status == "PRE_STAGED_LISTING" {
			if targetPair != nil && (targetPair.IsValidForTransfer || targetPair.BaseMarketInfo.IsDepositEnabled) {
				if ft, ok := e.findStrategy(StrategyForeignTransfer).(*ForeignTransferStrategy); ok {
					posCopy := pos
					if err := ft.CompletePreStagedListingToBase(ctx, e.executor, *targetPair, &posCopy, e.walletProxy); err == nil {
						_ = e.db.UpdatePositionStatus(pos.PositionID, posCopy.Status)
						e.persistPaperState()
						e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(fmt.Sprintf(
							`{"action":"POSITION_STAGED_ASSASSINATION","position_id":"%s","message":"Stage-2 listing deposit arrived on %s. Awaiting trading surge!"}`,
							pos.PositionID, pos.BaseExchange,
						)))
					}
				}
			}
			continue
		}

		baseQuote, hasBase := quotes[pos.BaseSymbol]
		foreignQuote, hasForeign := quotes[pos.ForeignSymbol]

		// Skip evaluation until we have fresh data for both legs
		if !hasBase || !hasForeign {
			continue
		}

		// 2. Handle STAGED_ASSASSINATION (Coin pre-deposited on Base + hedged on Foreign, waiting for Kimp surge or timeout)
		if pos.Status == "STAGED_ASSASSINATION" {
			fxKRW := GetFXRate("KRW")
			tetherKRW := GetExchangeTetherRate(pos.BaseExchange, "KRW")
			baseBidKRW := baseQuote.RawBid
			if baseBidKRW <= 0 && baseQuote.BidUSD > 0 {
				baseBidKRW = baseQuote.BidUSD * fxKRW
			}
			foreignAskUSD := foreignQuote.AskUSD
			if foreignAskUSD <= 0 {
				foreignAskUSD = foreignQuote.RawAsk
			}

			var nominalKimp, realKimp float64
			if foreignAskUSD > 0 && fxKRW > 0 && baseBidKRW > 0 {
				nominalKimp = (baseBidKRW / (foreignAskUSD * fxKRW)) - 1.0
			}
			if foreignAskUSD > 0 && tetherKRW > 0 && baseBidKRW > 0 {
				realKimp = (baseBidKRW / (foreignAskUSD * tetherKRW)) - 1.0
			}

			targetExitDec := 0.045 // Default +4.5% Kimp target
			maxHoldDur := 12 * time.Hour
			if pos.AssassinationRationaleJSON != "" {
				var rat AssassinationRationale
				if json.Unmarshal([]byte(pos.AssassinationRationaleJSON), &rat) == nil && rat.TargetExitKimpPct > 0 {
					targetExitDec = rat.TargetExitKimpPct / 100.0
				}
			}

			// Check for emergency DELISTING event
			isDelisting := false
			coinSym := strings.Split(pos.BaseSymbol, "-")[0]
			if e.redisClient != nil && coinSym != "" {
				for _, ev := range e.redisClient.GetCoinAnnouncementEvents(ctx, coinSym) {
					if ev.EventType == "DELISTING" {
						isDelisting = true
						break
					}
				}
			}

			// Compute unrealized PnL while staged
			if e.liveUnrealizedPnLs == nil {
				e.liveUnrealizedPnLs = make(map[string]float64)
			}
			stagedUnrealizedUSD := (realKimp - 0.0025) * pos.InvestmentUSD
			e.liveUnrealizedPnLs[pos.BaseExchange+"|"+pos.BaseSymbol] = stagedUnrealizedUSD
			e.liveUnrealizedPnLs[pos.ForeignExchange+"|"+pos.ForeignSymbol] = stagedUnrealizedUSD

			shouldExitAssassination := realKimp >= targetExitDec ||
				nominalKimp >= targetExitDec ||
				time.Since(pos.OpenedAt) >= maxHoldDur ||
				isDelisting

			if shouldExitAssassination && targetPair != nil {
				targetStrategy := e.findStrategy(StrategyType(pos.Type))
				if targetStrategy != nil {
					log.Printf("🎯 [ASSASSINATION EXIT] %s | RealKimp=%.2f%% NominalKimp=%.2f%% Target=%.2f%% Delisting=%v",
						pos.PositionID, realKimp*100, nominalKimp*100, targetExitDec*100, isDelisting)
					if err := targetStrategy.ExecuteExit(ctx, e.executor, *targetPair, pos, 1.0, e.walletProxy); err == nil {
						_ = e.db.UpdatePositionStatus(pos.PositionID, "CLOSED")
						e.persistPaperState()
						e.notifyPositionClosed(ctx, pos.PositionID)
					}
				}
			}
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
				e.persistPaperState()
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
	Mode                string  `json:"mode,omitempty"`
	BaseExchange        string  `json:"base_exchange"`
	ForeignExchange     string  `json:"foreign_exchange"`
	BaseSymbol          string  `json:"base_symbol"`
	ForeignSymbol       string  `json:"foreign_symbol"`
	ActionScore         float64 `json:"action_score"`
	TargetWeight        float64 `json:"target_weight"`
	ActionType          int     `json:"action_type"`
	PremiumPct          float64 `json:"premium_pct"`
	PredictedPremiumPct float64 `json:"predicted_premium_pct"`
	MeanPremiumPct      float64 `json:"mean_premium_pct"`
	MaxPremiumPct       float64 `json:"max_premium_pct"`
	MinPremiumPct       float64 `json:"min_premium_pct"`
	TargetOffsetMin     int     `json:"target_offset_min"`
	MaxOffsetMin        int     `json:"max_offset_min"`
	MinOffsetMin        int     `json:"min_offset_min"`
	Direction           string  `json:"direction"`
	Timestamp           float64 `json:"timestamp"`
}

// computePairFeeRates dynamically fetches and computes effective trading and withdrawal fee rates
// (combining both percentage and fixed fees for the given investmentUSD).
func (e *Engine) computePairFeeRates(ctx context.Context, pair *CrossMarketPair, investmentUSD float64, needs2Hop bool) (baseOneWayFee, foreignOneWayFee, roundTripReversionFee, oneWayTransferFee, networkWithdrawFeeRate float64) {
	if investmentUSD <= 0 {
		investmentUSD = 10000.0
	}
	baseOneWayFee = 0.0005
	foreignOneWayFee = 0.0020
	if pair == nil {
		return baseOneWayFee, foreignOneWayFee, 2.0 * (baseOneWayFee + foreignOneWayFee), baseOneWayFee + foreignOneWayFee, 0.0005
	}

	baseFX := e.getExchangeCurrencyFX(pair.BaseClient.GetName(), string(pair.BaseMarketInfo.Symbol))
	foreignFX := e.getExchangeCurrencyFX(pair.ForeignClient.GetName(), string(pair.ForeignMarketInfo.Symbol))

	if bf, err := pair.BaseClient.GetTradingFee(ctx, pair.BaseMarketInfo.Symbol); err == nil {
		if eff := bf.EffectiveTakerFeeRate(investmentUSD * baseFX); eff > 0 {
			baseOneWayFee = eff
		}
	}
	if ff, err := pair.ForeignClient.GetTradingFee(ctx, pair.ForeignMarketInfo.Symbol); err == nil {
		if eff := ff.EffectiveTakerFeeRate(investmentUSD * foreignFX); eff > 0 {
			foreignOneWayFee = eff
		}
	}

	roundTripReversionFee = 2.0 * (baseOneWayFee + foreignOneWayFee)
	oneWayTransferFee = baseOneWayFee + foreignOneWayFee

	var coinPriceUSD float64
	if e.latestQuotes != nil {
		if fq, ok := e.latestQuotes[string(pair.ForeignMarketInfo.Symbol)]; ok {
			coinPriceUSD = fq.AskUSD
			if coinPriceUSD <= 0 && fq.RawAsk > 0 && foreignFX > 0 {
				coinPriceUSD = fq.RawAsk / foreignFX
			}
		}
	}
	if coinPriceUSD <= 0 {
		if px, err := pair.ForeignClient.GetTicker(ctx, pair.ForeignMarketInfo.Symbol); err == nil && px > 0 && foreignFX > 0 {
			coinPriceUSD = px / foreignFX
		}
	}

	withdrawCoins := pair.ForeignNetwork.WithdrawFeeFixed
	if withdrawCoins <= 0 {
		withdrawCoins, _ = exchange.DefaultNetworkSpec(pair.ForeignClient.GetName(), pair.BaseCurrency)
	}
	withdrawFeeUSD := (withdrawCoins * coinPriceUSD) + (investmentUSD * pair.ForeignNetwork.WithdrawFeeRate)
	if needs2Hop {
		withdrawFeeUSD += 2.50
	}
	networkWithdrawFeeRate = withdrawFeeUSD / math.Max(investmentUSD, 100.0)
	return
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
			var aiOps []AIPredictedOpportunity
			if err := json.Unmarshal(payload, &aiOps); err != nil {
				log.Printf("[Engine-%d] Failed to parse AI opportunities: %v", e.userID, err)
				continue
			}

			// 1. Filter by UserID and Mode ("PAPER" vs "LIVE")
			var filteredOps []AIPredictedOpportunity
			for _, opp := range aiOps {
				if opp.UserID != 0 && uint(opp.UserID) != e.userID {
					continue
				}
				if opp.Mode != "" && !strings.EqualFold(opp.Mode, e.ModeString()) {
					continue
				}
				filteredOps = append(filteredOps, opp)
			}

			logDetails := time.Since(lastLogTime) > 10*time.Second
			if logDetails {
				log.Printf("[%sEngine] Received %d AI predictions from Redis. Filtered for user %d: %d.",
					e.ModeString(), len(aiOps), e.userID, len(filteredOps))
				lastLogTime = time.Now()
			}

			if len(filteredOps) == 0 {
				continue
			}

			e.mu.Lock()

			activePos, _ := e.db.GetActivePositionsByUserAndMode(e.userID, e.isPaper)

			// Apply Paper Mode gradual price offset scenario (±N% over X minutes) to opportunities
			if e.isPaper {
				simEx, currPct, _, _, _, _ := e.GetSimStatus()
				if math.Abs(currPct) > 1e-6 {
					offsetDec := currPct / 100.0
					for i := range filteredOps {
						if strings.EqualFold(filteredOps[i].BaseExchange, simEx) {
							filteredOps[i].PremiumPct = (1.0+filteredOps[i].PremiumPct)*(1.0+offsetDec) - 1.0
							filteredOps[i].PredictedPremiumPct = (1.0+filteredOps[i].PredictedPremiumPct)*(1.0+offsetDec*0.5) - 1.0
							filteredOps[i].MeanPremiumPct = (1.0+filteredOps[i].MeanPremiumPct)*(1.0+offsetDec*0.5) - 1.0
							filteredOps[i].MaxPremiumPct = (1.0+filteredOps[i].MaxPremiumPct)*(1.0+offsetDec*0.5) - 1.0
							filteredOps[i].MinPremiumPct = (1.0+filteredOps[i].MinPremiumPct)*(1.0+offsetDec*0.5) - 1.0
						} else if strings.EqualFold(filteredOps[i].ForeignExchange, simEx) {
							filteredOps[i].PremiumPct = (1.0+filteredOps[i].PremiumPct)/(1.0+offsetDec) - 1.0
							filteredOps[i].PredictedPremiumPct = (1.0+filteredOps[i].PredictedPremiumPct)/(1.0+offsetDec*0.5) - 1.0
							filteredOps[i].MeanPremiumPct = (1.0+filteredOps[i].MeanPremiumPct)/(1.0+offsetDec*0.5) - 1.0
							filteredOps[i].MaxPremiumPct = (1.0+filteredOps[i].MaxPremiumPct)/(1.0+offsetDec*0.5) - 1.0
							filteredOps[i].MinPremiumPct = (1.0+filteredOps[i].MinPremiumPct)/(1.0+offsetDec*0.5) - 1.0
						}

						var hasCanonicalPos, hasReversePos bool
						for _, pos := range activePos {
							if pos.Status == "CLOSED" {
								continue
							}
							if pos.BaseExchange == filteredOps[i].BaseExchange &&
								pos.ForeignExchange == filteredOps[i].ForeignExchange &&
								pos.BaseSymbol == filteredOps[i].BaseSymbol {
								hasCanonicalPos = true
							} else if pos.BaseExchange == filteredOps[i].ForeignExchange &&
								pos.ForeignExchange == filteredOps[i].BaseExchange &&
								pos.BaseSymbol == filteredOps[i].ForeignSymbol {
								hasReversePos = true
							}
						}

						// Reflect offset-driven arbitrage edge in target weight & action type for realistic simulation
						minTargetDec := e.config.MinTargetProfit / 100.0
						if minTargetDec <= 0 {
							minTargetDec = 0.01
						}
						if filteredOps[i].PremiumPct >= minTargetDec {
							// Positive Kimchi Premium >= MinTargetProfit: Canonical direction (Base Short / Foreign Long)
							filteredOps[i].ActionScore = 2.0
							filteredOps[i].Direction = "NARROWING"
							filteredOps[i].ActionType = 0
							if filteredOps[i].TargetWeight < 0.20 {
								filteredOps[i].TargetWeight = 0.25
							}
						} else if filteredOps[i].PremiumPct <= -minTargetDec {
							// Reverse Premium (역프) <= -MinTargetProfit:
							// 1. If a canonical position (UPBIT -> COINBASE) is open, close it first to lock in profit.
							// 2. Otherwise, flip direction to WIDENING (COINBASE Short / UPBIT Long) and hold until convergence.
							if hasCanonicalPos {
								filteredOps[i].ActionType = 2
								filteredOps[i].TargetWeight = 0.0
							} else {
								filteredOps[i].ActionScore = 8.0
								filteredOps[i].Direction = "WIDENING"
								filteredOps[i].ActionType = 0
								if filteredOps[i].TargetWeight < 0.20 {
									filteredOps[i].TargetWeight = 0.25
								}
							}
						} else {
							// Spread is inside the neutral band (-minTargetDec, +minTargetDec)
							if hasCanonicalPos && filteredOps[i].PremiumPct <= 0.0015 {
								filteredOps[i].ActionType = 2
								filteredOps[i].TargetWeight = 0.0
							} else if hasReversePos && filteredOps[i].PremiumPct >= -0.0015 {
								filteredOps[i].ActionType = 2
								filteredOps[i].TargetWeight = 0.0
							} else if !hasCanonicalPos && !hasReversePos {
								// Prevent opening new positions when spread is inside the neutral band
								filteredOps[i].ActionType = 0
								filteredOps[i].TargetWeight = 0.0
							}
						}
					}
				}
			}

			// 2. Clamp individual weights and only renormalize (scale down) when totalWeight > 1.0
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

			// 3. Execution Pipeline and ActionType Handling (Only execute when BotStatus is RUNNING or DRAINING)
			if e.botStatus != "STOPPED" {
				for _, opp := range filteredOps {
					targetDollarVolume := e.config.CapitalLimitUsd * opp.TargetWeight

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
							log.Printf("🚨 [AI REPAY TRIGGERED] (%s) User %d %s, closing position %s",
								e.ModeString(), e.userID, opp.BaseSymbol, matchingPos.PositionID)

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
									continue
								}
								exitRatio := 1.0
								err := targetStrategy.ExecuteExit(ctx, e.executor, *targetPair, *matchingPos, exitRatio, e.walletProxy)
								if err == nil {
									e.db.UpdatePositionStatus(matchingPos.PositionID, "CLOSED")
									e.persistPaperState()
									e.notifyPositionClosed(ctx, matchingPos.PositionID)
								} else {
									if strings.Contains(err.Error(), "ERR_SESSION_EXPIRED") {
										_ = e.db.UpdatePositionStatus(matchingPos.PositionID, "AWAITING_REPAY")
									}
								}
							}
						}
						continue
					}

					// --- ActionType = 1 (TRANSFER / 전송 및 상환) ---
					if opp.ActionType == 1 {
						if matchingPos != nil && matchingPos.Status == "OPEN" {
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
								_ = e.db.UpdatePositionStatus(matchingPos.PositionID, "TRANSFERRING")
								e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(fmt.Sprintf(
									`{"action":"POSITION_TRANSFERRING", "position_id":"%s", "mode":"%s"}`, matchingPos.PositionID, e.ModeString(),
								)))
								e.publishAccountState(ctx)

								if err := transferStrat.ExecuteTransferAndRepay(ctx, e.executor, *posPair, *matchingPos, e.walletProxy); err == nil {
									_ = e.db.UpdatePositionStatus(matchingPos.PositionID, "CLOSED")
									e.persistPaperState()
									e.notifyPositionClosed(ctx, matchingPos.PositionID)
								} else {
									if strings.Contains(err.Error(), "ERR_SESSION_EXPIRED") {
										_ = e.db.UpdatePositionStatus(matchingPos.PositionID, "AWAITING_REPAY")
									} else {
										_ = e.db.UpdatePositionStatus(matchingPos.PositionID, "OPEN")
									}
								}
							}
						}
						continue
					}

					// Do not open or scale-in new positions if in DRAINING mode
					if e.isDraining || e.botStatus != "RUNNING" {
						continue
					}

					// --- ActionType = 0 (TRADE / 진입 및 리밸런싱 / Two-Wave 스위칭) ---
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

					toleranceAmount := e.config.CapitalLimitUsd * 0.02

					// Two-Wave Flip: If an open position exists in the OPPOSITE direction of the new AI signal,
					// immediately close the existing position to lock in Wave 1 profit before entering Wave 2.
					if matchingPos != nil && matchingPos.Status == "OPEN" && matchingPos.BaseExchange != entryBaseEx && targetDollarVolume > toleranceAmount {
						log.Printf("🔄 [AI TWO-WAVE FLIP] (%s) User %d flipping %s from (%s -> %s) to (%s -> %s)",
							e.ModeString(), e.userID, opp.BaseSymbol,
							matchingPos.BaseExchange, matchingPos.ForeignExchange,
							entryBaseEx, entryForeignEx)

						flipStrategy := e.findStrategy(StrategyType(matchingPos.Type))
						flipPair := e.findPair(matchingPos.BaseExchange, matchingPos.ForeignExchange, matchingPos.BaseSymbol)
						if flipStrategy != nil && flipPair != nil {
							if err := flipStrategy.ExecuteExit(ctx, e.executor, *flipPair, *matchingPos, 1.0, e.walletProxy); err == nil {
								_ = e.db.UpdatePositionStatus(matchingPos.PositionID, "CLOSED")
								e.persistPaperState()
								e.notifyPositionClosed(ctx, matchingPos.PositionID)
								matchingPos = nil
								currentExposure = 0.0
							}
						}
					}

					delta := targetDollarVolume - currentExposure

					if delta > toleranceAmount {
						targetPair := e.findPair(entryBaseEx, entryForeignEx, entryBaseSym)
						if targetPair == nil {
							targetPair = e.findPair(opp.BaseExchange, opp.ForeignExchange, opp.BaseSymbol)
						}

						if matchingPos == nil {
							if targetPair != nil {
								var selectedStrategy StrategyType = StrategyMarginSpot
								isBaseFuture := targetPair.BaseMarketInfo.MarketType == "FUTURE"
								isForeignFuture := targetPair.ForeignMarketInfo.MarketType == "FUTURE"
								isReverseDirection := opp.ActionScore > 5.0

								// Check active announcement events for this coin
								coinSym := strings.Split(entryBaseSym, "-")[0]
								var activeEvType, activeEvTitle string
								if e.redisClient != nil && coinSym != "" {
									if evs := e.redisClient.GetCoinAnnouncementEvents(ctx, coinSym); len(evs) > 0 {
										activeEvType = evs[0].EventType
										activeEvTitle = evs[0].Title
									}
								}
								if activeEvType == "DELISTING" {
									// Strictly block new entry on delisting announcement
									continue
								}

								// Compute Real Tether Kimp & quantitative Strategy C NetROI (including VASP Tier 3 2-hop fee if applicable)
								fxKRW := GetFXRate("KRW")
								tetherKRW := GetExchangeTetherRate(targetPair.BaseClient.GetName(), string(targetPair.BaseMarketInfo.QuoteCurrency))
								realEntrySpread := entrySpread
								if fxKRW > 0 && tetherKRW > 0 {
									realEntrySpread = ((1.0 + entrySpread) * fxKRW / tetherKRW) - 1.0
								}
								vaspTier := GetVASPTier(targetPair.BaseClient.GetName(), targetPair.ForeignClient.GetName())
								needs2Hop := vaspTier == exchange.VASPTier3RiskAssessed && e.walletProxy != nil

								_, _, roundTripReversionFee, oneWayTransferFee, networkWithdrawFeeRate := e.computePairFeeRates(ctx, targetPair, targetDollarVolume, needs2Hop)
								reversionNetROI := (entrySpread - entryPredictedSpread) - roundTripReversionFee
								feeEstimate := oneWayTransferFee + networkWithdrawFeeRate

								transferNetROI := realEntrySpread - feeEstimate

								stageAssassination := opp.ActionType == 3 ||
									((activeEvType == "INVESTMENT_WARNING" || activeEvType == "WALLET_SUSPENSION" || activeEvType == "NEW_KRW_LISTING") &&
										realEntrySpread < 0.030 && opp.MaxPremiumPct >= 0.040)
								preStageListing := activeEvType == "NEW_KRW_LISTING" && !targetPair.IsValidForTransfer && e.walletProxy != nil

								if isBaseFuture && isForeignFuture {
									selectedStrategy = StrategyFutureFuture
								} else if isBaseFuture {
									selectedStrategy = StrategyFutureSpot
								} else {
									// Quantitative NetROI comparison: Strategy C wins if its NetROI (even after 2-hop fees) beats Strategy A/B, or if assassination staging is triggered
									if !isReverseDirection && (preStageListing || (targetPair.IsValidForTransfer && (stageAssassination || (transferNetROI >= reversionNetROI && transferNetROI > 0.0005)))) {
										selectedStrategy = StrategyForeignTransfer
									} else if isForeignFuture {
										selectedStrategy = StrategyMarginFuture
									} else {
										selectedStrategy = StrategyMarginSpot
									}
								}

								expectedBestNetROI := reversionNetROI
								if selectedStrategy == StrategyForeignTransfer {
									expectedBestNetROI = transferNetROI
								}
								if !stageAssassination && !preStageListing && expectedBestNetROI <= 0.0005 {
									// Strictly block opening a position (Strategy A/B/C/D/E) when expected return after real exchange fees is negative or sub-threshold
									continue
								}

								targetStrategy := e.findStrategy(selectedStrategy)
								if targetStrategy != nil {
									eval := targetStrategy.GetLastEvaluation()
									eval.Type = selectedStrategy
									eval.EntryPremium = entrySpread
									eval.TetherRealPremiumPct = realEntrySpread
									eval.BasisReturn = entrySpread - entryPredictedSpread
									eval.InvestmentUSD = targetDollarVolume
									eval.TradingFees = oneWayTransferFee
									eval.TransferFees = networkWithdrawFeeRate
									eval.HedgeMarket = "SPOT"
									if isForeignFuture {
										eval.HedgeMarket = "FUTURE"
									}
									eval.StageAssassination = stageAssassination
									eval.PreStageListing = preStageListing
									eval.EventType = activeEvType
									eval.EventTitle = activeEvTitle
									eval.VASPTier = vaspTier
									eval.NeedsWalletProxy = needs2Hop
									targetStrategy.SetEvaluation(eval)
									_, err := e.ExecuteStrategy(ctx, targetStrategy, *targetPair)
									if err != nil {
										errMsg := err.Error()
										isInsuff := strings.Contains(strings.ToLower(errMsg), "insufficient") ||
											strings.Contains(strings.ToLower(errMsg), "skew protection")
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
											log.Printf("❌ [AI ENTRY FAILED] (%s) User %d %s via %s: %v", e.ModeString(), e.userID, entryBaseSym, selectedStrategy, err)
										}
									} else {
										log.Printf("🚀 [AI ENTRY TRIGGERED] (%s) User %d %s via %s, Target Vol: $%.2f", e.ModeString(), e.userID, entryBaseSym, selectedStrategy, targetDollarVolume)
									}
								}
							}
						} else {
							targetStrategy := e.findStrategy(StrategyType(matchingPos.Type))
							posPair := e.findPair(matchingPos.BaseExchange, matchingPos.ForeignExchange, matchingPos.BaseSymbol)
							if targetStrategy != nil && posPair != nil {
								prevBase := matchingPos.BaseAmount
								prevForeign := matchingPos.ForeignAmount
								if err := targetStrategy.ExecuteScale(ctx, e.executor, *posPair, matchingPos, delta, e.walletProxy); err == nil {
									addedBase := matchingPos.BaseAmount - prevBase
									addedForeign := matchingPos.ForeignAmount - prevForeign
									_ = e.db.AddPositionAmount(matchingPos.PositionID, delta, addedBase, addedForeign)
									e.persistPaperState()
								}
							}
						}
					} else if delta < -toleranceAmount && opp.TargetWeight > 1e-6 {
						// Only scale down partially when TargetWeight > 0; full 100% position closure is handled explicitly by opp.ActionType == 2
						if matchingPos != nil && currentExposure > 0 {
							exitRatio := math.Min(0.95, (-delta)/currentExposure)
							targetStrategy := e.findStrategy(StrategyType(matchingPos.Type))
							posPair := e.findPair(matchingPos.BaseExchange, matchingPos.ForeignExchange, matchingPos.BaseSymbol)
							if targetStrategy != nil && posPair != nil {
								if err := targetStrategy.ExecuteExit(ctx, e.executor, *posPair, *matchingPos, exitRatio, e.walletProxy); err == nil {
									reducedUsd := matchingPos.InvestmentUSD * exitRatio
									reducedBase := matchingPos.BaseAmount * exitRatio
									reducedForeign := matchingPos.ForeignAmount * exitRatio
									_ = e.db.ReducePositionAmount(matchingPos.PositionID, reducedUsd, reducedBase, reducedForeign)
									e.persistPaperState()
								}
							}
						}
					}
				}
			}

			// 4. Publish Filtered Opportunities back to user's dashboard Tab
			type FilteredOppDTO struct {
				BaseExchange        string  `json:"base_exchange"`
				ForeignExchange     string  `json:"foreign_exchange"`
				BaseSymbol          string  `json:"base_symbol"`
				ForeignSymbol       string  `json:"foreign_symbol"`
				Strategy            string  `json:"strategy"`
				Direction           string  `json:"direction"`
				ActionScore         float64 `json:"action_score"`
				ExpectedRoi         float64 `json:"expected_roi"`
				CurrentPremiumPct   float64 `json:"current_premium_pct"`
				PredictedPremiumPct float64 `json:"predicted_premium_pct"`
				MeanPremiumPct      float64 `json:"mean_premium_pct"`
				MaxPremiumPct       float64 `json:"max_premium_pct"`
				MinPremiumPct       float64 `json:"min_premium_pct"`
				TargetOffsetMin     int     `json:"target_offset_min"`
				MaxOffsetMin        int     `json:"max_offset_min"`
				MinOffsetMin        int     `json:"min_offset_min"`
				TargetWeight        float64 `json:"target_weight"`
				RiskScore           float64 `json:"risk_score"`
				InvestmentUsd       float64 `json:"investment_usd"`
				IsValid             bool    `json:"is_valid"`
			}

			var dtoList []FilteredOppDTO
			for _, opp := range filteredOps {
				targetPair := e.findPair(opp.BaseExchange, opp.ForeignExchange, opp.BaseSymbol)
				if targetPair != nil {
					isBaseFuture := targetPair.BaseMarketInfo.MarketType == "FUTURE"
					isForeignFuture := targetPair.ForeignMarketInfo.MarketType == "FUTURE"

					direction := opp.Direction
					if direction == "" {
						if opp.ActionScore > 5.0 || opp.PredictedPremiumPct > opp.PremiumPct {
							direction = "WIDENING"
						} else {
							direction = "NARROWING"
						}
					}
					directionalSpreadMove := math.Abs(opp.PremiumPct - opp.PredictedPremiumPct)
					meanPct := opp.MeanPremiumPct * 100.0
					if opp.MeanPremiumPct == 0 {
						meanPct = opp.PredictedPremiumPct * 100.0
					}

					investVolUSD := e.config.CapitalLimitUsd * opp.TargetWeight
					vaspTierDTO := GetVASPTier(targetPair.BaseClient.GetName(), targetPair.ForeignClient.GetName())
					needs2HopDTO := vaspTierDTO == exchange.VASPTier3RiskAssessed && e.walletProxy != nil
					_, _, roundTripReversionFee, oneWayTransferFee, networkWithdrawFeeRate := e.computePairFeeRates(ctx, targetPair, investVolUSD, needs2HopDTO)

					if isBaseFuture && isForeignFuture {
						// Strategy E: Future-Future
						dtoList = append(dtoList, FilteredOppDTO{
							BaseExchange:        opp.BaseExchange,
							ForeignExchange:     opp.ForeignExchange,
							BaseSymbol:          opp.BaseSymbol,
							ForeignSymbol:       opp.ForeignSymbol,
							Strategy:            "Future-Future (E)",
							Direction:           direction,
							ActionScore:         opp.ActionScore,
							ExpectedRoi:         directionalSpreadMove - roundTripReversionFee,
							CurrentPremiumPct:   opp.PremiumPct * 100.0,
							PredictedPremiumPct: opp.PredictedPremiumPct * 100.0,
							MeanPremiumPct:      meanPct,
							MaxPremiumPct:       opp.MaxPremiumPct * 100.0,
							MinPremiumPct:       opp.MinPremiumPct * 100.0,
							TargetOffsetMin:     opp.TargetOffsetMin,
							MaxOffsetMin:        opp.MaxOffsetMin,
							MinOffsetMin:        opp.MinOffsetMin,
							TargetWeight:        opp.TargetWeight,
							RiskScore:           15.0,
							InvestmentUsd:       investVolUSD,
							IsValid:             true,
						})
					} else if isBaseFuture {
						// Strategy D: Future-Spot
						dtoList = append(dtoList, FilteredOppDTO{
							BaseExchange:        opp.BaseExchange,
							ForeignExchange:     opp.ForeignExchange,
							BaseSymbol:          opp.BaseSymbol,
							ForeignSymbol:       opp.ForeignSymbol,
							Strategy:            "Future-Spot (D)",
							Direction:           direction,
							ActionScore:         opp.ActionScore,
							ExpectedRoi:         directionalSpreadMove - roundTripReversionFee,
							CurrentPremiumPct:   opp.PremiumPct * 100.0,
							PredictedPremiumPct: opp.PredictedPremiumPct * 100.0,
							MeanPremiumPct:      meanPct,
							MaxPremiumPct:       opp.MaxPremiumPct * 100.0,
							MinPremiumPct:       opp.MinPremiumPct * 100.0,
							TargetOffsetMin:     opp.TargetOffsetMin,
							MaxOffsetMin:        opp.MaxOffsetMin,
							MinOffsetMin:        opp.MinOffsetMin,
							TargetWeight:        opp.TargetWeight,
							RiskScore:           18.0,
							InvestmentUsd:       investVolUSD,
							IsValid:             true,
						})
					} else {
						// Base is Spot (Margin) - requires margin support on base exchange
						if targetPair.BaseClient.SupportsMargin() {
							// 1. Holding Strategy: Margin-Spot (A) or Margin-Future (B)
							strategyHoldingName := "Margin-Spot (A)"
							if isForeignFuture {
								strategyHoldingName = "Margin-Future (B)"
							}
							dtoList = append(dtoList, FilteredOppDTO{
								BaseExchange:        opp.BaseExchange,
								ForeignExchange:     opp.ForeignExchange,
								BaseSymbol:          opp.BaseSymbol,
								ForeignSymbol:       opp.ForeignSymbol,
								Strategy:            strategyHoldingName,
								Direction:           direction,
								ActionScore:         opp.ActionScore,
								ExpectedRoi:         directionalSpreadMove - roundTripReversionFee,
								CurrentPremiumPct:   opp.PremiumPct * 100.0,
								PredictedPremiumPct: opp.PredictedPremiumPct * 100.0,
								MeanPremiumPct:      meanPct,
								MaxPremiumPct:       opp.MaxPremiumPct * 100.0,
								MinPremiumPct:       opp.MinPremiumPct * 100.0,
								TargetOffsetMin:     opp.TargetOffsetMin,
								MaxOffsetMin:        opp.MaxOffsetMin,
								MinOffsetMin:        opp.MinOffsetMin,
								TargetWeight:        opp.TargetWeight,
								RiskScore:           18.0,
								InvestmentUsd:       investVolUSD,
								IsValid:             true,
							})

							// 2. Transfer Loop Strategy: Transfer Loop (C)
							if targetPair.IsValidForTransfer {
								fxKRW := GetFXRate("KRW")
								tetherKRW := GetExchangeTetherRate(targetPair.BaseClient.GetName(), string(targetPair.BaseMarketInfo.QuoteCurrency))
								roiTransfer := opp.PremiumPct
								if fxKRW > 0 && tetherKRW > 0 {
									roiTransfer = ((1.0 + opp.PremiumPct) * fxKRW / tetherKRW) - 1.0
								}
								
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
									BaseExchange:        opp.BaseExchange,
									ForeignExchange:     opp.ForeignExchange,
									BaseSymbol:          opp.BaseSymbol,
									ForeignSymbol:       opp.ForeignSymbol,
									Strategy:            "Transfer Loop (C)",
									Direction:           "TRANSFER",
									ActionScore:         opp.ActionScore,
									ExpectedRoi:         roiTransfer - (oneWayTransferFee + networkWithdrawFeeRate),
									CurrentPremiumPct:   opp.PremiumPct * 100.0,
									PredictedPremiumPct: opp.PredictedPremiumPct * 100.0,
									MeanPremiumPct:      meanPct,
									MaxPremiumPct:       opp.MaxPremiumPct * 100.0,
									MinPremiumPct:       opp.MinPremiumPct * 100.0,
									TargetOffsetMin:     opp.TargetOffsetMin,
									MaxOffsetMin:        opp.MaxOffsetMin,
									MinOffsetMin:        opp.MinOffsetMin,
									TargetWeight:        opp.TargetWeight,
									RiskScore:           totalRisk,
									InvestmentUsd:       investVolUSD,
									IsValid:             true,
								})
							}
						}
					}
				}
			}

			filteredPayload, err := json.Marshal(dtoList)
			if err == nil {
				e.redisClient.PublishUserOpportunitiesByMode(ctx, fmt.Sprint(e.userID), e.ModeString(), filteredPayload)
			}
			e.mu.Unlock()
		}
	}
}

// ExchangeBalanceSnapshot represents the real-time balance and skew share of a single connected exchange.
type ExchangeBalanceSnapshot struct {
	Name            string  `json:"name"`
	QuoteCurrency   string  `json:"quote_currency"`
	AvailableNative float64 `json:"available_native"`
	AvailableUSD    float64 `json:"available_usd"`
	Percent         float64 `json:"percent"`
	WebLendingOK    bool    `json:"web_lending_ok"`
}

// UserCommand represents a manual action sent from the frontend via the Node API server over Redis.
type UserCommand struct {
	Action             string  `json:"action"`               // "FORCE_ENTER" | "FORCE_CLOSE" | "SET_BOT_STATUS" | "SET_SIM_OFFSET" | "RESET_PAPER_ACCOUNT" | "SIM_EVENT"
	Mode               string  `json:"mode,omitempty"`       // "PAPER" | "LIVE"
	SubAction          string  `json:"sub_action,omitempty"` // "RUN" | "DRAIN" | "STOP_AND_CLOSE"
	PositionID         string  `json:"position_id,omitempty"`
	BaseExchange       string  `json:"base_exchange,omitempty"`
	ForeignExchange    string  `json:"foreign_exchange,omitempty"`
	BaseSymbol         string  `json:"base_symbol,omitempty"`
	ForeignSymbol      string  `json:"foreign_symbol,omitempty"`
	Strategy           string  `json:"strategy,omitempty"`
	ExpectedRoi        float64 `json:"expected_roi,omitempty"`
	InvestmentUsd      float64 `json:"investment_usd,omitempty"`
	StageAssassination bool    `json:"stage_assassination,omitempty"`
	PreStageListing    bool    `json:"pre_stage_listing,omitempty"`
	EventType          string  `json:"event_type,omitempty"`
	EventTitle         string  `json:"event_title,omitempty"`
	ScheduledOffsetMin int     `json:"scheduled_offset_min,omitempty"`
	SimExchange        string  `json:"sim_exchange,omitempty"`
	SimTargetOffsetPct float64 `json:"sim_target_offset_pct,omitempty"`
	SimRampMinutes     float64 `json:"sim_ramp_minutes,omitempty"`
	PaperUpbitKRW      float64 `json:"paper_upbit_krw,omitempty"`
	PaperCoinbaseUSD   float64 `json:"paper_coinbase_usd,omitempty"`
	UpbitKRW           float64 `json:"upbit_krw,omitempty"`
	CoinbaseUSD        float64 `json:"coinbase_usd,omitempty"`
}

// ListenUserCommands subscribes to user:{id}:cmd so manual UI commands ("Force Enter", "Force Close", "Set Bot Status", "Set Sim Offset", "Reset Paper")
// execute directly inside the user's targeted Engine goroutine (Paper or Live).
func (e *Engine) ListenUserCommands(ctx context.Context) {
	if e.redisClient == nil {
		return
	}
	cmdCh := make(chan []byte, 16)
	go e.redisClient.SubscribeUserCommands(ctx, fmt.Sprint(e.userID), cmdCh)

	for {
		select {
		case <-ctx.Done():
			return
		case rawCmd := <-cmdCh:
			var cmd UserCommand
			if err := json.Unmarshal(rawCmd, &cmd); err != nil {
				log.Printf("[Engine-%d] Invalid user command payload: %v", e.userID, err)
				continue
			}

			// Route command only to the matching engine mode ("PAPER" vs "LIVE") if specified
			if cmd.Mode != "" && !strings.EqualFold(cmd.Mode, e.ModeString()) {
				continue
			}

			e.mu.Lock()
			switch cmd.Action {
			case "SET_BOT_STATUS":
				switch strings.ToUpper(cmd.SubAction) {
				case "RUN":
					e.botStatus = "RUNNING"
					e.isDraining = false
					e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(fmt.Sprintf(
						`{"action":"BOT_STATUS_CHANGED","mode":"%s","status":"RUNNING","message":"%s Auto-Trading is now RUNNING."}`,
						e.ModeString(), e.ModeString(),
					)))
				case "DRAIN":
					e.botStatus = "DRAINING"
					e.isDraining = true
					e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(fmt.Sprintf(
						`{"action":"BOT_STATUS_CHANGED","mode":"%s","status":"DRAINING","message":"%s Bot entered DRAIN mode: existing positions will be monitored until exit, no new entries."}`,
						e.ModeString(), e.ModeString(),
					)))
				case "STOP_AND_CLOSE":
					e.botStatus = "STOPPED"
					e.isDraining = true
					positions, _ := e.db.GetActivePositionsByUserAndMode(e.userID, e.isPaper)
					for _, pos := range positions {
						if pos.Status == "CLOSED" {
							continue
						}
						targetStrategy := e.findStrategy(StrategyType(pos.Type))
						targetPair := e.findPair(pos.BaseExchange, pos.ForeignExchange, pos.BaseSymbol)
						if targetStrategy != nil && targetPair != nil {
							_ = targetStrategy.ExecuteExit(ctx, e.executor, *targetPair, pos, 1.0, e.walletProxy)
						}
						_ = e.db.UpdatePositionStatus(pos.PositionID, "CLOSED")
						e.notifyPositionClosed(ctx, pos.PositionID)
					}
					e.persistPaperState()
					e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(fmt.Sprintf(
						`{"action":"BOT_STATUS_CHANGED","mode":"%s","status":"STOPPED","message":"%s Bot STOPPED and all open positions were closed."}`,
						e.ModeString(), e.ModeString(),
					)))
				default:
					e.botStatus = "STOPPED"
					e.isDraining = true
				}

			case "SET_SIM_OFFSET":
				if e.isPaper {
					_, currPct, _, _, _, _ := e.GetSimStatus()
					if cmd.SimExchange != "" {
						e.simExchange = strings.ToUpper(cmd.SimExchange)
					}
					e.simStartOffsetPct = currPct
					e.simTargetOffsetPct = cmd.SimTargetOffsetPct
					e.simRampMinutes = cmd.SimRampMinutes
					e.simStartedAt = time.Now().Unix()
					if e.rawQuotes != nil {
						for sym, rawQ := range e.rawQuotes {
							e.latestQuotes[sym] = e.applySimOffsetToQuote(rawQ)
						}
					}
					e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(fmt.Sprintf(
						`{"action":"SIM_OFFSET_UPDATED","mode":"PAPER","message":"Paper Simulator: %s price ramping from %.2f%% to %.2f%% over %.1f min."}`,
						e.simExchange, e.simStartOffsetPct, e.simTargetOffsetPct, e.simRampMinutes,
					)))
				}

			case "RESET_PAPER_ACCOUNT":
				if e.isPaper {
					_ = e.db.CloseAllActivePositionsByUserAndMode(e.userID, true)
					upbitKRW := cmd.PaperUpbitKRW
					if upbitKRW <= 0 && cmd.UpbitKRW > 0 {
						upbitKRW = cmd.UpbitKRW
					}
					cbUSD := cmd.PaperCoinbaseUSD
					if cbUSD <= 0 && cmd.CoinbaseUSD > 0 {
						cbUSD = cmd.CoinbaseUSD
					}
					if upbitKRW <= 0 && cbUSD <= 0 {
						upbitKRW = 50000000.0
						cbUSD = 35000.0
					}
					for _, c := range e.clients {
						if pc, ok := c.(*exchange.PaperClient); ok {
							if strings.EqualFold(pc.GetName(), "UPBIT") {
								pc.ResetBalances(map[string]float64{"KRW": upbitKRW, "BTC": 0.0, "ETH": 0.0})
							} else {
								pc.ResetBalances(map[string]float64{"USD": cbUSD, "BTC": 0.0, "ETH": 0.0})
							}
						}
					}
					e.liveUnrealizedPnLs = make(map[string]float64)
					e.persistPaperState()
					e.resetSnapshotPending = true
					e.forceNextSnapshot = true
					e.publishAccountState(ctx)
					e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(fmt.Sprintf(
						`{"action":"PAPER_ACCOUNT_RESET","mode":"PAPER","message":"Paper account reset to ₩%.0f KRW & $%.0f USD. All virtual positions cleared."}`,
						upbitKRW, cbUSD,
					)))
				}

			case "FORCE_CLOSE":
				positions, _ := e.db.GetActivePositionsByUserAndMode(e.userID, e.isPaper)
				for _, pos := range positions {
					if pos.PositionID == cmd.PositionID && pos.Status != "CLOSED" {
						targetStrategy := e.findStrategy(StrategyType(pos.Type))
						targetPair := e.findPair(pos.BaseExchange, pos.ForeignExchange, pos.BaseSymbol)
						if targetStrategy != nil && targetPair != nil {
							log.Printf("⚡ [Manual Force Close] (%s) User %d closing Position %s on %s/%s...",
								e.ModeString(), e.userID, pos.PositionID, pos.BaseExchange, pos.ForeignExchange)
							if err := targetStrategy.ExecuteExit(ctx, e.executor, *targetPair, pos, 1.0, e.walletProxy); err != nil && !e.isPaper {
								if strings.Contains(err.Error(), "ERR_SESSION_EXPIRED") {
									_ = e.db.UpdatePositionStatus(pos.PositionID, "AWAITING_REPAY")
								}
								log.Printf("❌ [Manual Force Close Error] %s: %v", pos.PositionID, err)
								e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(fmt.Sprintf(
									`{"action":"COMMAND_ERROR","message":"Failed to close position %s: %s"}`, pos.PositionID, err.Error(),
								)))
							} else {
								_ = e.db.UpdatePositionStatus(pos.PositionID, "CLOSED")
								e.persistPaperState()
								e.notifyPositionClosed(ctx, pos.PositionID)
							}
						} else if e.isPaper {
							_ = e.db.UpdatePositionStatus(pos.PositionID, "CLOSED")
							e.persistPaperState()
							e.notifyPositionClosed(ctx, pos.PositionID)
						}
						break
					}
				}

			case "FORCE_ENTER":
				targetPair := e.findPair(cmd.BaseExchange, cmd.ForeignExchange, cmd.BaseSymbol)
				if targetPair == nil {
					e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(
						`{"action":"COMMAND_ERROR","message":"Could not find matching exchange pair for manual entry."}`,
					))
					break
				}

				selectedType := StrategyMarginSpot
				stratLabel := strings.ToUpper(cmd.Strategy)
				switch {
				case strings.Contains(stratLabel, "TRANSFER") || strings.Contains(stratLabel, "(C)"):
					selectedType = StrategyForeignTransfer
				case strings.Contains(stratLabel, "MARGIN-FUTURE") || strings.Contains(stratLabel, "(B)"):
					selectedType = StrategyMarginFuture
				case strings.Contains(stratLabel, "FUTURE-SPOT") || strings.Contains(stratLabel, "(D)"):
					selectedType = StrategyFutureSpot
				case strings.Contains(stratLabel, "FUTURE-FUTURE") || strings.Contains(stratLabel, "(E)"):
					selectedType = StrategyFutureFuture
				default:
					selectedType = StrategyMarginSpot
				}

				targetStrategy := e.findStrategy(selectedType)
				if targetStrategy != nil {
					investUSD := cmd.InvestmentUsd
					if investUSD <= 0 {
						investUSD = math.Min(1000.0, e.config.CapitalLimitUsd*0.20)
					}
					eval := targetStrategy.GetLastEvaluation()
					eval.Type = selectedType
					eval.EntryPremium = cmd.ExpectedRoi
					eval.TetherRealPremiumPct = cmd.ExpectedRoi
					eval.BasisReturn = cmd.ExpectedRoi
					eval.InvestmentUSD = investUSD
					eval.HedgeMarket = "SPOT"
					if targetPair.ForeignMarketInfo.MarketType == "FUTURE" {
						eval.HedgeMarket = "FUTURE"
					}
					eval.StageAssassination = cmd.StageAssassination
					eval.PreStageListing = cmd.PreStageListing
					eval.EventType = cmd.EventType
					eval.EventTitle = cmd.EventTitle
					targetStrategy.SetEvaluation(eval)
					if _, err := e.ExecuteStrategy(ctx, targetStrategy, *targetPair); err != nil {
						log.Printf("❌ [Manual Force Enter Error] (%s) %v", e.ModeString(), err)
						e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(fmt.Sprintf(
							`{"action":"COMMAND_ERROR","message":"Manual entry failed: %s"}`, err.Error(),
						)))
					}
				}

			case "SIM_EVENT":
				if e.isPaper {
					baseEx := cmd.BaseExchange
					if baseEx == "" {
						baseEx = "UPBIT"
					}
					foreignEx := cmd.ForeignExchange
					if foreignEx == "" {
						foreignEx = "COINBASE"
					}
					baseSym := cmd.BaseSymbol
					if baseSym == "" {
						baseSym = "BTC-KRW"
					}
					coinSym := strings.Split(baseSym, "-")[0]
					evType := cmd.EventType
					if evType == "" {
						evType = "INVESTMENT_WARNING"
					}
					evTitle := cmd.EventTitle
					if evTitle == "" {
						evTitle = fmt.Sprintf("[모의 이벤트] %s (%s) 공지 발생", evType, coinSym)
					}
					nowTime := time.Now().UTC()
					schedOffsetMin := cmd.ScheduledOffsetMin
					if schedOffsetMin <= 0 {
						schedOffsetMin = 95
					}
					schedTime := nowTime.Add(time.Duration(schedOffsetMin) * time.Minute)
					ev := exchange.AnnouncementEvent{
						ID:          fmt.Sprintf("SIM-%d", nowTime.UnixNano()),
						Exchange:    baseEx,
						EventType:   evType,
						Symbols:     []string{coinSym},
						Title:       evTitle,
						SummaryKO:   evTitle,
						SummaryEN:   fmt.Sprintf("[Simulated Event] %s on %s", evType, coinSym),
						AnnouncedAt: nowTime,
						ScheduledAt: &schedTime,
						IsCatalyst:  evType != "DELISTING" && evType != "NONE",
					}
					if e.redisClient != nil {
						e.redisClient.PublishAnnouncementEvent(ctx, ev)
					}

					// If DELISTING, close any open positions on this coin immediately
					if evType == "DELISTING" {
						positions, _ := e.db.GetActivePositionsByUserAndMode(e.userID, true)
						for _, pos := range positions {
							if strings.HasPrefix(pos.BaseSymbol, coinSym) && pos.Status != "CLOSED" {
								targetStrategy := e.findStrategy(StrategyType(pos.Type))
								targetPair := e.findPair(pos.BaseExchange, pos.ForeignExchange, pos.BaseSymbol)
								if targetStrategy != nil && targetPair != nil {
									_ = targetStrategy.ExecuteExit(ctx, e.executor, *targetPair, pos, 1.0, e.walletProxy)
								}
								_ = e.db.UpdatePositionStatus(pos.PositionID, "CLOSED")
								e.persistPaperState()
								e.notifyPositionClosed(ctx, pos.PositionID)
							}
						}
					} else if cmd.StageAssassination || cmd.PreStageListing {
						targetPair := e.findPair(baseEx, foreignEx, baseSym)
						targetStrategy := e.findStrategy(StrategyForeignTransfer)
						if targetPair != nil && targetStrategy != nil {
							investUSD := cmd.InvestmentUsd
							if investUSD <= 0 {
								investUSD = math.Min(1000.0, e.config.CapitalLimitUsd*0.20)
							}
							eval := targetStrategy.GetLastEvaluation()
							eval.Type = StrategyForeignTransfer
							eval.EntryPremium = 0.005
							eval.TetherRealPremiumPct = 0.004
							eval.BasisReturn = 0.005
							eval.InvestmentUSD = investUSD
							eval.HedgeMarket = "SPOT"
							eval.StageAssassination = cmd.StageAssassination
							eval.PreStageListing = cmd.PreStageListing
							eval.EventType = evType
							eval.EventTitle = evTitle
							targetStrategy.SetEvaluation(eval)
							_, _ = e.ExecuteStrategy(ctx, targetStrategy, *targetPair)
						}
					}

					e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(fmt.Sprintf(
						`{"action":"SIM_EVENT_TRIGGERED","mode":"PAPER","event_type":"%s","symbol":"%s","message":"Simulated event [%s] injected for %s."}`,
						evType, coinSym, evType, coinSym,
					)))
				}
			}
			e.publishAccountState(ctx)
			e.mu.Unlock()
		}
	}
}

// AIAccountState maps the user's balance, exchange skew, web session status, simulator telemetry, and positions for python DRL & UI SSE
type AIAccountState struct {
	UserID               uint                      `json:"user_id"`
	Mode                 string                    `json:"mode"`
	BotStatus            string                    `json:"bot_status"`
	AvailableBalance     float64                   `json:"available_balance"`
	TotalEquityUSD       float64                   `json:"total_equity_usd"`
	FXRateKRW            float64                   `json:"fx_rate_krw"`
	TetherRatesKRW       map[string]float64        `json:"tether_rates_krw"`
	ExchangeBalances     []ExchangeBalanceSnapshot `json:"exchange_balances"`
	ExchangeFees         map[string]float64        `json:"exchange_fees,omitempty"`
	ForeignShare         float64                   `json:"foreign_share"`
	RebalanceTriggerSkew float64                   `json:"rebalance_trigger_skew"`
	RebalanceTargetSkew  float64                   `json:"rebalance_target_skew"`
	WebLendingSessions   map[string]bool           `json:"web_lending_sessions"`
	PositionPnLs         map[string]float64        `json:"position_pnls"`
	CurrentExposures     map[string]float64        `json:"current_exposures"`
	UnrealizedPnLs       map[string]float64        `json:"unrealized_pnls"`
	PairStates           map[string]int            `json:"pair_states"`
	State                int                       `json:"state"`
	SimExchange          string                    `json:"sim_exchange"`
	SimCurrentOffsetPct  float64                   `json:"sim_current_offset_pct"`
	SimTargetOffsetPct   float64                   `json:"sim_target_offset_pct"`
	SimRampMinutes       float64                   `json:"sim_ramp_minutes"`
	SimProgressPct       float64                   `json:"sim_progress_pct"`
	SimRemainingSec      int64                     `json:"sim_remaining_sec"`
}

func (e *Engine) publishAccountState(ctx context.Context) {
	positions, err := e.db.GetActivePositionsByUserAndMode(e.userID, e.isPaper)
	if err != nil {
		log.Printf("[Engine-%d] Failed to load user active positions: %v", e.userID, err)
		return
	}

	exposures := make(map[string]float64)
	pnls := make(map[string]float64)
	positionPnLs := make(map[string]float64)
	pairStates := make(map[string]int)
	state := 0 // Global fallback: 0: Neutral, 1: Holding, 2: Transferring, 3: Repaying

	totalInvestedUSD := 0.0
	totalUnrealizedPnLUSD := 0.0
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
		positionPnLs[pos.PositionID] = posPnL
		totalUnrealizedPnLUSD += posPnL

		posState := 1
		if pos.Status == "TRANSFERRING" {
			posState = 2
		} else if pos.Status == "REPAYING" || pos.Status == "AWAITING_REPAY" || pos.Status == "AWAITING_TRAVEL_RULE" {
			posState = 3
		} else if pos.Status == "STAGED_ASSASSINATION" || pos.Status == "PRE_STAGED_LISTING" {
			posState = 4
		}
		pairStates[key] = posState
		pairStates[reverseKey] = posState
		if posState > state {
			state = posState
		}
	}

	var exSnapshots []ExchangeBalanceSnapshot
	webSessions := make(map[string]bool)
	exchangeFees := make(map[string]float64)
	totalLiquidUSD := 0.0
	totalPaperNetEquityUSD := 0.0
	foreignUSD := 0.0

	for _, client := range e.clients {
		exName := client.GetName()
		if feeInfo, err := client.GetTradingFee(ctx, ""); err == nil && feeInfo.TakerFeeRate > 0 {
			exchangeFees[exName] = feeInfo.TakerFeeRate
		}
		lendingOK := client.SupportsMargin()
		if e.isPaper {
			lendingOK = true
			if exName == "UPBIT" || exName == "BITHUMB" {
				webSessions[exName] = true
			}
		} else if wc, ok := client.(exchange.WebLendingCapable); ok && wc.LendingManager() != nil {
			lendingOK = wc.LendingManager().IsSessionValid()
			webSessions[exName] = lendingOK
		}

		var availNative, availUSD float64
		quoteCur := "USD"
		if exName == "UPBIT" || exName == "BITHUMB" {
			quoteCur = "KRW"
		}

		if pc, ok := client.(*exchange.PaperClient); ok {
			qCur, availQuote, netEquityQuote := pc.GetAccountValuation(ctx)
			quoteCur = qCur
			availNative = availQuote
			if quoteCur == "KRW" {
				fx := GetFXRate("KRW")
				availUSD = availQuote / fx
				totalPaperNetEquityUSD += netEquityQuote / fx
			} else {
				availUSD = availQuote
				totalPaperNetEquityUSD += netEquityQuote
			}
		} else {
			bals, bErr := client.GetBalances(ctx)
			if bErr == nil {
				for _, b := range bals {
					if string(b.Currency) == "KRW" {
						quoteCur = "KRW"
						availNative += b.Available
						availUSD += (b.Available + b.Locked) / GetFXRate("KRW")
					} else if string(b.Currency) == "USD" || string(b.Currency) == "USDT" || string(b.Currency) == "USDC" {
						quoteCur = string(b.Currency)
						availNative += b.Available
						availUSD += (b.Available + b.Locked)
					}
				}
			}
		}
		totalLiquidUSD += availUSD
		if quoteCur != "KRW" {
			foreignUSD += availUSD
		}
		exSnapshots = append(exSnapshots, ExchangeBalanceSnapshot{
			Name:            exName,
			QuoteCurrency:   quoteCur,
			AvailableNative: availNative,
			AvailableUSD:    availUSD,
			WebLendingOK:    lendingOK,
		})
	}

	if totalLiquidUSD > 0 {
		for i := range exSnapshots {
			exSnapshots[i].Percent = (exSnapshots[i].AvailableUSD / totalLiquidUSD) * 100.0
		}
	}
	foreignShare := 0.50
	if totalLiquidUSD > 0 {
		foreignShare = foreignUSD / totalLiquidUSD
	}

	var totalEquityUSD float64
	if e.isPaper && totalPaperNetEquityUSD > 0 {
		totalEquityUSD = totalPaperNetEquityUSD
	} else {
		totalEquityUSD = totalLiquidUSD + totalInvestedUSD + totalUnrealizedPnLUSD
	}

	simEx, simCurr, simTarget, simRamp, simProg, simRem := e.GetSimStatus()
	fxKRW := GetFXRate("KRW")

	hasMarketData := len(e.rawQuotes) > 0 || e.forceNextSnapshot
	if e.db != nil && totalEquityUSD > 0 && hasMarketData && (e.resetSnapshotPending || e.forceNextSnapshot || time.Since(e.lastSnapshotTime) >= 60*time.Second) {
		e.lastSnapshotTime = time.Now().UTC()
		breakdownBytes, _ := json.Marshal(exSnapshots)
		if e.resetSnapshotPending {
			e.resetSnapshotPending = false
			e.forceNextSnapshot = false
			_ = e.db.ResetPaperEquitySnapshots(e.userID, totalEquityUSD, fxKRW, string(breakdownBytes))
		} else {
			e.forceNextSnapshot = false
			_ = e.db.SaveEquitySnapshot(&postgres.EquitySnapshot{
				UserID:                e.userID,
				IsPaper:               e.isPaper,
				TotalEquityUSD:        totalEquityUSD,
				AvailableUSD:          totalLiquidUSD,
				UnrealizedPnLUSD:      totalUnrealizedPnLUSD,
				FXRateKRW:             fxKRW,
				ExchangeBreakdownJSON: string(breakdownBytes),
				RecordedAt:            e.lastSnapshotTime,
			})
		}
	}

	payload, err := json.Marshal(AIAccountState{
		UserID:               e.userID,
		Mode:                 e.ModeString(),
		BotStatus:            e.botStatus,
		AvailableBalance:     totalLiquidUSD,
		TotalEquityUSD:       totalEquityUSD,
		FXRateKRW:            fxKRW,
		TetherRatesKRW:       GetAllExchangeTetherRatesSnapshot(),
		ExchangeBalances:     exSnapshots,
		ExchangeFees:         exchangeFees,
		ForeignShare:         foreignShare,
		RebalanceTriggerSkew: e.config.RebalanceTriggerSkew,
		RebalanceTargetSkew:  e.config.RebalanceTargetSkew,
		WebLendingSessions:   webSessions,
		PositionPnLs:         positionPnLs,
		CurrentExposures:     exposures,
		UnrealizedPnLs:       pnls,
		PairStates:           pairStates,
		State:                state,
		SimExchange:          simEx,
		SimCurrentOffsetPct:  simCurr,
		SimTargetOffsetPct:   simTarget,
		SimRampMinutes:       simRamp,
		SimProgressPct:       simProg,
		SimRemainingSec:      simRem,
	})
	if err == nil && e.redisClient != nil {
		e.redisClient.PublishUserStateByMode(ctx, fmt.Sprint(e.userID), e.ModeString(), payload)
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
	e.forceNextSnapshot = true
	e.publishAccountState(ctx)

	e.sendNotification(ctx, notification.AlertPositionOpened, fmt.Sprintf(
		"🚀 **Position Opened**\n- **ID**: `%s`\n- **Type**: `%s`\n- **Exchanges**: `%s` (Base) / `%s` (Foreign)\n- **Symbol**: `%s`\n- **Size**: `$%.2f`\n- **Premium**: `%.4f` (Target Profit: `%.2f%%`)",
		pos.PositionID, pos.Type, pos.BaseExchange, pos.ForeignExchange, pos.BaseSymbol, pos.InvestmentUSD, pos.EntryPremium, e.config.MinTargetProfit,
	))
}

func (e *Engine) getExchangeCurrencyFX(exchangeName, symbol string) float64 {
	if strings.HasSuffix(strings.ToUpper(symbol), "-KRW") ||
		strings.EqualFold(exchangeName, "UPBIT") ||
		strings.EqualFold(exchangeName, "BITHUMB") {
		fx := GetFXRate("KRW")
		if fx > 0 {
			return fx
		}
		return 1400.0
	}
	return 1.0
}

func (e *Engine) calculateRealizedPnLUSD(pos *postgres.ActivePosition) float64 {
	if pos == nil {
		return 0.0
	}
	baseFX := e.getExchangeCurrencyFX(pos.BaseExchange, pos.BaseSymbol)
	foreignFX := e.getExchangeCurrencyFX(pos.ForeignExchange, pos.ForeignSymbol)
	targetPair := e.findPair(pos.BaseExchange, pos.ForeignExchange, pos.BaseSymbol)
	vaspTier := GetVASPTier(pos.BaseExchange, pos.ForeignExchange)
	needs2Hop := vaspTier == exchange.VASPTier3RiskAssessed && e.walletProxy != nil
	baseOneWayFee, foreignOneWayFee, roundTripReversionFee, oneWayTransferFee, networkWithdrawFeeRate := e.computePairFeeRates(context.Background(), targetPair, pos.InvestmentUSD, needs2Hop)

	// 1. Strategy C (FOREIGN_TRANSFER): Borrow & Sell on Base, Buy on Foreign, Transfer & 1:1 Repay (or Staged Assassination exit)
	if pos.Type == string(StrategyForeignTransfer) {
		sellBasePrice := pos.EntryBasePrice
		if sellBasePrice <= 0 && e.latestQuotes != nil {
			if bq, ok := e.latestQuotes[pos.BaseSymbol]; ok {
				if bq.RawBid > 0 {
					sellBasePrice = bq.RawBid
				} else if bq.BidUSD > 0 {
					sellBasePrice = bq.BidUSD * baseFX
				}
			}
		}
		if sellBasePrice > 0 && pos.EntryForeignPrice > 0 && pos.BaseAmount > 0 && pos.ForeignAmount > 0 {
			baseSellUSD := (pos.BaseAmount * sellBasePrice) / baseFX
			foreignBuyUSD := (pos.ForeignAmount * pos.EntryForeignPrice) / foreignFX
			baseFeeUSD := baseSellUSD * baseOneWayFee
			foreignFeeUSD := foreignBuyUSD * foreignOneWayFee
			withdrawFeeUSD := foreignBuyUSD * networkWithdrawFeeRate
			return baseSellUSD - foreignBuyUSD - baseFeeUSD - foreignFeeUSD - withdrawFeeUSD
		}
		return pos.InvestmentUSD * (pos.EntryPremium - (oneWayTransferFee + networkWithdrawFeeRate))
	}

	// 2. Strategies A, B, D, E (Convergence / Widening Delta-Neutral Hedging):
	// Entry: Short Base @ EntryBasePrice, Long Foreign @ EntryForeignPrice
	// Exit:  Buy Base @ exitBasePrice, Sell Foreign @ exitForeignPrice
	var exitBasePrice, exitForeignPrice float64
	if e.latestQuotes != nil {
		if bq, ok := e.latestQuotes[pos.BaseSymbol]; ok {
			if bq.RawAsk > 0 {
				exitBasePrice = bq.RawAsk
			} else if bq.AskUSD > 0 {
				exitBasePrice = bq.AskUSD * baseFX
			}
		}
		if fq, ok := e.latestQuotes[pos.ForeignSymbol]; ok {
			if fq.RawBid > 0 {
				exitForeignPrice = fq.RawBid
			} else if fq.BidUSD > 0 {
				exitForeignPrice = fq.BidUSD * foreignFX
			}
		}
	}

	if pos.EntryBasePrice > 0 && pos.EntryForeignPrice > 0 && exitBasePrice > 0 && exitForeignPrice > 0 && pos.BaseAmount > 0 && pos.ForeignAmount > 0 {
		baseShortPnLUSD := (pos.BaseAmount * (pos.EntryBasePrice - exitBasePrice)) / baseFX
		foreignLongPnLUSD := (pos.ForeignAmount * (exitForeignPrice - pos.EntryForeignPrice)) / foreignFX
		baseNotionalUSD := (pos.BaseAmount * (pos.EntryBasePrice + exitBasePrice)) / baseFX
		foreignNotionalUSD := (pos.ForeignAmount * (pos.EntryForeignPrice + exitForeignPrice)) / foreignFX
		roundTripFeesUSD := (baseNotionalUSD * baseOneWayFee) + (foreignNotionalUSD * foreignOneWayFee)
		return baseShortPnLUSD + foreignLongPnLUSD - roundTripFeesUSD
	}

	if e.liveUnrealizedPnLs != nil {
		key := pos.BaseExchange + "|" + pos.BaseSymbol
		if liveVal, ok := e.liveUnrealizedPnLs[key]; ok && math.Abs(liveVal) > 1e-6 {
			return liveVal
		}
	}

	return pos.InvestmentUSD * (-roundTripReversionFee)
}

func (e *Engine) notifyPositionClosed(ctx context.Context, positionID string) {
	profitStr := "N/A"
	pos, err := e.db.GetPositionByID(positionID)
	if err == nil && pos != nil {
		netProfitUSD := e.calculateRealizedPnLUSD(pos)
		_ = e.db.ClosePositionWithPnL(positionID, netProfitUSD)
		profitStr = fmt.Sprintf("$%.2f", netProfitUSD)
		log.Printf("💰 [REALIZED PROFIT] Position %s (%s) closed. Settled Net Profit: %s",
			positionID, pos.Type, profitStr)
	}

	e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(fmt.Sprintf(
		`{"action":"POSITION_CLOSED", "position_id":"%s"}`, positionID,
	)))
	e.forceNextSnapshot = true
	e.publishAccountState(ctx)

	e.sendNotification(ctx, notification.AlertPositionClosed, fmt.Sprintf(
		"✅ **Position Closed**\n- **ID**: `%s`\n- **Realized Profit**: `%s`\n- **Timestamp**: `%s`",
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
