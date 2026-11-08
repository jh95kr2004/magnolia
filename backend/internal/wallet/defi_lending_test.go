package wallet

import (
	"context"
	"math"
	"strings"
	"testing"
)

func TestEvaluateDeFiHealthFactor_Guardrails(t *testing.T) {
	// Collateral = $10,000 USDC, LiquidationThreshold = 0.78 -> MaxSafeBorrow at HF 1.30 = $6,000
	maxSafe := ComputeMaxSafeDeFiBorrowUSD(10000, 0.78, 1.30)
	if math.Abs(maxSafe-6000.0) > 1e-6 {
		t.Fatalf("expected MaxSafeBorrowUSD=6000.0, got %.2f", maxSafe)
	}

	// Case 1: Borrowed $5,000 -> HF = 7800 / 5000 = 1.56 (HEALTHY)
	rep1 := EvaluateDeFiHealthFactor(10000, 50, 100.0, 0.78)
	if rep1.Signal != DeFiSignalHealthy {
		t.Fatalf("expected HEALTHY at HF=1.56, got %s", rep1.Signal)
	}

	// Case 2: Coin pumps to $138 -> Borrowed = $6,900 -> HF = 7800 / 6900 = 1.13 (<= 1.15 WARN)
	rep2 := EvaluateDeFiHealthFactor(10000, 50, 138.0, 0.78)
	if rep2.Signal != DeFiSignalWarnTopUp {
		t.Fatalf("expected WARN_TOPUP_COLLATERAL at HF=1.13, got %s (HF=%.2f)", rep2.Signal, rep2.HealthFactor)
	}

	// Case 3: Coin pumps to $145 -> Borrowed = $7,250 -> HF = 7800 / 7250 = 1.075 (<= 1.10 EMERGENCY REPAY!)
	rep3 := EvaluateDeFiHealthFactor(10000, 50, 145.0, 0.78)
	if rep3.Signal != DeFiSignalEmergencyRepay {
		t.Fatalf("expected EMERGENCY_DEFI_REPAY at HF=1.075, got %s (HF=%.2f)", rep3.Signal, rep3.HealthFactor)
	}
	if rep3.RequiredRepayUSD <= 0 {
		t.Fatalf("expected RequiredRepayUSD > 0 during emergency repay, got %.2f", rep3.RequiredRepayUSD)
	}
}

func TestDeFiLendingScanner_AndPersonalWalletGeneration(t *testing.T) {
	scanner := NewDeFiLendingScanner()
	pools, err := scanner.DiscoverPools(context.Background(), "ARB-KRW", 5000)
	if err != nil || len(pools) == 0 {
		t.Fatalf("expected ARB DeFi lending pools (Aave V3 / Radiant), got err=%v", err)
	}
	if pools[0].Protocol != "AAVE_V3" {
		t.Errorf("expected lowest-APY ARB pool to be AAVE_V3, got %s", pools[0].Protocol)
	}

	wallet, err := GenerateOnchainWalletKeypair()
	if err != nil {
		t.Fatalf("GenerateOnchainWalletKeypair failed: %v", err)
	}
	if !strings.HasPrefix(wallet.EVMAddress, "0x") || len(wallet.EVMAddress) != 42 {
		t.Fatalf("invalid generated EVM address: %s", wallet.EVMAddress)
	}
	if !ValidateOnchainAddress("EVM", wallet.EVMAddress) {
		t.Fatalf("ValidateOnchainAddress rejected generated EVM address: %s", wallet.EVMAddress)
	}
	if !ValidateOnchainAddress("SOLANA", wallet.SolanaAddress) {
		t.Fatalf("ValidateOnchainAddress rejected generated Solana address: %s", wallet.SolanaAddress)
	}
}
