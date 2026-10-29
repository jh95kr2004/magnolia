/*
Package postgres
This file is a client implementation that initializes the PostgreSQL database connection and encapsulates CRUD operations.
It also includes Leader Election (Test-and-Set) lock functionality for concurrency control and `pg_notify` publish functionality for Targeted Pub/Sub (Notifications) across distributed nodes.
*/
package postgres

import (
	"log"
	"strings"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type DB struct {
	conn *gorm.DB
}

func NewDB(dsn string) (*DB, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, err
	}

	// db.AutoMigrate automatically creates or updates the relational database schema (tables, columns) 
	// to match the latest defined Go structs (UserAPIKey, WalletWhitelist, etc.)
	// This ensures our PostgreSQL database is always in sync with our code without the need to run manual SQL scripts.
	err = db.AutoMigrate(
		&User{},
		&UserAPIKey{},
		&UserSettings{},
		&WalletWhitelist{},
		&TradeExecution{},
		&UserThreshold{},
		&ActivePosition{},
		&LeaderLease{},
		&SupportedExchange{},
	)
	if err != nil {
		log.Printf("Migration failed: %v", err)
		return nil, err
	}

	// Seed supported exchanges
	defaultExchanges := []string{"BINANCE", "BYBIT", "COINBASE", "KRAKEN", "UPBIT"}
	for _, name := range defaultExchanges {
		db.FirstOrCreate(&SupportedExchange{Name: name, IsActive: true}, SupportedExchange{Name: name})
	}

	return &DB{conn: db}, nil
}

// CreateUser inserts a new user record.
func (db *DB) CreateUser(user *User) error {
	user.Region = strings.ToLower(strings.TrimSpace(user.Region))
	if user.Region == "" {
		user.Region = "kr"
	}
	return db.conn.Create(user).Error
}

// GetUserByEmail finds a user by their email address for login.
func (db *DB) GetUserByEmail(email string) (*User, error) {
	var user User
	err := db.conn.Where("email = ?", email).First(&user).Error
	return &user, err
}

// GetUserByID finds a user by their primary ID.
func (db *DB) GetUserByID(userID uint) (*User, error) {
	var user User
	err := db.conn.First(&user, userID).Error
	return &user, err
}

// AssignEngineNode locks a user to a specific tracking engine node.
func (db *DB) AssignEngineNode(userID uint, nodeID string) error {
	return db.conn.Model(&User{}).Where("id = ?", userID).Update("engine_node_id", nodeID).Error
}

// ReleaseEngineNode removes the lock, returning the user to an unassigned state.
func (db *DB) ReleaseEngineNode(userID uint) error {
	return db.conn.Model(&User{}).Where("id = ?", userID).Update("engine_node_id", "").Error
}

// GetUsersByEngineNode fetches all users currently assigned to a specific engine node (Crash Recovery).
func (db *DB) GetUsersByEngineNode(nodeID string) ([]User, error) {
	var users []User
	err := db.conn.Where("engine_node_id = ?", nodeID).Find(&users).Error
	return users, err
}

// GetUnassignedUsers fetches users in the given region (or all if empty) who have no assigned engine node.
func (db *DB) GetUnassignedUsers(region string) ([]User, error) {
	var users []User
	q := db.conn.Where("engine_node_id IS NULL OR engine_node_id = ''")
	if region != "" {
		q = q.Where("LOWER(region) = LOWER(?)", region)
	}
	err := q.Find(&users).Error
	return users, err
}

// CleanOrphanedUsers finds users assigned to dead engine nodes, releases them, and returns them so they can be re-queued.
func (db *DB) CleanOrphanedUsers() ([]User, error) {
	var orphanedUsers []User
	now := time.Now()

	// Find users assigned to nodes that don't have an active lease
	err := db.conn.Raw(`
		SELECT * FROM users 
		WHERE engine_node_id IS NOT NULL AND engine_node_id != ''
		  AND engine_node_id NOT IN (
			SELECT node_id FROM leader_leases 
			WHERE lock_name LIKE 'engine_node:%' AND expires_at > ?
		  )
	`, now).Scan(&orphanedUsers).Error
	if err != nil {
		return nil, err
	}

	if len(orphanedUsers) == 0 {
		return nil, nil
	}

	// Atomically release the engine_node_id for these users in the DB
	var userIDs []uint
	for _, u := range orphanedUsers {
		userIDs = append(userIDs, u.ID)
	}

	err = db.conn.Model(&User{}).Where("id IN ?", userIDs).Update("engine_node_id", "").Error
	if err != nil {
		return nil, err
	}

	return orphanedUsers, nil
}

