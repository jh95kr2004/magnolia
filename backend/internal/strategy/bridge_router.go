/*
Package strategy
bridge_router.go implements Part 02 [12~14강] core transfer safety guardrails and multi-hop routing:
1. VerifyTokenContractMatch: Prevents same-ticker different-contract ("동명이인 코인") deposit losses.
2. VerifyCosmosMemoRequired: Prevents Cosmos SDK / Tag chain deposits without a Memo ("입금 락업").
3. FindOptimalTransferRoute: Evaluates Direct 1-Hop, Personal Wallet 2-Hop (Travel Rule),
   CEX-as-a-Bridge ("거래소 브릿지" — 0% swap fee & 0% price slippage), and On-Chain Bridges
   (Wormhole, Stargate, Orbiter, Rhino.fi with >= 10x TVL liquidity check) to select the path
   with the lowest effective cost (fees + time decay).
*/
package strategy

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/magnolia/backend/internal/exchange"
)

var (
	// ErrContractAddressMismatch is returned when source and destination token contract addresses differ.
	ErrContractAddressMismatch = errors.New("ERR_CONTRACT_MISMATCH: source and destination token contract addresses do not match (same-ticker collision blocked)")

	// ErrCosmosMemoMissing is returned when a Cosmos SDK or tag-based network transfer is attempted without a memo/tag.
	ErrCosmosMemoMissing = errors.New("ERR_MEMO_REQUIRED: destination network requires a Memo/Destination Tag for CEX deposit")

	// ErrNoValidTransferRoute is returned when neither direct, CEX-bridge, nor liquid on-chain bridge routes are open.
	ErrNoValidTransferRoute = errors.New("ERR_NO_TRANSFER_ROUTE: no open transfer or bridge route found between source and destination")
)

// TransferRouteType classifies how an asset is transferred from Foreign to Base exchange.
type TransferRouteType string

const (
	RouteDirect1Hop         TransferRouteType = "DIRECT_1HOP"          // Foreign CEX -> Base CEX directly (Tier 1/2 VASP, matching chain)
	RoutePersonalWallet2Hop TransferRouteType = "WALLET_PROXY_2HOP"    // Foreign CEX -> Personal Wallet -> Base CEX (Tier 3 VASP Travel Rule bypass)
	RouteCEXBridge2Hop      TransferRouteType = "CEX_AS_A_BRIDGE_2HOP" // Source (Chain A) -> Relay CEX (Chain A in, Chain B out, 0% swap fee) -> Base CEX (Chain B)
	RouteOnChainBridge      TransferRouteType = "ONCHAIN_BRIDGE"       // Source (Chain A) -> On-Chain Bridge (Chain A->B) -> Personal Wallet -> Base CEX (Chain B)
)

// RelayExchangeSpec describes a candidate relay exchange ("거래소 브릿지") and its supported networks for a coin.
type RelayExchangeSpec struct {
	ExchangeName string
	Networks     []exchange.NetworkInfo
}

// OnChainBridgeQuote describes an available on-chain cross-chain bridge pool (e.g. Wormhole, Stargate, Orbiter, Rhino.fi).
type OnChainBridgeQuote struct {
	BridgeName       string // e.g. "WORMHOLE_PORTAL", "STARGATE", "ORBITER", "RHINO_FI"
	FromNetwork      exchange.Network
	ToNetwork        exchange.Network
	PoolLiquidityUSD float64 // Available destination pool liquidity in USD
	BridgeFeeUSD     float64 // Total bridge protocol + gas fee in USD
	SlippageRate     float64 // Estimated bridge swap slippage (e.g. 0.001 for 0.1%)
	EstimatedTimeSec int     // Estimated cross-chain finality time in seconds
}

