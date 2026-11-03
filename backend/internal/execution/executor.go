/*
Package execution
This file is the Executor used by various strategy objects to actually execute orders across both markets.
It provides Atomic Dual Trade rollback capabilities to prevent scenarios where only one side of an order is executed due to network latency or errors, and securely orchestrates multi-step pipelines such as foreign transfers.
*/
package execution

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/magnolia/backend/internal/exchange"
	"github.com/magnolia/backend/internal/wallet"
)

var (
	// ErrTravelRuleManualRequired is returned when an incoming deposit is held by Travel Rule
	// and automatic API verification could not resolve it without human intervention.
	ErrTravelRuleManualRequired = errors.New("deposit requires manual Travel Rule verification")
)

type TransferStepType string

const (
	StepTypeOrder          TransferStepType = "ORDER"
	StepTypeWithdraw       TransferStepType = "WITHDRAW"
	StepTypeWalletTransfer TransferStepType = "WALLET_TRANSFER"
	StepTypeAwaitDeposit   TransferStepType = "AWAIT_DEPOSIT"
	StepTypeBorrow         TransferStepType = "BORROW"
	StepTypeRepay          TransferStepType = "REPAY"
)

// TransferAction represents a single sequence inside a complex Pipeline Trade
type TransferAction struct {
	Type        TransferStepType
	Description string

	// For StepTypeOrder
	ExchangeClient exchange.Client
	OrderReq       *exchange.OrderRequest

	// For StepTypeWithdraw
	WithdrawReq *exchange.WithdrawRequest

	// For StepTypeWalletTransfer
	WalletClient wallet.Client
	WalletReq    *wallet.TransferRequest

	// For StepTypeBorrow / StepTypeRepay / StepTypeAwaitDeposit
	Currency       exchange.Currency
	Amount         float64
	ExpectedAmount float64
	FromExchange   string
	TxID           string
	DepositTimeout time.Duration
}

// Executor coordinates complex multi-exchange trade execution and handles rollbacks.
// It is designed as a lightweight utility meant to be invoked by ArbitrageStrategy instances.
type Executor struct {
	retryOnFailure       bool
	WithdrawPollInterval time.Duration
}

// NewExecutor creates a new pure orchestration utility.
func NewExecutor(retryOnFailure bool) *Executor {
	return &Executor{
		retryOnFailure: retryOnFailure,
	}
}

