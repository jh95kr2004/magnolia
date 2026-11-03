/*
Package exchange
This file is a virtual wrapper that supports Paper Trading without using real money.
Internally, it implements the interface of a real exchange client, uses real live market data (with optional gradual scenario offset),
and processes Spot, Margin Borrow/Repay, Futures (Long/Short collateral & PnL), and Cross-Exchange Transfers in isolated per-user virtual ledgers.
*/
package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	paperRegistry   = make(map[string]*PaperClient)
	paperRegistryMu sync.RWMutex
)

type PaperFuturePosition struct {
	Symbol       Symbol    `json:"symbol"`
	Side         OrderSide `json:"side"`
	Amount       float64   `json:"amount"`
	EntryPrice   float64   `json:"entry_price"`
	MarginLocked float64   `json:"margin_locked"`
}

type PaperPriceHint struct {
	SpotPrice   float64
	FuturePrice float64
	UpdatedAt   time.Time
}

type PaperClientSnapshot struct {
	Balances map[string]float64               `json:"balances"`
	Borrowed map[string]float64               `json:"borrowed"`
	Futures  map[Symbol]*PaperFuturePosition  `json:"futures"`
}

// PaperClient wraps any real exchange.Client to provide virtual balance and order simulation,
// completely bypassing real order placements to the actual exchange.
type PaperClient struct {
	userID     uint
	realClient Client
	balances   map[string]float64
	borrowed   map[string]float64
	futures    map[Symbol]*PaperFuturePosition
	priceHints map[Symbol]PaperPriceHint
	mu         sync.Mutex
	orderIDSeq int
}

func paperRegistryKey(userID uint, exchangeName string) string {
	return fmt.Sprintf("%d:%s", userID, strings.ToUpper(strings.TrimSpace(exchangeName)))
}

// NewPaperClient creates a new mock execution client for default userID=1.
func NewPaperClient(realClient Client, initialBalances map[string]float64) *PaperClient {
	return NewPaperClientForUser(1, realClient, initialBalances)
}

// NewPaperClientForUser creates an isolated PaperClient scoped to a specific userID.
func NewPaperClientForUser(userID uint, realClient Client, initialBalances map[string]float64) *PaperClient {
	if userID == 0 {
		userID = 1
	}
	b := make(map[string]float64)
	for k, v := range initialBalances {
		b[strings.ToUpper(k)] = v
	}
	pc := &PaperClient{
		userID:     userID,
		realClient: realClient,
		balances:   b,
		borrowed:   make(map[string]float64),
		futures:    make(map[Symbol]*PaperFuturePosition),
		priceHints: make(map[Symbol]PaperPriceHint),
	}

	paperRegistryMu.Lock()
	paperRegistry[paperRegistryKey(userID, realClient.GetName())] = pc
	paperRegistryMu.Unlock()

	return pc
}

func (p *PaperClient) IsPaper() bool {
	return true
}

func (p *PaperClient) GetName() string {
	return p.realClient.GetName()
}

// SetPriceHint injects the latest real-time (and optionally offset-adjusted) quote price for a symbol.
func (p *PaperClient) SetPriceHint(symbol Symbol, spotPrice, futurePrice float64) {
	if spotPrice <= 0 && futurePrice <= 0 {
		return
	}
	if futurePrice <= 0 {
		futurePrice = spotPrice
	}
	if spotPrice <= 0 {
		spotPrice = futurePrice
	}
	p.mu.Lock()
	p.priceHints[symbol] = PaperPriceHint{
		SpotPrice:   spotPrice,
		FuturePrice: futurePrice,
		UpdatedAt:   time.Now(),
	}
	p.mu.Unlock()
}

// ResetBalances clears active futures/borrows and resets spot balances to the specified seed amounts.
func (p *PaperClient) ResetBalances(initialBalances map[string]float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.balances = make(map[string]float64)
	for k, v := range initialBalances {
		p.balances[strings.ToUpper(k)] = v
	}
	p.borrowed = make(map[string]float64)
	p.futures = make(map[Symbol]*PaperFuturePosition)
}