// BridgeRouteRequest contains all inputs needed to compute the optimal cross-exchange transfer route.
type BridgeRouteRequest struct {
	Currency           exchange.Currency
	CoinPriceUSD       float64
	TransferAmountUSD  float64
	SourceExchange     string
	TargetExchange     string
	SourceNetworks     []exchange.NetworkInfo
	TargetNetworks     []exchange.NetworkInfo
	RelayExchanges     []RelayExchangeSpec
	OnChainBridges     []OnChainBridgeQuote
	WalletGasFeeUSD    float64 // Gas fee for relaying through personal wallet (default ~$0.50)
	WalletRelayTimeSec int     // Confirmation time for personal wallet relay (default 60s)
	KimpDecayPerMin    float64 // Expected Kimp decay ratio per minute during transit (e.g. 0.0003 for 0.03%/min)
}

// OptimalTransferRoute represents the selected lowest-cost, safety-verified transfer path.
type OptimalTransferRoute struct {
	RouteType          TransferRouteType `json:"route_type"`
	SourceNetwork      exchange.Network  `json:"source_network"`
	TargetNetwork      exchange.Network  `json:"target_network"`
	RelayExchange      string            `json:"relay_exchange,omitempty"`
	OnChainBridgeName  string            `json:"onchain_bridge_name,omitempty"`
	NeedsWalletProxy   bool              `json:"needs_wallet_proxy"`
	VASPTier           exchange.VASPTier `json:"vasp_tier"`
	TotalFeeUSD        float64           `json:"total_fee_usd"`
	TotalEstimatedTime time.Duration     `json:"total_estimated_time"`
	EffectiveCostUSD   float64           `json:"effective_cost_usd"` // TotalFeeUSD + expected Kimp decay cost + bridge slippage
	DescriptionKO      string            `json:"description_ko"`
	DescriptionEN      string            `json:"description_en"`
}

// VerifyTokenContractMatch verifies that two NetworkInfo entries do not point to different token smart contracts.
// Supports full address comparison (`0x...`) as well as suffix comparison (when an exchange UI/API only provides the last 6 chars).
func VerifyTokenContractMatch(srcNet, dstNet exchange.NetworkInfo) error {
	a := normalizeContractAddr(srcNet.ContractAddress)
	b := normalizeContractAddr(dstNet.ContractAddress)
	if a == "" || b == "" {
		// If either side is native L1 or does not specify a contract address, cannot mismatch on contract string
		return nil
	}
	if a == b {
		return nil
	}
	// If one of the addresses is a suffix (e.g. last 6~8 chars exposed by exchange deposit page)
	if len(a) >= 6 && len(b) >= 6 {
		if len(a) < len(b) && strings.HasSuffix(b, a) {
			return nil
		}
		if len(b) < len(a) && strings.HasSuffix(a, b) {
			return nil
		}
	}
	return fmt.Errorf("%w: src(%s)=%s vs dst(%s)=%s",
		ErrContractAddressMismatch, srcNet.Network, srcNet.ContractAddress, dstNet.Network, dstNet.ContractAddress)
}

// VerifyCosmosMemoRequired ensures that depositing to a CEX on a Cosmos SDK or tag-based network includes a non-empty Memo/Tag.
func VerifyCosmosMemoRequired(dstNet exchange.NetworkInfo, memo string) error {
	needsMemo := dstNet.RequiresMemo ||
		exchange.IsMemoRequiredNetwork(dstNet.Network) ||
		exchange.IsMemoRequiredNetwork(exchange.Network(dstNet.ExchangeNetID))
	if needsMemo && strings.TrimSpace(memo) == "" {
		return fmt.Errorf("%w: network=%s", ErrCosmosMemoMissing, dstNet.Network)
	}
	return nil
}

