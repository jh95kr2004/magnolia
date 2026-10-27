/*
Package exchange
This file is a virtual wrapper that supports Paper Trading without using real money.
Internally, it implements the interface of a real exchange client but processes trade executions and balance updates virtually within local memory to assure testing stability.
*/
package exchange

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	paperRegistry   = make(map[string]*PaperClient)
	paperRegistryMu sync.RWMutex
)

// PaperClient wraps any real exchange.Client to provide virtual balance and order simulation,
// completely bypassing real order placements to the actual exchange.
type PaperClient struct {
	realClient Client
	balances   map[string]float64
	mu         sync.Mutex
	orderIDSeq int
}

// NewPaperClient creates a new mock execution client using the specified live realClient (for market data).
func NewPaperClient(realClient Client, initialBalances map[string]float64) *PaperClient {
	b := make(map[string]float64)
	for k, v := range initialBalances {
		b[k] = v // e.g. "BTC": 1.0, "USD": 50000.0, "KRW": 50000000.0
	}
	pc := &PaperClient{
		realClient: realClient,
		balances:   b,
	}

	paperRegistryMu.Lock()
	paperRegistry[realClient.GetName()] = pc
	paperRegistryMu.Unlock()

	return pc
}

func (p *PaperClient) GetName() string {
	return p.realClient.GetName()
}

func (p *PaperClient) GetSupportedNetworks(ctx context.Context, currency Currency) ([]NetworkInfo, error) {
	return p.realClient.GetSupportedNetworks(ctx, currency)
}

func (p *PaperClient) GetTradingFee(ctx context.Context, symbol Symbol) (TradeFeeInfo, error) {
	return p.realClient.GetTradingFee(ctx, symbol)
}

func (p *PaperClient) GetMarkets(ctx context.Context) ([]MarketInfo, error) {
	return p.realClient.GetMarkets(ctx)
}

func (p *PaperClient) GetTicker(ctx context.Context, symbol Symbol) (float64, error) {
	return p.realClient.GetTicker(ctx, symbol) // Use real market quotes
}

func (p *PaperClient) GetFutureTicker(ctx context.Context, symbol Symbol) (float64, error) {
	return p.realClient.GetFutureTicker(ctx, symbol)
}

func (p *PaperClient) GetBalances(ctx context.Context) ([]Balance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var res []Balance
	for k, v := range p.balances {
		res = append(res, Balance{
			Currency:  Currency(strings.ToUpper(k)),
			Available: v,
			Locked:    0,
		})
	}
	return res, nil
}

// PlaceOrder intercepts the order flow to avoid hitting the real exchange API.
// It instantly fills the order against virtual balances assuming the given price.
func (p *PaperClient) PlaceOrder(ctx context.Context, req OrderRequest) (*OrderResponse, error) {
	feeInfo, err := p.realClient.GetTradingFee(ctx, req.Symbol)
	feeRate := 0.001 // Fallback 0.1% fee
	if err == nil {
		feeRate = feeInfo.TakerFeeRate
	}

	price := req.Price
	if price <= 0 {
		var err error
		if req.Type == FuturesLong || req.Type == FuturesShort {
			price, err = p.GetFutureTicker(ctx, req.Symbol)
		} else {
			price, err = p.GetTicker(ctx, req.Symbol)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to fetch ticker price for paper market order: %w", err)
		}
	}

	// Simplistic parsing of Base / Quote currencies natively
	baseCur, quoteCur := ParseSymbol(req.Symbol)
	base := string(baseCur)
	quote := string(quoteCur)

	tradeVolumeAssets := req.Amount
	tradeVolumeQuote := req.Amount * price
	feeAmountQuote := tradeVolumeQuote * feeRate

	p.mu.Lock()
	defer p.mu.Unlock()

	p.orderIDSeq++
	orderID := fmt.Sprintf("paper-order-%s-%d", p.GetName(), p.orderIDSeq)

	if req.Side == BuyOrder {
		// Buying Base asset: Add Base, Deduct Quote
		requiredQuote := tradeVolumeQuote + feeAmountQuote
		if p.balances[quote] < requiredQuote {
			return nil, fmt.Errorf("insufficient paper funds for buy order: requires %f %s but have %f %s", requiredQuote, quote, p.balances[quote], quote)
		}
		p.balances[quote] -= requiredQuote
		p.balances[base] += tradeVolumeAssets
	} else {
		// Selling Base asset: Deduct Base, Add Quote
		if p.balances[base] < tradeVolumeAssets {
			return nil, fmt.Errorf("insufficient paper funds for sell order: requires %f %s but have %f %s", tradeVolumeAssets, base, p.balances[base], base)
		}
		p.balances[base] -= tradeVolumeAssets
		netQuoteEarned := tradeVolumeQuote - feeAmountQuote
		p.balances[quote] += netQuoteEarned
	}

	fmt.Printf("[PAPER EXEC] %s %s %f %s @ %f %s. Fee: %f %s\n",
		p.GetName(), string(req.Side), req.Amount, base, price, quote, feeAmountQuote, quote)

	return &OrderResponse{
		OrderID: orderID,
		Status:  "FILLED",
		Price:   price,
		Amount:  req.Amount,
		Fee:     feeAmountQuote,
	}, nil
}

