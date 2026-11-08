/*
Package postgres
This file defines the GORM database table schemas (Models) mapped to the global PostgreSQL repository.
It specifies structures for user API key information, threshold settings (UserThreshold), real-time positions by strategy (ActivePosition), and wallet whitelist statuses.
*/
package postgres

import (
	"time"
)

// User represents an authenticated dashboard account
type User struct {
	ID           uint   `gorm:"primaryKey"`           // Primary integer identifier
	Email        string `gorm:"uniqueIndex;not null"` // Unique email address for login
	PasswordHash string `gorm:"not null"`             // Bcrypt-hashed password
	IsAdmin      bool   `gorm:"default:false"`        // Flag indicating if user is an administrator
	Region       string `gorm:"index;default:'KR'"`   // User origin region dictating which queue to enter
	EngineNodeID string `gorm:"index"`                // ID of the dedicated Trading Engine Node currently managing this user
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// UserAPIKey stores encrypted exchange linkage information
type UserAPIKey struct {
	ID                     uint       `gorm:"primaryKey"`               // Internal ID
	UserID                 uint       `gorm:"index;not null"`           // Relation back to the User
	Exchange               string     `gorm:"index"`                    // Name of the exchange (e.g. UPBIT)
	AccessKey              string                                       // Public access API key
	SecretKey              string     `gorm:"type:bytea"`               // Encrypted AES-GCM secret key byte array
	DefaultWithdrawalLimit float64                                      // Default transaction limit
	IsActive               bool       `gorm:"default:true"`             // Toggled ON/OFF by the user in Settings
	IsValid                bool       `gorm:"default:true"`             // Toggled OFF by backend if API key expires or permissions change
	ErrorMessage           string                                       // Explanation for an IsValid=false state
	ExpiresAt              *time.Time                                   // Optional expiration date provided by the exchange
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// UserSettings stores notification and app-level preferences for a User
type UserSettings struct {
	ID                        uint      `gorm:"primaryKey"`
	UserID                    uint      `gorm:"uniqueIndex;not null"` // Relation back to the User
	DiscordWebhookURL         string                                  // Discord channel hook URL for alerts
	TelegramChatID            string                                  // Telegram chat target ID
	NotifyOnEntry             bool      `gorm:"default:true"`         // Send notification when a new position opens
	NotifyOnExit              bool      `gorm:"default:true"`         // Send notification on position closure
	NotifyOnException         bool      `gorm:"default:true"`         // Send alert for system errors
	PartialExitMode           string    `gorm:"default:'FIXED'"`      // 'FIXED' or 'AUTO' (AI Weight proportional)
	PartialExitRatio          float64   `gorm:"default:0.5"`          // If FIXED, the fraction of position to close on SCALE_OUT (e.g. 0.5)
	EnableStrategicSwitching  bool      `gorm:"default:false"`        // Allow closing a profitable position if a much better one appears
	SwitchingProfitMultiplier float64   `gorm:"default:2.0"`          // Required ROI multiple to trigger a strategic switch
	ActiveViewMode            string    `gorm:"default:'PAPER'"`      // 'PAPER' or 'LIVE'
	PaperBotStatus            string    `gorm:"default:'RUNNING'"`    // 'RUNNING', 'DRAINING', or 'STOPPED'
	LiveBotStatus             string    `gorm:"default:'STOPPED'"`    // 'RUNNING', 'DRAINING', or 'STOPPED'
	PaperUpbitKRW             float64   `gorm:"default:50000000"`     // Virtual seed KRW on Upbit
	PaperCoinbaseUSD          float64   `gorm:"default:35000"`        // Virtual seed USD on Coinbase
	PaperBalancesJSON         string    `gorm:"type:text"`            // Persisted JSON snapshot of PaperClient balances/futures/borrows
	SimExchange               string    `gorm:"default:'UPBIT'"`      // Target exchange for price offset simulator ('UPBIT' or 'COINBASE')
	SimStartOffsetPct         float64   `gorm:"default:0"`            // Starting offset (%) when ramp was initiated
	SimTargetOffsetPct        float64   `gorm:"default:0"`            // Target offset (%) to reach after SimRampMinutes
	SimRampMinutes            float64   `gorm:"default:0"`            // Duration in minutes to ramp from start to target offset
	SimStartedAt              int64     `gorm:"default:0"`            // Unix timestamp (seconds) when ramp was started
	EVMWalletAddress          string    `json:"evm_wallet_address"`   // Personal EVM wallet (0x... for ETH/ARB/BASE/BSC/POLYGON Travel Rule & DeFi)
	EVMPrivateKeyEnc          string    `gorm:"type:bytea" json:"-"`  // Encrypted AES-GCM EVM private key
	SolanaWalletAddress       string    `json:"solana_wallet_address"`// Personal Solana wallet for SPL Travel Rule & Kamino DeFi
	SolanaPrivateKeyEnc       string    `gorm:"type:bytea" json:"-"`  // Encrypted AES-GCM Solana private key
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
}

// WalletWhitelist manages approved external withdrawal addresses
type WalletWhitelist struct {
	ID        uint      `gorm:"primaryKey"`
	UserID    uint      `gorm:"index;not null"` // User who owns this whitelist
	Exchange  string                            // Target exchange name
	Network   string                            // Target blockchain network (e.g. ERC20, TRC20)
	Address   string                            // The crypto wallet address string
	Tag       string                            // Memo or Destination Tag if required
	Status    string                            // e.g. "APPROVED", "PENDING"
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TradeExecution logs individual spot, margin, or transfer operations 
type TradeExecution struct {
	ID         uint      `gorm:"primaryKey"`
	PositionID string    `gorm:"index"` // Link multiple specific trades forming one arb position together
	Exchange   string                   // Exchange where the sub-trade occurred
	Symbol     string                   // Trading pair (e.g. BTC/USD)
	Side       string                   // e.g. "BUY", "SELL"
	Type       string                   // e.g. "LIMIT", "MARKET"
	Amount     float64                  // Executed size
	Price      float64                  // Executed price 
	Fee        float64                  // Paid commission fee 
	Status     string                   // "FILLED", "PARTIAL", "FAILED"
	CreatedAt  time.Time
}

// UserThreshold configures algorithm variables and behavior constraints limits for the Strategy
type UserThreshold struct {
	ID                     uint      `gorm:"primaryKey"`
	UserID                 uint      `gorm:"index;not null"`
	StrategyName           string    `gorm:"index"`                          // Target strategy this applies to (e.g. PremiumStrategy)
	Enabled                bool      `gorm:"default:false"`                  // Is the strategy allowed to trade?
	MinTargetProfit        float64                                           // Required minimum ROI (USD value)
	MaxStopLoss            float64                                           // Forced liquidation loss bound (USD value)
	MinTargetProfitPercent float64                                           // Required minimum ROI (%)
	MaxStopLossPercent     float64                                           // Forced liquidation loss bound (%)
	UsePercentParams       bool      `gorm:"default:false"`                  // Toggle to use percentages instead of absolute USD
	MaxAcceptableSpread    float64                                           // Max threshold for slippage buffer
	CapitalLimitUsd        float64                                           // Upper bound allocation specifically for this strategy
	PerPositionLimitUsd    float64                                           // [NEW] Maximum allocation allowed per single individual position
	MaxRiskScore           float64                                           // Maximum allowed AI Risk Score threshold (e.g. 50.0)
	RiskMultiplier         float64                                           // Configurable multiplier weight amplifying the Risk Score formula
	BalanceSkewThreshold   float64                                           // Custom threshold to deter transfer looping (e.g. 0.15)
	RebalanceTriggerSkew   float64   `gorm:"default:0.20"`                   // Trigger reverse rebalancing when Foreign USD share drops <= this (e.g. 0.20)
	RebalanceTargetSkew    float64   `gorm:"default:0.50"`                   // Stop reverse rebalancing once Foreign USD share reaches this target (e.g. 0.50)
	RebalanceMaxPremium    float64   `gorm:"default:0.0"`                    // Max premium allowed to execute reverse rebalancing (e.g. 0.0 for <= 0%)
	RetryOnFailure         bool                                              // Toggle execution atomic rollback retries on failure
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// ActivePosition saves live open position state for failover and crash recovery
type ActivePosition struct {
	ID                 uint      `gorm:"primaryKey"`
	UserID             uint      `gorm:"index;not null"` // [NEW] Link position directly to the user
	IsPaper            bool      `gorm:"index;default:false"`
	PositionID         string    `gorm:"uniqueIndex"`
	MonitorNodeID      string    `gorm:"index"` // ID of the K8s Replica that opened and monitors this position
	Status             string    // e.g. "OPEN", "IN_TRANSIT", "CLOSED"
	Type               string    // e.g. "BORROW_TRANSFER"
	BaseExchange       string
	ForeignExchange    string
	BaseSymbol         string
	ForeignSymbol      string
	BaseAmount         float64
	ForeignAmount      float64
	InvestmentUSD      float64
	EntryBasePrice     float64
	EntryForeignPrice  float64
	EntryPremium       float64
	ExpectedFuturePrem float64
	MaxUnrealizedROI   float64   // Track peaks for Trailing Stop logic
	RealizedProfitUSD  float64   // Settled realized profit in USD on close
	BuyOrderID                 string
	SellOrderID                string
	HedgeMarket                string     // e.g. "SPOT" or "FUTURE", used natively by Strategy C
	HedgeMode                  string     // "BASE_BORROW", "FOREIGN_BORROW", or "FOREIGN_FUTURES_HEDGE"
	AssassinationRationaleJSON string     `gorm:"type:text"` // Serialized AssassinationRationale for UI/UX display
	OpenedAt                   time.Time
	ClosedAt                   *time.Time
	UpdatedAt                  time.Time
}

// LeaderLease handles cluster-wide locks to prevent multi-pod InfluxDB deduplication.
type LeaderLease struct {
	LockName  string    `gorm:"primaryKey"` // E.g. "influxdb_kr_writer"
	NodeID    string    `gorm:"not null"`   // The pod currently holding the lease
	ExpiresAt time.Time `gorm:"not null"`   // When the lease expires (e.g. Now + 10s)
}

// SupportedExchange defines exchanges available for the trading engine to connect to
type SupportedExchange struct {
	ID        uint      `gorm:"primaryKey"`
	Name      string    `gorm:"uniqueIndex;not null"` // e.g. UPBIT, COINBASE
	IsActive  bool      `gorm:"default:true"`         // Whether the backend supports it globally
	CreatedAt time.Time
	UpdatedAt time.Time
}

// EquitySnapshot records time-series snapshots of a user's total portfolio valuation (Paper or Live)
// for the Coinbase-style Total Equity History chart.
type EquitySnapshot struct {
	ID                    uint      `gorm:"primaryKey" json:"id"`
	UserID                uint      `gorm:"index:idx_user_mode_time,priority:1;not null" json:"user_id"`
	IsPaper               bool      `gorm:"index:idx_user_mode_time,priority:2;not null;default:false" json:"is_paper"`
	TotalEquityUSD        float64   `json:"total_equity_usd"`
	AvailableUSD          float64   `json:"available_usd"`
	UnrealizedPnLUSD      float64   `json:"unrealized_pnl_usd"`
	FXRateKRW             float64   `json:"fx_rate_krw"`
	ExchangeBreakdownJSON string    `gorm:"type:text" json:"exchange_breakdown_json,omitempty"`
	RecordedAt            time.Time `gorm:"index:idx_user_mode_time,priority:3" json:"recorded_at"`
}
