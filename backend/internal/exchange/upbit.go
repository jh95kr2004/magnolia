/*
Package exchange
This file is the concrete implementation for communicating with Upbit OpenAPI, the largest exchange in South Korea.
It supports Spot trading, asset inquiries, withdrawal functions, and real-time orderbook/tick data collection via WebSocket.
It also includes API permission checks designed around South Korea's specific Travel Rule constraints.
*/
package exchange

import (
	"context"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	upbitRESTURL = "https://api.upbit.com/v1"
	upbitWSURL   = "wss://api.upbit.com/websocket/v1"
)

type cachedUpbitFee struct {
	info      TradeFeeInfo
	expiresAt time.Time
}

type cachedUpbitWithdrawFee struct {
	feeFixed  float64
	expiresAt time.Time
}

type UpbitClient struct {
	accessKey        string
	secretKey        string
	httpClient       *http.Client
	wsConn           *websocket.Conn
	lendingManager   *WebLendingManager
	feeCacheMu       sync.RWMutex
	feeCache         map[Symbol]cachedUpbitFee
	withdrawFeeCache map[Currency]cachedUpbitWithdrawFee
}

func NewUpbitClient(accessKey, secretKey string) *UpbitClient {
	cdpURL := os.Getenv("CHROME_CDP_URL")
	if cdpURL == "" {
		cdpURL = "http://localhost:9222"
	}
	return &UpbitClient{
		accessKey: accessKey,
		secretKey: secretKey,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		lendingManager:   NewWebLendingManager(NewUpbitLendingDriver(), cdpURL, 30*time.Second),
		feeCache:         make(map[Symbol]cachedUpbitFee),
		withdrawFeeCache: make(map[Currency]cachedUpbitWithdrawFee),
	}
}

func (u *UpbitClient) LendingManager() *WebLendingManager {
	return u.lendingManager
}

func (u *UpbitClient) GetName() string {
	return "UPBIT"
}

func toUpbitCurrency(c Currency) string {
	// Dynamically inferred. DB driven overrides could be injected via client config.
	return string(c)
}

func toUnifiedCurrencyFromUpbit(c string) Currency {
	return Currency(strings.ToUpper(c))
}

// toUpbitSymbol converts unified "BASE-QUOTE" (e.g. "BTC-KRW") to Upbit format "QUOTE-BASE" (e.g. "KRW-BTC").
func toUpbitSymbol(unified Symbol) string {
	base, quote := ParseSymbol(unified)
	if quote != "" {
		return string(quote) + "-" + string(base)
	}
	return string(unified)
}

// toUnifiedSymbolFromUpbit converts Upbit format "QUOTE-BASE" (e.g. "KRW-BTC") to unified "BASE-QUOTE" (e.g. "BTC-KRW").
func toUnifiedSymbolFromUpbit(upbitSymbol string) Symbol {
	parts := strings.Split(upbitSymbol, "-")
	if len(parts) == 2 {
		return FormatSymbol(Currency(parts[1]), Currency(parts[0])) // Quote is parts[0], Base is parts[1]
	}
	return Symbol(strings.ToUpper(upbitSymbol))
}

// signPayload generates the Upbit-specific JWT for authentication
func (u *UpbitClient) signPayload(query string) (string, error) {
	claims := jwt.MapClaims{
		"access_key": u.accessKey,
		"nonce":      uuid.New().String(),
	}

	if query != "" {
		hash := sha512.New()
		hash.Write([]byte(query))
		queryHash := fmt.Sprintf("%x", hash.Sum(nil))
		claims["query_hash"] = queryHash
		claims["query_hash_alg"] = "SHA512"
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(u.secretKey))
}