// GetUserSettings fetches global settings and webhooks for a user.
func (db *DB) GetUserSettings(userID uint) (*UserSettings, error) {
	var settings UserSettings
	err := db.conn.Where("user_id = ?", userID).First(&settings).Error
	if err != nil {
		return &settings, err
	}
	if settings.ActiveViewMode == "" {
		settings.ActiveViewMode = "PAPER"
	}
	if settings.PaperBotStatus == "" {
		settings.PaperBotStatus = "RUNNING"
	}
	if settings.LiveBotStatus == "" {
		settings.LiveBotStatus = "STOPPED"
	}
	if settings.PaperUpbitKRW <= 0 {
		settings.PaperUpbitKRW = 50000000.0
	}
	if settings.PaperCoinbaseUSD <= 0 {
		settings.PaperCoinbaseUSD = 35000.0
	}
	if settings.SimExchange == "" {
		settings.SimExchange = "UPBIT"
	}
	return &settings, nil
}

// SaveUserSettings updates or creates global user configurations.
func (db *DB) SaveUserSettings(settings *UserSettings) error {
	return db.conn.Save(settings).Error
}

// GetAPIKey retrieves an active user's API key. Decryption should be done by the caller using Decrypt().
func (db *DB) GetAPIKey(userID uint, exchangeName string) (*UserAPIKey, error) {
	var key UserAPIKey
	if err := db.conn.Where("user_id = ? AND exchange = ? AND is_active = ?", userID, exchangeName, true).First(&key).Error; err != nil {
		return nil, err
	}
	return &key, nil
}

// GetAllAPIKeys fetches all API keys for a dashboard overview.
func (db *DB) GetAllAPIKeys(userID uint) ([]UserAPIKey, error) {
	var keys []UserAPIKey
	err := db.conn.Where("user_id = ?", userID).Find(&keys).Error
	return keys, err
}

// GetSupportedExchanges fetches all active exchanges configured in the system.
func (db *DB) GetSupportedExchanges() ([]SupportedExchange, error) {
	var exchanges []SupportedExchange
	err := db.conn.Where("is_active = ?", true).Order("name ASC").Find(&exchanges).Error
	return exchanges, err
}

// SaveAPIKey saves a user's API key. Encryption should be done by the caller using Encrypt() beforehand.
func (db *DB) SaveAPIKey(key *UserAPIKey) error {
	return db.conn.Save(key).Error
}

// DeleteAPIKey removes an API key from the database.
func (db *DB) DeleteAPIKey(userID uint, exchangeName string) error {
	return db.conn.Where("user_id = ? AND exchange = ?", userID, exchangeName).Delete(&UserAPIKey{}).Error
}

// UpdateAPIKeyStatus toggles the activation state of an API key.
func (db *DB) UpdateAPIKeyStatus(userID uint, exchangeName string, isActive bool) error {
	return db.conn.Model(&UserAPIKey{}).Where("user_id = ? AND exchange = ?", userID, exchangeName).Update("is_active", isActive).Error
}

// DefaultUserThreshold returns the standard recommended strategy & risk configuration for a user.
func DefaultUserThreshold(userID uint, strategyName string) *UserThreshold {
	if strategyName == "" {
		strategyName = "PremiumStrategy"
	}
	return &UserThreshold{
		UserID:                 userID,
		StrategyName:           strategyName,
		Enabled:                true,
		MinTargetProfit:        1.5,
		MaxStopLoss:            -5.0,
		MinTargetProfitPercent: 1.5,
		MaxStopLossPercent:     -5.0,
		UsePercentParams:       true,
		MaxAcceptableSpread:    0.5,
		CapitalLimitUsd:        50000.0,
		PerPositionLimitUsd:    10000.0,
		MaxRiskScore:           80.0,
		RiskMultiplier:         1.0,
		BalanceSkewThreshold:   0.15,
		RebalanceTriggerSkew:   0.20,
		RebalanceTargetSkew:    0.50,
		RebalanceMaxPremium:    0.0,
		RetryOnFailure:         true,
	}
}

// LoadThresholds retrieves the strategy settings threshold, auto-seeding defaults if none exist yet.
func (db *DB) LoadThresholds(userID uint, strategyName string) (*UserThreshold, error) {
	if strategyName == "" {
		strategyName = "PremiumStrategy"
	}
	var threshold UserThreshold
	err := db.conn.Where("user_id = ? AND strategy_name = ?", userID, strategyName).First(&threshold).Error
	if err != nil {
		def := DefaultUserThreshold(userID, strategyName)
		if saveErr := db.conn.Create(def).Error; saveErr == nil {
			return def, nil
		}
		return def, nil
	}
	return &threshold, nil
}

