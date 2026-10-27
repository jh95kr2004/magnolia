package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// ActiveConnections tracks all open UI WebSockets
var (
	wsConnMutex sync.RWMutex
	activeConns = make(map[uint][]chan []byte)
)

func (s *Server) pushDashboardSnapshot(ctx context.Context, w http.ResponseWriter, userID uint) {
	var stateMap map[string]interface{}
	if s.Redis != nil {
		if rawState, err := s.Redis.GetLatestUserState(ctx, fmt.Sprint(userID)); err == nil && len(rawState) > 0 {
			_ = json.Unmarshal(rawState, &stateMap)
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

	portfolioPayload := map[string]interface{}{
		"total_equity_usd":       capitalLimit,
		"available_balance":      capitalLimit,
		"foreign_share":          0.50,
		"rebalance_trigger_skew": triggerSkew,
		"rebalance_target_skew":  targetSkew,
		"exchange_balances":      []interface{}{},
		"web_lending_sessions":   map[string]bool{},
	}

	var posPnLs map[string]interface{}
	if stateMap != nil {
		if v, ok := stateMap["total_equity_usd"]; ok {
			portfolioPayload["total_equity_usd"] = v
		}
		if v, ok := stateMap["available_balance"]; ok {
			portfolioPayload["available_balance"] = v
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
	} else {
		// Fallback when Engine hasn't published state yet: list registered exchanges
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

	positions, _ := s.DB.GetActivePositionsByUser(userID)
	type enrichedPos struct {
		ID                 uint      `json:"ID"`
		PositionID         string    `json:"PositionID"`
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
	s.pushDashboardSnapshot(r.Context(), w, userID)

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
			s.pushDashboardSnapshot(r.Context(), w, userID)
		case <-ticker.C:
			s.pushDashboardSnapshot(r.Context(), w, userID)
		}
	}
}

// handleOpportunitiesWS streams the live Ranker Evaluations for quick UI updates
func (s *Server) handleOpportunitiesWS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	userID := getUserIDFromContext(r)
	ctx := r.Context()

	rawCh := make(chan []byte, 5)
	userCh := make(chan []byte, 5)

	if s.Redis != nil {
		go s.Redis.SubscribeRawOpportunities(ctx, rawCh)
		go s.Redis.SubscribeUserOpportunities(ctx, fmt.Sprint(userID), userCh)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case rawData := <-rawCh:
			w.Write([]byte("event: raw_opportunities\n"))
			w.Write([]byte("data: " + string(rawData) + "\n\n"))
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
