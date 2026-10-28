/*
Package wallet
This file is the interface for an intermediate personal wallet (e.g., Metamask Web3 Wallet) used as a Proxy bypass when direct transfers from overseas to domestic exchanges are unavailable due to Travel Rule or account restrictions.
It encapsulates wallet address mapping and Gas Fee estimations within the arbitrage transfer pipeline.
*/
package wallet

import (
	"context"

	"github.com/magnolia/backend/internal/exchange"
)

// Client defines the contract that each personal wallet implementation (e.g., Metamask, Phantom) must fulfill.
type Client interface {
	GetName() string

	// GetSupportedNetworks returns the bridging networks this personal wallet plugin supports,
	// allowing dynamic querying of expected fees or constraints.
	GetSupportedNetworks(ctx context.Context, currency string) ([]exchange.NetworkInfo, error)

	// GetReceiveAddress provides the on-chain deposit address for a specific currency/network pair
	GetReceiveAddress(ctx context.Context, currency string, network string) (string, error)

	// GetBalance retrieves the available token balances for this personal wallet
	GetBalance(ctx context.Context) ([]exchange.Balance, error)
	
	// CreateTransfer triggers an on-chain transfer to the destination address
	CreateTransfer(ctx context.Context, req TransferRequest) (txHash string, err error)
}

// TransferRequest contains metadata needed to transfer funds out of a personal wallet on-chain.
type TransferRequest struct {
	Currency     string
	Amount       float64
	Network      string
	ToAddress    string
	ToTag        string
}
