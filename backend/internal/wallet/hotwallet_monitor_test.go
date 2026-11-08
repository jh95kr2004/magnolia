package wallet

import (
	"errors"
	"testing"
	"time"
)

func TestClassifyExchangeWalletRole_HotVsCold(t *testing.T) {
	// Binance 20: Holds 12,000,000 EDEN, but last outbound tx was 47 days ago to only 1 recipient (internal hot wallet)
	binance20 := WalletOnchainStats{
		ExchangeName:                "BINANCE",
		Address:                     "0xF977814e90dA44bFA03b6295A0616a897441aceC",
		Label:                       "Binance 20",
		TokenBalance:                12000000,
		BaselineBalance24hAgo:       12000000,
		LastOutboundTxAge:           47 * 24 * time.Hour,
		OutboundTxCount24h:          0,
		UniqueOutboundRecipients24h: 0,
	}
	if role := ClassifyExchangeWalletRole(binance20); role != WalletRoleColdReserve {
		t.Fatalf("expected Binance 20 to be classified as COLD_RESERVE_WALLET, got %s", role)
	}

	// Binance 14: Active hot wallet sending tokens to 85 unique user addresses every few minutes
	binance14 := WalletOnchainStats{
		ExchangeName:                "BINANCE",
		Address:                     "0x28C6c06298d514Db089934071355E5743bf21d60",
		Label:                       "Binance 14",
		TokenBalance:                15000,
		BaselineBalance24hAgo:       500000,
		LastOutboundTxAge:           4 * time.Minute,
		OutboundTxCount24h:          140,
		UniqueOutboundRecipients24h: 85,
	}
	if role := ClassifyExchangeWalletRole(binance14); role != WalletRoleActiveHot {
		t.Fatalf("expected Binance 14 to be classified as ACTIVE_HOT_WALLET, got %s", role)
	}
}

func TestEvaluateHotWalletLiquidity_DetectsDepletionDespiteFullColdWallet(t *testing.T) {
	wallets := []WalletOnchainStats{
		{
			ExchangeName:                "BINANCE",
			Label:                       "Binance 20",
			TokenBalance:                12000000, // Huge cold reserve!
			BaselineBalance24hAgo:       12000000,
			LastOutboundTxAge:           30 * 24 * time.Hour,
			OutboundTxCount24h:          0,
			UniqueOutboundRecipients24h: 0,
		},
		{
			ExchangeName:                "BINANCE",
			Label:                       "Binance 14",
			TokenBalance:                4000, // Almost depleted active hot wallet!
			BaselineBalance24hAgo:       200000,
			LastOutboundTxAge:           2 * time.Minute,
			OutboundTxCount24h:          95,
			UniqueOutboundRecipients24h: 60,
		},
	}

	// Attempting to buy & withdraw 5,000 coins when active hot wallet only has 4,000 left -> MUST BLOCK!
	report, err := EvaluateHotWalletLiquidity(wallets, 5000)
	if !errors.Is(err, ErrHotWalletDepleting) {
		t.Fatalf("expected ErrHotWalletDepleting when active hot wallet has 4000 coins for 5000 coin transfer, got: %v", err)
	}
	if report.IsSafeToWithdraw {
		t.Fatalf("expected IsSafeToWithdraw=false")
	}
	if report.ColdReserveBalance != 12000000 {
		t.Errorf("expected ColdReserveBalance=12000000, got %f", report.ColdReserveBalance)
	}
	if report.ActiveHotBalance != 4000 {
		t.Errorf("expected ActiveHotBalance=4000, got %f", report.ActiveHotBalance)
	}
}
