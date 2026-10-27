//go:build ignore

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/magnolia/backend/internal/db/postgres"
	"github.com/magnolia/backend/internal/db/redis"
	"github.com/magnolia/backend/internal/exchange"
	"github.com/magnolia/backend/internal/execution"
	"github.com/magnolia/backend/internal/strategy"
	redisclient "github.com/redis/go-redis/v9"
)

func main() {
	fmt.Println("===========================================")
	fmt.Println("    Magnolia Full Architecture Sandbox     ")
	fmt.Println("===========================================")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Setup Redis
	redisURL := "redis://localhost:6379/0"
	opt, _ := redisclient.ParseURL(redisURL)
	rClient := redisclient.NewClient(opt)
	rClient.FlushAll(ctx)
	log.Println("[Sandbox] Redis Cleared & Connected")

	// 2. Setup DB
	dsn := "host=localhost user=magnolia_user password=magnolia_password dbname=magnolia_db port=5432 sslmode=disable"
	db, err := postgres.NewDB(dsn)
	if err != nil {
		log.Fatalf("FAILED to connect to Postgres. Ensure docker-compose is running: %v", err)
	}
	log.Println("[Sandbox] Postgres Database Connected")

	// Launch Feed Mock Concurrently
	go RunMockFeedHandler(ctx, rClient)

	// Give them a moment to start
	time.Sleep(1 * time.Second)

	RunMockNodeEngine(ctx, redisURL, db)
}

func RunMockFeedHandler(ctx context.Context, rClient *redisclient.Client) {
	log.Println("[Feed Handler] Started. Publishing ticks every 2 seconds...")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	prices := []float64{100000.0, 100050.0, 100100.0, 100020.0, 99950.0}
	idx := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Mock FX Rate Sync
			rClient.Publish(ctx, "fx:USD-KRW", "1400.0")

			// Publish Upbit KRW
			base := exchange.PriceUpdate{
				Exchange:          "UPBIT",
				Symbol:            "BTC-KRW",
				RawBid:            140000000,
				RawAsk:            140000000,
				BidUSD:            prices[idx] * 1.02, // 2% Kimchi Premium
				AskUSD:            prices[idx] * 1.025,
				BorrowRatePerHour: 0.0001,
			}
			bData, _ := json.Marshal(base)
			rClient.Publish(ctx, redis.TickerChannelPrefix+"BTC-KRW", bData)

			// Publish Coinbase USD
			cb := exchange.PriceUpdate{
				Exchange:          "COINBASE",
				Symbol:            "BTC-USD",
				RawBid:            prices[idx],
				RawAsk:            prices[idx] * 1.0001,
				BidUSD:            prices[idx],
				AskUSD:            prices[idx] * 1.0001,
				BorrowRatePerHour: 0.0001,
			}
			cData, _ := json.Marshal(cb)
			rClient.Publish(ctx, redis.TickerChannelPrefix+"BTC-USD", cData)

			log.Printf(
				"[Feed Handler] Published BTC: $%.2f (Upbit %2.2f%% Premium)",
				prices[idx],
				2.0,
			)

			idx = (idx + 1) % len(prices)
		}
	}
}



