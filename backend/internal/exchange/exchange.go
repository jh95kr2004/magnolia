/*
Package exchange
This file defines the unified Interface and base types (Order, PriceUpdate, etc.) for communicating with various exchanges (Upbit, Coinbase, etc.).
Other modules in the backend can handle REST API and WebSocket communications consistently through this interface without being tightly coupled to a specific exchange.
*/
package exchange

import (
	"context"
	"errors"
	"strings"
	"time"
)

var (
	// ErrWhitelistNotSupported is returned when an exchange does not provide a programmatic way to check its address whitelist via API.
	ErrWhitelistNotSupported = errors.New("whitelist retrieval not supported for this exchange")
)

type OrderSide string

const (
	BuyOrder  OrderSide = "BUY"
	SellOrder OrderSide = "SELL"
)

type OrderType string

const (
	LimitOrder  OrderType = "LIMIT"
	MarketOrder OrderType = "MARKET"

	// Advanced Order Types for specific strategies A-E
	FuturesLong  OrderType = "FUTURES_LONG"  // Enter long position on derivatives
	FuturesShort OrderType = "FUTURES_SHORT" // Enter short position on derivatives
)

// Currency represents a unified currency identifier across all exchanges
type Currency string

func (c Currency) String() string {
	return string(c)
}

// Symbol represents a unified trading pair identifier (BASE-QUOTE)
type Symbol string

func (s Symbol) String() string {
	return string(s)
}

// ParseSymbol takes a unified symbol "BASE-QUOTE" and returns base and quote currencies.
func ParseSymbol(unifiedSymbol Symbol) (base Currency, quote Currency) {
	parts := strings.Split(string(unifiedSymbol), "-")
	if len(parts) == 2 {
		return Currency(parts[0]), Currency(parts[1])
	}
	return Currency(unifiedSymbol), ""
}

// FormatSymbol formats base and quote currencies into a unified symbol "BASE-QUOTE".
func FormatSymbol(base, quote Currency) Symbol {
	return Symbol(strings.ToUpper(string(base)) + "-" + strings.ToUpper(string(quote)))
}

// Network represents a unified blockchain identifier (e.g. "TRC20", "ERC20", "BTC")
type Network string

func (n Network) String() string {
	return string(n)
}

// OrderRequest represents a standardized order layout for all exchanges
type OrderRequest struct {
	Symbol Symbol
	Side   OrderSide
	Type   OrderType
	Price  float64
	Amount float64
}

// OrderResponse brings back execution results
type OrderResponse struct {
	OrderID string
	Status  string
	Price   float64
	Amount  float64
	Fee     float64
}

// Balance struct maps available and locked asset amounts
type Balance struct {
	Currency  Currency
	Available float64
	Locked    float64
}

// NetworkInfo represents network transfer capabilities and status for a coin.
// The mapping between the unified Network (e.g. "TRC20") and the exchange's specific
// identifier (e.g. "TRX") will be driven by the database to allow no-downtime updates.
type NetworkInfo struct {
	Network          Network // Standardized network name fetched from DB (e.g. "TRC20")
	ExchangeNetID    string  // Exchange's custom identifier (e.g. "TRX")
	IsDepositEnable  bool
	IsWithdrawEnable bool
	WithdrawFeeFixed float64 // Fixed transfer fee magnitude (in coin units)
	WithdrawFeeRate  float64 // Percentage-based transfer fee (if applicable, e.g. 0.001 for 0.1%)
	EstimatedTime    int     // Estimated transfer time in seconds
	ContractAddress  string  // Token smart contract address (or native identifier) to prevent same-ticker collision
	RequiresMemo     bool    // True if network requires Destination Tag / Memo (e.g. Cosmos SDK chains, XRP, XLM)
	MinConfirmations int     // Required block confirmations before deposit is credited
}

// IsMemoRequiredNetwork returns true for Cosmos SDK chains and tag-based networks where depositing
// from a personal wallet into a CEX without a Memo/Destination Tag causes permanent deposit lockup.
func IsMemoRequiredNetwork(net Network) bool {
	n := strings.ToUpper(strings.TrimSpace(string(net)))
	switch n {
	case "SEI", "TIA", "CELESTIA", "OSMO", "OSMOSIS", "ATOM", "COSMOS", "INJ", "INJECTIVE",
		"KAVA", "AKT", "AKASH", "ARCH", "ARCHWAY", "AXL", "AXELAR", "BLD", "AGORIC",
		"XRP", "RIPPLE", "XLM", "STELLAR", "EOS", "HBAR", "HEDERA", "IOST", "XEM", "STX", "STACKS",
		"TON", "CRO", "CRONOS", "LUNA", "LUNC", "TERRA", "STEEM", "HIVE", "WAXP", "MED":
		return true
	default:
		return false
	}
}

