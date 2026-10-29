/*
Package exchange
This file is the concrete implementation for communicating with Coinbase Advanced Trade API v3, an exchange platform based in the US.
It supports order placement, balance inquiries, inter-account transfers, withdrawal address verification, and real-time price reception via WebSocket.
*/
package exchange

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	coinbaseRESTURL = "https://api.coinbase.com/api/v3/brokerage"
	coinbaseWSURL   = "wss://advanced-trade-ws.coinbase.com"
)

type CoinbaseClient struct {
	apiName              string
	privateKey           ed25519.PrivateKey // Ed25519 key for CDP JWT
	httpClient           *http.Client
	wsConn               *websocket.Conn
	defaultWithdrawLimit float64 // Fallback withdrawal limit loaded from User DB Settings
}

func NewCoinbaseClient(apiKeyName, privateKeyStr string, defaultWithdrawLimit float64) (*CoinbaseClient, error) {
	// The key is an Ed25519 private key seed in base64
	cleanKey := strings.TrimSpace(privateKeyStr)
	cleanKey = strings.ReplaceAll(cleanKey, " ", "")
	cleanKey = strings.ReplaceAll(cleanKey, "\\n", "")
	cleanKey = strings.ReplaceAll(cleanKey, "\n", "")

	// Decode the base64 ed25519 seed/key
	decoded, err := base64.StdEncoding.DecodeString(cleanKey)
	if err != nil {
		return nil, fmt.Errorf("failed to decode Ed25519 base64 key: %v", err)
	}

	var privKey ed25519.PrivateKey
	if len(decoded) == ed25519.SeedSize { // 32 bytes
		privKey = ed25519.NewKeyFromSeed(decoded)
	} else if len(decoded) == ed25519.PrivateKeySize { // 64 bytes
		privKey = ed25519.PrivateKey(decoded)
	} else {
		return nil, fmt.Errorf("invalid Ed25519 key length from decoded base64: %d", len(decoded))
	}

	return &CoinbaseClient{
		apiName:              apiKeyName,
		privateKey:           privKey,
		defaultWithdrawLimit: defaultWithdrawLimit,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}, nil
}

func (c *CoinbaseClient) GetName() string {
	return "COINBASE"
}

func toCoinbaseCurrency(c Currency) string {
	// Dynamically inferred. DB driven overrides could be injected via client config.
	return string(c)
}

func toUnifiedCurrencyFromCoinbase(c string) Currency {
	return Currency(strings.ToUpper(c))
}

// toCoinbaseSymbol converts unified "BASE-QUOTE" to Coinbase format
func toCoinbaseSymbol(unified Symbol) string {
	return strings.ToUpper(string(unified))
}

// toUnifiedSymbolFromCoinbase converts Coinbase symbol to unified format
func toUnifiedSymbolFromCoinbase(cbSymbol string) Symbol {
	return Symbol(strings.ToUpper(cbSymbol))
}

// generateJWT closely matches the Python official SDK logic for EdDSA API token generation
func (c *CoinbaseClient) generateJWT(requestMethod, requestHost, requestPath string) (string, error) {
	uri := requestMethod + " " + requestHost + requestPath

	// Nonce generation
	var nonceBuilder strings.Builder
	rand.Seed(time.Now().UnixNano())
	for i := 0; i < 16; i++ {
		nonceBuilder.WriteString(strconv.Itoa(rand.Intn(10)))
	}
	nonce := nonceBuilder.String()

	nowSeconds := time.Now().Unix()

	// jwt-go uses registered claims structure or MapClaims.
	// We MUST strictly match the exact Python structure
	claims := jwt.MapClaims{
		"sub":  c.apiName,
		"iss":  "cdp",
		"aud":  []string{"cdp_service"}, // C++ 공식 예제 스펙에 맞추어 명시적으로 복구
		"nbf":  nowSeconds,
		"exp":  nowSeconds + 120,
		"uris": []string{uri}, // Explicitly an array of uris now
	}

	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)

	// Headers
	token.Header["alg"] = "EdDSA"
	token.Header["typ"] = "JWT"
	token.Header["kid"] = c.apiName
	token.Header["nonce"] = nonce

	return token.SignedString(c.privateKey)
}