// ExecuteAtomicDualTrade attempts to place orders on both exchanges simultaneously.
// If one fails, it cancels the successful one and optionally retries based on config.
func (e *Executor) ExecuteAtomicDualTrade(ctx context.Context, baseReq, foreignReq exchange.OrderRequest, baseClient, foreignClient exchange.Client) (*exchange.OrderResponse, *exchange.OrderResponse, error) {
	// Pre-flight balance checks
	if err := e.CheckPreflightBalance(ctx, baseClient, baseReq); err != nil {
		return nil, nil, fmt.Errorf("pre-flight base balance check failed: %w", err)
	}
	if err := e.CheckPreflightBalance(ctx, foreignClient, foreignReq); err != nil {
		return nil, nil, fmt.Errorf("pre-flight foreign balance check failed: %w", err)
	}

	maxAttempts := 1
	if e.retryOnFailure {
		maxAttempts = 3
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		log.Printf("[Executor] Attempt %d/%d to execute dual-leg trade: %s / %s", attempt, maxAttempts, baseReq.Symbol, foreignReq.Symbol)

		// Create channels for async execution
		baseCh := make(chan *exchange.OrderResponse, 1)
		baseErrCh := make(chan error, 1)
		foreignCh := make(chan *exchange.OrderResponse, 1)
		foreignErrCh := make(chan error, 1)

		// Fire Base Order
		go func() {
			res, err := baseClient.PlaceOrder(ctx, baseReq)
			if err != nil {
				baseErrCh <- err
				return
			}
			baseCh <- res
		}()

		// Fire Foreign Order
		go func() {
			res, err := foreignClient.PlaceOrder(ctx, foreignReq)
			if err != nil {
				foreignErrCh <- err
				return
			}
			foreignCh <- res
		}()

		// Await Results
		var baseRes, foreignRes *exchange.OrderResponse
		var baseErr, foreignErr error

		// Wait for both to finish or error out
		for i := 0; i < 2; i++ {
			select {
			case res := <-baseCh:
				baseRes = res
			case err := <-baseErrCh:
				baseErr = err
			case res := <-foreignCh:
				foreignRes = res
			case err := <-foreignErrCh:
				foreignErr = err
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}

		// Success Case: Both executed flawlessly
		if baseErr == nil && foreignErr == nil {
			log.Printf("✅ [Executor] Both legs successful. Base ID: %s, Foreign ID: %s", baseRes.OrderID, foreignRes.OrderID)
			return baseRes, foreignRes, nil
		}

		// Rollback Case: One failed, one succeeded.
		log.Printf("❌ [Executor] Partial Failure detected. Triggering Rollback.")
		if baseRes != nil {
			log.Printf("[Rollback] Canceling/Reversing Base Order %s...", baseRes.OrderID)
			if baseRes.Status == "FILLED" && baseRes.Amount > 0 {
				e.rollbackBaseOrder(ctx, baseReq, baseRes.Amount, baseClient)
			} else if cancelErr := baseClient.CancelOrder(ctx, baseRes.OrderID, baseReq.Symbol); cancelErr != nil {
				log.Printf("CRITICAL [Rollback] Failed to cancel Base Order! Directional Risk Exposed: %v", cancelErr)
			}
		}
		if foreignRes != nil {
			log.Printf("[Rollback] Canceling/Reversing Foreign Order %s...", foreignRes.OrderID)
			if foreignRes.Status == "FILLED" && foreignRes.Amount > 0 {
				e.rollbackBaseOrder(ctx, foreignReq, foreignRes.Amount, foreignClient)
			} else if cancelErr := foreignClient.CancelOrder(ctx, foreignRes.OrderID, foreignReq.Symbol); cancelErr != nil {
				log.Printf("CRITICAL [Rollback] Failed to cancel Foreign Order! Directional Risk Exposed: %v", cancelErr)
			}
		}

		if attempt < maxAttempts {
			log.Printf("[Executor] Waiting 2 seconds before retry...")
			time.Sleep(2 * time.Second)
		} else {
			return nil, nil, fmt.Errorf("dual-leg execution permanently failed after %d attempts. BaseErr: %v, ForeignErr: %v", maxAttempts, baseErr, foreignErr)
		}
	}

	return nil, nil, fmt.Errorf("unexpected execution exit")
}

// ExecutePipeline orchestrates a sequential, multi-step sequence of orders and transfers.
// It tracks the first placed order and automatically rolls it back if any subsequent step fails before completion.
func (e *Executor) ExecutePipeline(ctx context.Context, actions []*TransferAction) error {
	var firstOrderRes *exchange.OrderResponse
	var firstOrderClient exchange.Client
	var firstOrderReq *exchange.OrderRequest

	log.Printf("[Executor] Starting Sequential Pipeline with %d steps.", len(actions))

	for i, action := range actions {
		log.Printf("[Executor] Step %d/%d: %s", i+1, len(actions), action.Description)

		switch action.Type {
		case StepTypeOrder:
			res, err := action.ExchangeClient.PlaceOrder(ctx, *action.OrderReq)
			if err != nil {
				// Rollback if this isn't the first step and a previous order succeeded
				if firstOrderRes != nil {
					log.Printf("❌ [Executor] Step %d Failed. Rolling back initial order...", i+1)
					e.rollbackBaseOrder(ctx, *firstOrderReq, firstOrderRes.Amount, firstOrderClient)
				}
				return fmt.Errorf("step %d failed: %w", i+1, err)
			}
			log.Printf("✅ [Executor] Step %d Order placed successfully. ID: %s", i+1, res.OrderID)
			
			// Save the first successful order for rollback reference
			if firstOrderRes == nil {
				firstOrderRes = res
				firstOrderClient = action.ExchangeClient
				firstOrderReq = action.OrderReq
			}

		case StepTypeWithdraw:
			withdrawRes, err := action.ExchangeClient.Withdraw(ctx, *action.WithdrawReq)
			if err != nil {
				if firstOrderRes != nil {
					e.rollbackBaseOrder(ctx, *firstOrderReq, firstOrderRes.Amount, firstOrderClient)
				}
				return fmt.Errorf("step %d withdraw initiation failed: %w", i+1, err)
			}
			log.Printf("⏳ [Executor] Step %d Withdrawal %s Initiated. Polling network...", i+1, withdrawRes.WithdrawID)

			// Poll for withdrawal success
			pollInterval := e.WithdrawPollInterval
			if pollInterval <= 0 {
				pollInterval = 15 * time.Second
			}
			ticker := time.NewTicker(pollInterval)
			defer ticker.Stop()

			success := false
		PollLoop:
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-ticker.C:
					status, err := action.ExchangeClient.CheckWithdrawalStatus(ctx, withdrawRes.WithdrawID)
					if err != nil {
						log.Printf("[Executor] Warning: failed to check withdraw status: %v", err)
						continue
					}

					if status == exchange.WithdrawCompleted {
						log.Printf("✅ [Executor] Step %d Withdrawal CONFIRMED.", i+1)
						success = true
						break PollLoop
					} else if status == exchange.WithdrawFailed || status == exchange.WithdrawCancelled {
						log.Printf("❌ [Executor] Step %d Withdrawal Failed on network.", i+1)
						break PollLoop
					}
					log.Printf("[Executor] Withdrawal Status: %s. Still waiting...", status)
				}
			}

			if !success {
				if firstOrderRes != nil {
					e.rollbackBaseOrder(ctx, *firstOrderReq, firstOrderRes.Amount, firstOrderClient)
				}
				return fmt.Errorf("step %d withdrawal rejected by network/operator", i+1)
			}

		case StepTypeWalletTransfer:
			var proxyErr error
			var txHash string
			for attempt := 1; attempt <= 3; attempt++ {
				txHash, proxyErr = action.WalletClient.CreateTransfer(ctx, *action.WalletReq)
				if proxyErr == nil {
					log.Printf("✅ [Executor] Step %d Wallet Transfer successful. TX Hash: %s", i+1, txHash)
					break
				}
				log.Printf("❌ [Executor] Step %d Wallet Transfer failed (Attempt %d): %v", i+1, attempt, proxyErr)
				time.Sleep(100 * time.Millisecond)
			}
			if proxyErr != nil {
				log.Printf("CRITICAL [Executor] Proxy Relay stuck in Personal Wallet! Funds safe but unhedged. Manual intervention req.")
				return fmt.Errorf("step %d wallet proxy permanent failure: %w", i+1, proxyErr)
			}

			// If destination is a PaperClient address, credit the virtual deposit immediately
			if action.WalletReq != nil {
				exchange.CreditPaperDepositByAddress(action.WalletReq.ToAddress, action.WalletReq.Currency, action.WalletReq.Amount)
			}

			pollWait := e.WithdrawPollInterval
			if pollWait <= 0 {
				pollWait = 100 * time.Millisecond
			}
			time.Sleep(pollWait)

		case StepTypeAwaitDeposit:
			targetAmt := action.ExpectedAmount
			if targetAmt <= 0 {
				targetAmt = action.Amount
			}
			minArrived := targetAmt * 0.985
			pollInterval := e.WithdrawPollInterval
			if pollInterval <= 0 {
				pollInterval = 5 * time.Second
			}
			timeout := action.DepositTimeout
			if timeout <= 0 {
				timeout = 10 * time.Minute
			}

			deadline := time.Now().Add(timeout)
			arrived := false
			travelRuleVerifiedAttempted := false

			checkArrived := func() (bool, error) {
				balances, err := action.ExchangeClient.GetBalances(ctx)
				if err == nil {
					for _, b := range balances {
						if strings.EqualFold(string(b.Currency), string(action.Currency)) && b.Available >= minArrived {
							return true, nil
						}
					}
				}
				// Attempt automatic Travel Rule OpenAPI verification if supported by destination exchange (e.g., Upbit)
				if verifier, ok := action.ExchangeClient.(exchange.TravelRuleVerifier); ok && !travelRuleVerifiedAttempted {
					travelRuleVerifiedAttempted = true
					status, vErr := verifier.VerifyTravelRuleDeposit(ctx, action.Currency, action.TxID, action.FromExchange)
					if vErr == nil && status != nil {
						log.Printf("[Executor] Travel Rule Auto-Verification result on %s: verified=%v state=%s (%s)",
							action.ExchangeClient.GetName(), status.Verified, status.DepositState, status.Message)
						if !status.Verified && strings.EqualFold(status.DepositState, "TRAVEL_RULE_SUSPECTED") {
							return false, fmt.Errorf("%w: %s", ErrTravelRuleManualRequired, status.Message)
						}
					}
				}
				return false, nil
			}

			if ok, trErr := checkArrived(); trErr != nil {
				return trErr
			} else if ok {
				arrived = true
			}

			if !arrived {
				ticker := time.NewTicker(pollInterval)
				defer ticker.Stop()
			AwaitLoop:
				for time.Now().Before(deadline) {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-ticker.C:
						ok, trErr := checkArrived()
						if trErr != nil {
							return trErr
						}
						if ok {
							arrived = true
							break AwaitLoop
						}
					}
				}
			}

			if !arrived {
				return fmt.Errorf("%w: deposit of %.6f %s did not arrive on %s within timeout", ErrTravelRuleManualRequired, targetAmt, action.Currency, action.ExchangeClient.GetName())
			}
			log.Printf("✅ [Executor] Step %d Deposit of %s confirmed in %s available balance.", i+1, action.Currency, action.ExchangeClient.GetName())

		case StepTypeBorrow:
			err := action.ExchangeClient.Borrow(ctx, action.Currency, action.Amount)
			if err != nil {
				if firstOrderRes != nil {
					e.rollbackBaseOrder(ctx, *firstOrderReq, firstOrderRes.Amount, firstOrderClient)
				}
				return fmt.Errorf("step %d borrow failed: %w", i+1, err)
			}
			log.Printf("✅ [Executor] Step %d Borrow %f %s successful.", i+1, action.Amount, action.Currency)

		case StepTypeRepay:
			err := action.ExchangeClient.Repay(ctx, action.Currency, action.Amount)
			if err != nil {
				if firstOrderRes != nil {
					e.rollbackBaseOrder(ctx, *firstOrderReq, firstOrderRes.Amount, firstOrderClient)
				}
				return fmt.Errorf("step %d repay failed: %w", i+1, err)
			}
			log.Printf("✅ [Executor] Step %d Repay %f %s successful.", i+1, action.Amount, action.Currency)
		}
	}

	log.Printf("🎯 [Executor] PIPELINE OPERATION COMPLETED.")
	return nil
}