// FindOptimalTransferRoute evaluates all valid 1-Hop, 2-Hop Personal Wallet, 2-Hop CEX-as-a-Bridge, and On-Chain Bridge routes
// and returns the route that minimizes EffectiveCostUSD (direct fees + slippage + time decay).
func FindOptimalTransferRoute(req BridgeRouteRequest) (*OptimalTransferRoute, error) {
	if req.TransferAmountUSD <= 0 {
		req.TransferAmountUSD = 2500.0
	}
	if req.CoinPriceUSD <= 0 {
		req.CoinPriceUSD = 1.0
	}
	if req.WalletGasFeeUSD <= 0 {
		req.WalletGasFeeUSD = 0.50
	}
	if req.WalletRelayTimeSec <= 0 {
		req.WalletRelayTimeSec = 60
	}
	if req.KimpDecayPerMin < 0 {
		req.KimpDecayPerMin = 0
	}

	vaspTier := GetVASPTier(req.TargetExchange, req.SourceExchange)
	needsWalletForDirect := vaspTier == exchange.VASPTier3RiskAssessed

	var candidates []OptimalTransferRoute

	// 1. Check same-network routes (Direct 1-Hop or Personal Wallet 2-Hop)
	for _, sNet := range req.SourceNetworks {
		if !sNet.IsWithdrawEnable {
			continue
		}
		for _, tNet := range req.TargetNetworks {
			if !tNet.IsDepositEnable {
				continue
			}
			if !networksMatch(sNet, tNet) {
				continue
			}
			if err := VerifyTokenContractMatch(sNet, tNet); err != nil {
				continue
			}

			wdFeeUSD := sNet.EffectiveWithdrawFeeUSD(req.CoinPriceUSD, req.TransferAmountUSD)
			estSec := sNet.EstimatedTime
			if tNet.EstimatedTime > estSec {
				estSec = tNet.EstimatedTime
			}
			if estSec <= 0 {
				estSec = 180
			}

			if !needsWalletForDirect {
				effCost := computeEffectiveRouteCost(wdFeeUSD, 0, estSec, req.TransferAmountUSD, req.KimpDecayPerMin)
				candidates = append(candidates, OptimalTransferRoute{
					RouteType:          RouteDirect1Hop,
					SourceNetwork:      sNet.Network,
					TargetNetwork:      tNet.Network,
					NeedsWalletProxy:   false,
					VASPTier:           vaspTier,
					TotalFeeUSD:        wdFeeUSD,
					TotalEstimatedTime: time.Duration(estSec) * time.Second,
					EffectiveCostUSD:   effCost,
					DescriptionKO:      fmt.Sprintf("%s ➔ %s (%s 1-Hop 직통, 트래블룰 %s)", req.SourceExchange, req.TargetExchange, sNet.Network, vaspTier),
					DescriptionEN:      fmt.Sprintf("%s -> %s (%s Direct 1-Hop, %s)", req.SourceExchange, req.TargetExchange, sNet.Network, vaspTier),
				})
			} else {
				totalFee := wdFeeUSD + req.WalletGasFeeUSD
				totalSec := estSec + req.WalletRelayTimeSec
				effCost := computeEffectiveRouteCost(totalFee, 0, totalSec, req.TransferAmountUSD, req.KimpDecayPerMin)
				candidates = append(candidates, OptimalTransferRoute{
					RouteType:          RoutePersonalWallet2Hop,
					SourceNetwork:      sNet.Network,
					TargetNetwork:      tNet.Network,
					NeedsWalletProxy:   true,
					VASPTier:           vaspTier,
					TotalFeeUSD:        totalFee,
					TotalEstimatedTime: time.Duration(totalSec) * time.Second,
					EffectiveCostUSD:   effCost,
					DescriptionKO:      fmt.Sprintf("%s ➔ 개인지갑 ➔ %s (%s 2-Hop 트래블룰 경유)", req.SourceExchange, req.TargetExchange, sNet.Network),
					DescriptionEN:      fmt.Sprintf("%s -> Personal Wallet -> %s (%s 2-Hop Travel Rule Relay)", req.SourceExchange, req.TargetExchange, sNet.Network),
				})
			}
		}
	}

	// 2. Check CEX-as-a-Bridge 2-Hop routes ("거래소 브릿지": Source Chain A -> Relay CEX -> Target Chain B)
	// Central exchanges credit unified token balances across chains with 0% swap fee and 0% price slippage!
	for _, relay := range req.RelayExchanges {
		relayName := strings.ToUpper(strings.TrimSpace(relay.ExchangeName))
		if relayName == "" ||
			strings.EqualFold(relayName, req.SourceExchange) ||
			strings.EqualFold(relayName, req.TargetExchange) {
			continue
		}
		relayVASPTier := GetVASPTier(req.TargetExchange, relayName)
		relayNeedsWallet := relayVASPTier == exchange.VASPTier3RiskAssessed

		for _, sNet := range req.SourceNetworks {
			if !sNet.IsWithdrawEnable {
				continue
			}
			for _, rInNet := range relay.Networks {
				if !rInNet.IsDepositEnable || !networksMatch(sNet, rInNet) {
					continue
				}
				if err := VerifyTokenContractMatch(sNet, rInNet); err != nil {
					continue
				}

				// Now check all outbound networks from Relay CEX to Target CEX
				for _, rOutNet := range relay.Networks {
					if !rOutNet.IsWithdrawEnable {
						continue
					}
					for _, tNet := range req.TargetNetworks {
						if !tNet.IsDepositEnable || !networksMatch(rOutNet, tNet) {
							continue
						}
						if err := VerifyTokenContractMatch(rOutNet, tNet); err != nil {
							continue
						}

						hop1Fee := sNet.EffectiveWithdrawFeeUSD(req.CoinPriceUSD, req.TransferAmountUSD)
						hop2Fee := rOutNet.EffectiveWithdrawFeeUSD(req.CoinPriceUSD, req.TransferAmountUSD)
						totalFee := hop1Fee + hop2Fee
						hop1Sec := maxInt(sNet.EstimatedTime, rInNet.EstimatedTime, 120)
						hop2Sec := maxInt(rOutNet.EstimatedTime, tNet.EstimatedTime, 120)
						totalSec := hop1Sec + hop2Sec

						if relayNeedsWallet {
							totalFee += req.WalletGasFeeUSD
							totalSec += req.WalletRelayTimeSec
						}

						effCost := computeEffectiveRouteCost(totalFee, 0, totalSec, req.TransferAmountUSD, req.KimpDecayPerMin)
						candidates = append(candidates, OptimalTransferRoute{
							RouteType:          RouteCEXBridge2Hop,
							SourceNetwork:      sNet.Network,
							TargetNetwork:      tNet.Network,
							RelayExchange:      relayName,
							NeedsWalletProxy:   relayNeedsWallet,
							VASPTier:           relayVASPTier,
							TotalFeeUSD:        totalFee,
							TotalEstimatedTime: time.Duration(totalSec) * time.Second,
							EffectiveCostUSD:   effCost,
							DescriptionKO:      fmt.Sprintf("거래소 브릿지: %s(%s) ➔ %s(스왑수수료 0%%) ➔ %s(%s)", req.SourceExchange, sNet.Network, relayName, req.TargetExchange, tNet.Network),
							DescriptionEN:      fmt.Sprintf("CEX Bridge: %s(%s) -> %s(0%% swap fee) -> %s(%s)", req.SourceExchange, sNet.Network, relayName, req.TargetExchange, tNet.Network),
						})
					}
				}
			}
		}
	}

	// 3. Check On-Chain Bridge routes (Wormhole, Stargate, Orbiter, Rhino.fi)
	// Hard guardrail from Part 02 [11~12강]: PoolLiquidityUSD must be >= 10x TransferAmountUSD to prevent severe slippage/depeg.
	for _, bQuote := range req.OnChainBridges {
		if bQuote.PoolLiquidityUSD < req.TransferAmountUSD*10.0 {
			continue
		}
		for _, sNet := range req.SourceNetworks {
			if !sNet.IsWithdrawEnable || !networkNameEquals(sNet.Network, bQuote.FromNetwork) {
				continue
			}
			for _, tNet := range req.TargetNetworks {
				if !tNet.IsDepositEnable || !networkNameEquals(tNet.Network, bQuote.ToNetwork) {
					continue
				}

				wdFeeUSD := sNet.EffectiveWithdrawFeeUSD(req.CoinPriceUSD, req.TransferAmountUSD)
				totalFee := wdFeeUSD + bQuote.BridgeFeeUSD + req.WalletGasFeeUSD
				totalSec := maxInt(sNet.EstimatedTime, 60) + maxInt(bQuote.EstimatedTimeSec, 60) + req.WalletRelayTimeSec
				effCost := computeEffectiveRouteCost(totalFee, bQuote.SlippageRate, totalSec, req.TransferAmountUSD, req.KimpDecayPerMin)

				candidates = append(candidates, OptimalTransferRoute{
					RouteType:          RouteOnChainBridge,
					SourceNetwork:      sNet.Network,
					TargetNetwork:      tNet.Network,
					OnChainBridgeName:  bQuote.BridgeName,
					NeedsWalletProxy:   true,
					VASPTier:           vaspTier,
					TotalFeeUSD:        totalFee,
					TotalEstimatedTime: time.Duration(totalSec) * time.Second,
					EffectiveCostUSD:   effCost,
					DescriptionKO:      fmt.Sprintf("온체인 브릿지(%s): %s(%s) ➔ %s(%s)", bQuote.BridgeName, req.SourceExchange, sNet.Network, req.TargetExchange, tNet.Network),
					DescriptionEN:      fmt.Sprintf("On-Chain Bridge(%s): %s(%s) -> %s(%s)", bQuote.BridgeName, req.SourceExchange, sNet.Network, req.TargetExchange, tNet.Network),
				})
			}
		}
	}

	if len(candidates) == 0 {
		return nil, ErrNoValidTransferRoute
	}

	// Pick candidate with minimum EffectiveCostUSD
	bestIdx := 0
	for i := 1; i < len(candidates); i++ {
		if candidates[i].EffectiveCostUSD < candidates[bestIdx].EffectiveCostUSD {
			bestIdx = i
		}
	}
	best := candidates[bestIdx]
	return &best, nil
}

