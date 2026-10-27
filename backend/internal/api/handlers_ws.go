package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/magnolia/backend/internal/exchange"
)

// ActiveConnections tracks all open UI WebSockets
var (
	wsConnMutex sync.RWMutex
	// activeConns store the user->connection pointers (mock implementation using ResponseWriter proxy)
	activeConns = make(map[uint][]chan []byte) 
)

// handleDashboardWS streams portfolio updates and Position state changes
func (s *Server) handleDashboardWS(w http.ResponseWriter, r *http.Request) {
	// 1. Upgrade HTTP to Server-Sent Events (SSE) for easy streaming without external ws libraries
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	userID := uint(1) // Mock UI Auth
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

	// Ticker for aggressive Portfolio Polling (Every 3 seconds)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	clientQuit := r.Context().Done()

	// Subscribe to Redis Notification Events specifically for this user
	eventCh := make(chan []byte, 10)
	go s.Redis.SubscribeUserEvents(r.Context(), fmt.Sprint(userID), eventCh)

	for {
		select {
		case <-clientQuit:
			log.Println("[Dashboard SSE] Client disconnected.")
			return
		case msg := <-msgChan: // Local UI-oriented direct messages (if any)
			w.Write([]byte("event: notification\n"))
			w.Write([]byte("data: " + string(msg) + "\n\n"))
			w.(http.Flusher).Flush()
		case redisEvent := <-eventCh: // Redis Pub/Sub Global Messages (Targeted to User)
			w.Write([]byte("event: notification\n"))
			w.Write([]byte("data: " + string(redisEvent) + "\n\n"))
			w.(http.Flusher).Flush()

		case <-ticker.C:
			// 1. Fetch live Portfolio (Hardcoded Upbit & CB mocks for now)
			// Upbit requires Decrypted keys. Mocked here.
			keys, _ := s.DB.GetAllAPIKeys(userID)
			var balances []exchange.Balance
			
			for _, key := range keys {
				if key.IsActive {
					// Dummy payload until Decrypt is refactored per user
					balances = append(balances, exchange.Balance{
						Currency: "BTC",
						Available: 0.5,
						Locked: 0.1,
					})
				}
			}

			portfolioData, _ := json.Marshal(map[string]interface{}{
				"portfolio": balances,
			})
			
			w.Write([]byte("event: portfolio\n"))
			w.Write([]byte("data: " + string(portfolioData) + "\n\n"))

			// 2. Stream Active Positions state directly to UI (using user-centric lookup)
			positions, _ := s.DB.GetActivePositionsByUser(userID)
			posData, _ := json.Marshal(map[string]interface{}{
				"positions": positions,
			})

			w.Write([]byte("event: positions\n"))
			w.Write([]byte("data: " + string(posData) + "\n\n"))
			w.(http.Flusher).Flush()
		}
	}
}

// handleOpportunitiesWS streams the live Ranker Evaluations for quick UI updates
func (s *Server) handleOpportunitiesWS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	userID := uint(1) // Mock UI Auth
	ctx := r.Context()
	
	rawCh := make(chan []byte, 5)
	userCh := make(chan []byte, 5)

	go s.Redis.SubscribeRawOpportunities(ctx, rawCh)
	go s.Redis.SubscribeUserOpportunities(ctx, fmt.Sprint(userID), userCh)

	for {
		select {
		case <-ctx.Done():
			return
		case rawData := <-rawCh:
			w.Write([]byte("event: raw_opportunities\n"))
			w.Write([]byte("data: " + string(rawData) + "\n\n"))
			w.(http.Flusher).Flush()
		case userData := <-userCh:
			w.Write([]byte("event: filtered_opportunities\n"))
			w.Write([]byte("data: " + string(userData) + "\n\n"))
			w.(http.Flusher).Flush()
		}
	}
}