// EffectiveWithdrawFeeCoins returns the total withdrawal fee in coin units (fixed coin fee + percentage of coin amount).
func (n NetworkInfo) EffectiveWithdrawFeeCoins(transferAmountCoins float64) float64 {
	feeCoins := 0.0
	if n.WithdrawFeeFixed > 0 {
		feeCoins += n.WithdrawFeeFixed
	}
	if n.WithdrawFeeRate > 0 && transferAmountCoins > 0 {
		feeCoins += transferAmountCoins * n.WithdrawFeeRate
	}
	return feeCoins
}

// EffectiveWithdrawFeeUSD returns the total withdrawal fee in USD (fixed coin fee * coinPriceUSD + transferVolumeUSD * rate).
func (n NetworkInfo) EffectiveWithdrawFeeUSD(coinPriceUSD, transferVolumeUSD float64) float64 {
	feeUSD := 0.0
	if n.WithdrawFeeFixed > 0 && coinPriceUSD > 0 {
		feeUSD += n.WithdrawFeeFixed * coinPriceUSD
	}
	if n.WithdrawFeeRate > 0 && transferVolumeUSD > 0 {
		feeUSD += transferVolumeUSD * n.WithdrawFeeRate
	}
	return feeUSD
}

// DefaultNetworkSpec returns baseline on-chain withdrawal fee (in coin units) and estimated confirmation time (seconds)
// when an exchange API is queried in unauthenticated public/paper mode.
func DefaultNetworkSpec(exchangeName string, currency Currency) (fixedFeeCoins float64, estTimeSec int) {
	cur := strings.ToUpper(string(currency))
	isUpbit := strings.EqualFold(exchangeName, "UPBIT") || strings.EqualFold(exchangeName, "BITHUMB")
	switch cur {
	case "BTC":
		if isUpbit {
			return 0.0002, 600
		}
		return 0.00005, 600
	case "ETH":
		if isUpbit {
			return 0.001, 300
		}
		return 0.0005, 300
	case "XRP":
		if isUpbit {
			return 0.25, 60
		}
		return 0.02, 60
	case "SOL":
		if isUpbit {
			return 0.008, 60
		}
		return 0.001, 60
	case "TRX":
		return 1.0, 90
	case "USDT", "USDC":
		return 1.0, 120
	case "DOGE":
		return 2.0, 300
	default:
		return 0.0, 300
	}
}

// TradeFeeInfo represents trading fees (both percentage rate and fixed fee per order), varying by user VIP tier
type TradeFeeInfo struct {
	MakerFeeRate  float64 `json:"maker_fee_rate"`
	TakerFeeRate  float64 `json:"taker_fee_rate"`
	MakerFeeFixed float64 `json:"maker_fee_fixed"` // Fixed fee per order in quote currency (if applicable)
	TakerFeeFixed float64 `json:"taker_fee_fixed"` // Fixed fee per order in quote currency (if applicable)
}

// EffectiveTakerFeeRate returns the combined percentage + fixed fee rate for a given quote currency order volume.
func (f TradeFeeInfo) EffectiveTakerFeeRate(orderVolumeQuote float64) float64 {
	rate := f.TakerFeeRate
	if orderVolumeQuote > 0 && f.TakerFeeFixed > 0 {
		rate += f.TakerFeeFixed / orderVolumeQuote
	}
	return rate
}

// EffectiveMakerFeeRate returns the combined percentage + fixed fee rate for a given quote currency order volume.
func (f TradeFeeInfo) EffectiveMakerFeeRate(orderVolumeQuote float64) float64 {
	rate := f.MakerFeeRate
	if orderVolumeQuote > 0 && f.MakerFeeFixed > 0 {
		rate += f.MakerFeeFixed / orderVolumeQuote
	}
	return rate
}