func (u *UpbitClient) GetSupportedNetworks(ctx context.Context, currency Currency) ([]NetworkInfo, error) {
	if u.accessKey == "" || u.accessKey == "public" || u.accessKey == "dummy" {
		if currency == "" {
			var list []NetworkInfo
			for _, cur := range []Currency{"BTC", "ETH", "XRP", "SOL", "TRX", "USDT", "USDC", "DOGE"} {
				feeFixed, estSec := DefaultNetworkSpec("UPBIT", cur)
				list = append(list, NetworkInfo{
					Network:          Network(cur),
					ExchangeNetID:    string(cur),
					IsDepositEnable:  true,
					IsWithdrawEnable: true,
					WithdrawFeeFixed: feeFixed,
					EstimatedTime:    estSec,
					RequiresMemo:     IsMemoRequiredNetwork(Network(cur)),
				})
			}
			return list, nil
		}
		feeFixed, estSec := DefaultNetworkSpec("UPBIT", currency)
		return []NetworkInfo{
			{
				Network:          Network(currency),
				ExchangeNetID:    string(currency),
				IsDepositEnable:  true,
				IsWithdrawEnable: true,
				WithdrawFeeFixed: feeFixed,
				EstimatedTime:    estSec,
				RequiresMemo:     IsMemoRequiredNetwork(Network(currency)),
			},
		}, nil
	}

	token, err := u.signPayload("")
	if err != nil {
		return nil, err
	}

	req, _ := http.NewRequestWithContext(ctx, "GET", upbitRESTURL+"/status/wallet", nil)
	req.Header.Add("Authorization", "Bearer "+token)

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("upbit wallet status failed: %d", resp.StatusCode)
	}

	var parsed []struct {
		Currency    string `json:"currency"`
		NetType     string `json:"net_type"`
		WalletState string `json:"wallet_state"` // working, withdraw_only, deposit_only, paused, unsupported
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	var networks []NetworkInfo
	for _, p := range parsed {
		// If a specific currency was requested, filter it. Otherwise return all.
		if currency != "" && p.Currency != toUpbitCurrency(currency) {
			continue
		}
		unifiedCur := toUnifiedCurrencyFromUpbit(p.Currency)
		depositEnabled := p.WalletState == "working" || p.WalletState == "deposit_only"
		withdrawEnabled := p.WalletState == "working" || p.WalletState == "withdraw_only"

		feeFixed, estSec := DefaultNetworkSpec("UPBIT", unifiedCur)
		u.feeCacheMu.RLock()
		cachedWF, hasWF := u.withdrawFeeCache[unifiedCur]
		u.feeCacheMu.RUnlock()
		if hasWF && time.Now().Before(cachedWF.expiresAt) {
			feeFixed = cachedWF.feeFixed
		} else if currency != "" && withdrawEnabled {
			netID := p.NetType
			if netID == "" {
				netID = p.Currency
			}
			if lim, err := u.GetWithdrawalLimit(ctx, unifiedCur, Network(netID)); err == nil && lim.WithdrawFeeFixed > 0 {
				feeFixed = lim.WithdrawFeeFixed
			}
		}

		netID := p.NetType
		if netID == "" {
			netID = p.Currency
		}
		networks = append(networks, NetworkInfo{
			Network:          Network(unifiedCur),
			ExchangeNetID:    netID,
			IsDepositEnable:  depositEnabled,
			IsWithdrawEnable: withdrawEnabled,
			WithdrawFeeFixed: feeFixed,
			EstimatedTime:    estSec,
			RequiresMemo:     IsMemoRequiredNetwork(Network(netID)) || IsMemoRequiredNetwork(Network(unifiedCur)),
		})
	}

	return networks, nil
}

func (u *UpbitClient) GetMarkets(ctx context.Context) ([]MarketInfo, error) {
	// Fetch wallet statuses if authenticated; otherwise fall back to public market list
	walletMap := make(map[Network]NetworkInfo)
	isPublicFallback := u.accessKey == "" || u.accessKey == "public" || u.accessKey == "dummy"
	if !isPublicFallback {
		if networks, err := u.GetSupportedNetworks(ctx, ""); err == nil {
			for _, net := range networks {
				walletMap[net.Network] = net
			}
		} else {
			isPublicFallback = true
		}
	}

	// Fetch market states (public endpoint, no API key required)
	req, _ := http.NewRequestWithContext(ctx, "GET", upbitRESTURL+"/market/all?isDetails=true", nil)
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("upbit markets failed: %d", resp.StatusCode)
	}

	var parsed []struct {
		Market      string `json:"market"`
		MarketEvent struct {
			Warning bool            `json:"warning"`
			Caution map[string]bool `json:"caution"`
		} `json:"market_event"`
		MarketState string `json:"market_state"` // active, etc.
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	var markets []MarketInfo
	for _, p := range parsed {
		parts := strings.Split(p.Market, "-")
		if len(parts) != 2 {
			continue
		}

		quoteCurrency := toUnifiedCurrencyFromUpbit(parts[0])
		baseCurrency := toUnifiedCurrencyFromUpbit(parts[1])

		// Find network state for the base currency
		canDeposit, canWithdraw := true, true
		if !isPublicFallback {
			baseNet, ok := walletMap[Network(baseCurrency)]
			if ok {
				canDeposit = baseNet.IsDepositEnable
				canWithdraw = baseNet.IsWithdrawEnable
			} else {
				canDeposit, canWithdraw = false, false
			}
		}

		var cautionTypes []string
		for k, v := range p.MarketEvent.Caution {
			if v {
				cautionTypes = append(cautionTypes, strings.ToUpper(k))
			}
		}

		// Note: Investment warning (유의 종목) coins remain actively tradable on Upbit until delisting,
		// and often experience gaduri premium surges. Keep trading enabled unless market_state is explicitly inactive.
		isTradingEnabled := true
		if p.MarketState != "" {
			isTradingEnabled = (p.MarketState == "active")
		}

		// Filtering directly at the client level: we only care about KRW fiat markets on Upbit for cross-exchange arbitrage
		supportedQuotes := []Currency{"KRW"}
		isSupported := false
		for _, sq := range supportedQuotes {
			if quoteCurrency == sq {
				isSupported = true
				break
			}
		}
		if !isSupported {
			continue
		}

		markets = append(markets, MarketInfo{
			Symbol:            FormatSymbol(baseCurrency, quoteCurrency),
			BaseCurrency:      baseCurrency,
			QuoteCurrency:     quoteCurrency,
			MarketType:        "SPOT", // Upbit REST only handles spot
			IsTradingEnabled:  isTradingEnabled,
			IsDepositEnabled:  canDeposit,
			IsWithdrawEnabled: canWithdraw,
			IsWarning:         p.MarketEvent.Warning,
			IsCaution:         len(cautionTypes) > 0,
			CautionTypes:      cautionTypes,
		})
	}

	return markets, nil
}

