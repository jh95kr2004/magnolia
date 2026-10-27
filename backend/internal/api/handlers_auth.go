package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/magnolia/backend/internal/db/postgres"
)

// AuthMiddleware intercepts and decodes the JWT token from the Authorization header
func (s *Server) AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			http.Error(w, `{"error": "Unauthorized"}`, http.StatusUnauthorized)
			return
		}

		// tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
		// TODO: Validate JWT and extract UserID
		// For prototyping, we force inject UserID 1 in the context.
		
		// Continue execution
		next.ServeHTTP(w, r)
	}
}

// WebSocketAuthMiddleware validates WS connection queries
func (s *Server) WebSocketAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// e.g., /api/ws/dashboard?token=xyz
		// token := r.URL.Query().Get("token")
		// if !Valid(token) { http.Error() }
		next.ServeHTTP(w, r)
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
		UserID: user.ID,
		NotifyOnEntry: true,
		NotifyOnExit: true,
	})
	if err != nil {
		http.Error(w, `{"error": "Could not bootstrap profile"}`, http.StatusInternalServerError)
		return
	}

	// Push newly created user to the Dangling Queue so an Engine picks them up
	if pushErr := s.Redis.PushDanglingClient(context.Background(), user.Region, fmt.Sprint(user.ID)); pushErr != nil {
		// Log error but don't fail registration
		fmt.Printf("Failed to push user to queue: %v\n", pushErr)
	}

	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(`{"message": "User registered successfully"}`))
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
	if err != nil || user.PasswordHash != req.Password { // Mock password check
		http.Error(w, `{"error": "Invalid credentials"}`, http.StatusUnauthorized)
		return
	}

	// TODO: Generate standard JWT Token including user.ID
	// Here we just return a fake token
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"token": "fake-jwt-token-123",
		"user": map[string]interface{}{
			"id":    user.ID,
			"email": user.Email,
		},
	})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	// Retrieve UserID from Middleware context (Hardcoded 1 for prototype)
	user, err := s.DB.GetUserByEmail("admin@magnolia.test") 
	if err != nil {
		http.Error(w, `{"error": "User missing"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(user)
}
