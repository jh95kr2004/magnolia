package exchange

import (
	"testing"
)

func TestDynamicMemoAndGlobalChainSpecRegistry(t *testing.T) {
	// A coin not in the static fallback list should start as false
	customChain := Network("NEWCOSMOSCHAIN2026")
	if IsMemoRequiredNetwork(customChain) {
		t.Fatalf("expected %s to not require memo before registration", customChain)
	}

	// Registering it dynamically (as EnsureGlobalCoinMetadataLoaded does from KuCoin V3 / Gate.io V4) flips it to true
	RegisterMemoRequiredNetwork(customChain)
	if !IsMemoRequiredNetwork(customChain) {
		t.Fatalf("expected %s to require memo after dynamic registration", customChain)
	}

	// Inject a mock global chain spec for a non-Coinbase altcoin
	globalMetaMu.Lock()
	globalChainSpecs["INIT"] = []GlobalChainSpec{
		{
			Currency:         "INIT",
			ChainName:        "INITIA",
			ContractAddress:  "0xinitia1234",
			RequiresMemo:     true,
			MemoRegex:        "^.{1,256}$",
			MinConfirmations: 15,
			WithdrawFeeFixed: 0.1,
		},
	}
	globalMetaMu.Unlock()

	spec, ok := LookupGlobalChainSpec("INIT", "INITIA")
	if !ok {
		t.Fatalf("expected LookupGlobalChainSpec to find INIT")
	}
	if !spec.RequiresMemo || spec.ContractAddress != "0xinitia1234" || spec.MinConfirmations != 15 {
		t.Fatalf("unexpected spec for INIT: %+v", spec)
	}
}