// ExportSnapshot returns a serializable copy of the PaperClient's ledger.
func (p *PaperClient) ExportSnapshot() PaperClientSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap := PaperClientSnapshot{
		Balances: make(map[string]float64),
		Borrowed: make(map[string]float64),
		Futures:  make(map[Symbol]*PaperFuturePosition),
	}
	for k, v := range p.balances {
		snap.Balances[k] = v
	}
	for k, v := range p.borrowed {
		snap.Borrowed[k] = v
	}
	for k, v := range p.futures {
		if v != nil {
			cp := *v
			snap.Futures[k] = &cp
		}
	}
	return snap
}

// ImportSnapshot restores balances, borrowed liabilities, and futures positions from a persisted snapshot.
func (p *PaperClient) ImportSnapshot(snap PaperClientSnapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(snap.Balances) > 0 {
		p.balances = make(map[string]float64)
		for k, v := range snap.Balances {
			p.balances[strings.ToUpper(k)] = v
		}
	}
	if snap.Borrowed != nil {
		p.borrowed = make(map[string]float64)
		for k, v := range snap.Borrowed {
			p.borrowed[strings.ToUpper(k)] = v
		}
	}
	if snap.Futures != nil {
		p.futures = make(map[Symbol]*PaperFuturePosition)
		for k, v := range snap.Futures {
			if v != nil {
				cp := *v
				p.futures[k] = &cp
			}
		}
	}
}

// ExportAllUserPaperState serializes all PaperClients belonging to a user into a single JSON string.
func ExportAllUserPaperState(userID uint) string {
	paperRegistryMu.RLock()
	defer paperRegistryMu.RUnlock()

	all := make(map[string]PaperClientSnapshot)
	prefix := fmt.Sprintf("%d:", userID)
	for k, client := range paperRegistry {
		if strings.HasPrefix(k, prefix) {
			all[client.GetName()] = client.ExportSnapshot()
		}
	}
	bytes, err := json.Marshal(all)
	if err != nil {
		return ""
	}
	return string(bytes)
}

// ImportAllUserPaperState restores all PaperClients belonging to a user from a JSON string.
func ImportAllUserPaperState(userID uint, rawJSON string) {
	rawJSON = strings.TrimSpace(rawJSON)
	if rawJSON == "" {
		return
	}
	var all map[string]PaperClientSnapshot
	if err := json.Unmarshal([]byte(rawJSON), &all); err != nil {
		return
	}
	paperRegistryMu.RLock()
	defer paperRegistryMu.RUnlock()
	for exName, snap := range all {
		if client, ok := paperRegistry[paperRegistryKey(userID, exName)]; ok {
			client.ImportSnapshot(snap)
		}
	}
}

func (p *PaperClient) GetSupportedNetworks(ctx context.Context, currency Currency) ([]NetworkInfo, error) {
	nets, err := p.realClient.GetSupportedNetworks(ctx, currency)
	if err == nil && len(nets) > 0 {
		return nets, nil
	}
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

func (p *PaperClient) GetTradingFee(ctx context.Context, symbol Symbol) (TradeFeeInfo, error) {
	return p.realClient.GetTradingFee(ctx, symbol)
}

func (p *PaperClient) GetMarkets(ctx context.Context) ([]MarketInfo, error) {
	return p.realClient.GetMarkets(ctx)
}

func (p *PaperClient) GetTicker(ctx context.Context, symbol Symbol) (float64, error) {
	p.mu.Lock()
	hint, ok := p.priceHints[symbol]
	p.mu.Unlock()
	if ok && hint.SpotPrice > 0 && time.Since(hint.UpdatedAt) < 5*time.Minute {
		return hint.SpotPrice, nil
	}
	return p.realClient.GetTicker(ctx, symbol)
}

func (p *PaperClient) GetFutureTicker(ctx context.Context, symbol Symbol) (float64, error) {
	p.mu.Lock()
	hint, ok := p.priceHints[symbol]
	p.mu.Unlock()
	if ok && hint.FuturePrice > 0 && time.Since(hint.UpdatedAt) < 5*time.Minute {
		return hint.FuturePrice, nil
	}
	fPrice, err := p.realClient.GetFutureTicker(ctx, symbol)
	if err == nil && fPrice > 0 {
		return fPrice, nil
	}
	return p.GetTicker(ctx, symbol)
}

func (p *PaperClient) nativeQuoteCurrency() string {
	name := strings.ToUpper(p.GetName())
	if name == "UPBIT" || name == "BITHUMB" {
		return "KRW"
	}
	return "USD"
}

func (p *PaperClient) lookupMarkPriceUnlocked(baseCur, quoteCur string) (spotPrice, futurePrice float64) {
	baseUpper := strings.ToUpper(baseCur)
	quoteUpper := strings.ToUpper(quoteCur)
	candidates := []Symbol{
		Symbol(baseUpper + "-" + quoteUpper),
		Symbol(baseUpper + "/" + quoteUpper),
	}
	for _, sym := range candidates {
		if hint, ok := p.priceHints[sym]; ok && (hint.SpotPrice > 0 || hint.FuturePrice > 0) {
			return hint.SpotPrice, hint.FuturePrice
		}
	}
	for sym, hint := range p.priceHints {
		b, q := ParseSymbol(sym)
		if strings.EqualFold(string(b), baseUpper) && strings.EqualFold(string(q), quoteUpper) {
			return hint.SpotPrice, hint.FuturePrice
		}
	}
	return 0, 0
}

// GetBorrowedAmount returns the current margin-borrowed coin liability for a currency.
func (p *PaperClient) GetBorrowedAmount(currency Currency) float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.borrowed[strings.ToUpper(string(currency))]
}

