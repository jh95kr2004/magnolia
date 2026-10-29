package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/magnolia/backend/internal/db/postgres"
	"github.com/magnolia/backend/internal/exchange"
)

// handleExchangeKeySubmit validates and saves the user API keys
func (s *Server) handleExchangeKeySubmit(w http.ResponseWriter, r *http.Request) {
	userID := getUserIDFromContext(r)
	if r.Method == http.MethodGet {
		// Return list of keys
		keys, err := s.DB.GetAllAPIKeys(userID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		
		// Map for UI
		type uiKey struct {
			Exchange string `json:"exchange"`
			IsActive bool   `json:"is_active"`
			IsValid  bool   `json:"is_valid"`
			ErrorMsg string `json:"error_msg"`
			ExpiresAt string `json:"expires_at"`
		}
		resp := []uiKey{}
		for _, k := range keys {
			exp := ""
			if k.ExpiresAt != nil {
				exp = k.ExpiresAt.Format("2006-01-02")
			}
			resp = append(resp, uiKey{
				Exchange: k.Exchange, 
				IsActive: k.IsActive,
				IsValid: k.IsValid,
				ErrorMsg: k.ErrorMessage,
				ExpiresAt: exp,
			})
		}
		
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}


	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Exchange  string `json:"exchange"`
		AccessKey string `json:"access_key"`
		SecretKey string `json:"secret_key"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Immediate Verification Hook
	var client exchange.Client
	if req.Exchange == "UPBIT" {
		client = exchange.NewUpbitClient(req.AccessKey, req.SecretKey)
	} else if req.Exchange == "COINBASE" {
		// Mock Limit
		client, _ = exchange.NewCoinbaseClient(req.AccessKey, req.SecretKey, 250000.0) 
	} else {
		http.Error(w, `{"error": "Unsupported exchange"}`, http.StatusBadRequest)
		return
	}

	// Test the key validity by forcing a balance check
	_, err := client.GetBalances(r.Context())
	if err != nil {
		http.Error(w, `{"error": "Invalid API Key or unauthorized IP/permissions"}`, http.StatusForbidden)
		return
	}

	// It's valid! Encrypt & Save to Postgres
	encryptedSecret, err := postgres.Encrypt([]byte(req.SecretKey))
	if err != nil {
		http.Error(w, `{"error": "Failed to encrypt secret"}`, http.StatusInternalServerError)
		return
	}

	keyRecord := &postgres.UserAPIKey{
		UserID:                 userID,
		Exchange:               req.Exchange,
		AccessKey:              req.AccessKey,
		SecretKey:              string(encryptedSecret),
		DefaultWithdrawalLimit: 250000.0,
		IsActive:               true,
		IsValid:                true,
	}

	// Delete old key if overwriting
	s.DB.DeleteAPIKey(userID, req.Exchange)

	if err := s.DB.SaveAPIKey(keyRecord); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(`{"message": "API Key verified and saved successfully"}`))
}

// handleExchangeKeyDelete removes an API key, preventing breaking live positions.
func (s *Server) handleExchangeKeyDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	userID := getUserIDFromContext(r)
	var req struct {
		Exchange string `json:"exchange"`
		Force    bool   `json:"force"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	count, _ := s.DB.CountActivePositions(req.Exchange)
	if count > 0 && !req.Force {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error": "Active positions exist", "requires_force": true}`))
		return
	}

	if err := s.DB.DeleteAPIKey(userID, req.Exchange); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write([]byte(`{"message": "Exchange Key successfully deleted"}`))
}

// handleExchangeKeyToggle pauses an API key. 
func (s *Server) handleExchangeKeyToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	userID := getUserIDFromContext(r)
	var req struct {
		Exchange string `json:"exchange"`
		IsActive bool   `json:"is_active"`
		Force    bool   `json:"force"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	// If trying to disable, check constraints
	if !req.IsActive {
		count, _ := s.DB.CountActivePositions(req.Exchange)
		if count > 0 && !req.Force {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error": "Active positions exist", "requires_force": true}`))
			return
		}
	}

	s.DB.UpdateAPIKeyStatus(userID, req.Exchange, req.IsActive)
	w.Write([]byte(`{"message": "Exchange status toggled"}`))
}

