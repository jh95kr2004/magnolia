/*
Package wallet
hotwallet_monitor.go implements on-chain behavioral classification to distinguish an exchange's
Active Hot Wallets (which process user withdrawals in real time) from Cold / Reserve Wallets
(which hold long-term custody and do not directly process user withdrawals).
Based on FastCampus Part 02 & Part 06 [55강] (e.g., Binance 14/15/16 Active Hot Wallets vs
Binance 20 Cold Reserve Wallet during the EDEN withdrawal suspension case).
*/
package wallet

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrHotWalletDepleting is returned when active exchange hot wallets are critically low or rapidly depleting,
	// indicating an imminent withdrawal suspension ("출금 닫힘 위험").
	ErrHotWalletDepleting = errors.New("ERR_HOT_WALLET_DEPLETING: active hot wallet liquidity is critically low or depleting; high risk of withdrawal suspension")
)

// ExchangeWalletRole classifies the operational function of an exchange-owned blockchain address.
type ExchangeWalletRole string

const (
	WalletRoleActiveHot    ExchangeWalletRole = "ACTIVE_HOT_WALLET"
	WalletRoleColdReserve  ExchangeWalletRole = "COLD_RESERVE_WALLET"
	WalletRoleDepositSweep ExchangeWalletRole = "DEPOSIT_SWEEP_WALLET"
)

// WalletOnchainStats captures explorer/RPC metrics for a labeled exchange wallet address.
type WalletOnchainStats struct {
	ExchangeName                string
	Address                     string
	Label                       string
	TokenBalance                float64
	BaselineBalance24hAgo       float64
	LastOutboundTxAge           time.Duration
	OutboundTxCount24h          int
	UniqueOutboundRecipients24h int
}

// HotWalletLiquidityReport summarizes the real withdrawable inventory of a token on an exchange.
type HotWalletLiquidityReport struct {
	ExchangeName        string             `json:"exchange_name"`
	ActiveHotBalance    float64            `json:"active_hot_balance"`
	ColdReserveBalance  float64            `json:"cold_reserve_balance"`
	ActiveHotWalletCnt  int                `json:"active_hot_wallet_count"`
	ColdWalletCnt       int                `json:"cold_wallet_count"`
	DepletionRatio24h   float64            `json:"depletion_ratio_24h"` // 0.0 (full) to 1.0 (100% depleted vs 24h baseline)
	RequiredSafetyCoins float64            `json:"required_safety_coins"`
	IsSafeToWithdraw    bool               `json:"is_safe_to_withdraw"`
	WarningMessageKO    string             `json:"warning_message_ko,omitempty"`
	WarningMessageEN    string             `json:"warning_message_en,omitempty"`
}

// ClassifyExchangeWalletRole determines whether an exchange address is an Active Hot Wallet
// or a Cold/Reserve Wallet using a 3-layer heuristic:
// 1. Explicit cold/reserve labels (e.g. "Cold", "Reserve", "Binance 20" long-term vault).
// 2. High-frequency outbound transfers to many unique user addresses (`LastOutboundTxAge <= 6h` && `UniqueOutboundRecipients24h >= 10`).
// 3. Dormant outbound behavior (`LastOutboundTxAge > 24h` or `UniqueOutboundRecipients24h <= 2`, which only sweeps to own hot wallets).
func ClassifyExchangeWalletRole(stats WalletOnchainStats) ExchangeWalletRole {
	lbl := strings.ToLower(strings.TrimSpace(stats.Label))

	// Layer 1: Explicit Cold / Vault label
	if strings.Contains(lbl, "cold") || strings.Contains(lbl, "vault") || strings.Contains(lbl, "reserve") {
		return WalletRoleColdReserve
	}

	// Layer 2: Behavioral Cold Wallet detection (dormant outbound or only transfers to 1~2 internal hot wallets)
	if stats.LastOutboundTxAge > 24*time.Hour || stats.UniqueOutboundRecipients24h <= 2 {
		return WalletRoleColdReserve
	}

	// Layer 3: Behavioral Active Hot Wallet detection (high-frequency outbound to many distinct external user addresses)
	if stats.LastOutboundTxAge <= 6*time.Hour &&
		stats.UniqueOutboundRecipients24h >= 10 &&
		stats.OutboundTxCount24h >= 10 {
		return WalletRoleActiveHot
	}

	// Known active hot wallet labels with recent activity (< 12h)
	if (strings.Contains(lbl, "hot") ||
		strings.Contains(lbl, "binance 14") ||
		strings.Contains(lbl, "binance 15") ||
		strings.Contains(lbl, "binance 16")) && stats.LastOutboundTxAge <= 12*time.Hour {
		return WalletRoleActiveHot
	}

	return WalletRoleDepositSweep
}

