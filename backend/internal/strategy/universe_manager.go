/*
Package strategy
universe_manager.go implements DynamicUniverseManager, which discovers all overlapping altcoin
pairs across connected domestic and foreign exchanges without hardcoded ticker lists.
It also supports zero-code runtime hot-reloading when a NEW_KRW_LISTING announcement arrives,
automatically synthesizing pre-listing pairs if the coin is already tradable on foreign exchanges
before domestic trading opens.
*/
package strategy

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/magnolia/backend/internal/db/redis"
	"github.com/magnolia/backend/internal/exchange"
)

// PriorityAltcoins defines the high-activity arbitrage & major altcoin priority order
// so that when a universe cap is configured, the most active Kimp/Gaduri/Transfer coins are prioritized first,
// right after any coins with live catalyst announcements.
var PriorityAltcoins = []string{
	"BTC", "ETH", "SOL", "XRP", "DOGE", "SUI", "SEI", "AVAX", "LINK", "NEAR",
	"APT", "ARB", "OP", "DOT", "ADA", "SHIB", "PEPE", "ONDO", "RENDER", "FET",
	"AAVE", "UNI", "POL", "INJ", "TIA", "STX", "SAND", "MANA", "IMX", "ZETA",
	"BLUR", "GMT", "ORCA", "PYTH", "JTO", "BONK", "MEW", "DRIFT", "ZRO", "ENA",
	"PENDLE", "EIGEN", "TAO", "WLD", "ARKM", "STRK", "ZK", "ORDI", "FIL", "ATOM",
}

// IsAllCoinsFilter returns true if TARGET_COINS is empty, "*", or "ALL".
func IsAllCoinsFilter(targetCoinsEnv string) bool {
	cleaned := strings.ToUpper(strings.TrimSpace(targetCoinsEnv))
	return cleaned == "" || cleaned == "ALL" || cleaned == "*"
}

// ParseMaxUniverseCoins parses MAX_UNIVERSE_COINS env string (defaults to 30 if unset/invalid, 0 = unlimited).
func ParseMaxUniverseCoins(envVal string) int {
	cleaned := strings.TrimSpace(envVal)
	if cleaned == "" {
		return 30
	}
	if strings.EqualFold(cleaned, "ALL") || cleaned == "0" {
		return 0
	}
	n, err := strconv.Atoi(cleaned)
	if err != nil || n < 0 {
		return 30
	}
	return n
}

// DynamicUniverseManager manages the live set of overlapping CrossMarketPairs across all active exchanges,
// supporting dynamic runtime expansion for newly listed altcoins.
type DynamicUniverseManager struct {
	mu               sync.RWMutex
	ranker           *GlobalRawRanker
	redisClient      *redis.PubSubClient
	clients          []exchange.Client
	targetCoinsEnv   string
	upbitQuoteEnv    string
	coinbaseQuoteEnv string
	maxCoins         int
	pairs            []CrossMarketPair
	activeCoins      []string
	forcedCoins      map[string]bool // Coins dynamically injected via NEW_KRW_LISTING / catalyst events
}

// NewDynamicUniverseManager creates a manager for discovering and refreshing tradable cross-exchange pairs.
func NewDynamicUniverseManager(
	ranker *GlobalRawRanker,
	redisClient *redis.PubSubClient,
	clients []exchange.Client,
	targetCoinsEnv, upbitQuoteEnv, coinbaseQuoteEnv string,
	maxCoins int,
) *DynamicUniverseManager {
	if upbitQuoteEnv == "" {
		upbitQuoteEnv = "KRW"
	}
	if coinbaseQuoteEnv == "" {
		coinbaseQuoteEnv = "USD"
	}
	return &DynamicUniverseManager{
		ranker:           ranker,
		redisClient:      redisClient,
		clients:          clients,
		targetCoinsEnv:   targetCoinsEnv,
		upbitQuoteEnv:    strings.ToUpper(strings.TrimSpace(upbitQuoteEnv)),
		coinbaseQuoteEnv: strings.ToUpper(strings.TrimSpace(coinbaseQuoteEnv)),
		maxCoins:         maxCoins,
		forcedCoins:      make(map[string]bool),
	}
}