func RunMockNodeEngine(ctx context.Context, redisURL string, db *postgres.DB) {
	log.Println("[Node Engine] Started. Subscribing to Redis & Evaluating AI predictions.")

	pubsubClient := redis.NewPubSubClient(redisURL)

	priceCh := make(chan exchange.PriceUpdate, 100)
	go pubsubClient.SubscribeTickers(ctx, []exchange.Symbol{"BTC-KRW", "BTC-USD"}, priceCh)

	strats := []strategy.ArbitrageStrategy{
		&strategy.FutureSpotStrategy{},      // D
		&strategy.FutureFutureStrategy{},    // E
		&strategy.ForeignTransferStrategy{}, // C
	}

	var clients []exchange.Client
	mockUpbit := &mockExchange{Name: "UPBIT", MarginSupported: false}
	clients = append(clients, mockUpbit)
	mockCoinbase := &mockExchange{Name: "COINBASE", MarginSupported: true}
	clients = append(clients, mockCoinbase)

	exec := execution.NewExecutor(false)
	// Create engine
	engine := strategy.NewEngine("test-node", 1, db, pubsubClient, "default", clients, nil)
	_ = engine

	var upbitQuote, cbQuote exchange.PriceUpdate
	hasUpbit, hasCb := false, false

	params := strategy.PremiumEvalParams{
		BaseSpotBuyFeeRate:     0.0005,
		BaseSpotSellFeeRate:    0.0005,
		ForeignSpotBuyFeeRate:  0.0040,
		ForeignSpotSellFeeRate: 0.0040,
		TransferFeeFixedUsd:    10.0,
		EstTransferTime:        15 * time.Minute,
		InvestmentUsd:          50000.0,
		BaseBalanceUsd:         100000.0,
		ForeignBalanceUsd:      100000.0,
		BaseSupportsMargin:     false,
		ForeignSupportsMargin:  true,
	}

	timer := time.NewTimer(45 * time.Second) // Run the test for 45 seconds
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			log.Println(
				"[Node Engine] Integration Sandbox Test time limit reached. Exiting cleanly.",
			)
			return
		case quote := <-priceCh:
			if quote.Exchange == "UPBIT" {
				upbitQuote = quote
				hasUpbit = true
			} else {
				cbQuote = quote
				hasCb = true
			}

			if hasUpbit && hasCb {
				// We have a matched market tick, inject mock AI predictions natively
				params.PredictedPremiumTrajectory = []float64{0.02, 0.015, 0.01, 0.005, 0.005} // Mock trajectory
				params.PredictionConfidence = 0.85
				aiSignal := "HOLD"
				aiWeight := 1.0

				// Mocks: After ~10 seconds of runtime, simulate an AI decision
				if time.Now().Unix()%20 > 8 && time.Now().Unix()%20 < 15 {
					aiSignal = "SCALE_OUT"
					aiWeight = 0.5
				}
				params.AISignal = aiSignal
				params.AIWeight = aiWeight

				log.Printf(
					"[Node Engine] Tick: Base(%.2f) Foreign(%.2f). AI Predicts Mean Premium: %.2f%% | Signal: %s (Wt: %.2f)",
					upbitQuote.BidUSD,
					cbQuote.AskUSD,
					params.GetExpectedPremium()*100,
					aiSignal,
					aiWeight,
				)

					var bestEval *strategy.StrategyEvaluation
					var bestStrat strategy.ArbitrageStrategy

					for _, strat := range strats {
						eval := strat.Evaluate(upbitQuote, cbQuote, params)
						if eval.NetROI > 0 {
							log.Printf(
								"   -> [SIGNAL] Strategy %s expects %.2f%% ROI (Risk: %.1f, Score: %.1f)",
								eval.Type,
								eval.NetROI*100,
								eval.RiskScore,
								eval.FinalScore,
							)
							if bestEval == nil || eval.FinalScore > bestEval.FinalScore {
								copiedEval := eval
								bestEval = &copiedEval
								bestStrat = strat
							}
						}
					}

					if bestEval != nil {
						// Check if we hold positions. If zero, execute an entry.
						// If > 0, we let the Monitor handle exits/scaling. (Sandbox simplified
						// logic)
						activePositions, _ := engine.GetActivePositions()

						if len(activePositions) == 0 {
							log.Printf(
								"[Node Engine] ⚡ Triggering Execution for Best Strategy: %s",
								bestEval.Type,
							)

							dummyPair := strategy.CrossMarketPair{
								BaseCurrency: "BTC",
								BaseClient:   mockUpbit,
								BaseMarketInfo: exchange.MarketInfo{
									Symbol: "BTC-KRW", BaseCurrency: "BTC", QuoteCurrency: "KRW",
								},
								ForeignClient: mockCoinbase,
								ForeignMarketInfo: exchange.MarketInfo{
									Symbol: "BTC-USD", BaseCurrency: "BTC", QuoteCurrency: "USD",
								},
							}

							// ExecuteEntry takes (ctx, exec, pair) and returns
							// (*postgres.ActivePosition, error)
							_, err := engine.ExecuteStrategy(ctx, bestStrat, dummyPair)
							if err != nil {
								log.Printf("[Node Engine] ❌ ExecuteEntry Failed: %v", err)
							} else {
								log.Printf("[Node Engine] ✅ Entry Positions Opened successfully!")
							}
						} else {
							// For Sandbox, directly force evaluate open positions with the AI
							// injected mock quotes! Note: Because evaluateAllOpenPositions is
							// unexported, we can't call it natively from main. But we can replicate
							// the logic here.
							for _, pos := range activePositions {
								if aiSignal == "SCALE_OUT" {
									log.Printf(
										"📉 [AI Scaling Out] Decreasing exposure for %s from Sandbox Ticker",
										pos.PositionID,
									)
									exitRatio := 0.5 // Default fallback config

									var targetStrategy strategy.ArbitrageStrategy
									for _, strat := range strats {
										if string(strat.ID()) == pos.Type {
											targetStrategy = strat
											break
										}
									}

									var mockPair = strategy.CrossMarketPair{
										BaseCurrency: "BTC",
										BaseClient:   mockUpbit,
										BaseMarketInfo: exchange.MarketInfo{
											Symbol: "BTC-KRW", BaseCurrency: "BTC", QuoteCurrency: "KRW",
										},
										ForeignClient: mockCoinbase,
										ForeignMarketInfo: exchange.MarketInfo{
											Symbol: "BTC-USD", BaseCurrency: "BTC", QuoteCurrency: "USD",
										},
									}

									if targetStrategy != nil {
										err := targetStrategy.ExecuteExit(
											ctx, exec, mockPair, pos, exitRatio, nil,
										)
										if err == nil {
											reducedUsd := pos.InvestmentUSD * exitRatio
											reducedBase := pos.BaseAmount * exitRatio
											reducedForeign := pos.ForeignAmount * exitRatio

											_ = db.ReducePositionAmount(
												pos.PositionID,
												reducedUsd,
												reducedBase,
												reducedForeign,
											)
											log.Printf(
												"✅ DB Atomically Decremented Position Balances for %s by %.2f%%",
												pos.PositionID,
												exitRatio*100,
											)
											// Optional: Close it fully if needed later, but this
											// proves the exit ratio works
										}
									}
								}
							}
						}
					}

				// Reset
				hasUpbit, hasCb = false, false
			}
		}
	}
}