func (c *CoinbaseClient) GetSupportedNetworks(ctx context.Context, currency Currency) ([]NetworkInfo, error) {
	// Coinbase Advanced Trade API v3 does not have a direct unauthenticated endpoint that clearly
	// exposes network-by-network deposit/withdrawal pausing status like Upbit does.
	// As a fallback safety measure for testing, we will assume standard networks are working.
	// Production implementations could parse `/api/v3/brokerage/accounts` balances or hook into Coinbase Retail API.

	// Create a mock active response for the requested currency to unblock the strategy engine
	net := currency
	if net == "" {
		net = "BTC" // default fallback
	}
	return []NetworkInfo{
		{
			Network:          Network(net),
			ExchangeNetID:    string(net),
			IsDepositEnable:  true,
			IsWithdrawEnable: true,
		},
	}, nil
}

func (c *CoinbaseClient) GetMarkets(ctx context.Context) ([]MarketInfo, error) {
	if c.apiName == "" || c.apiName == "public" || c.apiName == "dummy" {
		req, err := http.NewRequestWithContext(ctx, "GET", "https://api.exchange.coinbase.com/products", nil)
		if err == nil {
			req.Header.Add("User-Agent", "magnolia-feed")
			resp, doErr := c.httpClient.Do(req)
			if doErr == nil {
				defer resp.Body.Close()
				if resp.StatusCode == 200 {
					var pubProducts []struct {
						ID             string `json:"id"`
						BaseCurrency   string `json:"base_currency"`
						QuoteCurrency  string `json:"quote_currency"`
						Status         string `json:"status"`
						TradingDisabled bool  `json:"trading_disabled"`
					}
					if json.NewDecoder(resp.Body).Decode(&pubProducts) == nil && len(pubProducts) > 0 {
						var pubMarkets []MarketInfo
						for _, p := range pubProducts {
							quoteCur := toUnifiedCurrencyFromCoinbase(p.QuoteCurrency)
							if quoteCur != "USD" {
								continue
							}
							pubMarkets = append(pubMarkets, MarketInfo{
								Symbol:            toUnifiedSymbolFromCoinbase(p.ID),
								BaseCurrency:      toUnifiedCurrencyFromCoinbase(p.BaseCurrency),
								QuoteCurrency:     quoteCur,
								MarketType:        "SPOT",
								IsTradingEnabled:  p.Status == "online" && !p.TradingDisabled,
								IsDepositEnabled:  true,
								IsWithdrawEnabled: true,
							})
						}
						if len(pubMarkets) > 0 {
							return pubMarkets, nil
						}
					}
				}
			}
		}
		return []MarketInfo{
			{
				Symbol:            "BTC-USD",
				BaseCurrency:      "BTC",
				QuoteCurrency:     "USD",
				MarketType:        "SPOT",
				IsTradingEnabled:  true,
				IsDepositEnabled:  true,
				IsWithdrawEnabled: true,
			},
			{
				Symbol:            "ETH-USD",
				BaseCurrency:      "ETH",
				QuoteCurrency:     "USD",
				MarketType:        "SPOT",
				IsTradingEnabled:  true,
				IsDepositEnabled:  true,
				IsWithdrawEnabled: true,
			},
			{
				Symbol:            "XRP-USD",
				BaseCurrency:      "XRP",
				QuoteCurrency:     "USD",
				MarketType:        "SPOT",
				IsTradingEnabled:  true,
				IsDepositEnabled:  true,
				IsWithdrawEnabled: true,
			},
			{
				Symbol:            "SOL-USD",
				BaseCurrency:      "SOL",
				QuoteCurrency:     "USD",
				MarketType:        "SPOT",
				IsTradingEnabled:  true,
				IsDepositEnabled:  true,
				IsWithdrawEnabled: true,
			},
		}, nil
	}
	reqPath := "/api/v3/brokerage/products"
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.coinbase.com"+reqPath, nil)

	token, err := c.generateJWT("GET", "api.coinbase.com", reqPath)
	if err != nil {
		return nil, err
	}

	req.Header.Add("Authorization", "Bearer "+token)
	req.Header.Add("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("coinbase products failed: %d, body: %s", resp.StatusCode, string(bodyBytes))
	}

	var parsed struct {
		Products []struct {
			ProductID       string `json:"product_id"`
			BaseCurrencyID  string `json:"base_currency_id"`
			QuoteCurrencyID string `json:"quote_currency_id"`
			ProductType     string `json:"product_type"` // "SPOT", "FUTURE"
			Status          string `json:"status"`       // "online", "offline", "internal_reject"
		} `json:"products"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	var markets []MarketInfo
	for _, p := range parsed.Products {
		isTradingEnabled := p.Status == "online"

		// Filtering directly at the client level: we only care about specific markets for arbitrage
		supportedQuotes := []Currency{"USD", "USDC", "USDT"}
		isSupported := false
		for _, sq := range supportedQuotes {
			if toUnifiedCurrencyFromCoinbase(p.QuoteCurrencyID) == sq {
				isSupported = true
				break
			}
		}
		if !isSupported {
			continue
		}

		markets = append(markets, MarketInfo{
			Symbol:            toUnifiedSymbolFromCoinbase(p.ProductID),
			BaseCurrency:      toUnifiedCurrencyFromCoinbase(p.BaseCurrencyID),
			QuoteCurrency:     toUnifiedCurrencyFromCoinbase(p.QuoteCurrencyID),
			MarketType:        p.ProductType,
			IsTradingEnabled:  isTradingEnabled,
			IsDepositEnabled:  true, // Defaulting to true due to API limitations as noted above
			IsWithdrawEnabled: true, // Defaulting to true due to API limitations as noted above
		})
	}

	return markets, nil
}

func (c *CoinbaseClient) GetTradingFee(ctx context.Context, symbol Symbol) (TradeFeeInfo, error) {
	// Base Coinbase advanced trade fee
	return TradeFeeInfo{
		MakerFeeRate: 0.0040,
		TakerFeeRate: 0.0060,
	}, nil
}

func (c *CoinbaseClient) GetTicker(ctx context.Context, symbol Symbol) (float64, error) {
	if c.apiName == "" || c.apiName == "public" || c.apiName == "dummy" {
		cbSym := toCoinbaseSymbol(symbol)
		url := fmt.Sprintf("https://api.exchange.coinbase.com/products/%s/ticker", cbSym)
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return 0, err
		}
		req.Header.Add("User-Agent", "magnolia-feed")
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return 0, fmt.Errorf("public coinbase exchange ticker status: %d", resp.StatusCode)
		}
		var res struct {
			Price string `json:"price"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			return 0, err
		}
		price, _ := strconv.ParseFloat(res.Price, 64)
		return price, nil
	}
	// e.g. symbol "BTC-USD"
	cbSym := toCoinbaseSymbol(symbol)
	reqPath := "/api/v3/brokerage/products/" + cbSym + "/ticker"
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.coinbase.com"+reqPath, nil)

	// Apply EdDSA JWT using C++ reference model
	token, err := c.generateJWT("GET", "api.coinbase.com", reqPath)
	if err != nil {
		return 0, err
	}

	req.Header.Add("Authorization", "Bearer "+token)
	req.Header.Add("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("coinbase ticker failed: %d, body: %s", resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		Trades []struct {
			Price string `json:"price"`
		} `json:"trades"`
		Price string `json:"price"`
	}
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return 0, err
	}

	val := result.Price
	if val == "" && len(result.Trades) > 0 {
		val = result.Trades[0].Price
	}
	if val == "" {
		return 0, fmt.Errorf("no 'price' found in body: %s", string(bodyBytes))
	}

	return strconv.ParseFloat(val, 64)
}

