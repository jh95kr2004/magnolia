package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/magnolia/backend/internal/db/postgres"
	"github.com/magnolia/backend/internal/db/redis"
	"github.com/magnolia/backend/internal/exchange"
	"github.com/magnolia/backend/internal/strategy"
)

func main() {
	log.Println("[Engine] Starting Dedicated Trading Engine Microservice...")

	if err := godotenv.Load(); err != nil {
		log.Println("[Engine] Warning: No .env file found. Using system environment variables.")
	}

	nodeID := os.Getenv("NODE_ID")
	if nodeID == "" {
		nodeID = "engine-kr-1"
	}

	engineRegion := os.Getenv("ENGINE_REGION")
	if engineRegion == "" {
		engineRegion = "kr"
	}

	log.Printf("[Engine] Node ID: %s | Region: %s", nodeID, engineRegion)

	psqlDSN := os.Getenv("DATABASE_URL")
	if psqlDSN == "" {
		psqlDSN = os.Getenv("POSTGRES_DSN")
	}
	if psqlDSN == "" {
		psqlDSN = "host=localhost user=magnolia_user password=magnolia_password dbname=magnolia_db port=5432 sslmode=disable"
	}
	db, err := postgres.NewDB(psqlDSN)
	if err != nil {
		log.Fatalf("[Engine] Failed to connect to DB: %v", err)
	}

	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379/0"
	}
	redisClient := redis.NewPubSubClient(redisURL)

	// Immediately acquire node heartbeat lease
	if _, err := db.AcquireLeaderLease("engine_node:"+nodeID, nodeID, 30*time.Second); err != nil {
		log.Printf("[Engine] Warning: Initial heartbeat lease acquisition failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	// Start node heartbeat daemon
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, err := db.AcquireLeaderLease("engine_node:"+nodeID, nodeID, 30*time.Second)
				if err != nil {
					log.Printf("[Engine] Warning: Heartbeat lease renewal failed: %v", err)
				}
			}
		}
	}()

	// Start dead node recovery daemon
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				isLeader, err := db.AcquireLeaderLease("engine_cleanup_leader", nodeID, 30*time.Second)
				if err != nil {
					continue
				}
				if !isLeader {
					continue
				}

				orphanedUsers, err := db.CleanOrphanedUsers()
				if err != nil {
					log.Printf("[Engine] CleanOrphanedUsers failed: %v", err)
					continue
				}
				if len(orphanedUsers) > 0 {
					log.Printf("[Engine] Found %d orphaned users from dead engine nodes. Re-queuing...", len(orphanedUsers))
					for _, u := range orphanedUsers {
						pushErr := redisClient.PushDanglingClient(ctx, u.Region, fmt.Sprint(u.ID))
						if pushErr != nil {
							log.Printf("[Engine] Failed to re-queue orphaned user %d: %v", u.ID, pushErr)
						} else {
							log.Printf("[Engine] Orphaned user %d successfully returned to regional %s queue.", u.ID, u.Region)
						}
					}
				}
			}
		}
	}()

	// 1단계: 부팅 시 Crash Recovery.
	// DB를 읽어서 이전에 이 NodeID가 담당하고 있던 User들을 불러와 복구합니다.
	recoveredUsers, err := db.GetUsersByEngineNode(nodeID)
	if err != nil {
		log.Fatalf("[Engine] Failed to scan DB for crash recovery: %v", err)
	}

	log.Printf("[Engine] Recovered %d users from DB.", len(recoveredUsers))

	// 복구된 유저의 독립된 Engine 고루틴 실행
	for _, u := range recoveredUsers {
		wg.Add(1)
		go runUserEngine(ctx, &wg, db, redisClient, nodeID, engineRegion, u.ID)
	}

	// 미할당(Unassigned) 유저 자동 인수 (부팅 시 초기 시드 유저 포함)
	if unassignedUsers, err := db.GetUnassignedUsers(engineRegion); err == nil && len(unassignedUsers) > 0 {
		for _, u := range unassignedUsers {
			if assignErr := db.AssignEngineNode(u.ID, nodeID); assignErr == nil {
				log.Printf("[Engine] Claimed unassigned user %d (%s) on startup.", u.ID, u.Email)
				wg.Add(1)
				go runUserEngine(ctx, &wg, db, redisClient, nodeID, engineRegion, u.ID)
			}
		}
	}

	// 2단계: 무한 루프 진입하여 Dangling 큐에서 떨어져 나온 사용자 Pop
	go func() {
		for {
			// Check context cancellation first before blocking
			if ctx.Err() != nil {
				return
			}
			// 블로킹 팝
			userIDStr, err := redisClient.PopDanglingClient(ctx, engineRegion)
			if err != nil {
				// If context is canceled, BRPop will return context.Canceled
				if ctx.Err() != nil {
					return
				}
				continue
			}
			if userIDStr != "" {
				log.Printf("[Engine] Picked up dangling user %s from queue.", userIDStr)
				parsedID, convErr := strconv.ParseUint(userIDStr, 10, 32)
				if convErr == nil {
					// 락 점유 및 할당 확인
					err := db.AssignEngineNode(uint(parsedID), nodeID)
					if err != nil {
						log.Printf("[Engine] ERROR: Failed to assign engine node for user %s: %v", userIDStr, err)
						// Push back to dangling client queue so another node or we can retry later
						redisClient.PushDanglingClient(context.Background(), engineRegion, userIDStr)
						continue
					}
					wg.Add(1)
					go runUserEngine(ctx, &wg, db, redisClient, nodeID, engineRegion, uint(parsedID))
				}
			}
		}
	}()

	// 3단계: SIGTERM / SIGINT 시큐어 종료
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	<-sigCh
	log.Println("[Engine] Received shutdown signal. Initiating Graceful Shutdown...")

	// 취소 시그널 브로드캐스트. 모든 `runUserEngine` 루프가 이 시그널을 받으면
	// 현재 하던 로직만 마무리하고 PushDanglingClient를 호출한 후 안전히 반환(return)함.
	cancel()

	// 모든 유저 엔진 고루틴이 큐에 반환하고 종료될 때까지 대기
	wg.Wait()
	log.Println("[Engine] Graceful Shutdown Complete. Goodbye.")
}