// GetAccountValuation computes the unencumbered available quote cash and the true mark-to-market
// net equity (in native quote currency) for this PaperClient:
// NetEquity = Cash_quote + sum((Spot_c - Borrowed_c) * MarkPrice_c) + sum(FuturesMarginLocked + FuturesUnrealizedPnL).
func (p *PaperClient) GetAccountValuation(ctx context.Context) (quoteCur string, availableQuote float64, netEquityQuote float64) {
	quoteCur = p.nativeQuoteCurrency()

	p.mu.Lock()
	cashQuote := p.balances[quoteCur]
	assets := make(map[string]struct{})
	for k := range p.balances {
		assets[strings.ToUpper(k)] = struct{}{}
	}
	for k := range p.borrowed {
		assets[strings.ToUpper(k)] = struct{}{}
	}

	type assetEntry struct {
		cur       string
		spot      float64
		borrowed  float64
		markPrice float64
	}
	var entries []assetEntry
	for cur := range assets {
		if cur == quoteCur || cur == "KRW" || cur == "USD" || cur == "USDT" || cur == "USDC" {
			continue
		}
		spotAmt := p.balances[cur]
		borrowedAmt := p.borrowed[cur]
		if math.Abs(spotAmt) <= 1e-9 && math.Abs(borrowedAmt) <= 1e-9 {
			continue
		}
		sp, _ := p.lookupMarkPriceUnlocked(cur, quoteCur)
		entries = append(entries, assetEntry{
			cur:       cur,
			spot:      spotAmt,
			borrowed:  borrowedAmt,
			markPrice: sp,
		})
	}

	var futuresEquity float64
	for sym, fPos := range p.futures {
		if fPos == nil || fPos.Amount <= 1e-9 {
			continue
		}
		futPrice := fPos.EntryPrice
		if hint, ok := p.priceHints[sym]; ok && hint.FuturePrice > 0 {
			futPrice = hint.FuturePrice
		} else if ok && hint.SpotPrice > 0 {
			futPrice = hint.SpotPrice
		}
		var uPnL float64
		if fPos.Side == BuyOrder {
			uPnL = (futPrice - fPos.EntryPrice) * fPos.Amount
		} else {
			uPnL = (fPos.EntryPrice - futPrice) * fPos.Amount
		}
		futuresEquity += fPos.MarginLocked + uPnL
	}
	p.mu.Unlock()

	var shortReserveQuote, netCryptoQuote float64
	for _, e := range entries {
		price := e.markPrice
		if price <= 0 {
			sym := Symbol(e.cur + "-" + quoteCur)
			if p.GetName() == "BINANCE" || p.GetName() == "BYBIT" || p.GetName() == "OKX" || p.GetName() == "KRAKEN" {
				sym = Symbol(e.cur + "/" + quoteCur)
			}
			if fetched, err := p.realClient.GetTicker(ctx, sym); err == nil && fetched > 0 {
				price = fetched
				p.SetPriceHint(sym, fetched, fetched)
			}
		}
		if price > 0 {
			netCryptoQuote += (e.spot - e.borrowed) * price
			if e.borrowed > e.spot {
				shortReserveQuote += (e.borrowed - e.spot) * price
			}
		}
	}

	netEquityQuote = cashQuote + netCryptoQuote + futuresEquity
	if netEquityQuote < 0 {
		netEquityQuote = 0
	}
	availableQuote = math.Max(0, cashQuote-shortReserveQuote)
	return quoteCur, availableQuote, netEquityQuote
}

