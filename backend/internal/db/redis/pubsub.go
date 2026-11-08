/*
Package redis
This file is a client for distributed backend nodes to synchronize at high speeds via the Redis Pub/Sub system.
It allows the Feed Handler to publish public market data and enables individual Node instances to subscribe to those channels and forward real-time tick data to the analysis engine.
*/
package redis

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/magnolia/backend/internal/exchange"
	redisclient "github.com/redis/go-redis/v9"
)

const (
	TickerChannelPrefix = "ticker_"
)

type PubSubClient struct {
	client *redisclient.Client
}

func NewPubSubClient(redisURL string) *PubSubClient {
	opt, err := redisclient.ParseURL(redisURL)
	if err != nil {
		log.Fatalf("Failed to parse Redis URL: %v", err)
	}

	client := redisclient.NewClient(opt)
	return &PubSubClient{
		client: client,
	}
}

// PublishTicker broadcasts a new PriceUpdate to the cluster.
func (p *PubSubClient) PublishTicker(ctx context.Context, update exchange.PriceUpdate) error {
	channel := TickerChannelPrefix + string(update.Symbol)
	// Simple JSON marshaling for fast network transport
	payload, err := json.Marshal(update)
	if err != nil {
		return err
	}
	return p.client.Publish(ctx, channel, payload).Err()
}

// SubscribeTickers listens to the specified symbols and pipes them into a standard PriceUpdate channel.
// If symbols is empty or contains "*" or "ALL", it uses PSubscribe("ticker_*") to dynamically receive all coins,
// including newly listed altcoins added at runtime without restarting the subscriber.
func (p *PubSubClient) SubscribeTickers(ctx context.Context, symbols []exchange.Symbol, outCh chan<- exchange.PriceUpdate) {
	useWildcard := len(symbols) == 0
	for _, s := range symbols {
		u := strings.ToUpper(strings.TrimSpace(string(s)))
		if u == "*" || u == "ALL" || strings.HasPrefix(u, "ALL-") {
			useWildcard = true
			break
		}
	}

	var pubsub *redisclient.PubSub
	if useWildcard {
		pubsub = p.client.PSubscribe(ctx, TickerChannelPrefix+"*")
	} else {
		channels := make([]string, len(symbols))
		for i, sym := range symbols {
			channels[i] = TickerChannelPrefix + string(sym)
		}
		pubsub = p.client.Subscribe(ctx, channels...)
	}
	defer pubsub.Close()

	ch := pubsub.Channel()

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			if msg == nil {
				continue
			}
			var update exchange.PriceUpdate
			if err := json.Unmarshal([]byte(msg.Payload), &update); err != nil {
				log.Printf("[Redis] Failed to unmarshal ticker: %v", err)
				continue
			}
			outCh <- update
		}
	}
}

// PublishActiveUniverse caches the currently discovered overlapping coin tickers and exchange symbols in Redis.
func (p *PubSubClient) PublishActiveUniverse(ctx context.Context, coins []string, symbols []string) error {
	if coinsBytes, err := json.Marshal(coins); err == nil {
		_ = p.client.Set(ctx, "market:universe:coins", coinsBytes, 24*time.Hour).Err()
	}
	if symsBytes, err := json.Marshal(symbols); err == nil {
		_ = p.client.Set(ctx, "market:universe:symbols", symsBytes, 24*time.Hour).Err()
	}
	return nil
}

// GetActiveUniverseCoins retrieves the cached list of active base coin tickers (e.g. ["BTC", "ETH", "SOL", ...]).
func (p *PubSubClient) GetActiveUniverseCoins(ctx context.Context) []string {
	raw, err := p.client.Get(ctx, "market:universe:coins").Bytes()
	if err != nil || len(raw) == 0 {
		return nil
	}
	var coins []string
	if err := json.Unmarshal(raw, &coins); err != nil {
		return nil
	}
	return coins
}