// RefreshUniverse queries all connected exchanges, computes overlapping pairs, injects any pre-listing
// catalyst coins, applies quote/priority filtering, and publishes the updated universe to Redis.
func (m *DynamicUniverseManager) RefreshUniverse(ctx context.Context) ([]CrossMarketPair, []string) {
	var rawPairs []CrossMarketPair
	for i, base := range m.clients {
		for j, foreign := range m.clients {
			if i != j {
				pairs, err := m.ranker.FindIntersections(ctx, base, foreign)
				if err == nil && len(pairs) > 0 {
					rawPairs = append(rawPairs, pairs...)
				}
			}
		}
	}

	m.mu.RLock()
	forcedSnapshot := make(map[string]bool, len(m.forcedCoins))
	for k, v := range m.forcedCoins {
		forcedSnapshot[k] = v
	}
	m.mu.RUnlock()

	// Synthesize pre-listing pairs for newly announced coins (NEW_KRW_LISTING) that are already listed on
	// foreign exchanges even if the domestic exchange has not yet enabled trading in /v1/market/all.
	rawPairs = m.appendPreListingPairs(ctx, rawPairs, forcedSnapshot)

	// Filter by quote currency and explicit targetCoins (if not ALL)
	explicitCoins := make(map[string]bool)
	isAll := IsAllCoinsFilter(m.targetCoinsEnv)
	if !isAll {
		for _, tc := range strings.Split(m.targetCoinsEnv, ",") {
			c := strings.ToUpper(strings.TrimSpace(tc))
			if c != "" {
				explicitCoins[c] = true
			}
		}
		for fc := range forcedSnapshot {
			explicitCoins[fc] = true
		}
	}

	var quoteFiltered []CrossMarketPair
	coinSet := make(map[string]bool)
	for _, pair := range rawPairs {
		coin := strings.ToUpper(string(pair.BaseCurrency))
		if !isAll && !explicitCoins[coin] {
			continue
		}
		if !m.matchesQuoteFilter(pair) {
			continue
		}
		quoteFiltered = append(quoteFiltered, pair)
		coinSet[coin] = true
	}

	// Rank coins by priority: 1) Forced/Catalyst event coins, 2) PriorityAltcoins, 3) Alphabetical
	sortedCoins := prioritizeCoins(coinSet, forcedSnapshot)
	if isAll && m.maxCoins > 0 && len(sortedCoins) > m.maxCoins {
		sortedCoins = sortedCoins[:m.maxCoins]
	}
	allowedCoins := make(map[string]bool, len(sortedCoins))
	for _, c := range sortedCoins {
		allowedCoins[c] = true
	}

	var finalPairs []CrossMarketPair
	symbolSet := make(map[string]bool)
	for _, pair := range quoteFiltered {
		coin := strings.ToUpper(string(pair.BaseCurrency))
		if allowedCoins[coin] {
			finalPairs = append(finalPairs, pair)
			symbolSet[string(pair.BaseMarketInfo.Symbol)] = true
			symbolSet[string(pair.ForeignMarketInfo.Symbol)] = true
		}
	}

	var allSymbols []string
	for s := range symbolSet {
		allSymbols = append(allSymbols, s)
	}
	sort.Strings(allSymbols)

	m.mu.Lock()
	m.pairs = finalPairs
	m.activeCoins = sortedCoins
	m.mu.Unlock()

	if m.redisClient != nil {
		_ = m.redisClient.PublishActiveUniverse(ctx, sortedCoins, allSymbols)
	}

	return finalPairs, sortedCoins
}

// InjectPreListingCoin registers a newly announced coin symbol (e.g. from NEW_KRW_LISTING) into the universe
// and immediately refreshes the active pairs without requiring any code deployment or restart.
func (m *DynamicUniverseManager) InjectPreListingCoin(ctx context.Context, coinSymbol string) ([]CrossMarketPair, bool) {
	coin := strings.ToUpper(strings.Split(strings.TrimSpace(coinSymbol), "-")[0])
	if coin == "" {
		return m.GetPairs(), false
	}
	m.mu.Lock()
	alreadyForced := m.forcedCoins[coin]
	m.forcedCoins[coin] = true
	m.mu.Unlock()

	pairs, _ := m.RefreshUniverse(ctx)
	return pairs, !alreadyForced
}

// GetPairs returns a thread-safe snapshot of the current CrossMarketPairs.
func (m *DynamicUniverseManager) GetPairs() []CrossMarketPair {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]CrossMarketPair, len(m.pairs))
	copy(out, m.pairs)
	return out
}

// GetActiveCoins returns a thread-safe snapshot of active base coin tickers.
func (m *DynamicUniverseManager) GetActiveCoins() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.activeCoins))
	copy(out, m.activeCoins)
	return out
}