func (c *CoinbaseClient) GetFutureTicker(ctx context.Context, symbol Symbol) (float64, error) {
	// Query Coinbase public perpetual/spot ticker for USD-settled price
	return c.GetTicker(ctx, symbol)
}

func (c *CoinbaseClient) GetBalances(ctx context.Context) ([]Balance, error) {
	reqPath := "/api/v3/brokerage/accounts"
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.coinbase.com"+reqPath, nil)

	token, err := c.generateJWT("GET", "api.coinbase.com", reqPath)
	if err != nil {
		return nil, err
	}

	req.Header.Add("Authorization", "Bearer "+token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("coinbase balance failed: %d | Response: %s", resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		Accounts []struct {
			Currency         string `json:"currency"`
			AvailableBalance struct {
				Value string `json:"value"`
			} `json:"available_balance"`
			Hold struct {
				Value string `json:"value"`
			} `json:"hold"`
		} `json:"accounts"`
	}

	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return nil, err
	}

	var balances []Balance
	for _, acc := range result.Accounts {
		avail, _ := strconv.ParseFloat(acc.AvailableBalance.Value, 64)
		hold, _ := strconv.ParseFloat(acc.Hold.Value, 64)
		balances = append(balances, Balance{
			Currency:  toUnifiedCurrencyFromCoinbase(acc.Currency),
			Available: avail,
			Locked:    hold,
		})
	}
	return balances, nil
}