// PublishFXRate broadcasts a normalized FX rate for a given quote/base pair to the cluster.
func (p *PubSubClient) PublishFXRate(ctx context.Context, base, quote string, rate float64) error {
	channel := "fx:" + base + "-" + quote
	_ = p.client.Set(ctx, channel+":latest", rate, 24*time.Hour).Err()
	return p.client.Publish(ctx, channel, rate).Err()
}

// GetCachedFXRate returns the latest cached FX rate from Redis if available.
func (p *PubSubClient) GetCachedFXRate(ctx context.Context, base, quote string) (float64, error) {
	channel := "fx:" + base + "-" + quote + ":latest"
	return p.client.Get(ctx, channel).Float64()
}

// PublishExchangeTetherRate broadcasts and caches the real-time USDT-KRW rate for a specific domestic exchange (e.g., "UPBIT", "BITHUMB").
func (p *PubSubClient) PublishExchangeTetherRate(ctx context.Context, exchangeName string, rate float64) error {
	ex := strings.ToUpper(strings.TrimSpace(exchangeName))
	if ex == "" {
		ex = "UPBIT"
	}
	key := "fx:" + ex + ":USDT-KRW"
	_ = p.client.Set(ctx, key+":latest", rate, 24*time.Hour).Err()
	_ = p.client.HSet(ctx, "market:tether_rates:krw", ex, rate).Err()
	payload, _ := json.Marshal(map[string]interface{}{
		"exchange": ex,
		"rate":     rate,
	})
	return p.client.Publish(ctx, "fx:tether_krw", payload).Err()
}

// GetAllExchangeTetherRates returns a map of domestic exchange -> USDT-KRW rate.
func (p *PubSubClient) GetAllExchangeTetherRates(ctx context.Context) map[string]float64 {
	res := make(map[string]float64)
	raw, err := p.client.HGetAll(ctx, "market:tether_rates:krw").Result()
	if err != nil {
		return res
	}
	for k, v := range raw {
		var f float64
		if err := json.Unmarshal([]byte(v), &f); err == nil && f > 0 {
			res[strings.ToUpper(k)] = f
		}
	}
	return res
}

// SubscribeExchangeTetherRates listens for per-exchange USDT-KRW rate updates.
func (p *PubSubClient) SubscribeExchangeTetherRates(ctx context.Context, onUpdate func(exchangeName string, rate float64)) {
	if existing := p.GetAllExchangeTetherRates(ctx); len(existing) > 0 {
		for ex, r := range existing {
			onUpdate(ex, r)
		}
	}
	pubsub := p.client.Subscribe(ctx, "fx:tether_krw")
	defer pubsub.Close()
	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			var parsed struct {
				Exchange string  `json:"exchange"`
				Rate     float64 `json:"rate"`
			}
			if err := json.Unmarshal([]byte(msg.Payload), &parsed); err == nil && parsed.Rate > 0 {
				onUpdate(strings.ToUpper(parsed.Exchange), parsed.Rate)
			}
		}
	}
}

// PublishAnnouncementEvent stores and broadcasts an LLM-classified exchange announcement / catalyst event.
func (p *PubSubClient) PublishAnnouncementEvent(ctx context.Context, evt exchange.AnnouncementEvent) error {
	payload, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	for _, sym := range evt.Symbols {
		coin := strings.ToUpper(strings.Split(strings.TrimSpace(sym), "-")[0])
		if coin == "" {
			continue
		}
		coinKey := "market:events:coin:" + coin
		_ = p.client.LPush(ctx, coinKey, payload).Err()
		_ = p.client.LTrim(ctx, coinKey, 0, 19).Err()
		_ = p.client.Expire(ctx, coinKey, 48*time.Hour).Err()
	}
	_ = p.client.LPush(ctx, "market:events:recent", payload).Err()
	_ = p.client.LTrim(ctx, "market:events:recent", 0, 49).Err()
	_ = p.client.Expire(ctx, "market:events:recent", 48*time.Hour).Err()
	return p.client.Publish(ctx, "event:announcement", payload).Err()
}

