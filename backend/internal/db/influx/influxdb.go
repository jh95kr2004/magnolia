/*
Package influx
This file provides a client wrapper for loading and querying real-time exchange tick data in the InfluxDB time-series database.
The AI model (PremiumForecaster) will later consume this data to construct deep learning tensors and predict short-term swing probabilities such as historical premium and funding rates.
*/
package influx

import (
	"context"
	"fmt"
	"log"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/magnolia/backend/internal/exchange"
)

type Client struct {
	client   influxdb2.Client
	writeAPI api.WriteAPIBlocking
	queryAPI api.QueryAPI
}

func NewClient(url, token, org, bucket string) *Client {
	client := influxdb2.NewClient(url, token)
	writeAPI := client.WriteAPIBlocking(org, bucket)
	queryAPI := client.QueryAPI(org)
	return &Client{
		client:   client,
		writeAPI: writeAPI,
		queryAPI: queryAPI,
	}
}

func (c *Client) WritePriceUpdate(ctx context.Context, update exchange.PriceUpdate) error {
	now := time.Now()
	
	// 1. Write exchange_tickers (Spot and Future)
	isFutureStr := "false"
	if update.IsFutures {
		isFutureStr = "true"
	}
	
	pTicker := influxdb2.NewPoint("exchange_tickers",
		map[string]string{
			"exchange":       update.Exchange, 
			"symbol":         string(update.Symbol),
			"quote_currency": string(update.QuoteCurrency),
			"is_future":      isFutureStr,
		},
		map[string]interface{}{
			"bid_usd": update.BidUSD, 
			"ask_usd": update.AskUSD,
			"bid_raw": update.RawBid,
			"ask_raw": update.RawAsk,
		},
		now)
	
	if err := c.writeAPI.WritePoint(ctx, pTicker); err != nil {
		log.Printf("Error writing exchange_tickers to InfluxDB: %v", err)
		return err
	}

	// 2. Write funding_rates (If applicable)
	if update.FundingRate != 0 {
		pFunding := influxdb2.NewPoint("funding_rates",
			map[string]string{
				"exchange": update.Exchange, 
				"symbol":   string(update.Symbol),
			},
			map[string]interface{}{
				"funding_rate": update.FundingRate,
			},
			now)
		
		if err := c.writeAPI.WritePoint(ctx, pFunding); err != nil {
			log.Printf("Error writing funding_rates to InfluxDB: %v", err)
		}
	}

	// 3. Write fx_rate
	if update.FXCurrencyPair != "" {
		pFX := influxdb2.NewPoint("fx_rate",
			map[string]string{
				"currency_pair": update.FXCurrencyPair,
			},
			map[string]interface{}{
				"fx_rate": update.FXRate,
			},
			now)
		
		if err := c.writeAPI.WritePoint(ctx, pFX); err != nil {
			log.Printf("Error writing fx_rate to InfluxDB: %v", err)
		}
	}

	return nil
}

func (c *Client) Close() {
	c.client.Close()
}

