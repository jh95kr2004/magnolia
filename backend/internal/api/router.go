package api

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/magnolia/backend/internal/db/postgres"
	"github.com/magnolia/backend/internal/db/redis"
)

// Server holds the dependencies for the HTTP API Layer
type Server struct {
	DB     *postgres.DB
	Redis  *redis.PubSubClient
}

// NewServer initializes the HTTP REST & WebSocket API router.
func NewServer(db *postgres.DB, rclient *redis.PubSubClient) *Server {
	return &Server{
		DB:     db,
		Redis:  rclient,
	}
}

// Start configures the routes and begins listening on the specified port.
func (s *Server) Start(port string) error {
	mux := http.NewServeMux()

	// 1. Auth Service Routes
	mux.HandleFunc("/api/auth/register", s.handleRegister)
	mux.HandleFunc("/api/auth/login", s.handleLogin)
	mux.HandleFunc("/api/auth/me", s.AuthMiddleware(s.handleMe))

	// 2. Settings Service Routes (Protected)
	mux.HandleFunc("/api/settings/exchange", s.AuthMiddleware(s.handleExchangeKeySubmit))            // POST, GET
	mux.HandleFunc("/api/settings/exchange/delete", s.AuthMiddleware(s.handleExchangeKeyDelete))     // POST
	mux.HandleFunc("/api/settings/exchange/toggle", s.AuthMiddleware(s.handleExchangeKeyToggle))     // POST
	mux.HandleFunc("/api/settings/supported_exchanges", s.AuthMiddleware(s.handleSupportedExchanges)) // GET
	mux.HandleFunc("/api/settings/parameters", s.AuthMiddleware(s.handleAlgoParameters))             // POST, GET
	mux.HandleFunc("/api/settings/profile", s.AuthMiddleware(s.handleSettingsProfile))               // POST, GET
	mux.HandleFunc("/api/settings/bot_status", s.AuthMiddleware(s.handleBotStatus))                  // POST, GET
	mux.HandleFunc("/api/settings/sim_offset", s.AuthMiddleware(s.handleSimOffset))                  // POST, GET
	mux.HandleFunc("/api/settings/sim_event", s.AuthMiddleware(s.handleSimEvent))                    // POST, GET
	mux.HandleFunc("/api/settings/paper_reset", s.AuthMiddleware(s.handlePaperReset))                // POST

	// 3. Trading, Positions & Portfolio Routes (Protected)
	mux.HandleFunc("/api/positions/active", s.AuthMiddleware(s.handleActivePositions))        // GET
	mux.HandleFunc("/api/positions/active/", s.AuthMiddleware(s.handleForceClose))            // DELETE REST ID
	mux.HandleFunc("/api/positions/history", s.AuthMiddleware(s.handlePositionHistory))       // GET
	mux.HandleFunc("/api/positions/force_enter", s.AuthMiddleware(s.handleForceEnter))        // POST
	mux.HandleFunc("/api/portfolio/history", s.AuthMiddleware(s.handlePortfolioHistory))      // GET
	mux.HandleFunc("/api/opportunities/chart", s.AuthMiddleware(s.handleOpportunityChart))    // GET

	// 4. WebSocket Steaming Endpoints
	mux.HandleFunc("/api/ws/dashboard", s.WebSocketAuthMiddleware(s.handleDashboardWS))
	mux.HandleFunc("/api/ws/opportunities", s.WebSocketAuthMiddleware(s.handleOpportunitiesWS))

	// 5. Static Frontend Serving (SPA)
	staticDir := os.Getenv("STATIC_DIR")
	if staticDir == "" {
		staticDir = "../frontend/dist" // Default when run from backend/ fallback
	}
	
	// Ensure the static directory exists before trying to serve it
	if _, err := os.Stat(staticDir); err == nil {
		fs := http.FileServer(http.Dir(staticDir))
		
		// Catch-all handler for React/Vite SPA routing
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			// If it's an API route or WS, it shouldn't hit this due to exact matching above,
			// but just in case, verify it doesn't start with /api/
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.NotFound(w, r)
				return
			}
			
			// Check if the requested file exists
			path := filepath.Join(staticDir, r.URL.Path)
			_, err := os.Stat(path)
			if os.IsNotExist(err) || r.URL.Path == "/" {
				// File doesn't exist, serve index.html for client-side routing
				http.ServeFile(w, r, filepath.Join(staticDir, "index.html"))
				return
			}
			
			// File exists, serve it
			fs.ServeHTTP(w, r)
		})
		log.Printf("[API Server] Serving static frontend from %s", staticDir)
	} else {
		log.Printf("[API Server] WARNING: STATIC_DIR (%s) not found. Frontend will not be served.", staticDir)
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("Magnolia API is running. Frontend assets not found."))
		})
	}

	// CORS Middleware Blanket
	handler := corsMiddleware(mux)

	log.Printf("[API Server] Starting REST/WS Server on port %s", port)
	return http.ListenAndServe(":"+port, handler)
}

// corsMiddleware simple wrapper allowing frontend domains to communicate with backend
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*") // Specify frontend URL in production
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		// Handle preflight requests
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}