// rollbackBaseOrder attempts to execute an opposite market order to neutralize the exposure
func (e *Executor) rollbackBaseOrder(ctx context.Context, originalReq exchange.OrderRequest, filledAmount float64, baseClient exchange.Client) {
	// If the original was BUY, we need to SELL the identical amount.
	rollbackSide := exchange.SellOrder
	if originalReq.Side == exchange.SellOrder {
		rollbackSide = exchange.BuyOrder
	}

	// Create a Market order with the opposite direction (preserving futures type if futures)
	rollbackType := exchange.MarketOrder
	if originalReq.Type == exchange.FuturesLong || originalReq.Type == exchange.FuturesShort {
		rollbackType = originalReq.Type
	}
	rollbackReq := exchange.OrderRequest{
		Symbol: originalReq.Symbol,
		Side:   rollbackSide,
		Type:   rollbackType,
		Price:  0,
		Amount: filledAmount,
	}

	log.Printf("[Rollback] Firing Emergency %s %s...", rollbackReq.Side, rollbackReq.Symbol)
	// We use max retries because leaving a one-sided leg is highly dangerous
	for attempt := 1; attempt <= 3; attempt++ {
		res, err := baseClient.PlaceOrder(ctx, rollbackReq)
		if err == nil {
			log.Printf("✅ [Rollback] Emergency %s executed. Neutralized exposure. OrderID: %s", rollbackSide, res.OrderID)
			return
		}
		log.Printf("[Rollback] Attempt %d failed: %v", attempt, err)
		time.Sleep(1 * time.Second)
	}
	log.Printf("CRITICAL [Rollback] Emergency rollback FAILED completely. Manual intervention required.")
}