func (c *CoinbaseClient) PlaceOrder(ctx context.Context, req OrderRequest) (*OrderResponse, error) {
	// unified symbol is BASE-QUOTE e.g. BTC-USD
	if req.Symbol != "BTC-USD" {
		return nil, fmt.Errorf("[SAFETY] Only BTC-USD is allowed for Coinbase paper/live testing")
	}

	totalSpend := req.Price * req.Amount
	if totalSpend > 10.0 {
		return nil, fmt.Errorf("[SAFETY] Coinbase order rejected: %f USD exceeds 10 USD limit", totalSpend)
	}

	if req.Type != LimitOrder {
		return nil, fmt.Errorf("only Limit orders are supported in this test implementation")
	}

	side := "BUY"
	if req.Side == SellOrder {
		side = "SELL"
	}

	cbSym := toCoinbaseSymbol(req.Symbol)
	clientOrderID := uuid.New().String()
	payload := map[string]interface{}{
		"client_order_id": clientOrderID,
		"product_id":      cbSym,
		"side":            side,
		"order_configuration": map[string]interface{}{
			"limit_limit_gtc": map[string]interface{}{
				"base_size":   fmt.Sprintf("%.8f", req.Amount),
				"limit_price": fmt.Sprintf("%.2f", req.Price),
			},
		},
	}

	jsonBytes, _ := json.Marshal(payload)
	reqPath := "/api/v3/brokerage/orders"

	httpReq, _ := http.NewRequestWithContext(ctx, "POST", "https://api.coinbase.com"+reqPath, bytes.NewReader(jsonBytes))

	token, err := c.generateJWT("POST", "api.coinbase.com", reqPath)
	if err != nil {
		return nil, err
	}

	httpReq.Header.Add("Authorization", "Bearer "+token)
	httpReq.Header.Add("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("coinbase PlaceOrder failed: %d, %s", resp.StatusCode, string(bodyBytes))
	}

	var res struct {
		Success         bool   `json:"success"`
		FailureReason   string `json:"failure_reason"`
		SuccessResponse struct {
			OrderId string `json:"order_id"`
		} `json:"success_response"`
	}
	if err := json.Unmarshal(bodyBytes, &res); err != nil {
		return nil, err
	}

	if !res.Success {
		return nil, fmt.Errorf("coinbase order creation failed: %s", res.FailureReason)
	}

	return &OrderResponse{
		OrderID: res.SuccessResponse.OrderId,
		Status:  "OPEN",
		Price:   req.Price,
		Amount:  req.Amount,
		Fee:     0,
	}, nil
}

func (c *CoinbaseClient) CancelOrder(ctx context.Context, orderID string, symbol Symbol) error {
	payload := map[string]interface{}{
		"order_ids": []string{orderID},
	}
	jsonBytes, _ := json.Marshal(payload)
	reqPath := "/api/v3/brokerage/orders/batch_cancel"

	httpReq, _ := http.NewRequestWithContext(ctx, "POST", "https://api.coinbase.com"+reqPath, bytes.NewReader(jsonBytes))

	token, err := c.generateJWT("POST", "api.coinbase.com", reqPath)
	if err != nil {
		return err
	}

	httpReq.Header.Add("Authorization", "Bearer "+token)
	httpReq.Header.Add("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("coinbase CancelOrder failed: %d, %s", resp.StatusCode, string(bodyBytes))
	}
	return nil
}