func (p *PaperClient) CancelOrder(ctx context.Context, orderID string, symbol Symbol) error {
	// Paper orders fill instantly, so there is nothing to cancel.
	return nil
}

func (p *PaperClient) ConnectWS(ctx context.Context, symbols []Symbol, priceUpdateCh chan<- PriceUpdate) error {
	return p.realClient.ConnectWS(ctx, symbols, priceUpdateCh)
}

func (p *PaperClient) CloseWS() error {
	return p.realClient.CloseWS()
}

func (p *PaperClient) SupportsMargin() bool {
	return true
}

func (p *PaperClient) LendingManager() *WebLendingManager {
	if wc, ok := p.realClient.(WebLendingCapable); ok {
		return wc.LendingManager()
	}
	return nil
}

func (p *PaperClient) Borrow(ctx context.Context, currency Currency, amount float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.balances[string(currency)] += amount
	return nil
}

func (p *PaperClient) Repay(ctx context.Context, currency Currency, amount float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.balances[string(currency)] -= amount
	return nil
}

func (p *PaperClient) Deposit(currency string, amount float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.balances[currency] += amount
}

// === Transfer API Mocks ===

func (p *PaperClient) GetAPIKeyPermissions(ctx context.Context) (APIKeyPermissions, error) {
	return p.realClient.GetAPIKeyPermissions(ctx) // Mirror the real key's scope
}

func (p *PaperClient) GetDepositAddress(ctx context.Context, currency Currency, network Network) (DepositAddress, error) {
	return DepositAddress{
		Address: "PAPER-DEPOSIT-" + p.GetName() + "-" + string(currency) + "-" + string(network),
		Tag:     "PAPER-TAG",
	}, nil
}

func (p *PaperClient) GetWithdrawalLimit(ctx context.Context, currency Currency, network Network) (WithdrawalLimit, error) {
	return WithdrawalLimit{
		Currency:       currency,
		Network:        network,
		MinWithdrawal:  0.0,
		DailyLimit:     999999.0,
		DailyRemaining: 999999.0,
		MaxPerTx:       999999.0,
	}, nil
}

func (p *PaperClient) Withdraw(ctx context.Context, req WithdrawRequest) (*WithdrawResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Virtual deduction
	cost := req.Amount // Assuming 0 fee for paper tests, else subtract fees
	if p.balances[string(req.Currency)] < cost {
		return nil, fmt.Errorf("insufficient paper funds for withdrawal: requires %f %s but have %f %s", cost, req.Currency, p.balances[string(req.Currency)], req.Currency)
	}
	p.balances[string(req.Currency)] -= cost

	// Auto-deposit to target paper exchange client if matches paper pattern
	if strings.HasPrefix(req.Address, "PAPER-DEPOSIT-") {
		parts := strings.Split(req.Address, "-")
		if len(parts) >= 3 {
			destExchange := parts[2]
			paperRegistryMu.RLock()
			destClient, ok := paperRegistry[destExchange]
			paperRegistryMu.RUnlock()
			if ok {
				destClient.Deposit(string(req.Currency), req.Amount)
			}
		}
	}

	return &WithdrawResponse{
		WithdrawID: fmt.Sprintf("paper-wd-%d", time.Now().UnixNano()),
		TxID:       "0xpaperhash123",
		Fee:        0.0,
		Amount:     req.Amount,
		Status:     string(WithdrawCompleted), // Instantly complete in paper env
	}, nil
}

func (p *PaperClient) GetWithdrawHistory(ctx context.Context, currency Currency) ([]WithdrawRecord, error) {
	return []WithdrawRecord{}, nil // Return empty list for sim
}

func (p *PaperClient) CheckWithdrawalStatus(ctx context.Context, withdrawID string) (WithdrawStatus, error) {
	return WithdrawCompleted, nil // Always instantly completed
}

func (p *PaperClient) GetWithdrawalWhitelist(ctx context.Context, currency Currency) ([]string, error) {
	return []string{"ANY_PAPER_WHITELIST"}, nil // Paper client allows sending anywhere
}