// GetCoinAnnouncementEvents returns recent LLM-classified announcement events for a specific coin ticker (e.g. "BTC", "SOL").
func (p *PubSubClient) GetCoinAnnouncementEvents(ctx context.Context, coin string) []exchange.AnnouncementEvent {
	cleanCoin := strings.ToUpper(strings.Split(strings.TrimSpace(coin), "-")[0])
	if cleanCoin == "" {
		return nil
	}
	rawList, err := p.client.LRange(ctx, "market:events:coin:"+cleanCoin, 0, 19).Result()
	if err != nil || len(rawList) == 0 {
		return nil
	}
	var events []exchange.AnnouncementEvent
	seen := make(map[string]bool)
	for _, item := range rawList {
		var evt exchange.AnnouncementEvent
		if err := json.Unmarshal([]byte(item), &evt); err == nil {
			dedupKey := evt.ID + ":" + evt.EventType
			if !seen[dedupKey] {
				seen[dedupKey] = true
				events = append(events, evt)
			}
		}
	}
	return events
}

// ClearCoinAnnouncementEvents deletes stored announcement events for a coin ticker (or all if symbol == "").
func (p *PubSubClient) ClearCoinAnnouncementEvents(ctx context.Context, coin string) error {
	cleanCoin := strings.ToUpper(strings.Split(strings.TrimSpace(coin), "-")[0])
	if cleanCoin != "" {
		_ = p.client.Del(ctx, "market:events:coin:"+cleanCoin).Err()
	}
	return p.client.Del(ctx, "market:events:recent").Err()
}

// SubscribeAnnouncementEvents subscribes to live LLM-classified exchange announcement events.
func (p *PubSubClient) SubscribeAnnouncementEvents(ctx context.Context, outCh chan<- exchange.AnnouncementEvent) {
	pubsub := p.client.Subscribe(ctx, "event:announcement")
	defer pubsub.Close()
	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			var evt exchange.AnnouncementEvent
			if err := json.Unmarshal([]byte(msg.Payload), &evt); err == nil {
				outCh <- evt
			}
		}
	}
}

// SubscribeFXRate listens for updates on a specific currency pair (e.g., "USD", "KRW") 
// and sends the new float64 rate into the outCh.
func (p *PubSubClient) SubscribeFXRate(ctx context.Context, base, quote string, outCh chan<- float64) {
	channel := "fx:" + base + "-" + quote
	pubsub := p.client.Subscribe(ctx, channel)
	defer pubsub.Close()

	ch := pubsub.Channel()

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			var rate float64
			if err := json.Unmarshal([]byte(msg.Payload), &rate); err != nil {
				log.Printf("[Redis] Failed to unmarshal FX rate: %v", err)
				continue
			}
			outCh <- rate
		}
	}
}

// PushDanglingClient adds a user to the regional queue for an engine node to pick up.
func (p *PubSubClient) PushDanglingClient(ctx context.Context, region string, userID string) error {
	normRegion := strings.ToLower(strings.TrimSpace(region))
	if normRegion == "" {
		normRegion = "kr"
	}
	queueName := "queue:dangling_clients:" + normRegion
	return p.client.LPush(ctx, queueName, userID).Err()
}

// PopDanglingClient blocks until a user is available in the regional queue, then pops and returns it.
func (p *PubSubClient) PopDanglingClient(ctx context.Context, region string) (string, error) {
	normRegion := strings.ToLower(strings.TrimSpace(region))
	if normRegion == "" {
		normRegion = "kr"
	}
	queueName := "queue:dangling_clients:" + normRegion
	// BRPop blocks until an element is available or timeout (0 = block indefinitely)
	result, err := p.client.BRPop(ctx, 0, queueName).Result()
	if err != nil {
		return "", err
	}
	// result is a slice: [queueName, value]
	if len(result) == 2 {
		return result[1], nil
	}
	return "", nil
}

