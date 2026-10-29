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
	lastSnapshotTime  time.Time
	forceNextSnapshot bool
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
	e.persistPaperState()

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
						} else if strings.EqualFold(filteredOps[i].ForeignExchange, simEx) {
							filteredOps[i].PremiumPct = (1.0+filteredOps[i].PremiumPct)/(1.0+offsetDec) - 1.0
							filteredOps[i].PredictedPremiumPct = (1.0+filteredOps[i].PredictedPremiumPct)/(1.0+offsetDec*0.5) - 1.0
						}

						// Reflect offset-driven arbitrage edge in target weight & action type for realistic simulation
						minTargetDec := e.config.MinTargetProfit / 100.0
						if minTargetDec <= 0 {
							minTargetDec = 0.01
						}
						if filteredOps[i].PremiumPct >= minTargetDec && filteredOps[i].TargetWeight < 0.20 {
							filteredOps[i].TargetWeight = 0.25
							filteredOps[i].ActionType = 0
						} else if filteredOps[i].PremiumPct <= 0.0015 && len(activePos) > 0 {
							// Spread converged back near 0%: trigger profit-taking exit
							filteredOps[i].ActionType = 2
							filteredOps[i].TargetWeight = 0.0
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

					// --- ActionType = 0 (TRADE / 진입 및 리밸런싱) ---
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
								var selectedStrategy StrategyType = StrategyMarginSpot
								isBaseFuture := targetPair.BaseMarketInfo.MarketType == "FUTURE"
								isForeignFuture := targetPair.ForeignMarketInfo.MarketType == "FUTURE"
								if isBaseFuture && isForeignFuture {
									selectedStrategy = StrategyFutureFuture
								} else if isBaseFuture {
									selectedStrategy = StrategyFutureSpot
								} else {
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
					} else if delta < -toleranceAmount {
						if matchingPos != nil && currentExposure > 0 {
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
				ExpectedRoi         float64 `json:"expected_roi"`
				CurrentPremiumPct   float64 `json:"current_premium_pct"`
				PredictedPremiumPct float64 `json:"predicted_premium_pct"`
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

					if isBaseFuture && isForeignFuture {
						// Strategy E: Future-Future
						roi := opp.PremiumPct - opp.PredictedPremiumPct
						dtoList = append(dtoList, FilteredOppDTO{
							BaseExchange:        opp.BaseExchange,
							ForeignExchange:     opp.ForeignExchange,
							BaseSymbol:          opp.BaseSymbol,
							ForeignSymbol:       opp.ForeignSymbol,
							Strategy:            "Future-Future (E)",
							ExpectedRoi:         roi - 0.0005, // minus estimated trading fee rate (0.05%)
							CurrentPremiumPct:   opp.PremiumPct * 100.0,
							PredictedPremiumPct: opp.PredictedPremiumPct * 100.0,
							TargetWeight:        opp.TargetWeight,
							RiskScore:           15.0,         // low risk, no transfer time exposure
							InvestmentUsd:       e.config.CapitalLimitUsd * opp.TargetWeight,
							IsValid:             true,
						})
					} else if isBaseFuture {
						// Strategy D: Future-Spot
						roi := opp.PremiumPct - opp.PredictedPremiumPct
						dtoList = append(dtoList, FilteredOppDTO{
							BaseExchange:        opp.BaseExchange,
							ForeignExchange:     opp.ForeignExchange,
							BaseSymbol:          opp.BaseSymbol,
							ForeignSymbol:       opp.ForeignSymbol,
							Strategy:            "Future-Spot (D)",
							ExpectedRoi:         roi - 0.0006, // minus estimated fee rate (0.06%)
							CurrentPremiumPct:   opp.PremiumPct * 100.0,
							PredictedPremiumPct: opp.PredictedPremiumPct * 100.0,
							TargetWeight:        opp.TargetWeight,
							RiskScore:           18.0,
							InvestmentUsd:       e.config.CapitalLimitUsd * opp.TargetWeight,
							IsValid:             true,
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
								BaseExchange:        opp.BaseExchange,
								ForeignExchange:     opp.ForeignExchange,
								BaseSymbol:          opp.BaseSymbol,
								ForeignSymbol:       opp.ForeignSymbol,
								Strategy:            strategyHoldingName,
								ExpectedRoi:         roiHolding - 0.0013, // accounting for spot trading fees + estimated borrow interest over hold (0.13%)
								CurrentPremiumPct:   opp.PremiumPct * 100.0,
								PredictedPremiumPct: opp.PredictedPremiumPct * 100.0,
								TargetWeight:        opp.TargetWeight,
								RiskScore:           18.0,                // lower risk, no block confirmation delay
								InvestmentUsd:       e.config.CapitalLimitUsd * opp.TargetWeight,
								IsValid:             true,
							})

							// 2. Transfer Loop Strategy: Transfer Loop (C)
							if targetPair.IsValidForTransfer {
								roiTransfer := opp.PremiumPct
								
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
									ExpectedRoi:         roiTransfer - 0.0022, // accounting for trading fees + network gas/withdrawal fees (0.22%)
									CurrentPremiumPct:   opp.PremiumPct * 100.0,
									PredictedPremiumPct: opp.PredictedPremiumPct * 100.0,
									TargetWeight:        opp.TargetWeight,
									RiskScore:           totalRisk,
									InvestmentUsd:       e.config.CapitalLimitUsd * opp.TargetWeight,
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
	Action             string  `json:"action"`     // "FORCE_ENTER" | "FORCE_CLOSE" | "SET_BOT_STATUS" | "SET_SIM_OFFSET" | "RESET_PAPER_ACCOUNT"
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
					if upbitKRW <= 0 {
						upbitKRW = cmd.UpbitKRW
					}
					if upbitKRW <= 0 {
						upbitKRW = 50000000.0
					}
					cbUSD := cmd.PaperCoinbaseUSD
					if cbUSD <= 0 {
						cbUSD = cmd.CoinbaseUSD
					}
					if cbUSD <= 0 {
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
					e.forceNextSnapshot = true
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
							if err := targetStrategy.ExecuteExit(ctx, e.executor, *targetPair, pos, 1.0, e.walletProxy); err != nil {
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
					eval.BasisReturn = cmd.ExpectedRoi
					eval.InvestmentUSD = investUSD
					eval.HedgeMarket = "SPOT"
					if targetPair.ForeignMarketInfo.MarketType == "FUTURE" {
						eval.HedgeMarket = "FUTURE"
					}
					targetStrategy.SetEvaluation(eval)
					if _, err := e.ExecuteStrategy(ctx, targetStrategy, *targetPair); err != nil {
						log.Printf("❌ [Manual Force Enter Error] (%s) %v", e.ModeString(), err)
						e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(fmt.Sprintf(
							`{"action":"COMMAND_ERROR","message":"Manual entry failed: %s"}`, err.Error(),
						)))
					}
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
	ExchangeBalances     []ExchangeBalanceSnapshot `json:"exchange_balances"`
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
		} else if pos.Status == "REPAYING" || pos.Status == "AWAITING_REPAY" {
			posState = 3
		}
		pairStates[key] = posState
		pairStates[reverseKey] = posState
		if posState > state {
			state = posState
		}
	}

	var exSnapshots []ExchangeBalanceSnapshot
	webSessions := make(map[string]bool)
	totalLiquidUSD := 0.0
	totalPaperNetEquityUSD := 0.0
	foreignUSD := 0.0

	for _, client := range e.clients {
		exName := client.GetName()
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
	if e.db != nil && totalEquityUSD > 0 && hasMarketData && (e.forceNextSnapshot || time.Since(e.lastSnapshotTime) >= 60*time.Second) {
		e.lastSnapshotTime = time.Now().UTC()
		e.forceNextSnapshot = false
		breakdownBytes, _ := json.Marshal(exSnapshots)
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

	payload, err := json.Marshal(AIAccountState{
		UserID:               e.userID,
		Mode:                 e.ModeString(),
		BotStatus:            e.botStatus,
		AvailableBalance:     totalLiquidUSD,
		TotalEquityUSD:       totalEquityUSD,
		FXRateKRW:            fxKRW,
		ExchangeBalances:     exSnapshots,
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

func (e *Engine) notifyPositionClosed(ctx context.Context, positionID string) {
	e.redisClient.PublishUserEvents(ctx, fmt.Sprint(e.userID), []byte(fmt.Sprintf(
		`{"action":"POSITION_CLOSED", "position_id":"%s"}`, positionID,
	)))
	e.forceNextSnapshot = true
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
