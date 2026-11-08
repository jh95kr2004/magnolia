package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/magnolia/backend/internal/db/postgres"
	"github.com/magnolia/backend/internal/db/redis"
	"github.com/magnolia/backend/internal/exchange"
	"github.com/magnolia/backend/internal/strategy"
)

func main() {
	resetDB := flag.Bool("reset-db", false, "Reset active positions in the database to CLOSED on start")
	flag.Parse()

	fmt.Println("===========================================")
	fmt.Println("    Magnolia Live Feed Paper Engine        ")
	fmt.Println("===========================================")

	// 1. Setup Context and Graceful Shutdown Signal Handler
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// 2. Load Configurations
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "host=localhost user=magnolia_user password=magnolia_password dbname=magnolia_db port=5432 sslmode=disable"
	}
	db, err := postgres.NewDB(dsn)
	if err != nil {
		log.Fatalf("FAILED to connect to Postgres. Ensure docker-compose is running: %v", err)
	}
	log.Println("[PaperEngine] Postgres Database Connected.")

	if *resetDB {
		log.Println("[PaperEngine] Resetting active positions in database...")
		if err := db.ResetAllActivePositions(); err != nil {
			log.Printf("[PaperEngine] Warning: failed to reset database: %v", err)
		} else {
			log.Println("[PaperEngine] Successfully reset all active positions to CLOSED.")
		}
	}

	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379/0"
	}
	pubsubClient := redis.NewPubSubClient(redisURL)
	log.Println("[PaperEngine] Redis Client Connected.")

	// 3. Load Exchange Credentials (Fallback to dummy keys for public-only WebSocket operations)
	upbitAccessKey := os.Getenv("UPBIT_ACCESS_KEY")
	upbitSecretKey := os.Getenv("UPBIT_SECRET_KEY")
	if upbitAccessKey == "" {
		log.Println("[PaperEngine] UPBIT API keys missing in environment. Using public-feed fallback.")
		upbitAccessKey = "public"
		upbitSecretKey = "public"
	}

	coinbaseKeyName := os.Getenv("COINBASE_API_KEY_NAME")
	coinbasePrivateKey := os.Getenv("COINBASE_PRIVATE_KEY")
	if coinbaseKeyName == "" {
		log.Println("[PaperEngine] COINBASE API keys missing in environment. Generating dummy Ed25519 key for websocket connection.")
		coinbaseKeyName = "public"
		// Base64 Ed25519 seed containing 32 zero bytes
		coinbasePrivateKey = base64.StdEncoding.EncodeToString(make([]byte, 32))
	}

	// 4. Initialize Live Exchange Clients
	log.Println("[PaperEngine] Initializing Coinbase Live Client...")
	realCoinbase, err := exchange.NewCoinbaseClient(coinbaseKeyName, coinbasePrivateKey, 0)
	if err != nil {
		log.Fatalf("[PaperEngine] FAILED to construct Coinbase client: %v", err)
	}

	log.Println("[PaperEngine] Initializing Upbit Live Client...")
	realUpbit := exchange.NewUpbitClient(upbitAccessKey, upbitSecretKey)

	userID := uint(1) // Admin user ID

	// 5. Wrap Clients inside Paper wrappers for risk-free simulation using live feeds
	paperUpbit := exchange.NewPaperClientForUser(userID, realUpbit, map[string]float64{
		"KRW": 50000000.0, // 50,000,000 KRW virtual balance
		"BTC": 0.0,
	})

	paperCoinbase := exchange.NewPaperClientForUser(userID, realCoinbase, map[string]float64{
		"USD": 35000.0, // 35,000 USD virtual balance
		"BTC": 0.0,
	})

	var clients []exchange.Client
	clients = append(clients, paperUpbit)
	clients = append(clients, paperCoinbase)

	// 6. Instantiate the Strategy Engine
	nodeID := "live-paper-node-1"
	engine := strategy.NewEngine(nodeID, userID, db, pubsubClient, "PremiumStrategy", clients, nil)
	log.Println("[PaperEngine] Core Arbitrage Strategy Engine initialized.")

	// 7. Start AI Opportunities Subscription Daemon
	go engine.ListenOpportunities(ctx)
	log.Println("[PaperEngine] AI Opportunity execution listener daemon started.")

	// 8. Start Position Monitoring Daemon
	targetCoinsEnv := os.Getenv("TARGET_COINS")
	upbitQuoteEnv := os.Getenv("UPBIT_QUOTE")
	if upbitQuoteEnv == "" {
		upbitQuoteEnv = "KRW"
	}
	coinbaseQuoteEnv := os.Getenv("COINBASE_QUOTE")
	if coinbaseQuoteEnv == "" {
		coinbaseQuoteEnv = "USD"
	}

	var symbolsToMonitor []exchange.Symbol
	if strategy.IsAllCoinsFilter(targetCoinsEnv) {
		symbolsToMonitor = []exchange.Symbol{"*"}
	} else {
		for _, tc := range strings.Split(targetCoinsEnv, ",") {
			tc = strings.TrimSpace(strings.ToUpper(tc))
			if tc != "" {
				symbolsToMonitor = append(symbolsToMonitor, exchange.Symbol(tc+"-"+upbitQuoteEnv))
				symbolsToMonitor = append(symbolsToMonitor, exchange.Symbol(tc+"-"+coinbaseQuoteEnv))
			}
		}
	}

	priceCh := make(chan exchange.PriceUpdate, 1000)
	go pubsubClient.SubscribeTickers(ctx, symbolsToMonitor, priceCh)
	log.Printf("[PaperEngine] Position Monitor subscribed to %v tickers.\n", symbolsToMonitor)

	go engine.RunPositionMonitor(ctx, priceCh)
	log.Println("[PaperEngine] Position Monitor daemon started.")

	log.Println("[PaperEngine] Live Feed Paper Engine is RUNNING. Press Ctrl+C to terminate.")

	// 9. Keep Running Awaiting Signal Interrupt
	<-sigCh
	log.Println("[PaperEngine] Shutting down paper engine gracefully...")
}