// PublishRawOpportunities broadcasts raw opportunity calculations from the Global Ranker.
func (p *PubSubClient) PublishRawOpportunities(ctx context.Context, payload []byte) error {
	_ = p.client.Set(ctx, "broadcast:opportunities:raw:latest", payload, 2*time.Minute).Err()
	return p.client.Publish(ctx, "broadcast:opportunities:raw", payload).Err()
}

// SubscribeRawOpportunities listens for global raw opportunity data broadcasts.
func (p *PubSubClient) SubscribeRawOpportunities(ctx context.Context, outCh chan<- []byte) {
	if cached, err := p.client.Get(ctx, "broadcast:opportunities:raw:latest").Bytes(); err == nil && len(cached) > 0 {
		select {
		case outCh <- cached:
		default:
		}
	}
	pubsub := p.client.Subscribe(ctx, "broadcast:opportunities:raw")
	defer pubsub.Close()
	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			outCh <- []byte(msg.Payload)
		}
	}
}

// SubscribeAIPredictedOpportunities listens for AI predictions output from the Python inference daemon.
func (p *PubSubClient) SubscribeAIPredictedOpportunities(ctx context.Context, outCh chan<- []byte) {
	pubsub := p.client.Subscribe(ctx, "broadcast:opportunities:ai_predicted")
	defer pubsub.Close()
	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			outCh <- []byte(msg.Payload)
		}
	}
}

// PublishUserOpportunities sends filtered opportunity lists to a specific user's dashboard.
func (p *PubSubClient) PublishUserOpportunities(ctx context.Context, userID string, payload []byte) error {
	channel := "user:" + userID + ":opportunities"
	return p.client.Publish(ctx, channel, payload).Err()
}

// PublishUserOpportunitiesByMode sends filtered opportunities for a specific mode ("paper" or "live").
func (p *PubSubClient) PublishUserOpportunitiesByMode(ctx context.Context, userID, mode string, payload []byte) error {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "paper"
	}
	channel := "user:" + userID + ":opportunities:" + mode
	_ = p.client.Set(ctx, channel+":latest", payload, 2*time.Minute).Err()
	_ = p.PublishUserOpportunities(ctx, userID, payload)
	return p.client.Publish(ctx, channel, payload).Err()
}

// SubscribeUserOpportunities listens for filtered opportunities targeted at a specific user.
func (p *PubSubClient) SubscribeUserOpportunities(ctx context.Context, userID string, outCh chan<- []byte) {
	channel := "user:" + userID + ":opportunities"
	pubsub := p.client.Subscribe(ctx, channel)
	defer pubsub.Close()
	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			outCh <- []byte(msg.Payload)
		}
	}
}

// SubscribeUserOpportunitiesByMode listens for filtered opportunities for a specific mode ("paper" or "live").
func (p *PubSubClient) SubscribeUserOpportunitiesByMode(ctx context.Context, userID, mode string, outCh chan<- []byte) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "paper"
	}
	channel := "user:" + userID + ":opportunities:" + mode
	if cached, err := p.client.Get(ctx, channel+":latest").Bytes(); err == nil && len(cached) > 0 {
		select {
		case outCh <- cached:
		default:
		}
	}
	pubsub := p.client.Subscribe(ctx, channel)
	defer pubsub.Close()
	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			outCh <- []byte(msg.Payload)
		}
	}
}

// PublishUserState broadcasts the customized account metrics to python daemon and caches the latest snapshot for SSE.
func (p *PubSubClient) PublishUserState(ctx context.Context, userID string, payload []byte) error {
	channel := "user:" + userID + ":state"
	_ = p.client.Set(ctx, channel+":latest", payload, 5*time.Minute).Err()
	return p.client.Publish(ctx, channel, payload).Err()
}

