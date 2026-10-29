package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/magnolia/backend/internal/strategy"
)

// ActiveConnections tracks all open UI WebSockets
var (
	wsConnMutex sync.RWMutex
	activeConns = make(map[uint][]chan []byte)
)

func (s *Server) pushDashboardSnapshot(ctx context.Context, w http.ResponseWriter, userID uint, mode string) {
	if mode != "PAPER" && mode != "LIVE" {
		mode = "PAPER"
	}
	isPaper := (mode == "PAPER")

	var stateMap map[string]interface{}
	if s.Redis != nil {
		if rawState, err := s.Redis.GetLatestUserStateByMode(ctx, fmt.Sprint(userID), mode); err == nil && len(rawState) > 0 {
			_ = json.Unmarshal(rawState, &stateMap)
		}
	}

	settings, _ := s.DB.GetUserSettings(userID)
	paperBotStatus := "RUNNING"
	liveBotStatus := "STOPPED"
	paperUpbitKRW := 50000000.0
	paperCoinbaseUSD := 35000.0
	if settings != nil {
		if settings.PaperBotStatus != "" {
			paperBotStatus = settings.PaperBotStatus
		}
		if settings.LiveBotStatus != "" {
			liveBotStatus = settings.LiveBotStatus
		}
		if settings.PaperUpbitKRW > 0 {
			paperUpbitKRW = settings.PaperUpbitKRW
		}
		if settings.PaperCoinbaseUSD > 0 {
			paperCoinbaseUSD = settings.PaperCoinbaseUSD
		}
	}

	threshold, _ := s.DB.LoadThresholds(userID, "PremiumStrategy")
	capitalLimit := 10000.0
	triggerSkew := 0.20
	targetSkew := 0.50
	if threshold != nil && threshold.CapitalLimitUsd > 0 {
		capitalLimit = threshold.CapitalLimitUsd
		if threshold.RebalanceTriggerSkew > 0 {
			triggerSkew = threshold.RebalanceTriggerSkew
		}
		if threshold.RebalanceTargetSkew > 0 {
			targetSkew = threshold.RebalanceTargetSkew
		}
	}

	activeBotStatus := liveBotStatus
	if isPaper {
		activeBotStatus = paperBotStatus
	}

	fxRate := strategy.GetFXRate("KRW")
	if fxRate <= 0 {
		fxRate = 1400.0
	}

	portfolioPayload := map[string]interface{}{
		"mode":                   mode,
		"is_paper":               isPaper,
		"bot_status":             activeBotStatus,
		"paper_bot_status":       paperBotStatus,
		"live_bot_status":        liveBotStatus,
		"paper_upbit_krw":        paperUpbitKRW,
		"paper_coinbase_usd":     paperCoinbaseUSD,
		"sim_status":             computeSimStatusFromSettings(settings),
		"total_equity_usd":       capitalLimit,
		"available_balance":      capitalLimit,
		"fx_rate_krw":            fxRate,
		"foreign_share":          0.50,
		"rebalance_trigger_skew": triggerSkew,
		"rebalance_target_skew":  targetSkew,
		"exchange_balances":      []interface{}{},
		"web_lending_sessions":   map[string]bool{},
	}

	var posPnLs map[string]interface{}
	if stateMap != nil {
		if v, ok := stateMap["bot_status"]; ok {
			portfolioPayload["bot_status"] = v
		}
		if v, ok := stateMap["sim_status"]; ok && isPaper {
			portfolioPayload["sim_status"] = v
		}
		if v, ok := stateMap["total_equity_usd"]; ok {
			portfolioPayload["total_equity_usd"] = v
		}
		if v, ok := stateMap["available_balance"]; ok {
			portfolioPayload["available_balance"] = v
		}
		if v, ok := stateMap["fx_rate_krw"]; ok {
			portfolioPayload["fx_rate_krw"] = v
		}
		if v, ok := stateMap["foreign_share"]; ok {
			portfolioPayload["foreign_share"] = v
		}
		if v, ok := stateMap["rebalance_trigger_skew"]; ok {
			portfolioPayload["rebalance_trigger_skew"] = v
		}
		if v, ok := stateMap["rebalance_target_skew"]; ok {
			portfolioPayload["rebalance_target_skew"] = v
		}
		if v, ok := stateMap["exchange_balances"]; ok {
			portfolioPayload["exchange_balances"] = v
		}
		if v, ok := stateMap["web_lending_sessions"]; ok {
			portfolioPayload["web_lending_sessions"] = v
		}
		if v, ok := stateMap["position_pnls"].(map[string]interface{}); ok {
			posPnLs = v
		}
	} else if isPaper {
		// Fallback for Paper Mode before first Engine tick: show configured virtual seed balances
		upbitUSD := paperUpbitKRW / 1380.0
		totalUSD := upbitUSD + paperCoinbaseUSD
		upbitPct := 50.0
		cbPct := 50.0
		foreignShare := 0.50
		if totalUSD > 0 {
			upbitPct = (upbitUSD / totalUSD) * 100.0
			cbPct = (paperCoinbaseUSD / totalUSD) * 100.0
			foreignShare = paperCoinbaseUSD / totalUSD
		}
		portfolioPayload["total_equity_usd"] = totalUSD
		portfolioPayload["available_balance"] = totalUSD
		portfolioPayload["foreign_share"] = foreignShare
		portfolioPayload["exchange_balances"] = []map[string]interface{}{
			{
				"name":             "UPBIT",
				"quote_currency":   "KRW",
				"available_native": paperUpbitKRW,
				"available_usd":    upbitUSD,
				"percent":          upbitPct,
				"web_lending_ok":   true,
			},
			{
				"name":             "COINBASE",
				"quote_currency":   "USD",
				"available_native": paperCoinbaseUSD,
				"available_usd":    paperCoinbaseUSD,
				"percent":          cbPct,
				"web_lending_ok":   true,
			},
		}
		portfolioPayload["web_lending_sessions"] = map[string]bool{
			"UPBIT":    true,
			"COINBASE": true,
		}
	} else {
		// Fallback when Live Engine hasn't published state yet: list registered exchanges
		keys, _ := s.DB.GetAllAPIKeys(userID)
		var fallbackExchanges []map[string]interface{}
		activeCount := 0
		for _, k := range keys {
			if k.IsActive {
				activeCount++
			}
		}
		for _, k := range keys {
			if !k.IsActive {
				continue
			}
			sharePct := 0.0
			if activeCount > 0 {
				sharePct = 100.0 / float64(activeCount)
			}
			quoteCur := "USD"
			if k.Exchange == "UPBIT" || k.Exchange == "BITHUMB" {
				quoteCur = "KRW"
			}
			fallbackExchanges = append(fallbackExchanges, map[string]interface{}{
				"name":             k.Exchange,
				"quote_currency":   quoteCur,
				"available_native": 0.0,
				"available_usd":    capitalLimit * (sharePct / 100.0),
				"percent":          sharePct,
				"web_lending_ok":   k.IsValid,
			})
		}
		portfolioPayload["exchange_balances"] = fallbackExchanges
	}

	portfolioBytes, _ := json.Marshal(portfolioPayload)
	w.Write([]byte("event: portfolio\n"))
	w.Write([]byte("data: " + string(portfolioBytes) + "\n\n"))

	positions, _ := s.DB.GetActivePositionsByUserAndMode(userID, isPaper)
	type enrichedPos struct {
		ID                 uint      `json:"ID"`
		PositionID         string    `json:"PositionID"`
		IsPaper            bool      `json:"IsPaper"`
		Status             string    `json:"Status"`
		Type               string    `json:"Type"`
		HedgeMarket        string    `json:"HedgeMarket"`
		BaseExchange       string    `json:"BaseExchange"`
		ForeignExchange    string    `json:"ForeignExchange"`
		BaseSymbol         string    `json:"BaseSymbol"`
		ForeignSymbol      string    `json:"ForeignSymbol"`
		BaseAmount         float64   `json:"BaseAmount"`
		ForeignAmount      float64   `json:"ForeignAmount"`
		InvestmentUSD      float64   `json:"InvestmentUSD"`
		EntryBasePrice     float64   `json:"EntryBasePrice"`
		EntryForeignPrice  float64   `json:"EntryForeignPrice"`
		EntryPremium       float64   `json:"EntryPremium"`
		ExpectedFuturePrem float64   `json:"ExpectedFuturePrem"`
		MaxUnrealizedROI   float64   `json:"MaxUnrealizedROI"`
		CurrentPnLUSD      float64   `json:"CurrentPnLUSD"`
		CurrentPnLPct      float64   `json:"CurrentPnLPct"`
		OpenedAt           time.Time `json:"OpenedAt"`
	}

	var enrichedList []enrichedPos
	for _, p := range positions {
		livePnL := p.MaxUnrealizedROI * p.InvestmentUSD
		if posPnLs != nil {
			if rawVal, ok := posPnLs[p.PositionID]; ok {
				if fv, ok := rawVal.(float64); ok {
					livePnL = fv
				}
			}
		}
		livePct := 0.0
		if p.InvestmentUSD > 0 {
			livePct = (livePnL / p.InvestmentUSD) * 100.0
		}
		enrichedList = append(enrichedList, enrichedPos{
			ID:                 p.ID,
			PositionID:         p.PositionID,
			IsPaper:            p.IsPaper,
			Status:             p.Status,
			Type:               p.Type,
			HedgeMarket:        p.HedgeMarket,
			BaseExchange:       p.BaseExchange,
			ForeignExchange:    p.ForeignExchange,
			BaseSymbol:         p.BaseSymbol,
			ForeignSymbol:      p.ForeignSymbol,
			BaseAmount:         p.BaseAmount,
			ForeignAmount:      p.ForeignAmount,
			InvestmentUSD:      p.InvestmentUSD,
			EntryBasePrice:     p.EntryBasePrice,
			EntryForeignPrice:  p.EntryForeignPrice,
			EntryPremium:       p.EntryPremium,
			ExpectedFuturePrem: p.ExpectedFuturePrem,
			MaxUnrealizedROI:   p.MaxUnrealizedROI,
			CurrentPnLUSD:      livePnL,
			CurrentPnLPct:      livePct,
			OpenedAt:           p.OpenedAt,
		})
	}

	posData, _ := json.Marshal(map[string]interface{}{
		"mode":      mode,
		"positions": enrichedList,
	})
	w.Write([]byte("event: positions\n"))
	w.Write([]byte("data: " + string(posData) + "\n\n"))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// handleDashboardWS streams portfolio updates, Position state changes, and system notifications
func (s *Server) handleDashboardWS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	userID := getUserIDFromContext(r)
	mode := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("mode")))
	if mode != "PAPER" && mode != "LIVE" {
		if settings, err := s.DB.GetUserSettings(userID); err == nil && settings != nil && settings.ActiveViewMode != "" {
			mode = strings.ToUpper(settings.ActiveViewMode)
		} else {
			mode = "PAPER"
		}
	}

	msgChan := make(chan []byte, 10)

	wsConnMutex.Lock()
	activeConns[userID] = append(activeConns[userID], msgChan)
	wsConnMutex.Unlock()

	defer func() {
		wsConnMutex.Lock()
		defer wsConnMutex.Unlock()
		conns := activeConns[userID]
		for i, c := range conns {
			if c == msgChan {
				activeConns[userID] = append(conns[:i], conns[i+1:]...)
				break
			}
		}
		close(msgChan)
	}()

	// Stream initial snapshot immediately upon connection
	s.pushDashboardSnapshot(r.Context(), w, userID, mode)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	clientQuit := r.Context().Done()

	eventCh := make(chan []byte, 10)
	if s.Redis != nil {
		go s.Redis.SubscribeUserEvents(r.Context(), fmt.Sprint(userID), eventCh)
	}

	for {
		select {
		case <-clientQuit:
			log.Println("[Dashboard SSE] Client disconnected.")
			return
		case msg := <-msgChan:
			w.Write([]byte("event: notification\n"))
			w.Write([]byte("data: " + string(msg) + "\n\n"))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		case redisEvent := <-eventCh:
			w.Write([]byte("event: notification\n"))
			w.Write([]byte("data: " + string(redisEvent) + "\n\n"))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			s.pushDashboardSnapshot(r.Context(), w, userID, mode)
		case <-ticker.C:
			s.pushDashboardSnapshot(r.Context(), w, userID, mode)
		}
	}
}

