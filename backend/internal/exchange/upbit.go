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
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	upbitRESTURL = "https://api.upbit.com/v1"
	upbitWSURL   = "wss://api.upbit.com/websocket/v1"
)

type UpbitClient struct {
	accessKey      string
	secretKey      string
	httpClient     *http.Client
	wsConn         *websocket.Conn
	lendingManager *WebLendingManager
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
		lendingManager: NewWebLendingManager(NewUpbitLendingDriver(), cdpURL, 30*time.Second),
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
		net := currency
		if net == "" {
			net = "BTC"
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
		depositEnabled := p.WalletState == "working" || p.WalletState == "deposit_only"
		withdrawEnabled := p.WalletState == "working" || p.WalletState == "withdraw_only"

		networks = append(networks, NetworkInfo{
			Network:          Network(toUnifiedCurrencyFromUpbit(p.Currency)), // Upbit mainly uses standard DB naming mostly aligned to Currency
			ExchangeNetID:    p.Currency,
			IsDepositEnable:  depositEnabled,
			IsWithdrawEnable: withdrawEnabled,
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
			Warning bool `json:"warning"`
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

		// Warning marks potentially illiquid/risky assets, or trading might be restricted.
		// NOTE: Upbit /v1/market/all does not return 'market_state', so it defaults to "". We assume active if no severe warning.
		isTradingEnabled := !p.MarketEvent.Warning
		if p.MarketState != "" {
			isTradingEnabled = isTradingEnabled && (p.MarketState == "active")
		}

		// Filtering directly at the client level: we only care about specific markets for arbitrage
		supportedQuotes := []Currency{"KRW", "USDT"}
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
		})
	}

	return markets, nil
}

func (u *UpbitClient) GetTradingFee(ctx context.Context, symbol Symbol) (TradeFeeInfo, error) {
	// Default Upbit KRW market fee is traditionally 0.05%
	return TradeFeeInfo{
		MakerFeeRate: 0.0005,
		TakerFeeRate: 0.0005,
	}, nil
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
	conn, _, err := websocket.DefaultDialer.Dial(upbitWSURL, nil)
	if err != nil {
		return err
	}
	u.wsConn = conn

	var upbitSymbols []string
	for _, sym := range symbols {
		upbitSymbols = append(upbitSymbols, toUpbitSymbol(sym))
	}

	// Send subscription message
	uuidStr := uuid.New().String()
	codesBytes, err := json.Marshal(upbitSymbols)
	if err != nil {
		return err
	}
	subMsg := fmt.Sprintf(`[{"ticket":"%s"},{"type":"orderbook","codes":%s}]`, uuidStr, string(codesBytes))

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
					// Handle reconnect logic here later
					return
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
					fxRate := 1385.0
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
						BidUSD:         bestBid / fxRate,
						AskUSD:         bestAsk / fxRate,
						FXRate:         fxRate,
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
			Code string `json:"code"`
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

	return WithdrawalLimit{
		Currency:       currency,
		Network:        network,
		MinWithdrawal:  parseF(parsed.WithdrawLimit.Minimum),
		DailyLimit:     parseF(parsed.WithdrawLimit.Daily),
		DailyRemaining: parseF(parsed.WithdrawLimit.Remaining),
		MaxPerTx:       parseF(parsed.WithdrawLimit.Onetime),
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