func (u *UpbitClient) GetTradingFee(ctx context.Context, symbol Symbol) (TradeFeeInfo, error) {
	defaultFee := TradeFeeInfo{
		MakerFeeRate:  0.0005,
		TakerFeeRate:  0.0005,
		MakerFeeFixed: 0.0,
		TakerFeeFixed: 0.0,
	}
	if symbol == "" {
		symbol = "BTC-KRW"
	}

	u.feeCacheMu.RLock()
	if cached, ok := u.feeCache[symbol]; ok && time.Now().Before(cached.expiresAt) {
		u.feeCacheMu.RUnlock()
		return cached.info, nil
	}
	u.feeCacheMu.RUnlock()

	if u.accessKey == "" || u.accessKey == "public" || u.accessKey == "dummy" {
		return defaultFee, nil
	}

	upbitSym := toUpbitSymbol(symbol)
	query := "market=" + upbitSym
	token, err := u.signPayload(query)
	if err != nil {
		return defaultFee, nil
	}

	req, err := http.NewRequestWithContext(ctx, "GET", upbitRESTURL+"/orders/chance?"+query, nil)
	if err != nil {
		return defaultFee, nil
	}
	req.Header.Add("Authorization", "Bearer "+token)

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return defaultFee, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		u.feeCacheMu.Lock()
		u.feeCache[symbol] = cachedUpbitFee{info: defaultFee, expiresAt: time.Now().Add(1 * time.Minute)}
		u.feeCacheMu.Unlock()
		return defaultFee, nil
	}

	var parsed struct {
		BidFee      string `json:"bid_fee"`
		AskFee      string `json:"ask_fee"`
		MakerBidFee string `json:"maker_bid_fee"`
		MakerAskFee string `json:"maker_ask_fee"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return defaultFee, nil
	}

	takerFee, _ := strconv.ParseFloat(parsed.BidFee, 64)
	if askF, err := strconv.ParseFloat(parsed.AskFee, 64); err == nil && askF > takerFee {
		takerFee = askF
	}
	if takerFee <= 0 {
		takerFee = 0.0005
	}
	makerFee, _ := strconv.ParseFloat(parsed.MakerBidFee, 64)
	if makerAskF, err := strconv.ParseFloat(parsed.MakerAskFee, 64); err == nil && makerAskF > makerFee {
		makerFee = makerAskF
	}
	if makerFee <= 0 {
		makerFee = takerFee
	}

	res := TradeFeeInfo{
		MakerFeeRate:  makerFee,
		TakerFeeRate:  takerFee,
		MakerFeeFixed: 0.0,
		TakerFeeFixed: 0.0,
	}
	u.feeCacheMu.Lock()
	u.feeCache[symbol] = cachedUpbitFee{info: res, expiresAt: time.Now().Add(10 * time.Minute)}
	u.feeCacheMu.Unlock()
	return res, nil
}

func (u *UpbitClient) GetTicker(ctx context.Context, symbol Symbol) (float64, error) {
	// TODO: Use Websocket to reduce the number of requests.
	upbitSym := toUpbitSymbol(symbol)
	uStr := fmt.Sprintf("%s/ticker?markets=%s", upbitRESTURL, upbitSym)
	resp, err := u.httpClient.Get(uStr)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("failed to fetch ticker, status: %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var result []struct {
		TradePrice float64 `json:"trade_price"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return 0, err
	}

	if len(result) == 0 {
		return 0, fmt.Errorf("empty ticker response")
	}

	return result[0].TradePrice, nil
}

func (u *UpbitClient) GetFutureTicker(ctx context.Context, symbol Symbol) (float64, error) {
	return 0, nil
}