func normalizeContractAddr(addr string) string {
	cleaned := strings.ToLower(strings.TrimSpace(addr))
	cleaned = strings.TrimPrefix(cleaned, "0x")
	cleaned = strings.TrimPrefix(cleaned, "...")
	return cleaned
}

func networksMatch(a, b exchange.NetworkInfo) bool {
	if networkNameEquals(a.Network, b.Network) {
		return true
	}
	if a.ExchangeNetID != "" && b.ExchangeNetID != "" &&
		networkNameEquals(exchange.Network(a.ExchangeNetID), exchange.Network(b.ExchangeNetID)) {
		return true
	}
	return false
}

func networkNameEquals(a, b exchange.Network) bool {
	return canonicalNetworkID(string(a)) == canonicalNetworkID(string(b))
}

func canonicalNetworkID(net string) string {
	u := strings.ToUpper(strings.TrimSpace(net))
	switch u {
	case "ETH", "ERC20", "ETHEREUM":
		return "ERC20"
	case "TRX", "TRC20", "TRON":
		return "TRC20"
	case "BSC", "BEP20", "BNB":
		return "BEP20"
	case "ARB", "ARBITRUM", "ARBITRUM_ONE":
		return "ARBITRUM"
	case "OP", "OPTIMISM":
		return "OPTIMISM"
	case "MATIC", "POLYGON", "POL":
		return "POLYGON"
	case "SOL", "SOLANA", "SPL":
		return "SOLANA"
	case "AVAX", "AVAXC", "AVALANCHE":
		return "AVAXC"
	default:
		return u
	}
}

func computeEffectiveRouteCost(feeUSD, slippageRate float64, totalTimeSec int, amountUSD, decayPerMin float64) float64 {
	slippageCost := amountUSD * math.Max(0, slippageRate)
	minutes := float64(totalTimeSec) / 60.0
	decayCost := amountUSD * math.Max(0, decayPerMin) * minutes
	return feeUSD + slippageCost + decayCost
}

func maxInt(vals ...int) int {
	m := 0
	for _, v := range vals {
		if v > m {
			m = v
		}
	}
	return m
}
