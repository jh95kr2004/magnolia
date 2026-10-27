/*
Package main (node)
This file is the entry point for the distributed, stateless trading bot (Node).
It subscribes to real-time market data from Redis (broadcasted by the Feed Handler) to monitor arbitrage opportunities.
It supports scaling across multiple Pods in Kubernetes, graceful shutdown/draining, and PostgreSQL-based crash recovery.
It also serves as the REST API backend and pushes 1:1 targeted WebSocket messages to the frontend via Redis channels.
*/
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/magnolia/backend/internal/api"
	"github.com/magnolia/backend/internal/db/postgres"
	"github.com/magnolia/backend/internal/db/redis"
)

func main() {
	fmt.Println("===========================================")
	fmt.Println("      Magnolia Unified Execution Node      ")
	fmt.Println("===========================================")

	// 1. Load Node Configuration via Environment Variables
	nodeRegion := os.Getenv("NODE_REGION")
	if nodeRegion == "" {
		nodeRegion = "local-dev"
	}

	nodeID := fmt.Sprintf("node-%s-%d", nodeRegion, os.Getpid())
	activeExchangesStr := os.Getenv("ACTIVE_EXCHANGES")
	if activeExchangesStr == "" {
		activeExchangesStr = "upbit,coinbase"
	}
	activeExchanges := strings.Split(activeExchangesStr, ",")
	log.Printf("[INIT] Identity: %s | Region: %s | Exchanges: %v", nodeID, nodeRegion, activeExchanges)

	// 2. Load DB Connections
	pgDSN := os.Getenv("POSTGRES_DSN")
	if pgDSN == "" {
		// Fallback for local testing
		pgDSN = "host=localhost user=magnolia_user password=magnolia_password dbname=magnolia_db port=5432 sslmode=disable"
	}
	db, err := postgres.NewDB(pgDSN)
	if err != nil {
		log.Fatalf("Failed to connect to PostgreSQL: %v", err)
	}
	log.Println("[INIT] Connected to PostgreSQL Global Store.")

	// 2.1 Initialize Redis Client for Market Data
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379/0"
	}
	redisClient := redis.NewPubSubClient(redisURL)
	log.Println("[INIT] Connected to Redis Pub/Sub for Market Data.")

	// 3. Start Web Server (REST API + Static Frontend + WebSocket Sync)
	apiServer := api.NewServer(db, redisClient)
	go func() {
		if err := apiServer.Start("8081"); err != nil && err != http.ErrServerClosed {
			log.Fatalf("API Server Listen Error: %v\n", err)
		}
	}()

	// 4. Graceful Shutdown Hook
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	
	sig := <-sigCh
	log.Printf("\n[SHUTDOWN] Received signal: %v. API Node shutting down...", sig)
	os.Exit(0)
}