func (p *PaperClient) GetBalances(ctx context.Context) ([]Balance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	quoteCur := p.nativeQuoteCurrency()
	lockedByQuote := make(map[string]float64)
	for sym, fPos := range p.futures {
		if fPos != nil && fPos.MarginLocked > 0 {
			_, qCur := ParseSymbol(sym)
			q := strings.ToUpper(string(qCur))
			if q == "" {
				q = quoteCur
			}
			lockedByQuote[q] += fPos.MarginLocked
		}
	}

	// Reserve short-sale proceeds for borrowed coins that were sold short so Available cash is never double-counted
	var shortReserveQuote float64
	for cur, borrowedAmt := range p.borrowed {
		curUpper := strings.ToUpper(cur)
		spotAmt := p.balances[curUpper]
		if borrowedAmt > spotAmt+1e-9 {
			sp, _ := p.lookupMarkPriceUnlocked(curUpper, quoteCur)
			if sp > 0 {
				shortReserveQuote += (borrowedAmt - spotAmt) * sp
			}
		}
	}

	seen := make(map[string]bool)
	var res []Balance
	for k, v := range p.balances {
		cur := strings.ToUpper(k)
		seen[cur] = true
		avail := v
		locked := lockedByQuote[cur]
		if cur == quoteCur && shortReserveQuote > 0 {
			reserved := math.Min(avail, shortReserveQuote)
			avail = math.Max(0, avail-reserved)
			locked += reserved
		}
		res = append(res, Balance{
			Currency:  Currency(cur),
			Available: avail,
			Locked:    locked,
		})
	}
	for cur, locked := range lockedByQuote {
		if !seen[cur] {
			res = append(res, Balance{
				Currency:  Currency(cur),
				Available: 0,
				Locked:    locked,
			})
		}
	}
	return res, nil
}