// handleAlgoParameters sets tracking limits for a strategy
func (s *Server) handleAlgoParameters(w http.ResponseWriter, r *http.Request) {
	userID := getUserIDFromContext(r)
	if r.Method == http.MethodGet {
		threshold, _ := s.DB.LoadThresholds(userID, "PremiumStrategy")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(threshold)
		return
	}

	if r.Method == http.MethodPut || r.Method == http.MethodPost {
		var req postgres.UserThreshold
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		existing, err := s.DB.LoadThresholds(userID, "PremiumStrategy")
		if err == nil && existing != nil {
			req.ID = existing.ID
		}
		req.UserID = userID
		if req.StrategyName == "" {
			req.StrategyName = "PremiumStrategy"
		}
		if err := s.DB.SaveThresholds(&req); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Write([]byte(`{"message": "Thresholds updated"}`))
		return
	}
	http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
}

// handleSettingsProfile manages user meta-data
func (s *Server) handleSettingsProfile(w http.ResponseWriter, r *http.Request) {
	userID := getUserIDFromContext(r)
	if r.Method == http.MethodGet {
		settings, _ := s.DB.GetUserSettings(userID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(settings)
		return
	}

	if r.Method == http.MethodPut || r.Method == http.MethodPost {
		var req postgres.UserSettings
		json.NewDecoder(r.Body).Decode(&req)

		// Map the existing settings with new updates, preserving ID and Paper/Live state
		existing, _ := s.DB.GetUserSettings(userID)
		if existing != nil {
			req.ID = existing.ID
			if req.ActiveViewMode == "" {
				req.ActiveViewMode = existing.ActiveViewMode
			}
			if req.PaperBotStatus == "" {
				req.PaperBotStatus = existing.PaperBotStatus
			}
			if req.LiveBotStatus == "" {
				req.LiveBotStatus = existing.LiveBotStatus
			}
			if req.PaperUpbitKRW <= 0 {
				req.PaperUpbitKRW = existing.PaperUpbitKRW
			}
			if req.PaperCoinbaseUSD <= 0 {
				req.PaperCoinbaseUSD = existing.PaperCoinbaseUSD
			}
			if req.PaperBalancesJSON == "" {
				req.PaperBalancesJSON = existing.PaperBalancesJSON
			}
			if req.SimExchange == "" {
				req.SimExchange = existing.SimExchange
				req.SimStartOffsetPct = existing.SimStartOffsetPct
				req.SimTargetOffsetPct = existing.SimTargetOffsetPct
				req.SimRampMinutes = existing.SimRampMinutes
				req.SimStartedAt = existing.SimStartedAt
			}
		}
		req.UserID = userID

		s.DB.SaveUserSettings(&req)
		w.Write([]byte(`{"message": "Settings updated"}`))
		return
	}
	http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
}

func computeSimStatusFromSettings(settings *postgres.UserSettings) map[string]interface{} {
	if settings == nil {
		return map[string]interface{}{
			"exchange":           "UPBIT",
			"start_offset_pct":   0.0,
			"target_offset_pct":  0.0,
			"current_offset_pct": 0.0,
			"ramp_minutes":       0.0,
			"progress_pct":       100.0,
			"remaining_seconds":  0.0,
			"is_ramping":         false,
		}
	}
	ex := settings.SimExchange
	if ex == "" {
		ex = "UPBIT"
	}
	startPct := settings.SimStartOffsetPct
	targetPct := settings.SimTargetOffsetPct
	rampMin := settings.SimRampMinutes
	currPct := targetPct
	progressPct := 100.0
	remSec := 0.0
	isRamping := false

	if settings.SimStartedAt > 0 && rampMin > 0 && math.Abs(targetPct-startPct) > 1e-6 {
		totalSec := rampMin * 60.0
		elapsedSec := time.Since(time.Unix(settings.SimStartedAt, 0)).Seconds()
		if elapsedSec < 0 {
			elapsedSec = 0
		}
		if elapsedSec < totalSec {
			u := elapsedSec / totalSec
			alpha := 0.5 * (1.0 - math.Cos(math.Pi*u))
			currPct = startPct + (targetPct-startPct)*alpha
			progressPct = u * 100.0
			remSec = totalSec - elapsedSec
			isRamping = true
		}
	}

	return map[string]interface{}{
		"exchange":           ex,
		"start_offset_pct":   startPct,
		"target_offset_pct":  targetPct,
		"current_offset_pct": currPct,
		"ramp_minutes":       rampMin,
		"progress_pct":       progressPct,
		"remaining_seconds":  remSec,
		"is_ramping":         isRamping,
	}
}

// handleBotStatus controls independent Paper and Live engine execution states (RUN, DRAIN, STOP_AND_CLOSE)
// and tracks the user's active UI view mode (PAPER vs LIVE).
func (s *Server) handleBotStatus(w http.ResponseWriter, r *http.Request) {
	userID := getUserIDFromContext(r)
	settings, _ := s.DB.GetUserSettings(userID)
	if settings == nil {
		settings = &postgres.UserSettings{
			UserID:           userID,
			ActiveViewMode:   "PAPER",
			PaperBotStatus:   "RUNNING",
			LiveBotStatus:    "STOPPED",
			PaperUpbitKRW:    50000000.0,
			PaperCoinbaseUSD: 35000.0,
			SimExchange:      "UPBIT",
		}
	}

	if r.Method == http.MethodGet {
		paperPos, _ := s.DB.GetActivePositionsByUserAndMode(userID, true)
		livePos, _ := s.DB.GetActivePositionsByUserAndMode(userID, false)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"active_view_mode":           settings.ActiveViewMode,
			"paper_bot_status":           settings.PaperBotStatus,
			"live_bot_status":            settings.LiveBotStatus,
			"paper_open_positions_count": len(paperPos),
			"live_open_positions_count":  len(livePos),
			"sim_status":                 computeSimStatusFromSettings(settings),
		})
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Mode           string `json:"mode"`             // "PAPER" | "LIVE"
		SubAction      string `json:"sub_action"`       // "RUN" | "DRAIN" | "STOP_AND_CLOSE" | "VIEW_ONLY"
		Action         string `json:"action"`           // Alias for sub_action
		ActiveViewMode string `json:"active_view_mode"` // Optional UI view mode switch
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	mode := strings.ToUpper(strings.TrimSpace(req.Mode))
	subAction := strings.ToUpper(strings.TrimSpace(req.SubAction))
	if subAction == "" {
		subAction = strings.ToUpper(strings.TrimSpace(req.Action))
	}
	viewMode := strings.ToUpper(strings.TrimSpace(req.ActiveViewMode))

	if viewMode == "PAPER" || viewMode == "LIVE" {
		settings.ActiveViewMode = viewMode
	}

	if subAction == "" || subAction == "VIEW_ONLY" {
		_ = s.DB.SaveUserSettings(settings)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message":          "View mode updated",
			"active_view_mode": settings.ActiveViewMode,
			"paper_bot_status": settings.PaperBotStatus,
			"live_bot_status":  settings.LiveBotStatus,
		})
		return
	}

	if mode != "PAPER" && mode != "LIVE" {
		mode = settings.ActiveViewMode
		if mode == "" {
			mode = "PAPER"
		}
	}

	// Safety guard: starting Live auto-trading requires at least 2 active & valid API keys
	if mode == "LIVE" && (subAction == "RUN" || subAction == "RUNNING") {
		keys, _ := s.DB.GetAllAPIKeys(userID)
		validActiveCount := 0
		for _, k := range keys {
			if k.IsActive && k.IsValid {
				validActiveCount++
			}
		}
		if validActiveCount < 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": "실매매(Live) 봇을 가동하려면 최소 2개 거래소(UPBIT, COINBASE)의 유효한 API 키가 등록 및 활성화되어 있어야 합니다.",
			})
			return
		}
	}

	newStatus := "RUNNING"
	switch subAction {
	case "RUN", "RUNNING":
		newStatus = "RUNNING"
	case "DRAIN", "DRAINING":
		newStatus = "DRAINING"
	case "STOP_AND_CLOSE", "STOP", "STOPPED":
		newStatus = "STOPPED"
	default:
		http.Error(w, `{"error": "Unsupported sub_action"}`, http.StatusBadRequest)
		return
	}

	if mode == "PAPER" {
		settings.PaperBotStatus = newStatus
	} else {
		settings.LiveBotStatus = newStatus
	}
	_ = s.DB.SaveUserSettings(settings)

	if s.Redis != nil {
		cmdPayload, _ := json.Marshal(map[string]interface{}{
			"action":     "SET_BOT_STATUS",
			"mode":       mode,
			"sub_action": subAction,
		})
		_ = s.Redis.PublishUserCommand(r.Context(), fmt.Sprint(userID), cmdPayload)
	} else if subAction == "STOP_AND_CLOSE" {
		_ = s.DB.CloseAllActivePositionsByUserAndMode(userID, mode == "PAPER")
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message":          fmt.Sprintf("%s bot status updated to %s", mode, newStatus),
		"mode":             mode,
		"status":           newStatus,
		"paper_bot_status": settings.PaperBotStatus,
		"live_bot_status":  settings.LiveBotStatus,
	})
}