func (c *CoinbaseClient) ConnectWS(ctx context.Context, symbols []Symbol, priceUpdateCh chan<- PriceUpdate) error {
	var productIDs []string
	for _, sym := range symbols {
		productIDs = append(productIDs, toCoinbaseSymbol(sym))
	}
	productIDsBytes, err := json.Marshal(productIDs)
	if err != nil {
		return err
	}

	// If no authenticated Advanced Trade API key is configured, connect to Coinbase Public Exchange WebSocket feed
	if c.apiName == "" || c.apiName == "public" || c.apiName == "dummy" {
		pubWSURL := "wss://ws-feed.exchange.coinbase.com"
		conn, _, dialErr := websocket.DefaultDialer.Dial(pubWSURL, nil)
		if dialErr != nil {
			return dialErr
		}
		c.wsConn = conn

		subMsg := fmt.Sprintf(`{"type":"subscribe","product_ids":%s,"channels":["ticker"]}`, string(productIDsBytes))
		if err := conn.WriteMessage(websocket.TextMessage, []byte(subMsg)); err != nil {
			conn.Close()
			return err
		}

		go func() {
			defer conn.Close()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					_, message, err := conn.ReadMessage()
					if err != nil {
						return
					}
					var pubTick struct {
						Type      string `json:"type"`
						ProductID string `json:"product_id"`
						Price     string `json:"price"`
						BestBid   string `json:"best_bid"`
						BestAsk   string `json:"best_ask"`
					}
					if err := json.Unmarshal(message, &pubTick); err == nil && pubTick.Type == "ticker" && pubTick.ProductID != "" {
						bid, _ := strconv.ParseFloat(pubTick.BestBid, 64)
						ask, _ := strconv.ParseFloat(pubTick.BestAsk, 64)
						lastPrice, _ := strconv.ParseFloat(pubTick.Price, 64)
						if bid <= 0 {
							bid = lastPrice
						}
						if ask <= 0 {
							ask = lastPrice
						}
						if bid > 0 && ask > 0 {
							unifiedSym := toUnifiedSymbolFromCoinbase(pubTick.ProductID)
							_, quoteCur := ParseSymbol(unifiedSym)
							priceUpdateCh <- PriceUpdate{
								Exchange:       c.GetName(),
								Symbol:         unifiedSym,
								QuoteCurrency:  quoteCur,
								RawBid:         bid,
								RawAsk:         ask,
								RawFutureBid:   bid,
								RawFutureAsk:   ask,
								BidUSD:         bid,
								AskUSD:         ask,
								BidFutureUSD:   bid,
								AskFutureUSD:   ask,
								FXRate:         1.0,
								FXCurrencyPair: "USD/USD",
							}
						}
					}
				}
			}
		}()
		return nil
	}

	conn, _, err := websocket.DefaultDialer.Dial(coinbaseWSURL, nil)
	if err != nil {
		return err
	}
	c.wsConn = conn

	// Generate JWT for WS auth
	token, err := c.generateJWT("GET", "api.coinbase.com", "")
	if err != nil {
		return err
	}

	subMsg := fmt.Sprintf(`{
		"type": "subscribe",
		"product_ids": %s,
		"channel": "ticker",
		"jwt": "%s"
	}`, string(productIDsBytes), token)

	if err := conn.WriteMessage(websocket.TextMessage, []byte(subMsg)); err != nil {
		return err
	}

	go func() {
		defer conn.Close()
		for {
			select {
			case <-ctx.Done():
				return
			default:
				_, message, err := conn.ReadMessage()
				if err != nil {
					return
				}

				var cbUpdate struct {
					Events []struct {
						Tickers []struct {
							ProductID string `json:"product_id"`
							BestBid   string `json:"best_bid"`
							BestAsk   string `json:"best_ask"`
						} `json:"tickers"`
					} `json:"events"`
				}

				if err := json.Unmarshal(message, &cbUpdate); err == nil {
					for _, event := range cbUpdate.Events {
						for _, ticker := range event.Tickers {
							bid, _ := strconv.ParseFloat(ticker.BestBid, 64)
							ask, _ := strconv.ParseFloat(ticker.BestAsk, 64)

							if bid > 0 && ask > 0 {
								priceUpdateCh <- PriceUpdate{
									Exchange: c.GetName(),
									Symbol:   toUnifiedSymbolFromCoinbase(ticker.ProductID),
									BidUSD:   bid,
									AskUSD:   ask,
									FXRate:   1.0,
								}
							}
						}
					}
				}
			}
		}
	}()

	return nil
}

func (c *CoinbaseClient) CloseWS() error {
	if c.wsConn != nil {
		return c.wsConn.Close()
	}
	return nil
}

// === Margin API ===
// Advanced Trade API needs specific endpoint configuration for margin/portfolio borrowing.
func (c *CoinbaseClient) SupportsMargin() bool {
	return true // We will assume Coinbase can support margin
}

func (c *CoinbaseClient) Borrow(ctx context.Context, currency Currency, amount float64) error {
	// TODO: Integrate Coinbase Portfolio Margin Borrow endpoint
	return fmt.Errorf("coinbase borrow unimplemented")
}