// mockExchange simulates an exchange API solely for Sandbox logging
type mockExchange struct {
	Name            string
	MarginSupported bool
}

func (m *mockExchange) GetName() string { return m.Name }
func (m *mockExchange) GetSupportedNetworks(ctx context.Context, currency exchange.Currency) ([]exchange.NetworkInfo, error) {
	return nil, nil
}
func (m *mockExchange) GetTradingFee(ctx context.Context, symbol exchange.Symbol) (exchange.TradeFeeInfo, error) {
	return exchange.TradeFeeInfo{}, nil
}
func (m *mockExchange) GetMarkets(ctx context.Context) ([]exchange.MarketInfo, error) {
	return nil, nil
}
func (m *mockExchange) GetTicker(ctx context.Context, symbol exchange.Symbol) (float64, error) {
	return 0, nil
}
func (m *mockExchange) GetFutureTicker(ctx context.Context, symbol exchange.Symbol) (float64, error) {
	return 0, nil
}
func (m *mockExchange) GetBalances(ctx context.Context) ([]exchange.Balance, error) { return nil, nil }
func (m *mockExchange) CancelOrder(ctx context.Context, orderID string, symbol exchange.Symbol) error {
	return nil
}
func (m *mockExchange) ConnectWS(ctx context.Context, symbols []exchange.Symbol, priceUpdateCh chan<- exchange.PriceUpdate) error {
	return nil
}
func (m *mockExchange) CloseWS() error       { return nil }
func (m *mockExchange) SupportsMargin() bool { return m.MarginSupported }
func (m *mockExchange) GetAPIKeyPermissions(ctx context.Context) (exchange.APIKeyPermissions, error) {
	return exchange.APIKeyPermissions{}, nil
}
func (m *mockExchange) GetDepositAddress(ctx context.Context, currency exchange.Currency, network exchange.Network) (exchange.DepositAddress, error) {
	return exchange.DepositAddress{}, nil
}
func (m *mockExchange) GetWithdrawalLimit(ctx context.Context, currency exchange.Currency, network exchange.Network) (exchange.WithdrawalLimit, error) {
	return exchange.WithdrawalLimit{}, nil
}
func (m *mockExchange) GetWithdrawHistory(ctx context.Context, currency exchange.Currency) ([]exchange.WithdrawRecord, error) {
	return nil, nil
}
func (m *mockExchange) CheckWithdrawalStatus(ctx context.Context, withdrawID string) (exchange.WithdrawStatus, error) {
	return exchange.WithdrawCompleted, nil
}
func (m *mockExchange) GetWithdrawalWhitelist(ctx context.Context, currency exchange.Currency) ([]string, error) {
	return nil, nil
}

func (m *mockExchange) PlaceOrder(ctx context.Context, req exchange.OrderRequest) (*exchange.OrderResponse, error) {
	log.Printf("   [Mock %s] Order Placed: %s %s %.4f qty @ $%.2f", m.Name, req.Side, req.Symbol, req.Amount, req.Price)
	// Return a static fake execution output
	return &exchange.OrderResponse{OrderID: "mock-order-123", Status: "FILLED", Price: req.Price, Amount: req.Amount}, nil
}

func (m *mockExchange) Borrow(ctx context.Context, currency exchange.Currency, amount float64) error {
	log.Printf("   [Mock %s] Borrowed: %.4f %s from Margin Pool", m.Name, amount, currency)
	return nil
}

func (m *mockExchange) Repay(ctx context.Context, currency exchange.Currency, amount float64) error {
	log.Printf("   [Mock %s] Repaid: %.4f %s to Margin Pool", m.Name, amount, currency)
	return nil
}

func (m *mockExchange) Withdraw(ctx context.Context, req exchange.WithdrawRequest) (*exchange.WithdrawResponse, error) {
	log.Printf("   [Mock %s] Withdrawing: %.4f %s to External Network", m.Name, req.Amount, req.Currency)
	return &exchange.WithdrawResponse{WithdrawID: "mock-wd-123", TxID: "0xMockTx", Status: string(exchange.WithdrawPending)}, nil
}