func (e *Executor) CheckPreflightBalance(ctx context.Context, client exchange.Client, req exchange.OrderRequest) error {
	// Skip check for mock public-only connections
	if client.GetName() == "public" {
		return nil
	}

	// Futures orders do not require simple spot balance checks (leverage & collateral check is complex),
	// but we can check Spot orders easily.
	if req.Type == exchange.FuturesLong || req.Type == exchange.FuturesShort {
		return nil // Skip complex margin/leverage preflight for futures
	}

	balances, err := client.GetBalances(ctx)
	if err != nil {
		return fmt.Errorf("failed to check balance: %w", err)
	}

	baseCur, quoteCur := exchange.ParseSymbol(req.Symbol)

	// If selling and the account already holds sufficient baseCur (including already-borrowed coins),
	// selling baseCur for quoteCur requires no additional quoteCur balance.
	if req.Side == exchange.SellOrder {
		eps := math.Max(1e-5, req.Amount*0.0005)
		for _, b := range balances {
			if string(b.Currency) == string(baseCur) && b.Available+eps >= req.Amount {
				return nil
			}
		}
	}

	isMarginShort := req.Side == exchange.SellOrder && client.SupportsMargin()

	if req.Side == exchange.BuyOrder || isMarginShort {
		// Buying or Margin Short Selling: Need quote currency (either for buying or as collateral)
		price := req.Price
		if price <= 0 {
			price, err = client.GetTicker(ctx, req.Symbol)
			if err != nil {
				return fmt.Errorf("failed to check ticker price: %w", err)
			}
		}

		// Estimate with a 1% buffer for slippage + taker fees
		requiredQuote := req.Amount * price * 1.01
		isBorrowedBuyback := false
		if req.Side == exchange.BuyOrder {
			if pc, ok := client.(*exchange.PaperClient); ok && pc.GetBorrowedAmount(baseCur) > 1e-9 {
				isBorrowedBuyback = true
			}
		}

		for _, b := range balances {
			if string(b.Currency) == string(quoteCur) {
				usableQuote := b.Available
				if isBorrowedBuyback {
					usableQuote = b.Available + b.Locked
				}
				if usableQuote < requiredQuote {
					return fmt.Errorf("insufficient balance on %s: requires %f %s but have %f %s", client.GetName(), requiredQuote, quoteCur, usableQuote, quoteCur)
				}
				return nil
			}
		}
		return fmt.Errorf("quote currency %s balance not found on %s", quoteCur, client.GetName())
	} else {
		// Selling Spot: Need base currency
		requiredBase := req.Amount
		eps := math.Max(1e-5, requiredBase*0.0005)
		for _, b := range balances {
			if string(b.Currency) == string(baseCur) {
				if b.Available+eps < requiredBase {
					return fmt.Errorf("insufficient balance on %s: requires %f %s but have %f %s", client.GetName(), requiredBase, baseCur, b.Available, baseCur)
				}
				return nil
			}
		}
		return fmt.Errorf("base currency %s balance not found on %s", baseCur, client.GetName())
	}
}