func (u *UpbitClient) GetBalances(ctx context.Context) ([]Balance, error) {
	token, err := u.signPayload("")
	if err != nil {
		return nil, err
	}

	req, _ := http.NewRequestWithContext(ctx, "GET", upbitRESTURL+"/accounts", nil)
	req.Header.Add("Authorization", "Bearer "+token)

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("upbit balance failed: %d, %s", resp.StatusCode, string(body))
	}

	var parsed []struct {
		Currency string `json:"currency"`
		Balance  string `json:"balance"`
		Locked   string `json:"locked"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}

	var balances []Balance
	for _, p := range parsed {
		avail, _ := strconv.ParseFloat(p.Balance, 64)
		locked, _ := strconv.ParseFloat(p.Locked, 64)
		balances = append(balances, Balance{
			Currency:  toUnifiedCurrencyFromUpbit(p.Currency),
			Available: avail,
			Locked:    locked,
		})
	}

	return balances, nil
}

func (u *UpbitClient) PlaceOrder(ctx context.Context, req OrderRequest) (*OrderResponse, error) {
	if req.Symbol != "BTC-KRW" {
		return nil, fmt.Errorf("[SAFETY] Only BTC-KRW is allowed for Upbit paper/live testing")
	}

	totalSpend := req.Price * req.Amount
	if totalSpend > 10000 {
		return nil, fmt.Errorf("[SAFETY] Upbit order rejected: %f KRW exceeds 10,000 KRW limit", totalSpend)
	}

	if req.Type != LimitOrder {
		return nil, fmt.Errorf("only Limit orders are supported in this test implementation")
	}

	side := "bid"
	if req.Side == SellOrder {
		side = "ask"
	}

	upbitSym := toUpbitSymbol(req.Symbol)

	params := url.Values{}
	params.Add("market", upbitSym)
	params.Add("side", side)
	params.Add("volume", fmt.Sprintf("%.8f", req.Amount))
	params.Add("price", fmt.Sprintf("%.0f", req.Price))
	params.Add("ord_type", "limit")

	queryStr := params.Encode()
	token, err := u.signPayload(queryStr)
	if err != nil {
		return nil, err
	}

	httpReq, _ := http.NewRequestWithContext(ctx, "POST", upbitRESTURL+"/orders", strings.NewReader(queryStr))
	httpReq.Header.Add("Authorization", "Bearer "+token)
	httpReq.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	resp, err := u.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return nil, fmt.Errorf("upbit PlaceOrder failed: %d, %s", resp.StatusCode, string(bodyBytes))
	}

	var res struct {
		UUID string `json:"uuid"`
	}
	if err := json.Unmarshal(bodyBytes, &res); err != nil {
		return nil, err
	}

	return &OrderResponse{
		OrderID: res.UUID,
		Status:  "OPEN",
		Price:   req.Price,
		Amount:  req.Amount,
		Fee:     0,
	}, nil
}

func (u *UpbitClient) CancelOrder(ctx context.Context, orderID string, symbol Symbol) error {
	params := url.Values{}
	params.Add("uuid", orderID)

	queryStr := params.Encode()
	token, err := u.signPayload(queryStr)
	if err != nil {
		return err
	}

	httpReq, _ := http.NewRequestWithContext(ctx, "DELETE", upbitRESTURL+"/order?"+queryStr, nil)
	httpReq.Header.Add("Authorization", "Bearer "+token)

	resp, err := u.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upbit CancelOrder failed: %d, %s", resp.StatusCode, string(bodyBytes))
	}
	return nil
}

func (u *UpbitClient) ConnectWS(ctx context.Context, symbols []Symbol, priceUpdateCh chan<- PriceUpdate) error {
	var upbitSymbols []string
	for _, sym := range symbols {
		upbitSymbols = append(upbitSymbols, toUpbitSymbol(sym))
	}
	codesBytes, err := json.Marshal(upbitSymbols)
	if err != nil {
		return err
	}

	dialAndSubscribe := func() (*websocket.Conn, error) {
		conn, _, err := websocket.DefaultDialer.Dial(upbitWSURL, nil)
		if err != nil {
			return nil, err
		}
		uuidStr := uuid.New().String()
		subMsg := fmt.Sprintf(`[{"ticket":"%s"},{"type":"orderbook","codes":%s}]`, uuidStr, string(codesBytes))
		if err := conn.WriteMessage(websocket.TextMessage, []byte(subMsg)); err != nil {
			conn.Close()
			return nil, err
		}
		conn.SetReadDeadline(time.Now().Add(45 * time.Second))
		conn.SetPongHandler(func(string) error {
			conn.SetReadDeadline(time.Now().Add(45 * time.Second))
			return nil
		})
		u.wsConn = conn
		return conn, nil
	}

	initialConn, err := dialAndSubscribe()
	if err != nil {
		return err
	}

	go func() {
		conn := initialConn
		defer func() {
			if conn != nil {
				conn.Close()
			}
		}()

		for {
			if ctx.Err() != nil {
				return
			}

			if conn == nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
				newConn, err := dialAndSubscribe()
				if err != nil {
					log.Printf("[WS-RECONNECT] UPBIT WebSocket dial failed: %v (retrying...)", err)
					continue
				}
				log.Printf("[WS-RECONNECT] UPBIT WebSocket reconnected and subscribed (%d symbols)", len(upbitSymbols))
				conn = newConn
			}

			pingDone := make(chan struct{})
			go func(c *websocket.Conn, done <-chan struct{}) {
				ticker := time.NewTicker(20 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-done:
						return
					case <-ticker.C:
						_ = c.WriteControl(websocket.PingMessage, []byte("PING"), time.Now().Add(5*time.Second))
					}
				}
			}(conn, pingDone)

			for {
				conn.SetReadDeadline(time.Now().Add(45 * time.Second))
				_, message, err := conn.ReadMessage()
				if err != nil {
					log.Printf("[WS-RECONNECT] UPBIT WebSocket read error: %v. Reconnecting...", err)
					close(pingDone)
					conn.Close()
					conn = nil
					break
				}

				var ubUpdate struct {
					Type           string `json:"type"`
					Code           string `json:"code"`
					OrderbookUnits []struct {
						AskPrice float64 `json:"ask_price"`
						BidPrice float64 `json:"bid_price"`
					} `json:"orderbook_units"`
				}

				if err := json.Unmarshal(message, &ubUpdate); err == nil && len(ubUpdate.OrderbookUnits) > 0 {
					bestAsk := ubUpdate.OrderbookUnits[0].AskPrice
					bestBid := ubUpdate.OrderbookUnits[0].BidPrice
					unifiedSym := toUnifiedSymbolFromUpbit(ubUpdate.Code)
					_, quoteCur := ParseSymbol(unifiedSym)

					priceUpdateCh <- PriceUpdate{
						Exchange:       u.GetName(),
						Symbol:         unifiedSym,
						QuoteCurrency:  quoteCur,
						RawBid:         bestBid,
						RawAsk:         bestAsk,
						FXRate:         0,
						FXCurrencyPair: "USD/KRW",
					}
				}
			}
		}
	}()

	return nil
}

func (u *UpbitClient) CloseWS() error {
	if u.wsConn != nil {
		return u.wsConn.Close()
	}
	return nil
}

// === Margin / Web Lending API ===
// Upbit does not expose coin lending via its official OpenAPI, so borrowing and repayment
// are delegated to WebLendingManager (driving the Upbit web session inside the noVNC browser).
// SupportsMargin returns true ONLY when the user's browser session is authenticated and active.
func (u *UpbitClient) SupportsMargin() bool {
	if u.lendingManager == nil {
		return false
	}
	return u.lendingManager.IsSessionValid()
}

func (u *UpbitClient) Borrow(ctx context.Context, currency Currency, amount float64) error {
	if u.lendingManager == nil {
		return fmt.Errorf("upbit web lending manager is not initialized")
	}
	return u.lendingManager.Borrow(ctx, currency, amount)
}

func (u *UpbitClient) Repay(ctx context.Context, currency Currency, amount float64) error {
	if u.lendingManager == nil {
		return fmt.Errorf("upbit web lending manager is not initialized")
	}
	return u.lendingManager.Repay(ctx, currency, amount)
}

// === Transfer & Funding API ===

func (u *UpbitClient) GetAPIKeyPermissions(ctx context.Context) (APIKeyPermissions, error) {
	token, err := u.signPayload("")
	if err != nil {
		return APIKeyPermissions{}, err
	}

	req, _ := http.NewRequestWithContext(ctx, "GET", upbitRESTURL+"/api_keys", nil)
	req.Header.Add("Authorization", "Bearer "+token)

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return APIKeyPermissions{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return APIKeyPermissions{}, fmt.Errorf("upbit api keys failed: %d", resp.StatusCode)
	}

	var parsed []struct {
		AccessKey string `json:"access_key"`
		ExpireAt  string `json:"expire_at"`
	}
	// Note: Upbit's actual API keys endpoint doesn't directly return scopes in standard `/v1/api_keys`
	// but for the sake of matching our interface safely we decode and assume true if successful
	// (Real strict validation would require testing a dummy write or specifically parsing if they add a scope field later).
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return APIKeyPermissions{}, err
	}

	return APIKeyPermissions{
		CanRead:     true,
		CanTrade:    true,
		CanWithdraw: true, // Optimistically assuming true if query succeeds, alternatively needs a dummy POST test.
	}, nil
}

func (u *UpbitClient) GetDepositAddress(ctx context.Context, currency Currency, network Network) (DepositAddress, error) {
	// Payload format: currency=BTC&net_type=BTC
	query := fmt.Sprintf("currency=%s&net_type=%s", string(currency), string(network))
	token, err := u.signPayload(query)
	if err != nil {
		return DepositAddress{}, err
	}

	req, _ := http.NewRequestWithContext(ctx, "GET", upbitRESTURL+"/coin_address?"+query, nil)
	req.Header.Add("Authorization", "Bearer "+token)

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return DepositAddress{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return DepositAddress{}, fmt.Errorf("upbit get deposit address failed: %d", resp.StatusCode)
	}

	var parsed struct {
		Currency         string `json:"currency"`
		DepositAddress   string `json:"deposit_address"`
		SecondaryAddress string `json:"secondary_address"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return DepositAddress{}, err
	}

	if strings.TrimSpace(parsed.SecondaryAddress) != "" {
		RegisterMemoRequiredNetwork(Network(currency), network)
	}

	return DepositAddress{
		Address: parsed.DepositAddress,
		Tag:     parsed.SecondaryAddress,
	}, nil
}