// SaveThresholds updates or creates a strategy tuning configuration.
func (db *DB) SaveThresholds(threshold *UserThreshold) error {
	return db.conn.Save(threshold).Error
}

// SavePosition inserts a newly opened position into the database.
func (db *DB) SavePosition(pos *ActivePosition) error {
	return db.conn.Create(pos).Error
}

// UpdatePositionStatus modifies the live status (e.g., "CLOSED")
func (db *DB) UpdatePositionStatus(positionID string, status string) error {
	if status == "CLOSED" {
		return db.conn.Model(&ActivePosition{}).Where("position_id = ?", positionID).Update("status", status).Error
	}
	// Prevent transitioning a CLOSED position back to an active state
	return db.conn.Model(&ActivePosition{}).
		Where("position_id = ? AND status != ?", positionID, "CLOSED").
		Update("status", status).Error
}

// ResetAllActivePositions closes all non-closed active positions in the database.
func (db *DB) ResetAllActivePositions() error {
	return db.conn.Model(&ActivePosition{}).Where("status != ?", "CLOSED").Update("status", "CLOSED").Error
}

// UpdatePositionMaxROI modifies the peak unrealized ROI for trailing stops
func (db *DB) UpdatePositionMaxROI(positionID string, maxROI float64) error {
	return db.conn.Model(&ActivePosition{}).Where("position_id = ?", positionID).Update("max_unrealized_roi", maxROI).Error
}

// UpdatePositionType updates the strategy type and hedge market when dynamically switching between Spot and Futures legs.
func (db *DB) UpdatePositionType(positionID, newType, newHedgeMarket string) error {
	return db.conn.Model(&ActivePosition{}).
		Where("position_id = ? AND status != ?", positionID, "CLOSED").
		Updates(map[string]interface{}{
			"type":         newType,
			"hedge_market": newHedgeMarket,
		}).Error
}

// AddPositionAmount atomically increases the investment size and accumulated coins for Pyramiding (Scale In).
func (db *DB) AddPositionAmount(positionID string, additionalUsd, addedBase, addedForeign float64) error {
	return db.conn.Model(&ActivePosition{}).
		Where("position_id = ? AND status = ?", positionID, "OPEN").
		Updates(map[string]interface{}{
			"investment_usd": gorm.Expr("investment_usd + ?", additionalUsd),
			"base_amount":    gorm.Expr("base_amount + ?", addedBase),
			"foreign_amount": gorm.Expr("foreign_amount + ?", addedForeign),
		}).Error
}

// ReducePositionAmount atomically decreases the investment size and accumulated coins for Partial Exit (Scale Out).
func (db *DB) ReducePositionAmount(positionID string, reducedUsd, subtractedBase, subtractedForeign float64) error {
	return db.conn.Model(&ActivePosition{}).
		Where("position_id = ? AND status = ?", positionID, "OPEN").
		Updates(map[string]interface{}{
			"investment_usd": gorm.Expr("GREATEST(investment_usd - ?, 0)", reducedUsd),
			"base_amount":    gorm.Expr("GREATEST(base_amount - ?, 0)", subtractedBase),
			"foreign_amount": gorm.Expr("GREATEST(foreign_amount - ?, 0)", subtractedForeign),
		}).Error
}

// GetActivePositions fetches all active positions to resume tracking during server restart / failover.
// Filters tightly by nodeID to prevent kubernetes master replicas from duplicate-monitoring.
func (db *DB) GetActivePositions(nodeID string) ([]ActivePosition, error) {
	var positions []ActivePosition
	err := db.conn.Where("status IN ? AND monitor_node_id = ?", []string{"OPEN", "IN_TRANSIT", "TRANSFERRING", "REPAYING", "AWAITING_REPAY"}, nodeID).Find(&positions).Error
	return positions, err
}

// GetActivePositionsByUser fetches all active positions belonging to a specific user.
// Used by PersonalFilter to prevent duplicate entries on the same coin/exchange pairs.
func (db *DB) GetActivePositionsByUser(userID uint) ([]ActivePosition, error) {
	var positions []ActivePosition
	err := db.conn.Where("user_id = ? AND status IN ?", userID, []string{"OPEN", "IN_TRANSIT", "TRANSFERRING", "REPAYING", "AWAITING_REPAY"}).Find(&positions).Error
	return positions, err
}