// PublishUserStateByMode broadcasts and caches account state for a specific mode ("paper" or "live").
// Mode channels ("user:{id}:state:paper" and "user:{id}:state:live") are strictly isolated so that
// a STOPPED engine in one mode never overwrites active position state in the other mode.
func (p *PubSubClient) PublishUserStateByMode(ctx context.Context, userID, mode string, payload []byte) error {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "paper"
	}
	modeChannel := "user:" + userID + ":state:" + mode
	_ = p.client.Set(ctx, modeChannel+":latest", payload, 5*time.Minute).Err()
	if strings.Contains(string(payload), `"bot_status":"RUNNING"`) || mode == "paper" {
		_ = p.client.Set(ctx, "user:"+userID+":state:latest", payload, 5*time.Minute).Err()
	}
	return p.client.Publish(ctx, modeChannel, payload).Err()
}

// GetLatestUserState retrieves the latest cached account state snapshot for a user.
func (p *PubSubClient) GetLatestUserState(ctx context.Context, userID string) ([]byte, error) {
	key := "user:" + userID + ":state:latest"
	val, err := p.client.Get(ctx, key).Bytes()
	if err != nil {
		return nil, err
	}
	return val, nil
}

// GetLatestUserStateByMode retrieves the latest cached account state for a user and mode ("paper" or "live").
func (p *PubSubClient) GetLatestUserStateByMode(ctx context.Context, userID, mode string) ([]byte, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "paper"
	}
	key := "user:" + userID + ":state:" + mode + ":latest"
	val, err := p.client.Get(ctx, key).Bytes()
	if err != nil {
		return p.GetLatestUserState(ctx, userID)
	}
	return val, nil
}

// PublishUserCommand dispatches a manual execution command (e.g. FORCE_ENTER, FORCE_CLOSE) to the user's Engine goroutine.
func (p *PubSubClient) PublishUserCommand(ctx context.Context, userID string, payload []byte) error {
	channel := "user:" + userID + ":cmd"
	return p.client.Publish(ctx, channel, payload).Err()
}

// SubscribeUserCommands listens for manual execution commands targeted at a specific user's Engine goroutine.
func (p *PubSubClient) SubscribeUserCommands(ctx context.Context, userID string, outCh chan<- []byte) {
	channel := "user:" + userID + ":cmd"
	pubsub := p.client.Subscribe(ctx, channel)
	defer pubsub.Close()
	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			outCh <- []byte(msg.Payload)
		}
	}
}

// PublishUserEvents sends notifications (e.g. position updates) to a specific user.
func (p *PubSubClient) PublishUserEvents(ctx context.Context, userID string, payload []byte) error {
	channel := "user:" + userID + ":events"
	return p.client.Publish(ctx, channel, payload).Err()
}

// SubscribeUserEvents listens for system notifications and trading events for a user.
func (p *PubSubClient) SubscribeUserEvents(ctx context.Context, userID string, outCh chan<- []byte) {
	channel := "user:" + userID + ":events"
	pubsub := p.client.Subscribe(ctx, channel)
	defer pubsub.Close()
	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			outCh <- []byte(msg.Payload)
		}
	}
}

// GetPremiumChart retrieves the cached 24h historical + 8h AI forecast trajectory payload for a market pair.
func (p *PubSubClient) GetPremiumChart(ctx context.Context, baseEx, baseSym, foreignEx, foreignSym string) ([]byte, error) {
	key := "market:premium_chart:" + baseEx + ":" + baseSym + ":" + foreignEx + ":" + foreignSym
	return p.client.Get(ctx, key).Bytes()
}

// GetPremiumChartByRange retrieves a resolution-specific chart payload (7D, 1M, 1Y) or the default 1m 24H payload.
func (p *PubSubClient) GetPremiumChartByRange(ctx context.Context, rangeCode, baseEx, baseSym, foreignEx, foreignSym string) ([]byte, error) {
	rc := rangeCode
	if rc == "7D" || rc == "1M" || rc == "1Y" {
		key := "market:premium_chart:" + rc + ":" + baseEx + ":" + baseSym + ":" + foreignEx + ":" + foreignSym
		if b, err := p.client.Get(ctx, key).Bytes(); err == nil && len(b) > 0 {
			return b, nil
		}
	}
	return p.GetPremiumChart(ctx, baseEx, baseSym, foreignEx, foreignSym)
}