func (s *Server) applySimOffsetToRawOpps(rawData []byte, userID uint) []byte {
	settings, err := s.DB.GetUserSettings(userID)
	if err != nil || settings == nil {
		return rawData
	}
	sim := computeSimStatusFromSettings(settings)
	currOffsetPct, _ := sim["current_offset_pct"].(float64)
	simEx, _ := sim["exchange"].(string)
	if math.Abs(currOffsetPct) < 1e-6 || simEx == "" {
		return rawData
	}

	var opps []map[string]interface{}
	if err := json.Unmarshal(rawData, &opps); err != nil {
		return rawData
	}

	for _, opp := range opps {
		baseEx, _ := opp["base_exchange"].(string)
		foreignEx, _ := opp["foreign_exchange"].(string)
		deltaPct := 0.0
		if strings.EqualFold(baseEx, simEx) {
			deltaPct = currOffsetPct
			if bp, ok := opp["base_price_usd"].(float64); ok && bp > 0 {
				opp["base_price_usd"] = bp * (1.0 + currOffsetPct/100.0)
			}
		} else if strings.EqualFold(foreignEx, simEx) {
			deltaPct = -currOffsetPct
			if fp, ok := opp["foreign_price_usd"].(float64); ok && fp > 0 {
				opp["foreign_price_usd"] = fp * (1.0 + currOffsetPct/100.0)
			}
		}
		if math.Abs(deltaPct) > 1e-6 {
			if premPct, ok := opp["premium_pct"].(float64); ok {
				newPremPct := premPct + deltaPct
				opp["premium_pct"] = newPremPct
				opp["expected_margin"] = math.Abs(newPremPct)
			}
			if roi, ok := opp["expected_roi"].(float64); ok {
				opp["expected_roi"] = roi + (deltaPct / 100.0)
			}
			if curPrem, ok := opp["current_prem"].(float64); ok {
				opp["current_prem"] = curPrem + (deltaPct / 100.0)
			}
			if futPrem, ok := opp["expected_future_prem"].(float64); ok {
				opp["expected_future_prem"] = futPrem + (deltaPct / 100.0)
			}
		}
	}

	if modified, err := json.Marshal(opps); err == nil {
		return modified
	}
	return rawData
}

