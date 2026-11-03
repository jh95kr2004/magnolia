package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/magnolia/backend/internal/db/postgres"
	"github.com/magnolia/backend/internal/strategy"
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
		ActionType      int     `json:"action_type"`
		EventType       string  `json:"event_type"`
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
		"action_type":      req.ActionType,
		"event_type":       req.EventType,
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

// handlePortfolioHistory returns chronological Total Equity snapshots for the Coinbase-style area chart.
// Query params: ?mode=PAPER|LIVE&range=1H|24H|1W|1M|1Y|ALL
func (s *Server) handlePortfolioHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	userID := getUserIDFromContext(r)
	mode := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("mode")))
	settings, _ := s.DB.GetUserSettings(userID)
	if mode != "PAPER" && mode != "LIVE" {
		if settings != nil && settings.ActiveViewMode != "" {
			mode = strings.ToUpper(settings.ActiveViewMode)
		} else {
			mode = "PAPER"
		}
	}
	isPaper := mode == "PAPER"

	rangeParam := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("range")))
	if rangeParam == "" {
		rangeParam = "24H"
	}

	now := time.Now().UTC()
	var since time.Time
	switch rangeParam {
	case "1H":
		since = now.Add(-1 * time.Hour)
	case "24H":
		since = now.Add(-24 * time.Hour)
	case "1W":
		since = now.Add(-7 * 24 * time.Hour)
	case "1M":
		since = now.Add(-30 * 24 * time.Hour)
	case "1Y":
		since = now.Add(-365 * 24 * time.Hour)
	case "ALL":
		since = time.Time{}
	default:
		rangeParam = "24H"
		since = now.Add(-24 * time.Hour)
	}

	fxRate := strategy.GetFXRate("KRW")
	if fxRate <= 0 {
		fxRate = 1400.0
	}

	snaps, err := s.DB.GetEquitySnapshots(userID, isPaper, since, 300)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// If fewer than 2 points exist in the requested range, seed/synthesize a clean baseline so the chart always renders smoothly
	if len(snaps) < 2 {
		baselineUSD := 0.0
		if len(snaps) == 1 && snaps[0].TotalEquityUSD > 0 {
			baselineUSD = snaps[0].TotalEquityUSD
		} else if isPaper {
			upbitKRW := 50000000.0
			cbUSD := 35000.0
			if settings != nil {
				if settings.PaperUpbitKRW > 0 {
					upbitKRW = settings.PaperUpbitKRW
				}
				if settings.PaperCoinbaseUSD > 0 {
					cbUSD = settings.PaperCoinbaseUSD
				}
			}
			baselineUSD = (upbitKRW / fxRate) + cbUSD
		}

		startTime := since
		if startTime.IsZero() {
			startTime = now.Add(-24 * time.Hour)
		}
		if len(snaps) == 0 {
			snaps = []postgres.EquitySnapshot{
				{
					UserID:           userID,
					IsPaper:          isPaper,
					TotalEquityUSD:   baselineUSD,
					AvailableUSD:     baselineUSD,
					UnrealizedPnLUSD: 0,
					FXRateKRW:        fxRate,
					RecordedAt:       startTime,
				},
				{
					UserID:           userID,
					IsPaper:          isPaper,
					TotalEquityUSD:   baselineUSD,
					AvailableUSD:     baselineUSD,
					UnrealizedPnLUSD: 0,
					FXRateKRW:        fxRate,
					RecordedAt:       now,
				},
			}
		} else if len(snaps) == 1 {
			first := snaps[0]
			snaps = []postgres.EquitySnapshot{
				{
					UserID:           userID,
					IsPaper:          isPaper,
					TotalEquityUSD:   first.TotalEquityUSD,
					AvailableUSD:     first.AvailableUSD,
					UnrealizedPnLUSD: first.UnrealizedPnLUSD,
					FXRateKRW:        first.FXRateKRW,
					RecordedAt:       first.RecordedAt.Add(-1 * time.Minute),
				},
				first,
			}
		}
	}

	type PointDTO struct {
		Timestamp        string  `json:"timestamp"`
		TotalEquityUSD   float64 `json:"total_equity_usd"`
		AvailableUSD     float64 `json:"available_usd"`
		UnrealizedPnLUSD float64 `json:"unrealized_pnl_usd"`
		FXRateKRW        float64 `json:"fx_rate_krw"`
	}

	points := make([]PointDTO, 0, len(snaps))
	for _, snap := range snaps {
		rFX := snap.FXRateKRW
		if rFX <= 0 {
			rFX = fxRate
		}
		points = append(points, PointDTO{
			Timestamp:        snap.RecordedAt.UTC().Format(time.RFC3339),
			TotalEquityUSD:   snap.TotalEquityUSD,
			AvailableUSD:     snap.AvailableUSD,
			UnrealizedPnLUSD: snap.UnrealizedPnLUSD,
			FXRateKRW:        rFX,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"mode":        mode,
		"range":       rangeParam,
		"fx_rate_krw": fxRate,
		"points":      points,
	})
}