// handleSimOffset configures the web-controlled gradual price offset simulator (±N% over X minutes) for Paper Mode.
func (s *Server) handleSimOffset(w http.ResponseWriter, r *http.Request) {
	userID := getUserIDFromContext(r)
	settings, _ := s.DB.GetUserSettings(userID)
	if settings == nil {
		settings = &postgres.UserSettings{
			UserID:           userID,
			ActiveViewMode:   "PAPER",
			PaperBotStatus:   "RUNNING",
			LiveBotStatus:    "STOPPED",
			PaperUpbitKRW:    50000000.0,
			PaperCoinbaseUSD: 35000.0,
			SimExchange:      "UPBIT",
		}
	}

	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(computeSimStatusFromSettings(settings))
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Exchange        string  `json:"exchange"`
		TargetOffsetPct float64 `json:"target_offset_pct"`
		RampMinutes     float64 `json:"ramp_minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ex := strings.ToUpper(strings.TrimSpace(req.Exchange))
	if ex == "" {
		ex = "UPBIT"
	}
	if req.RampMinutes < 0 {
		req.RampMinutes = 0
	}

	currSim := computeSimStatusFromSettings(settings)
	currOffset, _ := currSim["current_offset_pct"].(float64)
	if !strings.EqualFold(settings.SimExchange, ex) {
		currOffset = 0.0
	}

	now := time.Now().Unix()
	settings.SimExchange = ex
	settings.SimStartOffsetPct = currOffset
	settings.SimTargetOffsetPct = req.TargetOffsetPct
	settings.SimRampMinutes = req.RampMinutes
	settings.SimStartedAt = now
	_ = s.DB.SaveUserSettings(settings)

	if s.Redis != nil {
		cmdPayload, _ := json.Marshal(map[string]interface{}{
			"action":                "SET_SIM_OFFSET",
			"mode":                  "PAPER",
			"exchange":              ex,
			"sim_exchange":          ex,
			"target_offset_pct":     req.TargetOffsetPct,
			"sim_target_offset_pct": req.TargetOffsetPct,
			"ramp_minutes":          req.RampMinutes,
			"sim_ramp_minutes":      req.RampMinutes,
		})
		_ = s.Redis.PublishUserCommand(r.Context(), fmt.Sprint(userID), cmdPayload)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message":    "Gradual price offset simulator updated",
		"sim_status": computeSimStatusFromSettings(settings),
	})
}

// handlePaperReset resets virtual Paper Mode balances and closes open paper positions.
func (s *Server) handlePaperReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	userID := getUserIDFromContext(r)
	var req struct {
		UpbitKRW    float64 `json:"upbit_krw"`
		CoinbaseUSD float64 `json:"coinbase_usd"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	if req.UpbitKRW <= 0 {
		req.UpbitKRW = 50000000.0
	}
	if req.CoinbaseUSD <= 0 {
		req.CoinbaseUSD = 35000.0
	}

	settings, _ := s.DB.GetUserSettings(userID)
	if settings == nil {
		settings = &postgres.UserSettings{UserID: userID}
	}
	settings.PaperUpbitKRW = req.UpbitKRW
	settings.PaperCoinbaseUSD = req.CoinbaseUSD
	settings.PaperBalancesJSON = "{}"
	_ = s.DB.SaveUserSettings(settings)
	_ = s.DB.CloseAllActivePositionsByUserAndMode(userID, true)

	if s.Redis != nil {
		cmdPayload, _ := json.Marshal(map[string]interface{}{
			"action":       "RESET_PAPER_ACCOUNT",
			"mode":         "PAPER",
			"upbit_krw":    req.UpbitKRW,
			"coinbase_usd": req.CoinbaseUSD,
		})
		_ = s.Redis.PublishUserCommand(r.Context(), fmt.Sprint(userID), cmdPayload)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message":            "Paper trading account reset successfully",
		"paper_upbit_krw":    req.UpbitKRW,
		"paper_coinbase_usd": req.CoinbaseUSD,
	})
}

// handleSupportedExchanges returns a list of supported exchanges
func (s *Server) handleSupportedExchanges(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	exchanges, _ := s.DB.GetSupportedExchanges()
	var resp []string
	for _, ex := range exchanges {
		resp = append(resp, ex.Name)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
