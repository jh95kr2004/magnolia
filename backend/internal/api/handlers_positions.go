package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/magnolia/backend/internal/db/postgres"
)

// handleActivePositions lists all currently open positions (REST fallback)
func (s *Server) handleActivePositions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// This is node-agnostic for user UI viewing (can view US and KR positions)
	positions, err := s.DB.GetActivePositions("")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(positions)
}

// handlePositionHistory queries old closed positions
func (s *Server) handlePositionHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	
	userID := uint(1) // Mock UI Auth
	positions, err := s.DB.GetClosedPositionsByUser(userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(positions)
}

// handleForceClose allows manual termination of an active position
func (s *Server) handleForceClose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Clean position ID from path e.g. /api/positions/force_close/POS-123
	// Or from query if passed.
	posID := r.URL.Query().Get("id")
	if posID == "" {
		// Try to extract from Path
		parts := strings.Split(r.URL.Path, "/")
		posID = parts[len(parts)-1]
	}

	if posID == "" || posID == "force_close" || posID == "active" {
		http.Error(w, `{"error": "Missing position id"}`, http.StatusBadRequest)
		return
	}

	// Attempt to close natively via Engine (If the strategy resides on this node)
	// For production: The node would check if the position belongs to it. Wait, the load balancer
	// routes randomly. Need a Targeted Pub/Sub to trigger the specific node holding the position context!
	// Here, we just mark it CLOSED in the DB as a mock.

	err := s.DB.UpdatePositionStatus(posID, "CLOSED")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Fire completion notification for SSE streams
	s.DB.PublishUserEvent("1", `{"action": "POSITION_CLOSED", "position_id": "`+posID+`"}`)

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"message": "Position force closed"}`))
}

// handleForceEnter manually forces an entry into the Ranker's latest opportunity
func (s *Server) handleForceEnter(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		BaseExchange   string `json:"base_exchange"`
		ForeignExchange string `json:"foreign_exchange"`
		StrategyType   string `json:"strategy_type"`
		Symbol         string `json:"symbol"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// This would invoke the strategy.ArbitrageStrategy ExecuteEntry directly through the Engine.
	// For demo, we just create a mock active position row
	pos := &postgres.ActivePosition{
		PositionID:      "POS-MANUAL-1",
		MonitorNodeID:   "test-node",
		Status:          "OPEN",
		Type:            req.StrategyType,
		BaseExchange:    req.BaseExchange,
		ForeignExchange: req.ForeignExchange,
		BaseSymbol:      req.Symbol,
		ForeignSymbol:   req.Symbol,
		InvestmentUSD:   1000.0,
	}

	s.DB.SavePosition(pos)

	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(`{"message": "Manual Entry Triggered Successfully"}`))
}
