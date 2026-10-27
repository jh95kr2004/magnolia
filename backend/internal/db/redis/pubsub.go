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
func (p *PubSubClient) SubscribeTickers(ctx context.Context, symbols []exchange.Symbol, outCh chan<- exchange.PriceUpdate) {
	channels := make([]string, len(symbols))
	for i, sym := range symbols {
		channels[i] = TickerChannelPrefix + string(sym)
	}

	pubsub := p.client.Subscribe(ctx, channels...)
	defer pubsub.Close()
	
	ch := pubsub.Channel()

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			var update exchange.PriceUpdate
			if err := json.Unmarshal([]byte(msg.Payload), &update); err != nil {
				log.Printf("[Redis] Failed to unmarshal ticker: %v", err)
				continue
			}
			outCh <- update
		}
	}
}

// PublishFXRate broadcasts a normalized FX rate for a given quote/base pair to the cluster.
func (p *PubSubClient) PublishFXRate(ctx context.Context, base, quote string, rate float64) error {
	channel := "fx:" + base + "-" + quote
	return p.client.Publish(ctx, channel, rate).Err()
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
	queueName := "queue:dangling_clients:" + region
	return p.client.LPush(ctx, queueName, userID).Err()
}

// PopDanglingClient blocks until a user is available in the regional queue, then pops and returns it.
func (p *PubSubClient) PopDanglingClient(ctx context.Context, region string) (string, error) {
	queueName := "queue:dangling_clients:" + region
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
	return p.client.Publish(ctx, "broadcast:opportunities:raw", payload).Err()
}

// SubscribeRawOpportunities listens for global raw opportunity data broadcasts.
func (p *PubSubClient) SubscribeRawOpportunities(ctx context.Context, outCh chan<- []byte) {
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

// PublishUserState broadcasts the customized account metrics to python daemon.
func (p *PubSubClient) PublishUserState(ctx context.Context, userID string, payload []byte) error {
	channel := "user:" + userID + ":state"
	return p.client.Publish(ctx, channel, payload).Err()
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
