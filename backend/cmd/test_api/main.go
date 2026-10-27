/*
Package main (test_api)
This file is a standalone test binary to verify the REST API and WebSocket stream integration for Upbit and Coinbase.
It verifies authentication, balance checks, and withdrawal limit queries without executing actual trades.
*/
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/magnolia/backend/internal/exchange"
)

func main() {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("===========================================")
	fmt.Println("   Upbit & Coinbase API Testing Utility    ")
	fmt.Println("===========================================")
	
	fmt.Print("Select Exchange to test [1: Upbit, 2: Coinbase]: ")
	choiceStr, _ := reader.ReadString('\n')
	choiceStr = strings.TrimSpace(choiceStr)

	var accessKey, secretKey string
	var client exchange.Client
	
	if choiceStr == "1" {
		accessKey = os.Getenv("UPBIT_ACCESS_KEY")
		secretKey = os.Getenv("UPBIT_SECRET_KEY")

		if accessKey == "" || secretKey == "" {
			fmt.Print("Enter Upbit Access Key: ")
			accessKey, _ = reader.ReadString('\n')
			accessKey = strings.TrimSpace(accessKey)
			
			fmt.Print("Enter Upbit Secret Key: ")
			secretKey, _ = reader.ReadString('\n')
			secretKey = strings.TrimSpace(secretKey)
		} else {
			fmt.Println("Loaded Upbit API keys from environment variables.")
		}
		
		client = exchange.NewUpbitClient(accessKey, secretKey)
		fmt.Println("\n>> Initialized Upbit Client")
	} else if choiceStr == "2" {
		accessKey = os.Getenv("COINBASE_API_KEY")
		secretKey = os.Getenv("COINBASE_API_SECRET")

		if accessKey == "" || secretKey == "" {
			fmt.Print("Enter Coinbase API Name UUID: ")
			accessKey, _ = reader.ReadString('\n')
			accessKey = strings.TrimSpace(accessKey)
			
			fmt.Print("Enter Coinbase Secret Key (Base64 / PEM): ")
			secretKey, _ = reader.ReadString('\n')
			secretKey = strings.TrimSpace(secretKey)
		} else {
			fmt.Println("Loaded Coinbase API keys from environment variables.")
		}
		
		cbClient, err := exchange.NewCoinbaseClient(accessKey, secretKey, 250000.0)
		if err != nil {
			fmt.Printf("Failed to initialize Coinbase Client: %v\n", err)
			return
		}
		client = cbClient
		fmt.Println("\n>> Initialized Coinbase Client")
	} else {
		fmt.Println("Invalid choice. Exiting.")
		return
	}

	// Test 1: GetBalances (REST)
	fmt.Println("\n-- Testing REST: GetBalances() --")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	balances, err := client.GetBalances(ctx)
	if err != nil {
		fmt.Printf("[ERROR] Balances failed: %v\n", err)
	} else {
		fmt.Printf("[SUCCESS] Balances returned: %d assets\n", len(balances))
		for _, b := range balances {
			fmt.Printf("  - %s: %f available, %f locked\n", b.Currency, b.Available, b.Locked)
		}
	}

	// Test 1.5: Dynamic Market Discovery
	fmt.Println("\n-- Testing REST: GetMarkets() --")
	marketCtx, marketCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer marketCancel()

	markets, err := client.GetMarkets(marketCtx)
	if err != nil {
		fmt.Printf("[ERROR] GetMarkets failed: %v\n", err)
	} else {
		fmt.Printf("[SUCCESS] GetMarkets returned: %d markets\n", len(markets))
		
		// Print top 5 markets as a sample
		sampleLimit := 5
		if len(markets) < 5 {
			sampleLimit = len(markets)
		}
		
		fmt.Printf("Displaying Sample (%d/%d):\n", sampleLimit, len(markets))
		for i := 0; i < sampleLimit; i++ {
			m := markets[i]
			fmt.Printf("  - %s (%s): Trading Enabled? %t | Deposit? %t | Withdraw? %t\n", 
				m.Symbol, m.MarketType, m.IsTradingEnabled, m.IsDepositEnabled, m.IsWithdrawEnabled)
		}
	}

	// Test 2: GetTicker (REST)
	fmt.Println("\n-- Testing REST: GetTicker() --")
	tickerCtx, tickerCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer tickerCancel()
	
	var tickerSymbol exchange.Symbol
	if choiceStr == "1" {
		tickerSymbol = exchange.Symbol("BTC-KRW")
	} else {
		tickerSymbol = exchange.Symbol("BTC-USD")
	}

	price, err := client.GetTicker(tickerCtx, tickerSymbol)
	if err != nil {
		fmt.Printf("[ERROR] Ticker failed: %v\n", err)
	} else {
		fmt.Printf("[SUCCESS] Ticker for %s: %f\n", tickerSymbol, price)
	}

	// Test 3: Live Order Placement (Safety Caps Active)
	fmt.Println("\n-- Testing REST: PlaceOrder & CancelOrder --")
	orderCtx, orderCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer orderCancel()

	var orderSymbol exchange.Symbol
	var orderPrice, orderAmount float64
	if choiceStr == "1" {
		orderSymbol = exchange.Symbol("BTC-KRW")
		orderPrice = 60000000.0 // Arbitrarily very low limit buy (won't execute immediately)
		orderAmount = 0.0001    // 60,000,000 * 0.0001 = 6,000 KRW constraint
	} else {
		orderSymbol = exchange.Symbol("BTC-USD")
		orderPrice = 50000.0    // Very low limit
		orderAmount = 0.0001    // $5 USD constraint
	}

	req := exchange.OrderRequest{
		Symbol: orderSymbol,
		Side:   exchange.BuyOrder,
		Type:   exchange.LimitOrder,
		Price:  orderPrice,
		Amount: orderAmount,
	}

	fmt.Printf("Attempting to place Limit Buy Order: %.4f %s at %.2f...\n", req.Amount, req.Symbol, req.Price)
	orderRes, err := client.PlaceOrder(orderCtx, req)
	if err != nil {
		fmt.Printf("[ERROR] PlaceOrder failed: %v\n", err)
	} else {
		fmt.Printf("[SUCCESS] PlaceOrder returned OrderID: %s\n", orderRes.OrderID)
		
		fmt.Printf("Attempting to cancel OrderID: %s...\n", orderRes.OrderID)
		cancelErr := client.CancelOrder(orderCtx, orderRes.OrderID, req.Symbol)
		if cancelErr != nil {
			fmt.Printf("[ERROR] CancelOrder failed: %v\n", cancelErr)
		} else {
			fmt.Println("[SUCCESS] CancelOrder successful.")
		}
	}

	// Test 4: API Permissions & Deposit Addresses
	fmt.Println("\n-- Testing Transfer APIs: Key Scopes & Deposits --")
	
	perm, err := client.GetAPIKeyPermissions(tickerCtx)
	if err != nil {
		fmt.Printf("[ERROR] GetAPIKeyPermissions failed: %v\n", err)
	} else {
		fmt.Printf("[SUCCESS] Permissions: Read=%v, Trade=%v, Withdraw=%v\n", perm.CanRead, perm.CanTrade, perm.CanWithdraw)
	}

	var testCurrency exchange.Currency = "BTC"
	var testNetwork exchange.Network = "BTC"
	depAddr, err := client.GetDepositAddress(tickerCtx, testCurrency, testNetwork)
	if err != nil {
		fmt.Printf("[ERROR] GetDepositAddress failed for %s (%s): %v\n", testCurrency, testNetwork, err)
	} else {
		fmt.Printf("[SUCCESS] Deposit Address for %s (%s): %s (Tag: %s)\n", testCurrency, testNetwork, depAddr.Address, depAddr.Tag)
	}

	// Test 5: Connect WebSockets
	fmt.Println("\n-- Testing WebSockets: Price Streams (timeout 5s) --")
	wsCtx, wsCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wsCancel()

	priceCh := make(chan exchange.PriceUpdate, 100)
	
	var symbols []exchange.Symbol
	if choiceStr == "1" {
		symbols = []exchange.Symbol{exchange.Symbol("BTC-KRW")}
	} else {
		symbols = []exchange.Symbol{exchange.Symbol("BTC-USD")}
	}

	err = client.ConnectWS(wsCtx, symbols, priceCh)
	if err != nil {
		fmt.Printf("[ERROR] WS Connection failed: %v\n", err)
		return
	}

	fmt.Println(">> Stream connected. Listening for updates...")
	
	streamCount := 0
	loop:
	for {
		select {
		case update := <-priceCh:
			streamCount++
			fmt.Printf(" [WS TICK] %s | Bid: %.2f | Ask: %.2f\n", update.Exchange, update.BidUSD, update.AskUSD)
		case <-wsCtx.Done():
			fmt.Printf(">> Timeout reached. Received %d stream updates.\n", streamCount)
			break loop
		}
	}

	client.CloseWS()
	fmt.Println("\nTest Completed.")
}
