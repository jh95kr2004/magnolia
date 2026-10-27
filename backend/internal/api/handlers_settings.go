package api

import (
	"encoding/json"
	"net/http"

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
		
		// Map the existing settings with new updates, preserving ID
		existing, _ := s.DB.GetUserSettings(userID)
		if existing != nil {
			req.ID = existing.ID
		}
		req.UserID = userID
		
		s.DB.SaveUserSettings(&req)
		w.Write([]byte(`{"message": "Settings updated"}`))
		return
	}
	http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
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
