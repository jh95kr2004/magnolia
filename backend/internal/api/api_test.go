package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/magnolia/backend/internal/db/postgres"
	"github.com/magnolia/backend/internal/db/redis"
)

func setupTestServer(t *testing.T) (*Server, uint, string) {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = "host=127.0.0.1 user=magnolia_test password=magnolia_test dbname=magnolia_test port=15432 sslmode=disable"
	}
	db, err := postgres.NewDB(dsn)
	if err != nil {
		t.Skipf("Skipping API integration test (TEST_POSTGRES_DSN not reachable: %v)", err)
	}

	redisURL := os.Getenv("TEST_REDIS_URL")
	var rClient *redis.PubSubClient
	if redisURL != "" {
		rClient = redis.NewPubSubClient(redisURL)
	}

	srv := NewServer(db, rClient)

	// Register a fresh test user via handleRegister
	email := fmt.Sprintf("api_%d@magnolia.test", time.Now().UnixNano())
	body, _ := json.Marshal(map[string]string{
		"email":    email,
		"password": "testpassword123",
		"region":   "kr",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleRegister(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("handleRegister failed with status %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Token string `json:"token"`
		User  struct {
			ID uint `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse register response: %v", err)
	}
	return srv, resp.User.ID, resp.Token
}

func TestHandleAlgoParametersReturnsNonZeroDefaults(t *testing.T) {
	srv, _, token := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/settings/parameters", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	srv.AuthMiddleware(srv.handleAlgoParameters)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var th postgres.UserThreshold
	if err := json.Unmarshal(w.Body.Bytes(), &th); err != nil {
		t.Fatalf("unmarshal threshold failed: %v", err)
	}
	if th.MinTargetProfit <= 0 || th.CapitalLimitUsd <= 0 || th.RebalanceTargetSkew <= 0 {
		t.Fatalf("expected non-zero default thresholds, got: %+v", th)
	}
}

func TestHandleBotStatusAndSimOffsetLifecycle(t *testing.T) {
	srv, _, token := setupTestServer(t)

	// 1. Set gradual price offset (+4.0% over 5 minutes on UPBIT)
	simBody, _ := json.Marshal(map[string]interface{}{
		"exchange":          "UPBIT",
		"target_offset_pct": 4.0,
		"ramp_minutes":      5.0,
	})
	reqSim := httptest.NewRequest(http.MethodPost, "/api/settings/sim_offset", bytes.NewReader(simBody))
	reqSim.Header.Set("Authorization", "Bearer "+token)
	wSim := httptest.NewRecorder()
	srv.AuthMiddleware(srv.handleSimOffset)(wSim, reqSim)
	if wSim.Code != http.StatusOK {
		t.Fatalf("handleSimOffset POST failed: %d %s", wSim.Code, wSim.Body.String())
	}

	// 2. Toggle Paper Bot Status to DRAIN
	botBody, _ := json.Marshal(map[string]string{
		"mode":       "PAPER",
		"sub_action": "DRAIN",
	})
	reqBot := httptest.NewRequest(http.MethodPost, "/api/settings/bot_status", bytes.NewReader(botBody))
	reqBot.Header.Set("Authorization", "Bearer "+token)
	wBot := httptest.NewRecorder()
	srv.AuthMiddleware(srv.handleBotStatus)(wBot, reqBot)
	if wBot.Code != http.StatusOK {
		t.Fatalf("handleBotStatus POST failed: %d %s", wBot.Code, wBot.Body.String())
	}

	var botResp map[string]interface{}
	_ = json.Unmarshal(wBot.Body.Bytes(), &botResp)
	if botResp["paper_bot_status"] != "DRAINING" {
		t.Fatalf("expected paper_bot_status=DRAINING, got %v", botResp["paper_bot_status"])
	}
}

func TestDashboardSSESnapshotAndOpportunitiesSSE(t *testing.T) {
	srv, userID, token := setupTestServer(t)

	// Test pushDashboardSnapshot directly to verify SSE event format ("event: portfolio" & "event: positions")
	w := httptest.NewRecorder()
	srv.pushDashboardSnapshot(context.Background(), w, userID, "PAPER")
	out := w.Body.String()
	if !strings.Contains(out, "event: portfolio") || !strings.Contains(out, "event: positions") {
		t.Fatalf("expected SSE output to contain portfolio and positions events, got:\n%s", out)
	}

	// Test applySimOffsetToRawOpps adjusts premium_pct when sim offset is active
	rawOpps := `[{"base_exchange":"UPBIT","foreign_exchange":"COINBASE","base_symbol":"BTC-KRW","foreign_symbol":"BTC-USD","base_price_usd":100000,"foreign_price_usd":100000,"premium_pct":0.5,"expected_margin":0.5}]`
	simBody, _ := json.Marshal(map[string]interface{}{
		"exchange":          "UPBIT",
		"target_offset_pct": 3.0,
		"ramp_minutes":      0.0, // Immediate 3.0% offset
	})
	reqSim := httptest.NewRequest(http.MethodPost, "/api/settings/sim_offset", bytes.NewReader(simBody))
	reqSim.Header.Set("Authorization", "Bearer "+token)
	srv.AuthMiddleware(srv.handleSimOffset)(httptest.NewRecorder(), reqSim)

	adjusted := srv.applySimOffsetToRawOpps([]byte(rawOpps), userID)
	var parsed []map[string]interface{}
	if err := json.Unmarshal(adjusted, &parsed); err != nil || len(parsed) != 1 {
		t.Fatalf("failed to unmarshal adjusted opps: %v", err)
	}
	if parsed[0]["premium_pct"].(float64) < 3.4 {
		t.Fatalf("expected premium_pct adjusted from 0.5%% to ~3.5%%, got %v", parsed[0]["premium_pct"])
	}

	// Test GET /api/portfolio/history returns time-series points and FX rate
	reqHist := httptest.NewRequest(http.MethodGet, "/api/portfolio/history?mode=PAPER&range=24H", nil)
	reqHist.Header.Set("Authorization", "Bearer "+token)
	wHist := httptest.NewRecorder()
	srv.AuthMiddleware(srv.handlePortfolioHistory)(wHist, reqHist)
	if wHist.Code != http.StatusOK {
		t.Fatalf("handlePortfolioHistory GET failed: %d %s", wHist.Code, wHist.Body.String())
	}
	var histResp map[string]interface{}
	if err := json.Unmarshal(wHist.Body.Bytes(), &histResp); err != nil {
		t.Fatalf("failed to decode portfolio history JSON: %v", err)
	}
	pts, ok := histResp["points"].([]interface{})
	if !ok || len(pts) < 2 {
		t.Fatalf("expected at least 2 baseline points in portfolio history, got: %v", histResp)
	}
}