// MarketInfo represents the trading and transfer state of a market symbol.
type MarketInfo struct {
	Symbol                   Symbol
	BaseCurrency             Currency
	QuoteCurrency            Currency
	MarketType               string   // "SPOT" or "FUTURE"
	IsTradingEnabled         bool     // Can we execute buy/sell orders?
	IsDepositEnabled         bool     // Can we transfer assets into the exchange?
	IsWithdrawEnabled        bool     // Can we transfer assets out?
	IsWarning                bool     // Upbit/Bithumb investment warning (유의 종목)
	IsCaution                bool     // Upbit market caution (가격 급등락, 입금량 급등, 거래량 급등 등)
	CautionTypes             []string // Specific caution categories (e.g., "DEPOSIT_AMOUNT_SOARING", "TRADING_VOLUME_SOARING")
	MaintenanceScheduledTime *time.Time
}

// VASPTier classifies an exchange's Travel Rule interoperability level with Korean exchanges (Upbit/Bithumb).
type VASPTier string

const (
	VASPTier1Solution     VASPTier = "TIER1_SOLUTION"      // 100% auto-credited via VerifyVASP/CODE solution
	VASPTier2OwnerLinked  VASPTier = "TIER2_OWNER_LINKED"  // Account owner API verification supported (/v1/travel_rule/deposit/uuid)
	VASPTier3RiskAssessed VASPTier = "TIER3_RISK_ASSESSED" // Manual review if >=1M KRW direct deposit (e.g., Coinbase, Kraken); requires 2-hop walletProxy or non-transfer strategy
)