// GetActivePositionsByUserAndMode fetches active positions filtered by Paper vs Live mode.
func (db *DB) GetActivePositionsByUserAndMode(userID uint, isPaper bool) ([]ActivePosition, error) {
	var positions []ActivePosition
	err := db.conn.Where("user_id = ? AND is_paper = ? AND status IN ?", userID, isPaper, []string{"OPEN", "IN_TRANSIT", "TRANSFERRING", "REPAYING", "AWAITING_REPAY"}).Find(&positions).Error
	return positions, err
}

// GetClosedPositionsByUser fetches all closed/historical positions belonging to a specific user.
func (db *DB) GetClosedPositionsByUser(userID uint) ([]ActivePosition, error) {
	var positions []ActivePosition
	err := db.conn.Where("user_id = ? AND status = ?", userID, "CLOSED").Order("opened_at desc").Find(&positions).Error
	return positions, err
}

// GetClosedPositionsByUserAndMode fetches closed/historical positions filtered by Paper vs Live mode.
func (db *DB) GetClosedPositionsByUserAndMode(userID uint, isPaper bool) ([]ActivePosition, error) {
	var positions []ActivePosition
	err := db.conn.Where("user_id = ? AND is_paper = ? AND status = ?", userID, isPaper, "CLOSED").Order("opened_at desc").Find(&positions).Error
	return positions, err
}

// CloseAllActivePositionsByUserAndMode marks all open positions for a user and mode as CLOSED.
func (db *DB) CloseAllActivePositionsByUserAndMode(userID uint, isPaper bool) error {
	return db.conn.Model(&ActivePosition{}).
		Where("user_id = ? AND is_paper = ? AND status != ?", userID, isPaper, "CLOSED").
		Update("status", "CLOSED").Error
}

// GetPositionByID fetches a position record by its unique positionID
func (db *DB) GetPositionByID(positionID string) (*ActivePosition, error) {
	var pos ActivePosition
	err := db.conn.Where("position_id = ?", positionID).First(&pos).Error
	if err != nil {
		return nil, err
	}
	return &pos, nil
}

// CountActivePositions sums up any active positional exposure bound to a specific exchange.
func (db *DB) CountActivePositions(exchangeName string) (int64, error) {
	var count int64
	err := db.conn.Model(&ActivePosition{}).Where("status IN ? AND (base_exchange = ? OR foreign_exchange = ?)", []string{"OPEN", "IN_TRANSIT", "TRANSFERRING", "REPAYING"}, exchangeName, exchangeName).Count(&count).Error
	return count, err
}

// AcquireLeaderLease attempts to claim or renew a lock for a given resource (e.g. "influxdb_kr").
// It returns true if the lock was successfully acquired or renewed by the calling node.
func (db *DB) AcquireLeaderLease(lockName, nodeID string, ttl time.Duration) (bool, error) {
	now := time.Now()
	expiresAt := now.Add(ttl)

	// We use raw SQL with ON CONFLICT to act as an atomic Test-And-Set operation.
	// If the lock does not exist, insert it.
	// If it exists, but expired, overwrite it.
	// If it exists, and belongs to us, renew it.
	// Otherwise (belongs to someone else and alive), do nothing.

	query := `
		INSERT INTO leader_leases (lock_name, node_id, expires_at)
		VALUES (?, ?, ?)
		ON CONFLICT (lock_name) DO UPDATE
		SET node_id = EXCLUDED.node_id, expires_at = EXCLUDED.expires_at
		WHERE leader_leases.expires_at < ? OR leader_leases.node_id = ?
	`
	
	result := db.conn.Exec(query, lockName, nodeID, expiresAt, now, nodeID)
	if result.Error != nil {
		return false, result.Error
	}

	// RowsAffected == 1 means we successfully wrote the lock (either inserted or updated).
	// RowsAffected == 0 means the condition failed (someone else holds a valid lock).
	return result.RowsAffected > 0, nil
}

// PublishUserEvent sends an asynchronous JSON payload to only the specific nodes
// currently holding a WebSocket connection for this user (Targeted Pub/Sub).
func (db *DB) PublishUserEvent(userID string, payload string) error {
	channel := "user_" + userID + "_events"
	// Quote the payload string for safe raw SQL execution
	query := `SELECT pg_notify(?, ?)`
	return db.conn.Exec(query, channel, payload).Error
}