// CheckPreflightWalletGas verifies that the personal wallet (walletProxy) holds sufficient native gas token
// (e.g., ETH for ERC20/Arbitrum/Base, TRX for TRC20, SOL for Solana) BEFORE initiating a 2-hop transfer,
// preventing transferred assets from getting stuck inside the personal wallet.
func (e *Executor) CheckPreflightWalletGas(
	ctx context.Context,
	walletClient wallet.Client,
	network exchange.Network,
	gasToken exchange.Currency,
	minRequiredGas float64,
) error {
	return CheckPreflightWalletGas(ctx, walletClient, network, gasToken, minRequiredGas)
}

func CheckPreflightWalletGas(
	ctx context.Context,
	walletClient wallet.Client,
	network exchange.Network,
	gasToken exchange.Currency,
	minRequiredGas float64,
) error {
	if walletClient == nil || minRequiredGas <= 0 {
		return nil
	}
	bals, err := walletClient.GetBalance(ctx)
	if err != nil {
		return fmt.Errorf("failed to query personal wallet %s native gas (%s on %s): %w", walletClient.GetName(), gasToken, network, err)
	}
	var availGas float64
	for _, b := range bals {
		if strings.EqualFold(string(b.Currency), string(gasToken)) {
			availGas = b.Available
			break
		}
	}
	if availGas < minRequiredGas {
		return fmt.Errorf("insufficient native gas in personal wallet %s for %s relay: requires >= %.6f %s but have %.6f %s (2-hop entry blocked to prevent stuck funds)",
			walletClient.GetName(), network, minRequiredGas, gasToken, availGas, gasToken)
	}
	return nil
}