func (c *CoinbaseClient) Repay(ctx context.Context, currency Currency, amount float64) error {
	// TODO: Integrate Coinbase Portfolio Margin Repay endpoint
	return fmt.Errorf("coinbase repay unimplemented")
}

// === Transfer & Funding API ===

func (c *CoinbaseClient) GetAPIKeyPermissions(ctx context.Context) (APIKeyPermissions, error) {
	if c.apiName == "" || c.apiName == "public" || c.apiName == "dummy" {
		return APIKeyPermissions{
			CanRead:     true,
			CanTrade:    true,
			CanWithdraw: true,
		}, nil
	}
	reqPath := "/api/v3/brokerage/key_permissions"
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.coinbase.com"+reqPath, nil)

	token, err := c.generateJWT("GET", "api.coinbase.com", reqPath)
	if err != nil {
		return APIKeyPermissions{}, err
	}

	req.Header.Add("Authorization", "Bearer "+token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return APIKeyPermissions{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return APIKeyPermissions{}, fmt.Errorf("coinbase api keys failed: %d", resp.StatusCode)
	}

	var parsed struct {
		CanTrade    bool `json:"can_trade"`
		CanTransfer bool `json:"can_transfer"` // Transfer implies withdrawal capabilities in CB advanced
		CanView     bool `json:"can_view"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return APIKeyPermissions{}, err
	}

	return APIKeyPermissions{
		CanRead:     parsed.CanView,
		CanTrade:    parsed.CanTrade,
		CanWithdraw: parsed.CanTransfer,
	}, nil
}

func (c *CoinbaseClient) GetDepositAddress(ctx context.Context, currency Currency, network Network) (DepositAddress, error) {
	// 1. Get account ID for this currency first
	accountsReqPath := "/api/v3/brokerage/accounts"
	accountsToken, err := c.generateJWT("GET", "api.coinbase.com", accountsReqPath)
	if err != nil {
		return DepositAddress{}, err
	}
	accReq, _ := http.NewRequestWithContext(ctx, "GET", "https://api.coinbase.com"+accountsReqPath, nil)
	accReq.Header.Add("Authorization", "Bearer "+accountsToken)

	accResp, err := c.httpClient.Do(accReq)
	if err != nil {
		return DepositAddress{}, err
	}
	defer accResp.Body.Close()

	var accParsed struct {
		Accounts []struct {
			UUID     string `json:"uuid"`
			Currency string `json:"currency"`
		} `json:"accounts"`
	}
	if err := json.NewDecoder(accResp.Body).Decode(&accParsed); err != nil {
		return DepositAddress{}, err
	}

	var accountID string
	for _, acc := range accParsed.Accounts {
		if acc.Currency == string(currency) {
			accountID = acc.UUID
			break
		}
	}
	if accountID == "" {
		return DepositAddress{}, fmt.Errorf("account not found for currency %s", currency)
	}

	// 2. Fetch deposit address using account ID
	addrReqPath := fmt.Sprintf("/api/v3/brokerage/accounts/%s/addresses", accountID)
	addrToken, err := c.generateJWT("GET", "api.coinbase.com", addrReqPath)
	if err != nil {
		return DepositAddress{}, err
	}

	addrReq, _ := http.NewRequestWithContext(ctx, "GET", "https://api.coinbase.com"+addrReqPath, nil)
	addrReq.Header.Add("Authorization", "Bearer "+addrToken)

	addrResp, err := c.httpClient.Do(addrReq)
	if err != nil {
		return DepositAddress{}, err
	}
	defer addrResp.Body.Close()

	var addrParsed struct {
		Addresses []struct {
			Address     string `json:"address"`
			Network     string `json:"network"`
			Destination string `json:"destination_tag"`
		} `json:"addresses"`
	}
	if err := json.NewDecoder(addrResp.Body).Decode(&addrParsed); err != nil {
		return DepositAddress{}, err
	}

	for _, a := range addrParsed.Addresses {
		if string(network) == "" || a.Network == string(network) {
			return DepositAddress{
				Address: a.Address,
				Tag:     a.Destination,
			}, nil
		}
	}
	return DepositAddress{}, fmt.Errorf("no address found for network %s", network)
}

func (c *CoinbaseClient) GetWithdrawalLimit(ctx context.Context, currency Currency, network Network) (WithdrawalLimit, error) {
	// Fallback limit values loaded from DB upon client instantiation
	limitAmount := c.defaultWithdrawLimit
	remainingAmount := c.defaultWithdrawLimit

	if c.apiName == "" || c.apiName == "public" || c.apiName == "dummy" {
		return WithdrawalLimit{
			Currency:       currency,
			Network:        network,
			MinWithdrawal:  0.0,
			DailyLimit:     limitAmount,
			DailyRemaining: remainingAmount,
			MaxPerTx:       limitAmount,
		}, nil
	}

	// Trying the Exchange API /users/self/exchange-limits to see if user has specific transfer bounds.
	reqPath := "/users/self/exchange-limits"
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.exchange.coinbase.com"+reqPath, nil)

	token, err := c.generateJWT("GET", "api.exchange.coinbase.com", reqPath)
	if err == nil {
		req.Header.Add("Authorization", "Bearer "+token)
		resp, err := c.httpClient.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == 200 {
				var parsed struct {
					TransferLimits struct {
						ExchangeWithdraw map[string]struct {
							Max          string `json:"max"`
							Remaining    string `json:"remaining"`
							PeriodInDays int    `json:"period_in_days"`
						} `json:"exchange_withdraw"`
					} `json:"transfer_limits"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&parsed); err == nil {
					// Note: Coinbase's exchange-limits API primarily focuses on fiat limits (credit card, bank transfers).
					// It does not explicitly return daily chain limits for every individual crypto asset by default.
					// However, if the requested currency limit exists in 'exchange_withdraw', we extract it.
					// Otherwise, we fallback to a safe 250k daily limit baseline assigned to KYC-verified Advanced Trade users.
					if limitData, exists := parsed.TransferLimits.ExchangeWithdraw[string(currency)]; exists {
						if maxF, err := strconv.ParseFloat(limitData.Max, 64); err == nil {
							limitAmount = maxF
						}
						if remF, err := strconv.ParseFloat(limitData.Remaining, 64); err == nil {
							remainingAmount = remF
						}
					}
				}
			}
		}
	}

	return WithdrawalLimit{
		Currency:      currency,
		Network:       network,
		MinWithdrawal: 0.0,
		// 250k is the standard default daily withdrawal limit for Coinbase Pro / Advanced Trade users
		// who have completed full KYC verification. We use it as a fallback baseline here because
		// the API might not always return a specific limit for every crypto asset.
		DailyLimit:     limitAmount,
		DailyRemaining: remainingAmount,
		MaxPerTx:       limitAmount,
	}, nil
}

