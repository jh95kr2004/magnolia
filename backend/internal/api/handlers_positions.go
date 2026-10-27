package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// handleActivePositions lists all currently open positions for the authenticated user and requested mode (PAPER or LIVE)
func (s *Server) handleActivePositions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	userID := getUserIDFromContext(r)
	mode := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("mode")))
	if mode != "PAPER" && mode != "LIVE" {
		if settings, err := s.DB.GetUserSettings(userID); err == nil && settings != nil && settings.ActiveViewMode != "" {
			mode = strings.ToUpper(settings.ActiveViewMode)
		} else {
			mode = "PAPER"
		}
	}

	positions, err := s.DB.GetActivePositionsByUserAndMode(userID, mode == "PAPER")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(positions)
}

// handlePositionHistory queries closed positions for the authenticated user (optionally filtered by ?mode=PAPER|LIVE|ALL)
func (s *Server) handlePositionHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	userID := getUserIDFromContext(r)
	mode := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("mode")))
	if mode == "PAPER" || mode == "LIVE" {
		positions, err := s.DB.GetClosedPositionsByUserAndMode(userID, mode == "PAPER")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(positions)
		return
	}

	positions, err := s.DB.GetClosedPositionsByUser(userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(positions)
}

// handleForceClose dispatches a FORCE_CLOSE command over Redis to the user's Engine goroutine
// so real or paper exchange exit orders and loan repayments are executed safely.
func (s *Server) handleForceClose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	userID := getUserIDFromContext(r)
	posID := r.URL.Query().Get("id")
	if posID == "" {
		parts := strings.Split(r.URL.Path, "/")
		posID = parts[len(parts)-1]
	}

	if posID == "" || posID == "force_close" || posID == "active" {
		http.Error(w, `{"error": "Missing position id"}`, http.StatusBadRequest)
		return
	}

	mode := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("mode")))

	cmdPayload, _ := json.Marshal(map[string]interface{}{
		"action":      "FORCE_CLOSE",
		"position_id": posID,
		"mode":        mode,
	})

	if s.Redis != nil {
		if err := s.Redis.PublishUserCommand(r.Context(), fmt.Sprint(userID), cmdPayload); err != nil {
			http.Error(w, fmt.Sprintf(`{"error": "Failed to dispatch close command: %v"}`, err), http.StatusInternalServerError)
			return
		}
	} else {
		if err := s.DB.UpdatePositionStatus(posID, "CLOSED"); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"message": "Force close command dispatched to Strategy Engine"}`))
}

// handleForceEnter dispatches a FORCE_ENTER command over Redis to the user's target Engine goroutine (PAPER or LIVE)
// to execute atomic dual-leg orders.
func (s *Server) handleForceEnter(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	userID := getUserIDFromContext(r)
	var req struct {
		Mode            string  `json:"mode"`
		BaseExchange    string  `json:"base_exchange"`
		ForeignExchange string  `json:"foreign_exchange"`
		BaseSymbol      string  `json:"base_symbol"`
		ForeignSymbol   string  `json:"foreign_symbol"`
		Strategy        string  `json:"strategy"`
		StrategyType    string  `json:"strategy_type"`
		Symbol          string  `json:"symbol"`
		ExpectedRoi     float64 `json:"expected_roi"`
		InvestmentUsd   float64 `json:"investment_usd"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	mode := strings.ToUpper(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("mode")))
	}
	if mode != "PAPER" && mode != "LIVE" {
		if settings, err := s.DB.GetUserSettings(userID); err == nil && settings != nil && settings.ActiveViewMode != "" {
			mode = strings.ToUpper(settings.ActiveViewMode)
		} else {
			mode = "PAPER"
		}
	}

	baseSym := req.BaseSymbol
	if baseSym == "" {
		baseSym = req.Symbol
	}
	strat := req.Strategy
	if strat == "" {
		strat = req.StrategyType
	}

	cmdPayload, _ := json.Marshal(map[string]interface{}{
		"action":           "FORCE_ENTER",
		"mode":             mode,
		"base_exchange":    req.BaseExchange,
		"foreign_exchange": req.ForeignExchange,
		"base_symbol":      baseSym,
		"foreign_symbol":   req.ForeignSymbol,
		"strategy":         strat,
		"expected_roi":     req.ExpectedRoi,
		"investment_usd":   req.InvestmentUsd,
	})

	if s.Redis != nil {
		if err := s.Redis.PublishUserCommand(r.Context(), fmt.Sprint(userID), cmdPayload); err != nil {
			http.Error(w, fmt.Sprintf(`{"error": "Failed to dispatch entry command: %v"}`, err), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(`{"message": "Manual entry command dispatched to Strategy Engine"}`))
}
