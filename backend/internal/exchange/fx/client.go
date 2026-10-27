package fx

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"time"

	"github.com/magnolia/backend/internal/exchange"
)

// FXFetcher defines the interface for fetching real-time exchange rates.
type FXFetcher interface {
	// FetchRates retrieves exchange rates normalized to USD (where USD = 1.0).
	// e.g., if KRW/USD is 1400, then the returned map contains {"KRW": 1400.0}
	FetchRates(ctx context.Context, targetCurrencies []exchange.Currency) (map[exchange.Currency]float64, error)
	GetName() string
}

// FixerClient implements FXFetcher using the Fixer.io API.
type FixerClient struct {
	APIKey     string
	BaseURL    string
	IsFreeTier bool
	client     *http.Client
}

// NewFixerClient initializes a new Fixer API client.
func NewFixerClient(apiKey string, isFreeTier bool) *FixerClient {
	return &FixerClient{
		APIKey:     apiKey,
		BaseURL:    "http://data.fixer.io/api/latest",
		IsFreeTier: isFreeTier,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (f *FixerClient) GetName() string {
	return "FIXER_IO"
}

// FetchRates implements the FXFetcher interface.
// If IsFreeTier is true, Fixer.io FORCES the Base currency to be EUR.
// To normalize everything to USD (USD = 1.0), we must fetch USD along with target currencies,
// and then mathematically divide the targets by the EUR->USD rate.
// If IsFreeTier is false, we can directly query with `&base=USD`.
func (f *FixerClient) FetchRates(ctx context.Context, targetCurrencies []exchange.Currency) (map[exchange.Currency]float64, error) {
	if f.APIKey == "" {
		return nil, fmt.Errorf("fixer.io API key is empty")
	}

	normalizedRates := make(map[exchange.Currency]float64)
	normalizedRates["USD"] = 1.0 // Identity

	symbols := ""
	if f.IsFreeTier {
		// We must always fetch USD to do the conversion from the forced EUR base.
		symbols = "USD"
		for _, c := range targetCurrencies {
			if c != "USD" {
				symbols += "," + string(c)
			}
		}
	} else {
		for i, c := range targetCurrencies {
			if i == 0 {
				symbols += string(c)
			} else {
				symbols += "," + string(c)
			}
		}
	}

	url := fmt.Sprintf("%s?access_key=%s&symbols=%s", f.BaseURL, f.APIKey, symbols)
	if !f.IsFreeTier {
		url += "&base=USD"
	}
	
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create fx request: %w", err)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute fx request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := ioutil.ReadAll(resp.Body)
		return nil, fmt.Errorf("fixer API returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var parsed struct {
		Success bool               `json:"success"`
		Rates   map[string]float64 `json:"rates"`
		Error   struct {
			Code int    `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("failed to decode fx response: %w", err)
	}

	if !parsed.Success {
		return nil, fmt.Errorf("fixer API error. Code: %d, Type: %s", parsed.Error.Code, parsed.Error.Type)
	}

	if f.IsFreeTier {
		usdRate, hasUsd := parsed.Rates["USD"]
		if !hasUsd || usdRate == 0 {
			return nil, fmt.Errorf("fixer API response missing USD base conversion rate")
		}

		log.Printf("[FXFetcher] Fixer Raw: 1 EUR = %.4f USD", usdRate)

		// Convert all EUR-based rates to USD-based rates.
		for _, target := range targetCurrencies {
			if target == "USD" {
				continue
			}
			
			targetSymbol := string(target)
			targetRateEuroStr, hasTarget := parsed.Rates[targetSymbol]
			if !hasTarget {
				return nil, fmt.Errorf("fixer API response missing requested currency: %s", targetSymbol)
			}

			// Math: (EUR -> KRW) / (EUR -> USD) = (USD -> KRW)
			usdToBaseQuote := targetRateEuroStr / usdRate
			normalizedRates[target] = usdToBaseQuote
			
			log.Printf("[FXFetcher] Normalized: 1 USD = %.4f %s", usdToBaseQuote, targetSymbol)
		}
	} else {
		// Paid tier already gives base USD
		for _, target := range targetCurrencies {
			if target == "USD" {
				continue
			}
			targetSymbol := string(target)
			targetRate, hasTarget := parsed.Rates[targetSymbol]
			if !hasTarget {
				return nil, fmt.Errorf("fixer API response missing requested currency: %s", targetSymbol)
			}
			normalizedRates[target] = targetRate
			
			log.Printf("[FXFetcher] Synced: 1 USD = %.4f %s", targetRate, targetSymbol)
		}
	}

	return normalizedRates, nil
}
