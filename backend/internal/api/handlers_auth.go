package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/magnolia/backend/internal/db/postgres"
)

type ctxKeyUserID struct{}

func parseTokenUserID(tokenStr string) uint {
	tokenStr = strings.TrimSpace(tokenStr)
	if strings.HasPrefix(tokenStr, "magnolia-token-") {
		idStr := strings.TrimPrefix(tokenStr, "magnolia-token-")
		if parsed, err := strconv.ParseUint(idStr, 10, 64); err == nil && parsed > 0 {
			return uint(parsed)
		}
	}
	return 1
}

func getUserIDFromContext(r *http.Request) uint {
	if val := r.Context().Value(ctxKeyUserID{}); val != nil {
		if id, ok := val.(uint); ok && id > 0 {
			return id
		}
	}
	if qToken := r.URL.Query().Get("token"); qToken != "" {
		return parseTokenUserID(qToken)
	}
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		return parseTokenUserID(strings.TrimPrefix(authHeader, "Bearer "))
	}
	return 1
}

// AuthMiddleware intercepts and decodes the token from the Authorization header
func (s *Server) AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			http.Error(w, `{"error": "Unauthorized"}`, http.StatusUnauthorized)
			return
		}

		tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
		userID := parseTokenUserID(tokenStr)
		ctx := context.WithValue(r.Context(), ctxKeyUserID{}, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	}
}

// WebSocketAuthMiddleware validates SSE/WS connection queries and extracts UserID
func (s *Server) WebSocketAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := getUserIDFromContext(r)
		ctx := context.WithValue(r.Context(), ctxKeyUserID{}, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	}
}

// ============================================
// Auth Controllers
// ============================================

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Region   string `json:"region"` // [NEW] Client-provided region
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	region := req.Region
	if region == "" {
		region = "kr" // Default fallback
	}

	// TODO: Hash password with bcrypt before saving
	user := &postgres.User{
		Email:        req.Email,
		PasswordHash: req.Password, // Mock hash
		Region:       region,
	}

	if err := s.DB.CreateUser(user); err != nil {
		http.Error(w, `{"error": "Account exists or DB error"}`, http.StatusConflict)
		return
	}

	// Create Default Settings
	err := s.DB.SaveUserSettings(&postgres.UserSettings{
		UserID:           user.ID,
		NotifyOnEntry:    true,
		NotifyOnExit:     true,
		ActiveViewMode:   "PAPER",
		PaperBotStatus:   "RUNNING",
		LiveBotStatus:    "STOPPED",
		PaperUpbitKRW:    50000000.0,
		PaperCoinbaseUSD: 35000.0,
		SimExchange:      "UPBIT",
	})
	if err != nil {
		http.Error(w, `{"error": "Could not bootstrap profile"}`, http.StatusInternalServerError)
		return
	}

	// Create Default Strategy & Risk Thresholds
	_ = s.DB.SaveThresholds(postgres.DefaultUserThreshold(user.ID, "PremiumStrategy"))

	// Push newly created user to the Dangling Queue so an Engine picks them up
	if s.Redis != nil {
		if pushErr := s.Redis.PushDanglingClient(context.Background(), user.Region, fmt.Sprint(user.ID)); pushErr != nil {
			fmt.Printf("Failed to push user to queue: %v\n", pushErr)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "User registered successfully",
		"token":   fmt.Sprintf("magnolia-token-%d", user.ID),
		"user": map[string]interface{}{
			"id":     user.ID,
			"email":  user.Email,
			"region": user.Region,
		},
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	user, err := s.DB.GetUserByEmail(req.Email)
	if err != nil || user.PasswordHash != req.Password {
		http.Error(w, `{"error": "Invalid credentials"}`, http.StatusUnauthorized)
		return
	}

	if user.EngineNodeID == "" && s.Redis != nil {
		_ = s.Redis.PushDanglingClient(context.Background(), user.Region, fmt.Sprint(user.ID))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"token": fmt.Sprintf("magnolia-token-%d", user.ID),
		"user": map[string]interface{}{
			"id":     user.ID,
			"email":  user.Email,
			"region": user.Region,
		},
	})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	userID := getUserIDFromContext(r)
	user, err := s.DB.GetUserByID(userID)
	if err != nil {
		user, err = s.DB.GetUserByEmail("admin@magnolia.test")
		if err != nil {
			http.Error(w, `{"error": "User missing"}`, http.StatusNotFound)
			return
		}
	}

	if user.EngineNodeID == "" && s.Redis != nil {
		_ = s.Redis.PushDanglingClient(context.Background(), user.Region, fmt.Sprint(user.ID))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":     user.ID,
		"email":  user.Email,
		"region": user.Region,
	})
}