// appendPreListingPairs synthesizes CrossMarketPair entries for forced pre-listing coins if they exist on the
// foreign exchange but are not yet open on the domestic exchange.
func (m *DynamicUniverseManager) appendPreListingPairs(
	ctx context.Context,
	existing []CrossMarketPair,
	forced map[string]bool,
) []CrossMarketPair {
	if len(forced) == 0 {
		return existing
	}

	presentCoins := make(map[string]bool)
	for _, p := range existing {
		presentCoins[strings.ToUpper(string(p.BaseCurrency))] = true
	}

	var missingForced []string
	for coin := range forced {
		if !presentCoins[coin] {
			missingForced = append(missingForced, coin)
		}
	}
	if len(missingForced) == 0 {
		return existing
	}

	// Check if foreign exchanges list the missing forced coin
	var domesticClients []exchange.Client
	var foreignClients []exchange.Client
	for _, c := range m.clients {
		name := strings.ToUpper(c.GetName())
		if name == "UPBIT" || name == "BITHUMB" {
			domesticClients = append(domesticClients, c)
		} else {
			foreignClients = append(foreignClients, c)
		}
	}

	for _, fClient := range foreignClients {
		fMarkets, err := fClient.GetMarkets(ctx)
		if err != nil {
			continue
		}
		fByCoin := make(map[string]exchange.MarketInfo)
		for _, fm := range fMarkets {
			if fm.IsTradingEnabled {
				fByCoin[strings.ToUpper(string(fm.BaseCurrency))] = fm
			}
		}

		for _, coin := range missingForced {
			fm, ok := fByCoin[coin]
			if !ok {
				continue
			}
			var fNet exchange.NetworkInfo
			if nets, err := fClient.GetSupportedNetworks(ctx, exchange.Currency(coin)); err == nil && len(nets) > 0 {
				fNet = nets[0]
			} else {
				fNet = exchange.NetworkInfo{
					Network:          exchange.Network(coin),
					ExchangeNetID:    coin,
					IsDepositEnable:  true,
					IsWithdrawEnable: true,
					EstimatedTime:    180,
				}
			}

			for _, dClient := range domesticClients {
				domQuote := exchange.Currency(m.upbitQuoteEnv)
				domSym := exchange.FormatSymbol(exchange.Currency(coin), domQuote)
				bm := exchange.MarketInfo{
					Symbol:            domSym,
					BaseCurrency:      exchange.Currency(coin),
					QuoteCurrency:     domQuote,
					MarketType:        "SPOT",
					IsTradingEnabled:  true, // Pre-staged for upcoming KRW listing
					IsDepositEnabled:  true,
					IsWithdrawEnabled: true,
				}
				bNet := exchange.NetworkInfo{
					Network:          fNet.Network,
					ExchangeNetID:    fNet.ExchangeNetID,
					IsDepositEnable:  true,
					IsWithdrawEnable: true,
					ContractAddress:  fNet.ContractAddress,
					RequiresMemo:     fNet.RequiresMemo,
					EstimatedTime:    fNet.EstimatedTime,
				}
				existing = append(existing, CrossMarketPair{
					BaseCurrency:       exchange.Currency(coin),
					BaseClient:         dClient,
					BaseMarketInfo:     bm,
					BaseNetwork:        bNet,
					ForeignClient:      fClient,
					ForeignMarketInfo:  fm,
					ForeignNetwork:     fNet,
					IsValidForTransfer: true,
				})
			}
		}
	}

	return existing
}

func (m *DynamicUniverseManager) matchesQuoteFilter(pair CrossMarketPair) bool {
	if pair.BaseClient.GetName() == "UPBIT" && m.upbitQuoteEnv != "" {
		if strings.ToUpper(string(pair.BaseMarketInfo.QuoteCurrency)) != m.upbitQuoteEnv {
			return false
		}
	}
	if pair.ForeignClient.GetName() == "UPBIT" && m.upbitQuoteEnv != "" {
		if strings.ToUpper(string(pair.ForeignMarketInfo.QuoteCurrency)) != m.upbitQuoteEnv {
			return false
		}
	}
	if pair.BaseClient.GetName() == "COINBASE" && m.coinbaseQuoteEnv != "" {
		if strings.ToUpper(string(pair.BaseMarketInfo.QuoteCurrency)) != m.coinbaseQuoteEnv {
			return false
		}
	}
	if pair.ForeignClient.GetName() == "COINBASE" && m.coinbaseQuoteEnv != "" {
		if strings.ToUpper(string(pair.ForeignMarketInfo.QuoteCurrency)) != m.coinbaseQuoteEnv {
			return false
		}
	}
	return true
}

func prioritizeCoins(coinSet map[string]bool, forced map[string]bool) []string {
	priorityRank := make(map[string]int, len(PriorityAltcoins))
	for idx, c := range PriorityAltcoins {
		priorityRank[c] = idx + 1
	}

	coins := make([]string, 0, len(coinSet))
	for c := range coinSet {
		coins = append(coins, c)
	}

	sort.Slice(coins, func(i, j int) bool {
		ci, cj := coins[i], coins[j]
		fi, fj := forced[ci], forced[cj]
		if fi != fj {
			return fi
		}
		ri, okI := priorityRank[ci]
		rj, okJ := priorityRank[cj]
		if okI && okJ {
			return ri < rj
		}
		if okI != okJ {
			return okI
		}
		return ci < cj
	})

	return coins
}