// AnnouncementEvent represents a parsed exchange announcement or market catalyst signal.
type AnnouncementEvent struct {
	ID          string     `json:"id"`
	Exchange    string     `json:"exchange"`
	EventType   string     `json:"event_type"` // NEW_KRW_LISTING, INVESTMENT_WARNING, DELISTING, WALLET_SUSPENSION, VOLUME_SURGE, NONE
	Symbols     []string   `json:"symbols"`
	Title       string     `json:"title"`
	SummaryKO   string     `json:"summary_ko"`
	SummaryEN   string     `json:"summary_en"`
	AnnouncedAt time.Time  `json:"announced_at"`
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`
	IsCatalyst  bool       `json:"is_catalyst"`
}

// TravelRuleDepositStatus represents the verification result of a deposit subject to Travel Rule.
type TravelRuleDepositStatus struct {
	DepositUUID  string `json:"deposit_uuid"`
	TxID         string `json:"txid"`
	DepositState string `json:"deposit_state"` // ACCEPTED, PROCESSING, TRAVEL_RULE_SUSPECTED, REFUNDING
	Verified     bool   `json:"verified"`
	Message      string `json:"message"`
}

// TravelRuleVerifier is an optional interface implemented by domestic exchanges (e.g., Upbit)
// that expose a programmatic Travel Rule verification OpenAPI (`POST /v1/travel_rule/deposit/uuid`).
type TravelRuleVerifier interface {
	VerifyTravelRuleDeposit(ctx context.Context, currency Currency, txID string, fromExchange string) (*TravelRuleDepositStatus, error)
}

// APIKeyPermissions represents the granted scopes of the currently used API key
type APIKeyPermissions struct {
	CanRead     bool // Allowed to read balances/orders
	CanTrade    bool // Allowed to place/cancel orders
	CanWithdraw bool // Allowed to withdraw funds to external addresses
}

// DepositAddress contains the address and an optional destination tag/memo
type DepositAddress struct {
	Address string
	Tag     string // Optional secondary address, memo, destination tag
}

// WithdrawRequest specifies parameters for sending crypto out of the exchange
type WithdrawRequest struct {
	Currency Currency
	Network  Network
	Amount   float64
	Address  string
	Tag      string // Memo or Destination tag, if required by the network
}

// WithdrawResponse contains the immediate result of a withdrawal request
type WithdrawResponse struct {
	WithdrawID string
	TxID       string  // Transaction hash (may be empty initially)
	Fee        float64 // Network fee charged
	Amount     float64 // Amount after fee
	Status     string
}

// WithdrawStatus represents the polling state of a withdrawal
type WithdrawStatus string

const (
	WithdrawPending   WithdrawStatus = "PENDING"
	WithdrawCompleted WithdrawStatus = "COMPLETED"
	WithdrawFailed    WithdrawStatus = "FAILED"
	WithdrawCancelled WithdrawStatus = "CANCELLED"
	WithdrawUnknown   WithdrawStatus = "UNKNOWN"
)

// WithdrawRecord represents a historical withdrawal entry
type WithdrawRecord struct {
	WithdrawID string
	TxID       string
	Currency   Currency
	Network    Network
	Amount     float64
	Fee        float64
	Status     WithdrawStatus
	CreatedAt  time.Time
}

// WithdrawalLimit represents the user's remaining transfer capacity and fee schedule
type WithdrawalLimit struct {
	Currency         Currency
	Network          Network
	MinWithdrawal    float64
	DailyLimit       float64
	DailyRemaining   float64
	MaxPerTx         float64
	WithdrawFeeFixed float64
	WithdrawFeeRate  float64
}

// PriceUpdate represents real-time incoming orderbook or ticker data via WS
type PriceUpdate struct {
	Exchange      string
	Symbol        Symbol
	QuoteCurrency Currency // e.g., "KRW", "USD", "USDT"
	RawBid        float64  // Raw local bid price
	RawFutureBid  float64  // Raw local future bid price
	RawAsk        float64  // Raw local ask price
	RawFutureAsk  float64  // Raw local future ask price
	BidUSD        float64  // Normalized USD price. For KRW, this is calculated by (Exchange Price / Real-time KRW-USD FX Rate).
	BidFutureUSD  float64  // Normalized USD price.
	AskUSD        float64  // Normalized USD price.
	AskFutureUSD  float64  // Normalized USD price.

	FXRate         float64
	FXCurrencyPair string // e.g., "KRW/USD" if FXRate maps KRW to USD

	// For futures platforms, this field stores active funding rate if applicable
	FundingRate float64
	// Time of the next funding settlement (Snapshot time)
	NextFundingTime time.Time
	// Duration between funding settlements (e.g. 8 * time.Hour)
	FundingInterval time.Duration
	// Indicates whether this update is from a Spot or Futures market
	IsFutures bool

	// Represents borrowing interest rate per hour (for margin trading)
	BorrowRatePerHour float64
	// Mark price for futures liquidation reference
	MarkPrice float64
}

// Client defines the contract that each exchange implementation must fulfill.
// This allows the strategy engine to interact blindly without needing to know specific exchange rules.
type Client interface {
	GetName() string

	// Asset & Network Information
	GetSupportedNetworks(ctx context.Context, currency Currency) ([]NetworkInfo, error)
	GetTradingFee(ctx context.Context, symbol Symbol) (TradeFeeInfo, error)
	GetMarkets(ctx context.Context) ([]MarketInfo, error)

	// REST Endpoints
	GetTicker(ctx context.Context, symbol Symbol) (float64, error)
	GetFutureTicker(ctx context.Context, symbol Symbol) (float64, error)
	GetBalances(ctx context.Context) ([]Balance, error)
	PlaceOrder(ctx context.Context, req OrderRequest) (*OrderResponse, error)
	CancelOrder(ctx context.Context, orderID string, symbol Symbol) error

	// WebSocket Endpoints
	ConnectWS(ctx context.Context, symbols []Symbol, priceUpdateCh chan<- PriceUpdate) error
	CloseWS() error

	// === Margin API ===
	SupportsMargin() bool
	Borrow(ctx context.Context, currency Currency, amount float64) error
	Repay(ctx context.Context, currency Currency, amount float64) error

	// === Transfer & Funding API ===
	GetAPIKeyPermissions(ctx context.Context) (APIKeyPermissions, error)
	GetDepositAddress(ctx context.Context, currency Currency, network Network) (DepositAddress, error)
	GetWithdrawalLimit(ctx context.Context, currency Currency, network Network) (WithdrawalLimit, error)
	Withdraw(ctx context.Context, req WithdrawRequest) (*WithdrawResponse, error)
	GetWithdrawHistory(ctx context.Context, currency Currency) ([]WithdrawRecord, error)
	// CheckWithdrawalStatus allows polling the state of an ongoing requested withdrawal.
	CheckWithdrawalStatus(ctx context.Context, withdrawID string) (WithdrawStatus, error)
	// GetWithdrawalWhitelist lists authorized output addresses.
	// Resolves with ErrWhitelistNotSupported if the exchange does not expose its address book via API natively.
	GetWithdrawalWhitelist(ctx context.Context, currency Currency) ([]string, error)
}

// WebLendingCapable is an optional interface implemented by exchange clients (e.g., Upbit, Bithumb)
// that support browser-automated coin lending via a WebLendingManager connected to a noVNC browser session.
type WebLendingCapable interface {
	LendingManager() *WebLendingManager
}