func runUserEngine(
	ctx context.Context,
	wg *sync.WaitGroup,
	db *postgres.DB,
	rClient *redis.PubSubClient,
	nodeID, region string,
	userID uint,
) {
	defer wg.Done()

	log.Printf("[Engine] Starting user engine goroutine for User %d...", userID)

	// Fetch API keys from DB and decrypt them
	var upbitAccess, upbitSecret string
	upbitKey, err := db.GetAPIKey(userID, "UPBIT")
	if err != nil {
		log.Printf("[Engine] Warning: Could not load UPBIT API key for User %d: %v. Using public-feed fallback.", userID, err)
		upbitAccess = "public"
		upbitSecret = "public"
	} else {
		decrypted, err := postgres.Decrypt([]byte(upbitKey.SecretKey))
		if err != nil {
			log.Printf("[Engine] ERROR: Failed to decrypt UPBIT secret key for User %d: %v. Using public-feed fallback.", userID, err)
			upbitAccess = "public"
			upbitSecret = "public"
		} else {
			upbitAccess = upbitKey.AccessKey
			upbitSecret = string(decrypted)
		}
	}

	var coinbaseAccess, coinbaseSecret string
	var coinbaseLimit float64 = 250000.0
	coinbaseKey, err := db.GetAPIKey(userID, "COINBASE")
	if err != nil {
		log.Printf("[Engine] Warning: Could not load COINBASE API key for User %d: %v. Using public-feed fallback.", userID, err)
		coinbaseAccess = "public"
		coinbaseSecret = base64.StdEncoding.EncodeToString(make([]byte, 32))
	} else {
		decrypted, err := postgres.Decrypt([]byte(coinbaseKey.SecretKey))
		if err != nil {
			log.Printf("[Engine] ERROR: Failed to decrypt COINBASE secret key for User %d: %v. Using public-feed fallback.", userID, err)
			coinbaseAccess = "public"
			coinbaseSecret = base64.StdEncoding.EncodeToString(make([]byte, 32))
		} else {
			coinbaseAccess = coinbaseKey.AccessKey
			coinbaseSecret = string(decrypted)
			if coinbaseKey.DefaultWithdrawalLimit > 0 {
				coinbaseLimit = coinbaseKey.DefaultWithdrawalLimit
			}
		}
	}

	liveClients := []exchange.Client{}
	paperClients := []exchange.Client{}

	paperUpbitKRW := 50000000.0
	paperCoinbaseUSD := 35000.0
	if settings, err := db.GetUserSettings(userID); err == nil && settings != nil {
		if settings.PaperUpbitKRW > 0 {
			paperUpbitKRW = settings.PaperUpbitKRW
		}
		if settings.PaperCoinbaseUSD > 0 {
			paperCoinbaseUSD = settings.PaperCoinbaseUSD
		}
	}

	upbitClient := exchange.NewUpbitClient(upbitAccess, upbitSecret)
	liveClients = append(liveClients, upbitClient)
	paperClients = append(paperClients, exchange.NewPaperClientForUser(userID, upbitClient, map[string]float64{
		"KRW": paperUpbitKRW,
		"BTC": 0.0,
	}))

	coinbaseClient, err := exchange.NewCoinbaseClient(coinbaseAccess, coinbaseSecret, coinbaseLimit)
	if err != nil {
		log.Printf("[Engine] Failed to create Coinbase client for User %d: %v", userID, err)
	} else {
		liveClients = append(liveClients, coinbaseClient)
		paperClients = append(paperClients, exchange.NewPaperClientForUser(userID, coinbaseClient, map[string]float64{
			"USD": paperCoinbaseUSD,
			"BTC": 0.0,
		}))
	}

	// Create Dual Independent Engines for this userID:
	// 1. Live Engine (real exchange execution)
	liveEngine := strategy.NewEngine(
		nodeID,
		userID,
		db,
		rClient,
		"PremiumStrategy",
		liveClients,
		nil,
	)

	// 2. Paper Engine (virtual balance & simulator execution on 100% real feeds)
	paperEngine := strategy.NewEngine(
		nodeID,
		userID,
		db,
		rClient,
		"PremiumStrategy",
		paperClients,
		nil,
	)

	// Run parallel listeners for both Live and Paper engines
	go liveEngine.ListenOpportunities(ctx)
	go paperEngine.ListenOpportunities(ctx)

	// Start Position Monitor for the user's active positions and opportunities evaluation
	targetCoinsEnv := os.Getenv("TARGET_COINS")
	upbitQuote := os.Getenv("UPBIT_QUOTE")
	if upbitQuote == "" {
		upbitQuote = "KRW"
	}
	coinbaseQuote := os.Getenv("COINBASE_QUOTE")
	if coinbaseQuote == "" {
		coinbaseQuote = "USD"
	}

	var symbolsToMonitor []exchange.Symbol
	if targetCoinsEnv != "" {
		for _, tc := range strings.Split(targetCoinsEnv, ",") {
			tc = strings.TrimSpace(strings.ToUpper(tc))
			symbolsToMonitor = append(symbolsToMonitor, exchange.Symbol(tc+"-"+upbitQuote))
			symbolsToMonitor = append(symbolsToMonitor, exchange.Symbol(tc+"-"+coinbaseQuote))
		}
	} else {
		// Default fallback
		symbolsToMonitor = []exchange.Symbol{"BTC-KRW", "BTC-USD", "ETH-KRW", "ETH-USD"}
	}

	rawPriceCh := make(chan exchange.PriceUpdate, 1000)
	livePriceCh := make(chan exchange.PriceUpdate, 1000)
	paperPriceCh := make(chan exchange.PriceUpdate, 1000)

	go rClient.SubscribeTickers(ctx, symbolsToMonitor, rawPriceCh)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case tick := <-rawPriceCh:
				select {
				case livePriceCh <- tick:
				default:
				}
				select {
				case paperPriceCh <- tick:
				default:
				}
			}
		}
	}()

	go liveEngine.RunPositionMonitor(ctx, livePriceCh)
	go paperEngine.RunPositionMonitor(ctx, paperPriceCh)

	log.Printf("[Engine] Dual (Live + Paper) Go-routines started for User %d.", userID)

	// Wait until context is done for shutdown
	<-ctx.Done()

	log.Printf("[Engine] Graceful Shutdown triggered for User %d. Draining both Live and Paper engines...", userID)
	liveEngine.StartDraining()
	paperEngine.StartDraining()

	// 1. Release Lock in DB
	if err := db.ReleaseEngineNode(userID); err != nil {
		log.Printf("[Engine] Failed to release DB lock for User %d: %v", userID, err)
	}

	// 2. Push to Dangling Queue for another Node to pick up instantly
	rClient.PushDanglingClient(context.Background(), region, fmt.Sprint(userID))
	log.Printf("[Engine] User %d successfully pushed back to Dangling Queue.", userID)
}