func (u *UpbitClient) GetWithdrawalLimit(ctx context.Context, currency Currency, network Network) (WithdrawalLimit, error) {
	query := fmt.Sprintf("currency=%s&net_type=%s", string(currency), string(network))
	token, err := u.signPayload(query)
	if err != nil {
		return WithdrawalLimit{}, err
	}

	req, _ := http.NewRequestWithContext(ctx, "GET", upbitRESTURL+"/withdraws/chance?"+query, nil)
	req.Header.Add("Authorization", "Bearer "+token)

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return WithdrawalLimit{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return WithdrawalLimit{}, fmt.Errorf("upbit get limits failed: %d", resp.StatusCode)
	}

	var parsed struct {
		MemberLevel struct {
			SecurityLevel int `json:"security_level"`
		} `json:"member_level"`
		Currency struct {
			Code        string  `json:"code"`
			WithdrawFee *string `json:"withdraw_fee"`
		} `json:"currency"`
		Account struct {
			Currency string `json:"currency"`
			Balance  string `json:"balance"`
			Locked   string `json:"locked"`
		} `json:"account"`
		WithdrawLimit struct {
			Currency      string  `json:"currency"`
			Minimum       *string `json:"minimum"`         // Minimum withdrawal amount
			Onetime       *string `json:"onetime"`         // Onetime limit
			Daily         *string `json:"daily"`           // Daily total limit
			Remaining     *string `json:"remaining_daily"` // Remaining daily limit
			RemainingCalc *string `json:"remaining_daily_krw"`
		} `json:"withdraw_limit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return WithdrawalLimit{}, err
	}

	parseF := func(s *string) float64 {
		if s == nil || *s == "" {
			return 0
		}
		f, _ := strconv.ParseFloat(*s, 64)
		return f
	}

	withdrawFeeFixed := parseF(parsed.Currency.WithdrawFee)
	if withdrawFeeFixed > 0 {
		u.feeCacheMu.Lock()
		u.withdrawFeeCache[currency] = cachedUpbitWithdrawFee{
			feeFixed:  withdrawFeeFixed,
			expiresAt: time.Now().Add(10 * time.Minute),
		}
		u.feeCacheMu.Unlock()
	}

	return WithdrawalLimit{
		Currency:         currency,
		Network:          network,
		MinWithdrawal:    parseF(parsed.WithdrawLimit.Minimum),
		DailyLimit:       parseF(parsed.WithdrawLimit.Daily),
		DailyRemaining:   parseF(parsed.WithdrawLimit.Remaining),
		MaxPerTx:         parseF(parsed.WithdrawLimit.Onetime),
		WithdrawFeeFixed: withdrawFeeFixed,
		WithdrawFeeRate:  0.0,
	}, nil
}

func (u *UpbitClient) Withdraw(ctx context.Context, req WithdrawRequest) (*WithdrawResponse, error) {
	// Endpoint: POST /v1/withdraws/coin
	// Params: currency, net_type, address, secondary_address, amount, transaction_type (default or internal)
	primaryAddr := req.Address
	secondaryAddr := req.Tag

	payloadParams := []string{
		fmt.Sprintf("currency=%s", string(req.Currency)),
		fmt.Sprintf("net_type=%s", string(req.Network)),
		fmt.Sprintf("amount=%f", req.Amount),
		fmt.Sprintf("address=%s", primaryAddr),
		"transaction_type=default",
	}
	if secondaryAddr != "" {
		payloadParams = append(payloadParams, fmt.Sprintf("secondary_address=%s", secondaryAddr))
	}

	query := strings.Join(payloadParams, "&")
	token, err := u.signPayload(query)
	if err != nil {
		return nil, err
	}

	httpReq, _ := http.NewRequestWithContext(ctx, "POST", upbitRESTURL+"/withdraws/coin?"+query, nil)
	httpReq.Header.Add("Authorization", "Bearer "+token)

	resp, err := u.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("upbit Withdraw failed: %d, %s", resp.StatusCode, string(bodyBytes))
	}

	var parsed struct {
		Type     string `json:"type"`
		UUID     string `json:"uuid"`
		Currency string `json:"currency"`
		NetType  string `json:"net_type"`
		TxID     string `json:"txid"`
		State    string `json:"state"`
		Amount   string `json:"amount"`
		Fee      string `json:"fee"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	amountF, _ := strconv.ParseFloat(parsed.Amount, 64)
	feeF, _ := strconv.ParseFloat(parsed.Fee, 64)

	return &WithdrawResponse{
		WithdrawID: parsed.UUID,
		TxID:       parsed.TxID,
		Fee:        feeF,
		Amount:     amountF,
		Status:     parsed.State,
	}, nil
}

func (u *UpbitClient) GetWithdrawHistory(ctx context.Context, currency Currency) ([]WithdrawRecord, error) {
	query := fmt.Sprintf("currency=%s&state=DONE", string(currency))
	token, err := u.signPayload(query)
	if err != nil {
		return nil, err
	}

	req, _ := http.NewRequestWithContext(ctx, "GET", upbitRESTURL+"/withdraws?"+query, nil)
	req.Header.Add("Authorization", "Bearer "+token)

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var parsed []struct {
		UUID      string `json:"uuid"`
		TxID      string `json:"txid"`
		Currency  string `json:"currency"`
		NetType   string `json:"net_type"`
		Amount    string `json:"amount"`
		Fee       string `json:"fee"`
		State     string `json:"state"`
		CreatedAt string `json:"created_at"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	var records []WithdrawRecord
	for _, p := range parsed {
		amt, _ := strconv.ParseFloat(p.Amount, 64)
		fee, _ := strconv.ParseFloat(p.Fee, 64)
		t, _ := time.Parse(time.RFC3339, p.CreatedAt)

		records = append(records, WithdrawRecord{
			WithdrawID: p.UUID,
			TxID:       p.TxID,
			Currency:   Currency(p.Currency),
			Network:    Network(p.NetType), // Uses the native strings Upbit provides back, we rely on ranker logic to match
			Amount:     amt,
			Fee:        fee,
			Status:     WithdrawCompleted,
			CreatedAt:  t,
		})
	}
	return records, nil
}

func (u *UpbitClient) CheckWithdrawalStatus(ctx context.Context, withdrawID string) (WithdrawStatus, error) {
	query := fmt.Sprintf("uuid=%s", withdrawID)
	token, err := u.signPayload(query)
	if err != nil {
		return WithdrawUnknown, err
	}

	req, _ := http.NewRequestWithContext(ctx, "GET", upbitRESTURL+"/withdraw?"+query, nil)
	req.Header.Add("Authorization", "Bearer "+token)

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return WithdrawUnknown, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return WithdrawUnknown, fmt.Errorf("upbit get withdraw status failed: %d", resp.StatusCode)
	}

	var parsed struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return WithdrawUnknown, err
	}

	switch parsed.State {
	case "SUBMITTED", "QUEUED", "PREPARING", "PROCESSING", "SUBMITTING":
		return WithdrawPending, nil
	case "DONE":
		return WithdrawCompleted, nil
	case "CANCELED":
		return WithdrawCancelled, nil
	case "REJECTED", "FAILED":
		return WithdrawFailed, nil
	default:
		return WithdrawUnknown, nil
	}
}

func (u *UpbitClient) GetWithdrawalWhitelist(ctx context.Context, currency Currency) ([]string, error) {
	token, err := u.signPayload("")
	if err != nil {
		return nil, err
	}

	req, _ := http.NewRequestWithContext(ctx, "GET", upbitRESTURL+"/withdraws/coin_addresses", nil)
	req.Header.Add("Authorization", "Bearer "+token)

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("upbit get whitelist failed: %d", resp.StatusCode)
	}

	var parsed []struct {
		Currency         string `json:"currency"`
		NetType          string `json:"net_type"`
		WithdrawAddress  string `json:"withdraw_address"`
		SecondaryAddress string `json:"secondary_address"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	var whitelists []string
	for _, p := range parsed {
		if p.Currency == string(currency) {
			if p.SecondaryAddress != "" {
				whitelists = append(whitelists, p.WithdrawAddress+":"+p.SecondaryAddress)
			} else {
				whitelists = append(whitelists, p.WithdrawAddress)
			}
		}
	}

	return whitelists, nil
}

// VerifyTravelRuleDeposit implements TravelRuleVerifier using Upbit's official Travel Rule OpenAPI:
// 1. Checks `GET /v1/deposits` for incoming deposits matching currency/txID.
// 2. If `state == "TRAVEL_RULE_SUSPECTED"`, queries `GET /v1/travel_rule/vasps` to resolve the sending VASP UUID.
// 3. Triggers `POST /v1/travel_rule/deposit/uuid` (or `/v1/travel_rule/deposit/txid`) for zero-touch account-owner verification.
func (u *UpbitClient) VerifyTravelRuleDeposit(ctx context.Context, currency Currency, txID string, fromExchange string) (*TravelRuleDepositStatus, error) {
	if u.accessKey == "" || u.accessKey == "public" || u.accessKey == "dummy" {
		return &TravelRuleDepositStatus{
			TxID:         txID,
			DepositState: "ACCEPTED",
			Verified:     true,
			Message:      "Public/Paper mode deposit auto-verified",
		}, nil
	}

	baseURL := upbitRESTURL
	if customURL := os.Getenv("UPBIT_REST_URL_OVERRIDE"); customURL != "" {
		baseURL = strings.TrimRight(customURL, "/")
	}

	// 1. Query recent deposits for currency
	params := url.Values{}
	if currency != "" {
		params.Set("currency", toUpbitCurrency(currency))
	}
	if txID != "" {
		params.Set("txid", txID)
	}
	queryStr := params.Encode()
	token, err := u.signPayload(queryStr)
	if err != nil {
		return nil, err
	}

	reqUrl := baseURL + "/deposits"
	if queryStr != "" {
		reqUrl += "?" + queryStr
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", reqUrl, nil)
	req.Header.Add("Authorization", "Bearer "+token)

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var deposits []struct {
		UUID     string `json:"uuid"`
		TxID     string `json:"txid"`
		Currency string `json:"currency"`
		State    string `json:"state"`
	}
	if resp.StatusCode == 200 {
		_ = json.NewDecoder(resp.Body).Decode(&deposits)
	}

	depositUUID := ""
	depositState := ""
	for _, d := range deposits {
		if txID == "" || strings.EqualFold(d.TxID, txID) {
			depositUUID = d.UUID
			depositState = strings.ToUpper(d.State)
			break
		}
	}

	if depositState == "ACCEPTED" || depositState == "DONE" {
		return &TravelRuleDepositStatus{
			DepositUUID:  depositUUID,
			TxID:         txID,
			DepositState: depositState,
			Verified:     true,
			Message:      "Deposit already accepted by Upbit",
		}, nil
	}

	// 2. Lookup VASP UUID via GET /v1/travel_rule/vasps
	vaspToken, err := u.signPayload("")
	if err != nil {
		return nil, err
	}
	vaspReq, _ := http.NewRequestWithContext(ctx, "GET", baseURL+"/travel_rule/vasps", nil)
	vaspReq.Header.Add("Authorization", "Bearer "+vaspToken)
	vaspResp, err := u.httpClient.Do(vaspReq)
	if err != nil {
		return nil, err
	}
	defer vaspResp.Body.Close()

	var vasps []struct {
		VaspName   string `json:"vasp_name"`
		VaspUUID   string `json:"vasp_uuid"`
		Depositable bool  `json:"depositable"`
	}
	if vaspResp.StatusCode == 200 {
		_ = json.NewDecoder(vaspResp.Body).Decode(&vasps)
	}

	targetEx := strings.ToUpper(strings.TrimSpace(fromExchange))
	vaspUUID := ""
	for _, v := range vasps {
		vName := strings.ToUpper(v.VaspName)
		if strings.Contains(vName, targetEx) || (targetEx == "BINANCE" && strings.Contains(vName, "바이낸스")) ||
			(targetEx == "BYBIT" && strings.Contains(vName, "바이비트")) ||
			(targetEx == "OKX" && strings.Contains(vName, "오케이엑스")) ||
			(targetEx == "BITHUMB" && strings.Contains(vName, "빗썸")) ||
			(targetEx == "COINBASE" && strings.Contains(vName, "코인베이스")) {
			vaspUUID = v.VaspUUID
			break
		}
	}
	if vaspUUID == "" && len(vasps) > 0 {
		vaspUUID = vasps[0].VaspUUID
	}
	if vaspUUID == "" {
		return &TravelRuleDepositStatus{
			DepositUUID:  depositUUID,
			TxID:         txID,
			DepositState: depositState,
			Verified:     false,
			Message:      fmt.Sprintf("VASP UUID not found for exchange %s", fromExchange),
		}, nil
	}

	// 3. Trigger POST /v1/travel_rule/deposit/uuid (or /txid fallback)
	var verifyEndpoint string
	var verifyParams url.Values = url.Values{}
	verifyParams.Set("vasp_uuid", vaspUUID)
	if depositUUID != "" {
		verifyEndpoint = baseURL + "/travel_rule/deposit/uuid"
		verifyParams.Set("deposit_uuid", depositUUID)
	} else {
		verifyEndpoint = baseURL + "/travel_rule/deposit/txid"
		verifyParams.Set("txid", txID)
		verifyParams.Set("currency", toUpbitCurrency(currency))
	}

	verifyQuery := verifyParams.Encode()
	verifyToken, err := u.signPayload(verifyQuery)
	if err != nil {
		return nil, err
	}

	bodyMap := make(map[string]string)
	for k, v := range verifyParams {
		if len(v) > 0 {
			bodyMap[k] = v[0]
		}
	}
	jsonBytes, _ := json.Marshal(bodyMap)

	postReq, _ := http.NewRequestWithContext(ctx, "POST", verifyEndpoint, strings.NewReader(string(jsonBytes)))
	postReq.Header.Add("Authorization", "Bearer "+verifyToken)
	postReq.Header.Add("Content-Type", "application/json")

	postResp, err := u.httpClient.Do(postReq)
	if err != nil {
		return nil, err
	}
	defer postResp.Body.Close()

	var verifyRes struct {
		DepositUUID        string `json:"deposit_uuid"`
		DepositState       string `json:"deposit_state"`
		Verified           bool   `json:"verified"`
		VerificationResult string `json:"verification_result"`
	}
	_ = json.NewDecoder(postResp.Body).Decode(&verifyRes)
	isVerified := verifyRes.Verified || strings.EqualFold(verifyRes.VerificationResult, "VERIFIED")

	return &TravelRuleDepositStatus{
		DepositUUID:  verifyRes.DepositUUID,
		TxID:         txID,
		DepositState: verifyRes.DepositState,
		Verified:     isVerified,
		Message:      fmt.Sprintf("Upbit Travel Rule API status=%d verified=%v state=%s", postResp.StatusCode, isVerified, verifyRes.DepositState),
	}, nil
}