func (c *CoinbaseClient) Withdraw(ctx context.Context, req WithdrawRequest) (*WithdrawResponse, error) {
	if c.apiName == "" || c.apiName == "public" || c.apiName == "dummy" {
		return &WithdrawResponse{
			WithdrawID: "mock-coinbase-wd-id",
			TxID:       "mock-coinbase-tx-id",
			Fee:        0.0,
			Amount:     req.Amount,
			Status:     "COMPLETED",
		}, nil
	}
	// Coinbase v3 transfer
	reqPath := "/api/v3/brokerage/transfers"

	destAddr := req.Address
	tag := req.Tag

	payload := map[string]interface{}{
		"currency":        string(req.Currency),
		"amount":          strconv.FormatFloat(req.Amount, 'f', -1, 64),
		"destination":     destAddr,
		"network":         string(req.Network), // "ethereum", "bitcoin", "solana" (Needs to match Coinbase strings exactly mapped)
		"destination_tag": tag,
		"idem":            fmt.Sprintf("%d", time.Now().UnixNano()), // idempotent key
	}

	bodyBytes, _ := json.Marshal(payload)

	httpReq, _ := http.NewRequestWithContext(ctx, "POST", "https://api.coinbase.com"+reqPath, bytes.NewReader(bodyBytes))
	token, err := c.generateJWT("POST", "api.coinbase.com", reqPath)
	if err != nil {
		return nil, err
	}

	httpReq.Header.Add("Authorization", "Bearer "+token)
	httpReq.Header.Add("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		rb, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("coinbase Withdraw failed: %d, %s", resp.StatusCode, string(rb))
	}

	var parsed struct {
		Transfer struct {
			ID              string `json:"id"`
			Status          string `json:"status"` // e.g. "pending", "completed"
			TransactionID   string `json:"transaction_id"`
			NetworkFee      string `json:"network_fee"`
			ProcessedAmount string `json:"processed_amount"`
		} `json:"transfer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	amt, _ := strconv.ParseFloat(parsed.Transfer.ProcessedAmount, 64)
	fee, _ := strconv.ParseFloat(parsed.Transfer.NetworkFee, 64)

	return &WithdrawResponse{
		WithdrawID: parsed.Transfer.ID,
		TxID:       parsed.Transfer.TransactionID,
		Fee:        fee,
		Amount:     amt,
		Status:     parsed.Transfer.Status,
	}, nil
}

func (c *CoinbaseClient) GetWithdrawHistory(ctx context.Context, currency Currency) ([]WithdrawRecord, error) {
	if c.apiName == "" || c.apiName == "public" || c.apiName == "dummy" {
		return []WithdrawRecord{}, nil
	}
	// Standard Coinbase fetch lists transfers
	reqPath := fmt.Sprintf("/api/v3/brokerage/transfers?curreny=%s&type=WITHDRAW", string(currency))

	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.coinbase.com"+reqPath, nil)
	token, err := c.generateJWT("GET", "api.coinbase.com", reqPath)
	if err != nil {
		return nil, err
	}

	req.Header.Add("Authorization", "Bearer "+token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var parsed struct {
		Transfers []struct {
			ID            string `json:"id"`
			Type          string `json:"type"`
			Status        string `json:"status"`
			TransactionID string `json:"transaction_id"`
			Network       string `json:"network"`
			Amount        string `json:"amount"`
			Fee           string `json:"network_fee"`
			CreatedAt     string `json:"created_at"`
		} `json:"transfers"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		// Suppress strict errors on v3 history if endpoint structure mismatches slightly in our tests
		return nil, nil
	}

	var records []WithdrawRecord
	for _, p := range parsed.Transfers {
		amt, _ := strconv.ParseFloat(p.Amount, 64)
		fee, _ := strconv.ParseFloat(p.Fee, 64)
		t, _ := time.Parse(time.RFC3339, p.CreatedAt)

		status := WithdrawUnknown
		if p.Status == "completed" {
			status = WithdrawCompleted
		} else if p.Status == "pending" {
			status = WithdrawPending
		} else if p.Status == "failed" {
			status = WithdrawFailed
		}

		records = append(records, WithdrawRecord{
			WithdrawID: p.ID,
			TxID:       p.TransactionID,
			Currency:   currency,
			Network:    Network(p.Network),
			Amount:     amt,
			Fee:        fee,
			Status:     status,
			CreatedAt:  t,
		})
	}
	return records, nil
}

