package strategy

import (
	"errors"
	"testing"

	"github.com/magnolia/backend/internal/exchange"
)

func TestVerifyTokenContractMatch_BlocksSameTickerCollision(t *testing.T) {
	// Case 1: Matching full contract address (case-insensitive)
	src := exchange.NetworkInfo{
		Network:         "ERC20",
		ContractAddress: "0x6985884C4392D348587B19CB9EAAF157F13271CD",
	}
	dst := exchange.NetworkInfo{
		Network:         "ERC20",
		ContractAddress: "0x6985884c4392d348587b19cb9eaaf157f13271cd",
	}
	if err := VerifyTokenContractMatch(src, dst); err != nil {
		t.Fatalf("expected matching contracts to pass, got error: %v", err)
	}

	// Case 2: Suffix match (when an exchange only exposes last 6 chars like "...3271cd")
	dstSuffix := exchange.NetworkInfo{
		Network:         "ERC20",
		ContractAddress: "...3271cd",
	}
	if err := VerifyTokenContractMatch(src, dstSuffix); err != nil {
		t.Fatalf("expected matching 6-char suffix to pass, got error: %v", err)
	}

	// Case 3: Same ticker (e.g. "G" or "NEIRO") but completely different token contract -> MUST BLOCK!
	dstCollision := exchange.NetworkInfo{
		Network:         "ERC20",
		ContractAddress: "0x1111111111111111111111111111111111119999",
	}
	err := VerifyTokenContractMatch(src, dstCollision)
	if !errors.Is(err, ErrContractAddressMismatch) {
		t.Fatalf("expected ErrContractAddressMismatch for colliding ticker contracts, got: %v", err)
	}
}

func TestVerifyCosmosMemoRequired(t *testing.T) {
	seiNet := exchange.NetworkInfo{
		Network: "SEI",
	}
	if err := VerifyCosmosMemoRequired(seiNet, ""); !errors.Is(err, ErrCosmosMemoMissing) {
		t.Fatalf("expected ErrCosmosMemoMissing when depositing SEI without memo, got: %v", err)
	}
	if err := VerifyCosmosMemoRequired(seiNet, "10492831"); err != nil {
		t.Fatalf("expected SEI deposit with memo to pass, got: %v", err)
	}
}

func TestGetVASPTier_UpbitVsBithumbAsymmetry(t *testing.T) {
	// Upbit blocks direct Coinbase deposits >= 1M KRW (Tier 3 -> requires 2-hop personal wallet)
	if tier := GetVASPTier("UPBIT", "COINBASE"); tier != exchange.VASPTier3RiskAssessed {
		t.Fatalf("expected UPBIT<-COINBASE to be VASPTier3RiskAssessed, got %s", tier)
	}
	// Bithumb supports direct 1-hop Travel Rule with Coinbase & Kraken (Tier 2)
	if tier := GetVASPTier("BITHUMB", "COINBASE"); tier != exchange.VASPTier2OwnerLinked {
		t.Fatalf("expected BITHUMB<-COINBASE to be VASPTier2OwnerLinked, got %s", tier)
	}
	if tier := GetVASPTier("BITHUMB", "KRAKEN"); tier != exchange.VASPTier2OwnerLinked {
		t.Fatalf("expected BITHUMB<-KRAKEN to be VASPTier2OwnerLinked, got %s", tier)
	}
}

func TestFindOptimalTransferRoute_CEXAsABridge(t *testing.T) {
	// Scenario from Part 02 [12강]:
	// Source (Binance) only has Polygon (MATIC) open for GMT withdrawal,
	// while Target (Upbit) only accepts Solana (SOL) for GMT deposit!
	// Bybit supports both Polygon deposit AND Solana withdrawal -> CEX-as-a-Bridge route with 0% swap fee!
	req := BridgeRouteRequest{
		Currency:          "GMT",
		CoinPriceUSD:      0.20,
		TransferAmountUSD: 5000.0,
		SourceExchange:    "BINANCE",
		TargetExchange:    "UPBIT",
		SourceNetworks: []exchange.NetworkInfo{
			{Network: "POLYGON", IsDepositEnable: true, IsWithdrawEnable: true, WithdrawFeeFixed: 1.0, EstimatedTime: 90},
		},
		TargetNetworks: []exchange.NetworkInfo{
			{Network: "SOLANA", IsDepositEnable: true, IsWithdrawEnable: true, WithdrawFeeFixed: 2.0, EstimatedTime: 60},
		},
		RelayExchanges: []RelayExchangeSpec{
			{
				ExchangeName: "BYBIT",
				Networks: []exchange.NetworkInfo{
					{Network: "POLYGON", IsDepositEnable: true, IsWithdrawEnable: true, WithdrawFeeFixed: 1.0, EstimatedTime: 90},
					{Network: "SOLANA", IsDepositEnable: true, IsWithdrawEnable: true, WithdrawFeeFixed: 1.5, EstimatedTime: 60},
				},
			},
		},
		KimpDecayPerMin: 0.0002,
	}

	route, err := FindOptimalTransferRoute(req)
	if err != nil {
		t.Fatalf("expected CEX-as-a-Bridge route to be found, got error: %v", err)
	}
	if route.RouteType != RouteCEXBridge2Hop {
		t.Fatalf("expected RouteCEXBridge2Hop, got %s", route.RouteType)
	}
	if route.RelayExchange != "BYBIT" {
		t.Fatalf("expected relay exchange BYBIT, got %s", route.RelayExchange)
	}
	if route.NeedsWalletProxy {
		t.Fatalf("expected NeedsWalletProxy=false for BYBIT->UPBIT (Tier 2 VASP)")
	}
}