// EvaluateHotWalletLiquidity computes the real immediately withdrawable token balance across an exchange's wallets,
// excluding Cold Reserve wallets, and flags imminent withdrawal suspension risk if Active Hot Wallets are depleted.
func EvaluateHotWalletLiquidity(wallets []WalletOnchainStats, requiredTransferCoins float64) (HotWalletLiquidityReport, error) {
	report := HotWalletLiquidityReport{
		RequiredSafetyCoins: requiredTransferCoins * 3.0,
		IsSafeToWithdraw:    true,
	}
	if len(wallets) == 0 {
		return report, nil
	}
	report.ExchangeName = wallets[0].ExchangeName

	var hotBaselineSum float64
	for _, w := range wallets {
		role := ClassifyExchangeWalletRole(w)
		switch role {
		case WalletRoleActiveHot:
			report.ActiveHotBalance += w.TokenBalance
			report.ActiveHotWalletCnt++
			if w.BaselineBalance24hAgo > 0 {
				hotBaselineSum += w.BaselineBalance24hAgo
			}
		case WalletRoleColdReserve:
			report.ColdReserveBalance += w.TokenBalance
			report.ColdWalletCnt++
		}
	}

	if hotBaselineSum > 0 && report.ActiveHotBalance < hotBaselineSum {
		report.DepletionRatio24h = 1.0 - (report.ActiveHotBalance / hotBaselineSum)
	}

	// Safety check 1: Active hot wallet balance must be at least 3x the intended withdrawal amount
	if requiredTransferCoins > 0 && report.ActiveHotBalance < report.RequiredSafetyCoins {
		report.IsSafeToWithdraw = false
		report.WarningMessageKO = fmt.Sprintf(
			"출금 정지 위험: %s 활성 핫월렛 잔고(%.2f)가 필요 안전 수량(%.2f) 미달 (콜드월렛 %.2f개는 즉시 출금 불가)",
			report.ExchangeName, report.ActiveHotBalance, report.RequiredSafetyCoins, report.ColdReserveBalance,
		)
		report.WarningMessageEN = fmt.Sprintf(
			"Withdrawal suspension risk: %s active hot wallet balance (%.2f) is below safety threshold (%.2f); cold reserve (%.2f) is not immediately withdrawable",
			report.ExchangeName, report.ActiveHotBalance, report.RequiredSafetyCoins, report.ColdReserveBalance,
		)
		return report, fmt.Errorf("%w: %s", ErrHotWalletDepleting, report.WarningMessageEN)
	}

	// Safety check 2: Rapid depletion (>= 85% drained in 24h) and remaining buffer < 10x required amount
	if report.DepletionRatio24h >= 0.85 && requiredTransferCoins > 0 && report.ActiveHotBalance < requiredTransferCoins*10.0 {
		report.IsSafeToWithdraw = false
		report.WarningMessageKO = fmt.Sprintf(
			"핫월렛 급격 고갈 경고: %s 핫월렛 물량이 24시간 대비 %.1f%% 소진됨 (출금 일시중단 임박)",
			report.ExchangeName, report.DepletionRatio24h*100.0,
		)
		report.WarningMessageEN = fmt.Sprintf(
			"Rapid hot wallet depletion: %.1f%% of %s hot wallet drained in 24h",
			report.DepletionRatio24h*100.0, report.ExchangeName,
		)
		return report, fmt.Errorf("%w: %s", ErrHotWalletDepleting, report.WarningMessageEN)
	}

	return report, nil
}