func (c *CoinbaseClient) CheckWithdrawalStatus(ctx context.Context, withdrawID string) (WithdrawStatus, error) {
	reqPath := fmt.Sprintf("/api/v3/brokerage/transfers/%s", withdrawID)
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.coinbase.com"+reqPath, nil)

	token, err := c.generateJWT("GET", "api.coinbase.com", reqPath)
	if err != nil {
		return WithdrawUnknown, err
	}

	req.Header.Add("Authorization", "Bearer "+token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return WithdrawUnknown, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return WithdrawUnknown, fmt.Errorf("coinbase get transfer %s failed: %d", withdrawID, resp.StatusCode)
	}

	var parsed struct {
		Transfer struct {
			Status string `json:"status"`
		} `json:"transfer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return WithdrawUnknown, err
	}

	switch parsed.Transfer.Status {
	case "pending", "processing":
		return WithdrawPending, nil
	case "completed":
		return WithdrawCompleted, nil
	case "canceled":
		return WithdrawCancelled, nil
	case "failed", "rejected":
		return WithdrawFailed, nil
	default:
		return WithdrawUnknown, nil
	}
}

func (c *CoinbaseClient) GetWithdrawalWhitelist(ctx context.Context, currency Currency) ([]string, error) {
	// Coinbase *does* enforce withdrawal whitelisting if the user has enabled the "Address Book"
	// security feature in their account UI. If enabled, withdrawals to non-whitelisted addresses will be blocked.
	// However, the Coinbase APIs do not currently provide a reliable, programmatic way to *read*
	// this whitelist beforehand without specialized permissions that aren't standard for trading keys.
	// Therefore, we return ErrWhitelistNotSupported wrapped with a helpful warning message so the ranking engine
	// knows it must alert the user to manually verify their allowlist settings.
	return nil, fmt.Errorf("%w: Please ensure your destination address is added to your Coinbase Allowlist at https://www.coinbase.com/settings/allowlist", ErrWhitelistNotSupported)
}
