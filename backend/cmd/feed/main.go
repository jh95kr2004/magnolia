/*
Package main (feed)
This file is the entrance point for the dedicated Feed Handler.
It connects to public exchange WebSockets and REST Pollers, ingesting real-time data into InfluxDB,
keeping a concurrent memory PriceCache, and periodically broadcasting globally optimal arbitrage ranks.
*/
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/magnolia/backend/internal/db/influx"
	"github.com/magnolia/backend/internal/db/redis"
	"github.com/magnolia/backend/internal/exchange"
	"github.com/magnolia/backend/internal/exchange/fx"
	"github.com/magnolia/backend/internal/strategy"
)

func main() {
	fmt.Println("===========================================")
	fmt.Println("   Magnolia Market Data Feed Handler       ")
	fmt.Println("===========================================")

	// 1. Load Configurations
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379/0"
	}
	redisClient := redis.NewPubSubClient(redisURL)

	influxURL := os.Getenv("INFLUX_URL")
	if influxURL == "" {
		influxURL = "http://localhost:8086"
	}
	influxToken := os.Getenv("INFLUX_TOKEN")
	influxOrg := os.Getenv("INFLUX_ORG")
	if influxOrg == "" {
		influxOrg = "magnolia"
	}
	influxBucket := os.Getenv("INFLUX_BUCKET")
	if influxBucket == "" {
		influxBucket = "magnolia"
	}
	influxClient := influx.NewClient(influxURL, influxToken, influxOrg, influxBucket)

	log.Println("[INIT] Connected to Redis & InfluxDB.")

	// 2. Initialize Exchange Clients Dynamically
	activeExchangesStr := os.Getenv("ACTIVE_EXCHANGES")
	if activeExchangesStr == "" {
		activeExchangesStr = "upbit,coinbase"
	}
	activeExchanges := strings.Split(activeExchangesStr, ",")

	var clients []exchange.Client
	for _, ex := range activeExchanges {
		ex = strings.TrimSpace(strings.ToLower(ex))
		if ex == "upbit" {
			upbitAccessKey := os.Getenv("UPBIT_ACCESS_KEY")
			upbitSecretKey := os.Getenv("UPBIT_SECRET_KEY")
			if upbitAccessKey == "" {
				upbitAccessKey = "public"
				upbitSecretKey = "public"
			}
			realClient := exchange.NewUpbitClient(upbitAccessKey, upbitSecretKey)
			clients = append(clients, realClient)
		} else if ex == "coinbase" {
			cbKeyName := os.Getenv("COINBASE_API_KEY_NAME")
			cbPrivKey := os.Getenv("COINBASE_PRIVATE_KEY")
			if cbKeyName == "" {
				cbKeyName = "public"
				cbPrivKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
			}
			realClient, err := exchange.NewCoinbaseClient(cbKeyName, cbPrivKey, 0)
			if err != nil {
				log.Fatalf("FAILED to create Coinbase client in feed handler: %v", err)
			}
			clients = append(clients, realClient)
		}
	}

	// 3. Setup Price Cache and Global Ranker Pairs
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	priceCh := make(chan exchange.PriceUpdate, 1000)
	priceCache := &sync.Map{}
	lastUpdateCache := &sync.Map{}

	var fxMu sync.RWMutex
	liveKRWRate := 1385.0
	getKRWRate := func() float64 {
		fxMu.RLock()
		defer fxMu.RUnlock()
		return liveKRWRate
	}
	setKRWRate := func(r float64) {
		if r > 500 && r < 3000 {
			fxMu.Lock()
			liveKRWRate = r
			fxMu.Unlock()
		}
	}

	ranker := strategy.NewGlobalRawRanker(redisClient)

	// Build all combination pairs dynamically: N * (N-1)
	var rankerPairs []strategy.CrossMarketPair
	for i, base := range clients {
		for j, forign := range clients {
			if i != j {
				// Initialize pairs utilizing global ranker instance
				pairs, err := ranker.FindIntersections(ctx, base, forign)
				if err == nil && len(pairs) > 0 {
					rankerPairs = append(rankerPairs, pairs...)
				}
			}
		}
	}

	// Filter rankerPairs if environment filters are defined
	targetCoinsEnv := os.Getenv("TARGET_COINS")
	upbitQuoteEnv := os.Getenv("UPBIT_QUOTE")
	coinbaseQuoteEnv := os.Getenv("COINBASE_QUOTE")

	if targetCoinsEnv != "" || upbitQuoteEnv != "" || coinbaseQuoteEnv != "" {
		var filteredPairs []strategy.CrossMarketPair
		var targetCoins []string
		if targetCoinsEnv != "" {
			for _, tc := range strings.Split(targetCoinsEnv, ",") {
				targetCoins = append(targetCoins, strings.TrimSpace(strings.ToUpper(tc)))
			}
		}

		for _, pair := range rankerPairs {
			coinMatch := true
			if len(targetCoins) > 0 {
				coinMatch = false
				for _, tc := range targetCoins {
					if string(pair.BaseCurrency) == tc {
						coinMatch = true
						break
					}
				}
			}

			quoteMatch := true
			if pair.BaseClient.GetName() == "UPBIT" && upbitQuoteEnv != "" {
				if string(pair.BaseMarketInfo.QuoteCurrency) != strings.ToUpper(upbitQuoteEnv) {
					quoteMatch = false
				}
			}
			if pair.ForeignClient.GetName() == "UPBIT" && upbitQuoteEnv != "" {
				if string(pair.ForeignMarketInfo.QuoteCurrency) != strings.ToUpper(upbitQuoteEnv) {
					quoteMatch = false
				}
			}
			if pair.BaseClient.GetName() == "COINBASE" && coinbaseQuoteEnv != "" {
				if string(pair.BaseMarketInfo.QuoteCurrency) != strings.ToUpper(coinbaseQuoteEnv) {
					quoteMatch = false
				}
			}
			if pair.ForeignClient.GetName() == "COINBASE" && coinbaseQuoteEnv != "" {
				if string(pair.ForeignMarketInfo.QuoteCurrency) != strings.ToUpper(coinbaseQuoteEnv) {
					quoteMatch = false
				}
			}

			if coinMatch && quoteMatch {
				filteredPairs = append(filteredPairs, pair)
			}
		}
		rankerPairs = filteredPairs
	}

	log.Printf("[INIT] Pre-calculated %d overlapping market pairs for global scanning.", len(rankerPairs))

	// Determine unique symbols to monitor per exchange dynamically from ranker intersections
	for _, client := range clients {
		symbolMap := make(map[exchange.Symbol]bool)
		for _, pair := range rankerPairs {
			if pair.BaseClient.GetName() == client.GetName() {
				symbolMap[pair.BaseMarketInfo.Symbol] = true
			}
			if pair.ForeignClient.GetName() == client.GetName() {
				symbolMap[pair.ForeignMarketInfo.Symbol] = true
			}
		}

		// Always monitor USDT-KRW on Korean domestic exchanges to track per-exchange Tether Kimp
		if client.GetName() == "UPBIT" || client.GetName() == "BITHUMB" {
			symbolMap[exchange.Symbol("USDT-KRW")] = true
		}

		var symbolsToMonitor []exchange.Symbol
		for sym := range symbolMap {
			symbolsToMonitor = append(symbolsToMonitor, sym)
		}

		if len(symbolsToMonitor) == 0 {
			log.Printf("[WARNING] No valid intersection symbols found for %s. Skipping WSS.", client.GetName())
			continue
		}

		// 4. Connect to WebSockets or initiate pseudo-WSS REST Polling + Stale-Tick Watchdog
		go func(c exchange.Client, syms []exchange.Symbol) {
			log.Printf("[INIT] Staring WSS Connection for %s with %d symbols...", c.GetName(), len(syms))
			err := c.ConnectWS(ctx, syms, priceCh)
			if err != nil {
				log.Printf("[WARNING] %s WS Failed or Not Supported: %v. Falling back to REST polling logic.", c.GetName(), err)
				startRESTPseudoWebsocketFallback(ctx, c, syms, priceCh)
				return
			}

			// Watchdog: if any symbol has not received a WS tick in > 12s, poll REST so priceCache & InfluxDB never freeze
			watchdogTicker := time.NewTicker(10 * time.Second)
			defer watchdogTicker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-watchdogTicker.C:
					now := time.Now()
					for _, sym := range syms {
						key := c.GetName() + "|" + string(sym)
						stale := true
						if tsVal, ok := lastUpdateCache.Load(key); ok {
							if now.Sub(tsVal.(time.Time)) < 12*time.Second {
								stale = false
							}
						}
						if stale {
							spotPrice, err := c.GetTicker(ctx, sym)
							if err == nil && spotPrice > 0 {
								_, quoteCur := exchange.ParseSymbol(sym)
								krwRate := getKRWRate()
								bidUSD, askUSD := spotPrice, spotPrice
								fxRate := 1.0
								fxPair := "USD/USD"
								if quoteCur == "KRW" && krwRate > 0 {
									bidUSD = spotPrice / krwRate
									askUSD = spotPrice / krwRate
									fxRate = krwRate
									fxPair = "USD/KRW"
								}
								priceCh <- exchange.PriceUpdate{
									Exchange:       c.GetName(),
									Symbol:         sym,
									QuoteCurrency:  quoteCur,
									RawBid:         spotPrice,
									RawAsk:         spotPrice,
									BidUSD:         bidUSD,
									AskUSD:         askUSD,
									FXRate:         fxRate,
									FXCurrencyPair: fxPair,
								}
							}
						}
					}
				}
			}
		}(client, symbolsToMonitor)
	}

	// 5. Ingestion Loop (Receive from WSS/REST -> Merge Cache -> Save to Influx)
	go func() {
		for update := range priceCh {
			cacheKey := update.Exchange + "|" + string(update.Symbol)
			
			var merged exchange.PriceUpdate

			// A. Merge sparse WebSocket updates with existing cached values
			if existingVal, ok := priceCache.Load(cacheKey); ok {
				existing := existingVal.(*exchange.PriceUpdate)
				merged = *existing // Copy baseline

				// Overwrite non-zero/updated fields
				if update.RawBid > 0 { merged.RawBid = update.RawBid }
				if update.RawAsk > 0 { merged.RawAsk = update.RawAsk }
				if update.RawFutureBid > 0 { merged.RawFutureBid = update.RawFutureBid }
				if update.RawFutureAsk > 0 { merged.RawFutureAsk = update.RawFutureAsk }
				
				if update.BidUSD > 0 { merged.BidUSD = update.BidUSD }
				if update.AskUSD > 0 { merged.AskUSD = update.AskUSD }
				if update.BidFutureUSD > 0 { merged.BidFutureUSD = update.BidFutureUSD }
				if update.AskFutureUSD > 0 { merged.AskFutureUSD = update.AskFutureUSD }

				if update.FXRate > 0 { merged.FXRate = update.FXRate }
				if update.FundingRate != 0 { merged.FundingRate = update.FundingRate }
				if update.BorrowRatePerHour != 0 { merged.BorrowRatePerHour = update.BorrowRatePerHour }
				if update.MarkPrice > 0 { merged.MarkPrice = update.MarkPrice }
			} else {
				merged = update
			}

			// Apply latest live USD/KRW FX rate when QuoteCurrency is KRW
			if merged.QuoteCurrency == "KRW" || strings.HasSuffix(string(merged.Symbol), "-KRW") {
				krwRate := getKRWRate()
				if krwRate > 0 {
					merged.FXRate = krwRate
					merged.FXCurrencyPair = "USD/KRW"
					if merged.RawBid > 0 {
						merged.BidUSD = merged.RawBid / krwRate
					}
					if merged.RawAsk > 0 {
						merged.AskUSD = merged.RawAsk / krwRate
					}
					if merged.RawFutureBid > 0 {
						merged.BidFutureUSD = merged.RawFutureBid / krwRate
					}
					if merged.RawFutureAsk > 0 {
						merged.AskFutureUSD = merged.RawFutureAsk / krwRate
					}
				}

				// Broadcast per-exchange Tether (USDT-KRW) rate whenever updated
				if string(merged.Symbol) == "USDT-KRW" && merged.RawBid > 500 {
					strategy.SetExchangeTetherRate(merged.Exchange, "KRW", merged.RawBid)
					_ = redisClient.PublishExchangeTetherRate(ctx, merged.Exchange, merged.RawBid)
				}
			}

			// Save back to sync.Map
			priceCache.Store(cacheKey, &merged)
			lastUpdateCache.Store(cacheKey, time.Now())

			// Publish Ticker update to Redis
			redisClient.PublishTicker(ctx, merged)

			// B. Write the fully MERGED snapshot to InfluxDB for AI/Historical Analysis
			if err := influxClient.WritePriceUpdate(ctx, merged); err != nil {
				log.Printf("[Metrics] Failed to write %s to InfluxDB: %v", merged.Symbol, err)
			}
		}
	}()

	// 6. FX Rate Monitor Daemon (Supports Fixer API or unauthenticated public FX fallback)
	fxAPIKey := os.Getenv("FIXER_API_KEY")
	fxPollStr := os.Getenv("FX_POLL_INTERVAL")
	fxPollInterval, err := time.ParseDuration(fxPollStr)
	if err != nil || fxPollInterval == 0 {
		fxPollInterval = 30 * time.Minute
	}
	isFreeTier := true
	if os.Getenv("FIXER_FREE_TIER") == "false" || os.Getenv("FIXER_FREE_TIER") == "0" {
		isFreeTier = false
	}

	go func() {
		ticker := time.NewTicker(fxPollInterval)
		defer ticker.Stop()

		syncFX := func() {
			if fxAPIKey != "" {
				fxClient := fx.NewFixerClient(fxAPIKey, isFreeTier)
				if rate := fetchFX(ctx, fxClient, redisClient, influxClient); rate > 0 {
					setKRWRate(rate)
					strategy.SetFXRate("KRW", rate)
					return
				}
			}
			if rate := fetchPublicUSDKRW(ctx, redisClient, influxClient); rate > 0 {
				setKRWRate(rate)
				strategy.SetFXRate("KRW", rate)
			}
		}

		syncFX()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				syncFX()
			}
		}
	}()

	// 6b. Upbit Public Announcement Poller + AI LLM Catalyst Classifier Daemon
	go startUpbitAnnouncementMonitor(ctx, redisClient)

	// 7. High-Speed Background Ranker Daemon
	go func() {
		// Wait shortly for initial WebSocket cache populating
		time.Sleep(3 * time.Second)

		rankerPollStr := os.Getenv("RANKER_POLL_INTERVAL")
		rankerPollInterval, err := time.ParseDuration(rankerPollStr)
		if err != nil || rankerPollInterval == 0 {
			rankerPollInterval = 1 * time.Second // 1 second fast scanning
		}

		log.Printf("[GlobalRanker] Daemon starting with polling interval: %s", rankerPollInterval)

		ticker := time.NewTicker(rankerPollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Execute EvaluateAllCache which uses O(1) Memory Reads and naturally outputs to Redis
				_, err := ranker.EvaluateAllCache(ctx, rankerPairs, priceCache)
				if err != nil {
					log.Printf("[GlobalRanker] Fetch/Ranking error: %v", err)
				}
			}
		}
	}()

	log.Println("[RUNNING] Feed Handler is actively streaming and broadcasting market data.")

	// 8. Shutdown Handler
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("\n[SHUTDOWN] Shutting down Feed Handler...")
}

