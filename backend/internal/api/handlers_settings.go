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
	"github.com/magnolia/backend/internal/strategy"
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
			"started_at":         int64(0),
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
		"started_at":         settings.SimStartedAt,
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
	if math.Abs(req.TargetOffsetPct) < 1e-6 && req.RampMinutes <= 0 {
		settings.SimStartOffsetPct = 0
		settings.SimStartedAt = 0
	}
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
		UpbitKRW         *float64 `json:"upbit_krw"`
		CoinbaseUSD      *float64 `json:"coinbase_usd"`
		PaperUpbitKRW    *float64 `json:"paper_upbit_krw"`
		PaperCoinbaseUSD *float64 `json:"paper_coinbase_usd"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	upbitKRW := -1.0
	if req.PaperUpbitKRW != nil {
		upbitKRW = *req.PaperUpbitKRW
	} else if req.UpbitKRW != nil {
		upbitKRW = *req.UpbitKRW
	}

	coinbaseUSD := -1.0
	if req.PaperCoinbaseUSD != nil {
		coinbaseUSD = *req.PaperCoinbaseUSD
	} else if req.CoinbaseUSD != nil {
		coinbaseUSD = *req.CoinbaseUSD
	}

	if upbitKRW < 0 {
		upbitKRW = 50000000.0
	}
	if coinbaseUSD < 0 {
		coinbaseUSD = 35000.0
	}
	if upbitKRW <= 0 && coinbaseUSD <= 0 {
		upbitKRW = 50000000.0
		coinbaseUSD = 35000.0
	}

	settings, _ := s.DB.GetUserSettings(userID)
	if settings == nil {
		settings = &postgres.UserSettings{UserID: userID}
	}
	settings.PaperUpbitKRW = upbitKRW
	settings.PaperCoinbaseUSD = coinbaseUSD
	initialBalancesJSON, _ := json.Marshal(map[string]interface{}{
		"UPBIT": map[string]interface{}{
			"balances": map[string]float64{"KRW": upbitKRW, "BTC": 0, "ETH": 0},
			"borrowed": map[string]float64{},
			"futures":  map[string]interface{}{},
		},
		"COINBASE": map[string]interface{}{
			"balances": map[string]float64{"USD": coinbaseUSD, "BTC": 0, "ETH": 0},
			"borrowed": map[string]float64{},
			"futures":  map[string]interface{}{},
		},
	})
	settings.PaperBalancesJSON = string(initialBalancesJSON)
	_ = s.DB.SaveUserSettings(settings)
	_ = s.DB.CloseAllActivePositionsByUserAndMode(userID, true)

	fxRate := strategy.GetFXRate("KRW")
	if s.Redis != nil {
		if rawState, err := s.Redis.GetLatestUserStateByMode(r.Context(), fmt.Sprint(userID), "PAPER"); err == nil && len(rawState) > 0 {
			var prevState map[string]interface{}
			if json.Unmarshal(rawState, &prevState) == nil {
				if prevFX, ok := prevState["fx_rate_krw"].(float64); ok && prevFX > 500 {
					fxRate = prevFX
				}
			}
		}
	}
	if fxRate <= 0 {
		fxRate = 1385.0
	}

	upbitUSD := upbitKRW / fxRate
	initialEquityUSD := upbitUSD + coinbaseUSD
	upbitPct := 50.0
	cbPct := 50.0
	foreignShare := 0.50
	if initialEquityUSD > 0 {
		upbitPct = (upbitUSD / initialEquityUSD) * 100.0
		cbPct = (coinbaseUSD / initialEquityUSD) * 100.0
		foreignShare = coinbaseUSD / initialEquityUSD
	}

	exchangeBalances := []map[string]interface{}{
		{
			"name":             "UPBIT",
			"quote_currency":   "KRW",
			"available_native": upbitKRW,
			"available_usd":    upbitUSD,
			"percent":          upbitPct,
			"web_lending_ok":   true,
		},
		{
			"name":             "COINBASE",
			"quote_currency":   "USD",
			"available_native": coinbaseUSD,
			"available_usd":    coinbaseUSD,
			"percent":          cbPct,
			"web_lending_ok":   true,
		},
	}
	breakdownBytes, _ := json.Marshal(exchangeBalances)
	_ = s.DB.ResetPaperEquitySnapshots(userID, initialEquityUSD, fxRate, string(breakdownBytes))

	if s.Redis != nil {
		paperBotStatus := settings.PaperBotStatus
		if paperBotStatus == "" {
			paperBotStatus = "RUNNING"
		}
		resetStatePayload, _ := json.Marshal(map[string]interface{}{
			"user_id":                userID,
			"mode":                   "PAPER",
			"bot_status":             paperBotStatus,
			"available_balance":      initialEquityUSD,
			"total_equity_usd":       initialEquityUSD,
			"fx_rate_krw":            fxRate,
			"exchange_balances":      exchangeBalances,
			"foreign_share":          foreignShare,
			"rebalance_trigger_skew": 0.20,
			"rebalance_target_skew":  0.50,
			"web_lending_sessions":   map[string]bool{"UPBIT": true, "COINBASE": true},
			"position_pnls":          map[string]float64{},
			"current_exposures":      map[string]float64{},
			"unrealized_pnls":        map[string]float64{},
			"pair_states":            map[string]int{},
			"state":                  0,
		})
		_ = s.Redis.PublishUserStateByMode(r.Context(), fmt.Sprint(userID), "PAPER", resetStatePayload)

		cmdPayload, _ := json.Marshal(map[string]interface{}{
			"action":             "RESET_PAPER_ACCOUNT",
			"mode":               "PAPER",
			"upbit_krw":          upbitKRW,
			"coinbase_usd":       coinbaseUSD,
			"paper_upbit_krw":    upbitKRW,
			"paper_coinbase_usd": coinbaseUSD,
		})
		_ = s.Redis.PublishUserCommand(r.Context(), fmt.Sprint(userID), cmdPayload)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message":            "Paper trading account reset successfully",
		"paper_upbit_krw":    upbitKRW,
		"paper_coinbase_usd": coinbaseUSD,
		"total_equity_usd":   initialEquityUSD,
		"fx_rate_krw":        fxRate,
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

// handleSimEvent injects or clears simulated exchange announcement & assassination events in Paper Mode.
func (s *Server) handleSimEvent(w http.ResponseWriter, r *http.Request) {
	userID := getUserIDFromContext(r)

	if r.Method == http.MethodGet {
		sym := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol")))
		if sym == "" {
			sym = "BTC"
		}
		sym = strings.Split(sym, "-")[0]
		events := []exchange.AnnouncementEvent{}
		if s.Redis != nil {
			events = s.Redis.GetCoinAnnouncementEvents(r.Context(), sym)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"symbol": sym,
			"events": events,
		})
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Exchange               string  `json:"exchange"`
		Symbol                 string  `json:"symbol"`
		EventType              string  `json:"event_type"`
		ScheduledOffsetMin     int     `json:"scheduled_offset_min"`
		TargetOffsetPct        float64 `json:"target_offset_pct"`
		RampMinutes            float64 `json:"ramp_minutes"`
		AutoStageAssassination bool    `json:"auto_stage_assassination"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ex := strings.ToUpper(strings.TrimSpace(req.Exchange))
	if ex == "" {
		ex = "UPBIT"
	}
	coin := strings.ToUpper(strings.TrimSpace(req.Symbol))
	if coin == "" {
		coin = "BTC"
	}
	coin = strings.Split(coin, "-")[0]
	evType := strings.ToUpper(strings.TrimSpace(req.EventType))

	nowUnix := time.Now().Unix()

	if evType == "CLEAR" || evType == "RESET" {
		if s.Redis != nil {
			_ = s.Redis.ClearCoinAnnouncementEvents(r.Context(), coin)
		}
		if s.DB != nil {
			if settings, _ := s.DB.GetUserSettings(userID); settings != nil {
				settings.SimStartOffsetPct = 0
				settings.SimTargetOffsetPct = 0
				settings.SimRampMinutes = 0
				settings.SimStartedAt = 0
				_ = s.DB.SaveUserSettings(settings)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "Simulated events and offset reset",
			"symbol":  coin,
		})
		return
	}

	nowTime := time.Now().UTC()
	var schedPtr *time.Time
	if req.ScheduledOffsetMin > 0 {
		t := nowTime.Add(time.Duration(req.ScheduledOffsetMin) * time.Minute)
		schedPtr = &t
	} else if evType == "NEW_KRW_LISTING" {
		t := nowTime.Add(60 * time.Minute)
		schedPtr = &t
	} else if evType == "WALLET_SUSPENSION" {
		t := nowTime.Add(90 * time.Minute)
		schedPtr = &t
	}

	var title, summaryKO, summaryEN string
	switch evType {
	case "NEW_KRW_LISTING":
		title = fmt.Sprintf("[모의 공지] 원화(KRW) 마켓 디지털 자산 추가 (%s)", coin)
		summaryKO = fmt.Sprintf("🚀 원화 마켓 신규 상장 예고 (%s) — 상장 개시 직후 가두리 펌핑 기대", coin)
		summaryEN = fmt.Sprintf("🚀 New KRW Market Listing Scheduled (%s) — Listing Surge Expected", coin)
	case "INVESTMENT_WARNING":
		title = fmt.Sprintf("[모의 공지] 거래유의 종목 지정 안내 (%s)", coin)
		summaryKO = fmt.Sprintf("⚠️ 거래유의 종목 지정 및 입금 정지 (%s) — 가두리 프리미엄 급등 기대", coin)
		summaryEN = fmt.Sprintf("⚠️ Investment Warning & Deposit Suspended (%s) — Gaduri Surge Expected", coin)
	case "WALLET_SUSPENSION":
		title = fmt.Sprintf("[모의 공지] 네트워크 업그레이드에 따른 입출금 일시 중단 안내 (%s)", coin)
		summaryKO = fmt.Sprintf("🔒 입출금 일시 중단 예고 (%s) — 중단 전 선제 입금(암살 대기) 유효", coin)
		summaryEN = fmt.Sprintf("🔒 Wallet Suspension Scheduled (%s) — Pre-stage Assassination Active", coin)
	case "DELISTING":
		title = fmt.Sprintf("[모의 공지] 거래지원 종료(상장폐지) 안내 (%s)", coin)
		summaryKO = fmt.Sprintf("🚨 상장폐지 결정 (%s) — 신규 진입 즉시 차단 및 보유 포지션 긴급 청산", coin)
		summaryEN = fmt.Sprintf("🚨 Delisting Announced (%s) — Entry Veto & Emergency Unwind", coin)
	case "IN_TRANSIT_KIMP_DROP":
		title = fmt.Sprintf("[모의 시나리오] 전송 도중 김프 급락 방어 (%s)", coin)
		summaryKO = fmt.Sprintf("🛡️ 전송 도중 김프 급락 (%s) — 손절 매도 대신 STAGED_ASSASSINATION 자동 전환", coin)
		summaryEN = fmt.Sprintf("🛡️ In-Transit Kimp Drop (%s) — Auto-switched to STAGED_ASSASSINATION", coin)
	default:
		evType = "INVESTMENT_WARNING"
		title = fmt.Sprintf("[모의 공지] 이벤트 시뮬레이션 (%s)", coin)
		summaryKO = title
		summaryEN = title
	}

	annEvent := exchange.AnnouncementEvent{
		ID:          fmt.Sprintf("sim-%s-%d", strings.ToLower(coin), nowUnix),
		Exchange:    ex,
		EventType:   evType,
		Symbols:     []string{coin},
		Title:       title,
		SummaryKO:   summaryKO,
		SummaryEN:   summaryEN,
		AnnouncedAt: nowTime,
		ScheduledAt: schedPtr,
		IsCatalyst:  evType != "NONE" && evType != "DELISTING",
	}

	if s.Redis != nil && evType != "IN_TRANSIT_KIMP_DROP" {
		_ = s.Redis.PublishAnnouncementEvent(r.Context(), annEvent)
	}

	// Apply accompanying Paper Mode price offset ramp if requested
	if s.DB != nil {
		settings, _ := s.DB.GetUserSettings(userID)
		if settings == nil {
			settings = &postgres.UserSettings{
				UserID:           userID,
				ActiveViewMode:   "PAPER",
				PaperBotStatus:   "RUNNING",
				LiveBotStatus:    "STOPPED",
				PaperUpbitKRW:    50000000.0,
				PaperCoinbaseUSD: 35000.0,
				SimExchange:      ex,
			}
		}
		currSim := computeSimStatusFromSettings(settings)
		currOffset, _ := currSim["current_offset_pct"].(float64)
		settings.SimExchange = ex
		settings.SimStartOffsetPct = currOffset
		settings.SimTargetOffsetPct = req.TargetOffsetPct
		settings.SimRampMinutes = req.RampMinutes
		settings.SimStartedAt = nowUnix
		_ = s.DB.SaveUserSettings(settings)

		if s.Redis != nil {
			simCmd, _ := json.Marshal(map[string]interface{}{
				"action":                "SET_SIM_OFFSET",
				"mode":                  "PAPER",
				"exchange":              ex,
				"sim_exchange":          ex,
				"target_offset_pct":     req.TargetOffsetPct,
				"sim_target_offset_pct": req.TargetOffsetPct,
				"ramp_minutes":          req.RampMinutes,
				"sim_ramp_minutes":      req.RampMinutes,
			})
			_ = s.Redis.PublishUserCommand(r.Context(), fmt.Sprint(userID), simCmd)
		}
	}

	// Dispatch corresponding Paper Engine position action so the user sees immediate execution
	if s.Redis != nil {
		if evType == "DELISTING" {
			if activePos, err := s.DB.GetActivePositionsByUserAndMode(userID, true); err == nil {
				for _, p := range activePos {
					if strings.HasPrefix(strings.ToUpper(p.BaseSymbol), coin+"-") {
						closeCmd, _ := json.Marshal(map[string]interface{}{
							"action":      "FORCE_CLOSE",
							"position_id": p.PositionID,
							"mode":        "PAPER",
						})
						_ = s.Redis.PublishUserCommand(r.Context(), fmt.Sprint(userID), closeCmd)
					}
				}
			}
		} else if req.AutoStageAssassination {
			actionType := 3 // STAGE_ASSASSINATION
			if evType == "IN_TRANSIT_KIMP_DROP" {
				actionType = 1 // Start normal transfer first, then let Kimp drop trigger auto-switch
			}
			enterCmd, _ := json.Marshal(map[string]interface{}{
				"action":           "FORCE_ENTER",
				"mode":             "PAPER",
				"base_exchange":    "UPBIT",
				"foreign_exchange": "COINBASE",
				"base_symbol":      coin + "-KRW",
				"foreign_symbol":   coin + "-USD",
				"strategy":         "FOREIGN_TRANSFER",
				"action_type":      actionType,
				"event_type":       evType,
				"expected_roi":     0.045,
				"investment_usd":   2500.0,
			})
			_ = s.Redis.PublishUserCommand(r.Context(), fmt.Sprint(userID), enterCmd)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "Simulated announcement event dispatched",
		"event":   annEvent,
	})
}