// PlaceOrder intercepts the order flow to avoid hitting the real exchange API.
// Supports Spot, Margin Borrowed Spot, and Futures (FuturesLong / FuturesShort with margin locking & realized PnL).
func (p *PaperClient) PlaceOrder(ctx context.Context, req OrderRequest) (*OrderResponse, error) {
	feeInfo, err := p.realClient.GetTradingFee(ctx, req.Symbol)
	feeRate := 0.001 // Fallback 0.1% fee
	if err == nil && feeInfo.TakerFeeRate > 0 {
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
		if err != nil || price <= 0 {
			return nil, fmt.Errorf("failed to fetch ticker price for paper market order (%s): %v", req.Symbol, err)
		}
	}

	baseCur, quoteCur := ParseSymbol(req.Symbol)
	base := strings.ToUpper(string(baseCur))
	quote := strings.ToUpper(string(quoteCur))

	tradeVolumeAssets := req.Amount
	tradeVolumeQuote := req.Amount * price
	feeAmountQuote := tradeVolumeQuote * feeRate

	p.mu.Lock()
	defer p.mu.Unlock()

	p.orderIDSeq++
	orderID := fmt.Sprintf("paper-order-%s-%d", p.GetName(), p.orderIDSeq)

	// 1. Futures Order Accounting (Collateral Locking & Realized PnL Settlement)
	if req.Type == FuturesLong || req.Type == FuturesShort {
		existing := p.futures[req.Symbol]
		if existing == nil || existing.Amount <= 1e-9 || existing.Side == req.Side {
			// Opening or adding to a futures position (1x collateral locked in quote currency)
			requiredQuote := tradeVolumeQuote + feeAmountQuote
			if p.balances[quote] < requiredQuote {
				return nil, fmt.Errorf("insufficient paper quote margin for futures %s: requires %.2f %s, have %.2f %s",
					req.Side, requiredQuote, quote, p.balances[quote], quote)
			}
			p.balances[quote] -= requiredQuote
			if existing == nil || existing.Amount <= 1e-9 {
				p.futures[req.Symbol] = &PaperFuturePosition{
					Symbol:       req.Symbol,
					Side:         req.Side,
					Amount:       tradeVolumeAssets,
					EntryPrice:   price,
					MarginLocked: tradeVolumeQuote,
				}
			} else {
				totalAmt := existing.Amount + tradeVolumeAssets
				existing.EntryPrice = ((existing.EntryPrice * existing.Amount) + (price * tradeVolumeAssets)) / totalAmt
				existing.Amount = totalAmt
				existing.MarginLocked += tradeVolumeQuote
			}
		} else {
			// Closing or reducing an existing opposite futures position
			closeAmt := math.Min(existing.Amount, tradeVolumeAssets)
			ratio := closeAmt / existing.Amount
			releasedMargin := existing.MarginLocked * ratio

			var realizedPnL float64
			if existing.Side == BuyOrder {
				// Closing Long with SellOrder
				realizedPnL = (price - existing.EntryPrice) * closeAmt
			} else {
				// Closing Short with BuyOrder
				realizedPnL = (existing.EntryPrice - price) * closeAmt
			}

			netReturned := releasedMargin + realizedPnL - feeAmountQuote
			if netReturned > 0 {
				p.balances[quote] += netReturned
			}

			existing.Amount -= closeAmt
			existing.MarginLocked -= releasedMargin
			if existing.Amount <= 1e-9 {
				delete(p.futures, req.Symbol)
			}
		}

		fmt.Printf("[PAPER FUTURES EXEC] (User %d) %s %s %f %s @ %f %s. Fee: %f %s\n",
			p.userID, p.GetName(), string(req.Side), req.Amount, base, price, quote, feeAmountQuote, quote)

		return &OrderResponse{
			OrderID: orderID,
			Status:  "FILLED",
			Price:   price,
			Amount:  req.Amount,
			Fee:     feeAmountQuote,
		}, nil
	}

	// 2. Spot / Margin Spot Order Accounting
	if req.Side == BuyOrder {
		requiredQuote := tradeVolumeQuote + feeAmountQuote
		if p.balances[quote] < requiredQuote {
			return nil, fmt.Errorf("insufficient paper funds for buy order: requires %.2f %s but have %.2f %s",
				requiredQuote, quote, p.balances[quote], quote)
		}
		p.balances[quote] -= requiredQuote
		p.balances[base] += tradeVolumeAssets
	} else {
		if p.balances[base] < tradeVolumeAssets {
			// Clamp tiny float64 representation drift (up to 0.05% or 1e-5) when closing a position
			if p.balances[base] > 0 && (tradeVolumeAssets-p.balances[base]) <= math.Max(1e-5, tradeVolumeAssets*0.0005) {
				tradeVolumeAssets = p.balances[base]
				req.Amount = tradeVolumeAssets
				tradeVolumeQuote = tradeVolumeAssets * price
				feeAmountQuote = tradeVolumeQuote * feeRate
			} else {
				return nil, fmt.Errorf("insufficient paper funds for sell order: requires %.6f %s but have %.6f %s",
					tradeVolumeAssets, base, p.balances[base], base)
			}
		}
		p.balances[base] -= tradeVolumeAssets
		netQuoteEarned := tradeVolumeQuote - feeAmountQuote
		p.balances[quote] += netQuoteEarned
	}

	fmt.Printf("[PAPER SPOT EXEC] (User %d) %s %s %f %s @ %f %s. Fee: %f %s\n",
		p.userID, p.GetName(), string(req.Side), req.Amount, base, price, quote, feeAmountQuote, quote)

	return &OrderResponse{
		OrderID: orderID,
		Status:  "FILLED",
		Price:   price,
		Amount:  req.Amount,
		Fee:     feeAmountQuote,
	}, nil
}