// GetHistoricalPremium fetches the recent premium percentage for a given symbol pair between two exchanges.
// NOTE: For the AI Model (`PremiumForecaster`), it's expected that the AI service queries InfluxDB
// directly to pull raw prices, exchange rates, and funding rates. This function provides a rule-based
// numerical heuristic purely for the Go backend Ranker layer.
func (c *Client) GetHistoricalPremium(ctx context.Context, baseExchange, foreignExchange, symbol string, baseIsFuture, foreignIsFuture bool, lookback time.Duration) (float64, error) {
	// Query to fetch the average ask price of the base exchange for the symbol
	// We use AskUSD as the base purchase point representation.
	baseQuery := fmt.Sprintf(`
		from(bucket: "%s")
		  |> range(start: -%v)
		  |> filter(fn: (r) => r._measurement == "exchange_tickers")
		  |> filter(fn: (r) => r.exchange == "%s")
		  |> filter(fn: (r) => r.symbol == "%s")
		  |> filter(fn: (r) => r.is_future == "%t")
		  |> filter(fn: (r) => r._field == "ask_usd")
		  |> mean()
	`, "magnolia", int(lookback.Seconds()), baseExchange, symbol, baseIsFuture)

	baseResult, err := c.queryAPI.Query(ctx, baseQuery)
	if err != nil {
		return 0, fmt.Errorf("failed to query base exchange history: %w", err)
	}

	var baseMean float64
	for baseResult.Next() {
		if val, ok := baseResult.Record().Value().(float64); ok {
			baseMean = val
		}
	}
	if baseResult.Err() != nil {
		return 0, baseResult.Err()
	}

	// Query to fetch the average bid price of the foreign exchange for the symbol
	// We use BidUSD as the foreign sell point representation.
	foreignQuery := fmt.Sprintf(`
		from(bucket: "%s")
		  |> range(start: -%v)
		  |> filter(fn: (r) => r._measurement == "exchange_tickers")
		  |> filter(fn: (r) => r.exchange == "%s")
		  |> filter(fn: (r) => r.symbol == "%s")
		  |> filter(fn: (r) => r.is_future == "%t")
		  |> filter(fn: (r) => r._field == "bid_usd")
		  |> mean()
	`, "magnolia", int(lookback.Seconds()), foreignExchange, symbol, foreignIsFuture)

	foreignResult, err := c.queryAPI.Query(ctx, foreignQuery)
	if err != nil {
		return 0, fmt.Errorf("failed to query foreign exchange history: %w", err)
	}

	var foreignMean float64
	for foreignResult.Next() {
		if val, ok := foreignResult.Record().Value().(float64); ok {
			foreignMean = val
		}
	}
	if foreignResult.Err() != nil {
		return 0, foreignResult.Err()
	}

	// If we lack data for either leg, we cannot reliably compute a historical premium.
	if baseMean == 0 || foreignMean == 0 {
		return 0, fmt.Errorf("insufficient data points to calculate historical premium (base: %f, foreign: %f)", baseMean, foreignMean)
	}

	// Calculate mathematical spread
	// Since we query 'bid_usd' and 'ask_usd', this directly normalizes the KRW/USD spread.
	historicalPremium := (baseMean - foreignMean) / foreignMean
	return historicalPremium, nil
}

// GetPredictedFundingRates fetches the historical funding rate trends and returns a projected array
// of future funding snapshots. Like Premium, the external AI model pulls real-time measurements; 
// this is for Go Ranker heuristics.
func (c *Client) GetPredictedFundingRates(ctx context.Context, exchange, symbol string, periods int, fundingInterval time.Duration) ([]float64, error) {
	// Query historical funding rates over a rough lookback window to find a trend.
	lookbackHours := periods * int(fundingInterval.Hours())
	if lookbackHours == 0 {
		lookbackHours = 24 // Fallback
	}

	query := fmt.Sprintf(`
		from(bucket: "%s")
		  |> range(start: -%dh)
		  |> filter(fn: (r) => r._measurement == "funding_rates")
		  |> filter(fn: (r) => r.exchange == "%s")
		  |> filter(fn: (r) => r.symbol == "%s")
		  |> filter(fn: (r) => r._field == "funding_rate")
		  |> mean()
	`, "magnolia", lookbackHours, exchange, symbol)

	result, err := c.queryAPI.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to query historical funding rates: %w", err)
	}

	var meanFunding float64
	var found bool
	for result.Next() {
		if val, ok := result.Record().Value().(float64); ok {
			meanFunding = val
			found = true
		}
	}
	if result.Err() != nil {
		return nil, result.Err()
	}

	if !found {
		// Fallback to nominal fixed expected rate if no data is found
		meanFunding = 0.0001 // 0.01%
	}

	// Project the mean forward as a flat array for simplicity.
	// An advanced model would hook into ai_service's predictor endpoint.
	rates := make([]float64, periods)
	for i := 0; i < periods; i++ {
		rates[i] = meanFunding
	}
	return rates, nil
}