// handleOpportunitiesWS streams the live Ranker Evaluations for quick UI updates
func (s *Server) handleOpportunitiesWS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	userID := getUserIDFromContext(r)
	mode := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("mode")))
	if mode != "PAPER" && mode != "LIVE" {
		if settings, err := s.DB.GetUserSettings(userID); err == nil && settings != nil && settings.ActiveViewMode != "" {
			mode = strings.ToUpper(settings.ActiveViewMode)
		} else {
			mode = "PAPER"
		}
	}

	ctx := r.Context()

	rawCh := make(chan []byte, 5)
	userCh := make(chan []byte, 5)

	if s.Redis != nil {
		go s.Redis.SubscribeRawOpportunities(ctx, rawCh)
		go s.Redis.SubscribeUserOpportunitiesByMode(ctx, fmt.Sprint(userID), mode, userCh)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case rawData := <-rawCh:
			outRaw := rawData
			if mode == "PAPER" {
				outRaw = s.applySimOffsetToRawOpps(rawData, userID)
			}
			w.Write([]byte("event: raw_opportunities\n"))
			w.Write([]byte("data: " + string(outRaw) + "\n\n"))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		case userData := <-userCh:
			w.Write([]byte("event: filtered_opportunities\n"))
			w.Write([]byte("data: " + string(userData) + "\n\n"))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}
}