func fetchFX(ctx context.Context, fxClient fx.FXFetcher, rClient *redis.PubSubClient, iClient *influx.Client) float64 {
	rates, err := fxClient.FetchRates(ctx, []exchange.Currency{"KRW"})
	if err != nil {
		log.Printf("[FXFetcher] Error fetching FX rates: %v", err)
		return 0
	}

	krwRate, ok := rates["KRW"]
	if ok && krwRate > 0 {
		log.Printf("[FXFetcher] Synced Exchange Rate: 1 USD = %.4f KRW", krwRate)

		update := exchange.PriceUpdate{
			Exchange:       fxClient.GetName(),
			Symbol:         "USD-KRW",
			QuoteCurrency:  "KRW",
			FXRate:         krwRate,
			FXCurrencyPair: "USD/KRW",
		}

		iClient.WritePriceUpdate(ctx, update)
		rClient.PublishFXRate(ctx, "USD", "KRW", krwRate)
		return krwRate
	}
	return 0
}

func fetchPublicUSDKRW(ctx context.Context, rClient *redis.PubSubClient, iClient *influx.Client) float64 {
	client := &http.Client{Timeout: 8 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://open.er-api.com/v6/latest/USD", nil)
	if err != nil {
		return 0
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0
	}
	var parsed struct {
		Rates map[string]float64 `json:"rates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return 0
	}
	krwRate := parsed.Rates["KRW"]
	if krwRate > 500 {
		log.Printf("[FXFetcher] Synced Public USD/KRW Exchange Rate: 1 USD = %.2f KRW", krwRate)
		update := exchange.PriceUpdate{
			Exchange:       "PUBLIC_FX",
			Symbol:         "USD-KRW",
			QuoteCurrency:  "KRW",
			FXRate:         krwRate,
			FXCurrencyPair: "USD/KRW",
		}
		iClient.WritePriceUpdate(ctx, update)
		rClient.PublishFXRate(ctx, "USD", "KRW", krwRate)
		return krwRate
	}
	return 0
}

// startRESTPseudoWebsocketFallback acts as a shim pushing REST responses continuously as if they were WS streams
func startRESTPseudoWebsocketFallback(ctx context.Context, c exchange.Client, syms []exchange.Symbol, priceCh chan<- exchange.PriceUpdate) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	log.Printf("[PseudoWS] %s Started native REST polling for %d symbols", c.GetName(), len(syms))

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, sym := range syms {
				spotUsd, _ := c.GetTicker(ctx, sym)
				futureUsd, _ := c.GetFutureTicker(ctx, sym)

				// Push to central channel unified
				update := exchange.PriceUpdate{
					Exchange:      c.GetName(),
					Symbol:        sym,
					QuoteCurrency: "UNKNOWN",
					RawBid:        spotUsd,
					RawAsk:        spotUsd,
					RawFutureBid:  futureUsd,
					RawFutureAsk:  futureUsd,
					FundingRate:   0, // Assuming fetched if possible
				}
				
				priceCh <- update
			}
		}
	}
}

// startUpbitAnnouncementMonitor polls Upbit's public announcement API every 15s,
// passes only newly detected notices (id > lastSeenID) to the AI LLM classifier (/analyze_announcement),
// and broadcasts catalyst events (NEW_KRW_LISTING, INVESTMENT_WARNING, DELISTING, WALLET_SUSPENSION) to Redis.
func startUpbitAnnouncementMonitor(ctx context.Context, rClient *redis.PubSubClient) {
	if rClient == nil {
		return
	}
	aiBaseURL := os.Getenv("AI_SERVICE_URL")
	if aiBaseURL == "" {
		aiBaseURL = "http://ai_service:8000"
	}
	aiBaseURL = strings.TrimRight(aiBaseURL, "/")

	httpClient := &http.Client{Timeout: 8 * time.Second}
	var lastSeenID int64 = 0

	pollOnce := func(isInit bool) {
		req, err := http.NewRequestWithContext(ctx, "GET", "https://api-manager.upbit.com/api/v1/announcements?os=web&page=1&per_page=10&category=all", nil)
		if err != nil {
			return
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; MagnoliaFeed/1.0)")
		resp, err := httpClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return
		}

		var payload struct {
			Success bool `json:"success"`
			Data    struct {
				Notices []struct {
					ID        int64  `json:"id"`
					Title     string `json:"title"`
					Category  string `json:"category"`
					ListedAt  string `json:"listed_at"`
					CreatedAt string `json:"created_at"`
				} `json:"notices"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			return
		}

		notices := payload.Data.Notices
		if len(notices) == 0 {
			return
		}

		if isInit {
			for _, n := range notices {
				if n.ID > lastSeenID {
					lastSeenID = n.ID
				}
			}
			return
		}

		for i := len(notices) - 1; i >= 0; i-- {
			n := notices[i]
			if n.ID <= lastSeenID {
				continue
			}
			lastSeenID = n.ID

			reqBody, _ := json.Marshal(map[string]interface{}{
				"notice_id":    fmt.Sprintf("upbit-%d", n.ID),
				"exchange":     "UPBIT",
				"title":        n.Title,
				"category":     n.Category,
				"published_at": n.ListedAt,
			})
			aiReq, err := http.NewRequestWithContext(ctx, "POST", aiBaseURL+"/analyze_announcement", strings.NewReader(string(reqBody)))
			if err != nil {
				continue
			}
			aiReq.Header.Set("Content-Type", "application/json")
			aiResp, err := httpClient.Do(aiReq)
			if err != nil {
				continue
			}
			var evt exchange.AnnouncementEvent
			decodeErr := json.NewDecoder(aiResp.Body).Decode(&evt)
			aiResp.Body.Close()
			if decodeErr == nil && evt.EventType != "" && evt.EventType != "NONE" && len(evt.Symbols) > 0 {
				if evt.AnnouncedAt.IsZero() {
					evt.AnnouncedAt = time.Now().UTC()
				}
				log.Printf("[LLM-Announcement] Detected %s on %v: %s", evt.EventType, evt.Symbols, evt.Title)
				_ = rClient.PublishAnnouncementEvent(ctx, evt)
			}
		}
	}

	pollOnce(true)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pollOnce(false)
		}
	}
}