func (p *PaperClient) CancelOrder(ctx context.Context, orderID string, symbol Symbol) error {
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

func (p *PaperClient) Borrow(ctx context.Context, currency Currency, amount float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := strings.ToUpper(string(currency))
	p.balances[cur] += amount
	p.borrowed[cur] += amount
	fmt.Printf("[PAPER BORROW] (User %d) %s Borrowed %.6f %s (Total Borrowed: %.6f)\n",
		p.userID, p.GetName(), amount, cur, p.borrowed[cur])
	return nil
}

func (p *PaperClient) Repay(ctx context.Context, currency Currency, amount float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := strings.ToUpper(string(currency))
	repayAmt := math.Min(p.balances[cur], amount)
	p.balances[cur] -= repayAmt
	if p.borrowed[cur] >= repayAmt {
		p.borrowed[cur] -= repayAmt
	} else {
		p.borrowed[cur] = 0
	}
	fmt.Printf("[PAPER REPAY] (User %d) %s Repaid %.6f %s (Remaining Liability: %.6f)\n",
		p.userID, p.GetName(), repayAmt, cur, p.borrowed[cur])
	return nil
}

func (p *PaperClient) Deposit(currency string, amount float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.balances[strings.ToUpper(currency)] += amount
}

// CreditPaperDepositByAddress credits a PaperClient's virtual balance when a 2-hop walletProxy transfer
// targets a PAPER-DEPOSIT-{userID}-{exchange}-{currency}-{network} address.
func CreditPaperDepositByAddress(address, currency string, amount float64) bool {
	if !strings.HasPrefix(address, "PAPER-DEPOSIT-") || amount <= 0 {
		return false
	}
	parts := strings.Split(address, "-")
	if len(parts) < 4 {
		return false
	}
	uid, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return false
	}
	destExchange := parts[3]
	paperRegistryMu.RLock()
	destClient, ok := paperRegistry[paperRegistryKey(uint(uid), destExchange)]
	paperRegistryMu.RUnlock()
	if ok && destClient != nil {
		destClient.Deposit(currency, amount)
		return true
	}
	return false
}

// === Transfer API Mocks ===

func (p *PaperClient) GetAPIKeyPermissions(ctx context.Context) (APIKeyPermissions, error) {
	return APIKeyPermissions{
		CanRead:     true,
		CanTrade:    true,
		CanWithdraw: true,
	}, nil
}

func (p *PaperClient) GetDepositAddress(ctx context.Context, currency Currency, network Network) (DepositAddress, error) {
	return DepositAddress{
		Address: fmt.Sprintf("PAPER-DEPOSIT-%d-%s-%s-%s", p.userID, p.GetName(), string(currency), string(network)),
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
	cur := strings.ToUpper(string(req.Currency))
	cost := req.Amount
	if p.balances[cur] < cost {
		p.mu.Unlock()
		return nil, fmt.Errorf("insufficient paper funds for withdrawal: requires %f %s but have %f %s", cost, cur, p.balances[cur], cur)
	}
	p.balances[cur] -= cost
	p.mu.Unlock()

	// Route transfer to the same user's destination PaperClient
	if strings.HasPrefix(req.Address, "PAPER-DEPOSIT-") {
		parts := strings.Split(req.Address, "-")
		// Format: PAPER-DEPOSIT-{userID}-{exchange}-{currency}-{network}
		if len(parts) >= 4 {
			destExchange := parts[3]
			paperRegistryMu.RLock()
			destClient, ok := paperRegistry[paperRegistryKey(p.userID, destExchange)]
			paperRegistryMu.RUnlock()
			if ok {
				destClient.Deposit(cur, req.Amount)
			}
		}
	}

	return &WithdrawResponse{
		WithdrawID: fmt.Sprintf("paper-wd-%d", time.Now().UnixNano()),
		TxID:       "0xpaperhash123",
		Fee:        0.0,
		Amount:     req.Amount,
		Status:     string(WithdrawCompleted),
	}, nil
}

func (p *PaperClient) GetWithdrawHistory(ctx context.Context, currency Currency) ([]WithdrawRecord, error) {
	return []WithdrawRecord{}, nil
}

func (p *PaperClient) CheckWithdrawalStatus(ctx context.Context, withdrawID string) (WithdrawStatus, error) {
	return WithdrawCompleted, nil
}

func (p *PaperClient) GetWithdrawalWhitelist(ctx context.Context, currency Currency) ([]string, error) {
	return []string{"ANY_PAPER_WHITELIST"}, nil
}
