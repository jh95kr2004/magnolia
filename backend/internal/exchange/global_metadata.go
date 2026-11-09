package exchange

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GlobalChainSpec holds global coin/chain metadata aggregated from multi-exchange public registries
// (KuCoin V3 /api/v3/currencies [2,200+ coins], Gate.io V4 /api/v4/spot/currencies [5,700+ coins],
// Upbit Public CCX Wallet Status [400+ coins], and Coinbase /currencies).
type GlobalChainSpec struct {
	Currency         Currency
	ChainName        string
	ContractAddress  string
	RequiresMemo     bool
	MemoRegex        string
	MinConfirmations int
	WithdrawFeeFixed float64
}

// UpbitPublicWalletStatus represents an entry from Upbit's unauthenticated wallet status API
// (GET https://ccx.upbit.com/api/v1/status/network/wallet).
type UpbitPublicWalletStatus struct {
	Currency     string `json:"currency"`
	WalletState  string `json:"wallet_state"` // working, withdraw_only, deposit_only, paused, unsupported
	BlockState   string `json:"block_state"`
	BlockHeight  int64  `json:"block_height"`
	NetType      string `json:"net_type"`
	NetworkName  string `json:"network_name"`
}

var (
	globalMetaMu          sync.RWMutex
	globalChainSpecs      = make(map[Currency][]GlobalChainSpec)
	upbitPublicWallets    = make(map[Currency]UpbitPublicWalletStatus)
	globalMetaExpiresAt   time.Time
	upbitWalletsExpiresAt time.Time
	globalMetaHTTPClient  = &http.Client{Timeout: 8 * time.Second}
)

// EnsureGlobalCoinMetadataLoaded fetches and caches global coin/network metadata (RequiresMemo, ContractAddress,
// MinConfirmations, ChainName) from KuCoin V3 and Gate.io V4 public APIs so that even coins NOT listed on Coinbase
// are 100% dynamically supported.
func EnsureGlobalCoinMetadataLoaded(ctx context.Context) {
	globalMetaMu.RLock()
	fresh := len(globalChainSpecs) > 0 && time.Now().Before(globalMetaExpiresAt)
	globalMetaMu.RUnlock()
	if fresh {
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	mergedSpecs := make(map[Currency][]GlobalChainSpec, 2500)

	// 1. Primary Global Registry: KuCoin Public V3 Currencies API (2,270+ coins, includes needTag, memoRegex, contractAddress, confirms)
	if req, err := http.NewRequestWithContext(reqCtx, "GET", "https://api.kucoin.com/api/v3/currencies", nil); err == nil {
		req.Header.Set("User-Agent", "magnolia-global-metadata")
		if resp, doErr := globalMetaHTTPClient.Do(req); doErr == nil {
			if resp.StatusCode == 200 {
				var kcResp struct {
					Data []struct {
						Currency string `json:"currency"`
						Chains   []struct {
							ChainName        string `json:"chainName"`
							ChainID          string `json:"chainId"`
							ContractAddress  string `json:"contractAddress"`
							NeedTag          bool   `json:"needTag"`
							MemoRegex        string `json:"memoRegex"`
							Confirms         int    `json:"confirms"`
							WithdrawalMinFee string `json:"withdrawalMinFee"`
						} `json:"chains"`
					} `json:"data"`
				}
				if json.NewDecoder(resp.Body).Decode(&kcResp) == nil && len(kcResp.Data) > 0 {
					for _, item := range kcResp.Data {
						cur := Currency(strings.ToUpper(strings.TrimSpace(item.Currency)))
						if cur == "" {
							continue
						}
						for _, ch := range item.Chains {
							chainName := strings.ToUpper(strings.TrimSpace(ch.ChainName))
							if chainName == "" {
								chainName = strings.ToUpper(strings.TrimSpace(ch.ChainID))
							}
							requiresMemo := ch.NeedTag || strings.TrimSpace(ch.MemoRegex) != ""
							if requiresMemo {
								// Register the chain and native coin into the runtime memo registry
								if chainName != "" {
									RegisterMemoRequiredNetwork(Network(chainName))
								}
								if strings.EqualFold(chainName, string(cur)) || len(item.Chains) == 1 {
									RegisterMemoRequiredNetwork(Network(cur))
								}
							}
							feeFixed, _ := strconv.ParseFloat(ch.WithdrawalMinFee, 64)
							mergedSpecs[cur] = append(mergedSpecs[cur], GlobalChainSpec{
								Currency:         cur,
								ChainName:        chainName,
								ContractAddress:  strings.ToLower(strings.TrimSpace(ch.ContractAddress)),
								RequiresMemo:     requiresMemo,
								MemoRegex:        strings.TrimSpace(ch.MemoRegex),
								MinConfirmations: ch.Confirms,
								WithdrawFeeFixed: feeFixed,
							})
						}
					}
				}
			}
			_ = resp.Body.Close()
		}
	}

	// 2. Secondary Global Registry: Gate.io Public V4 Currencies API (5,700+ coins, supplements contract addresses for long-tail altcoins)
	if req, err := http.NewRequestWithContext(reqCtx, "GET", "https://api.gateio.ws/api/v4/spot/currencies", nil); err == nil {
		req.Header.Set("User-Agent", "magnolia-global-metadata")
		if resp, doErr := globalMetaHTTPClient.Do(req); doErr == nil {
			if resp.StatusCode == 200 {
				var gateCurrencies []struct {
					Currency string `json:"currency"`
					Chains   []struct {
						Name string `json:"name"`
						Addr string `json:"addr"`
					} `json:"chains"`
				}
				if json.NewDecoder(resp.Body).Decode(&gateCurrencies) == nil && len(gateCurrencies) > 0 {
					for _, item := range gateCurrencies {
						cur := Currency(strings.ToUpper(strings.TrimSpace(item.Currency)))
						if cur == "" {
							continue
						}
						existing := mergedSpecs[cur]
						for _, ch := range item.Chains {
							chainName := strings.ToUpper(strings.TrimSpace(ch.Name))
							contractAddr := strings.ToLower(strings.TrimSpace(ch.Addr))
							if chainName == "" && contractAddr == "" {
								continue
							}
							found := false
							for i := range existing {
								if strings.EqualFold(existing[i].ChainName, chainName) {
									if existing[i].ContractAddress == "" && contractAddr != "" {
										existing[i].ContractAddress = contractAddr
									}
									found = true
									break
								}
							}
							if !found {
								existing = append(existing, GlobalChainSpec{
									Currency:        cur,
									ChainName:       chainName,
									ContractAddress: contractAddr,
									RequiresMemo:    IsMemoRequiredNetwork(Network(chainName)) || IsMemoRequiredNetwork(Network(cur)),
								})
							}
						}
						if len(existing) > 0 {
							mergedSpecs[cur] = existing
						}
					}
				}
			}
			_ = resp.Body.Close()
		}
	}

	if len(mergedSpecs) > 0 {
		globalMetaMu.Lock()
		globalChainSpecs = mergedSpecs
		globalMetaExpiresAt = time.Now().Add(30 * time.Minute)
		globalMetaMu.Unlock()
	}
}

// LookupGlobalChainSpec returns the best matching global chain specification for a currency and optional network ID.
func LookupGlobalChainSpec(currency Currency, preferredNetID string) (GlobalChainSpec, bool) {
	cur := Currency(strings.ToUpper(strings.TrimSpace(string(currency))))
	netUpper := strings.ToUpper(strings.TrimSpace(preferredNetID))

	globalMetaMu.RLock()
	specs := globalChainSpecs[cur]
	globalMetaMu.RUnlock()

	if len(specs) == 0 {
		return GlobalChainSpec{}, false
	}
	if netUpper != "" {
		for _, sp := range specs {
			if strings.EqualFold(sp.ChainName, netUpper) {
				return sp, true
			}
		}
	}
	// Prefer native chain matching currency symbol if available
	for _, sp := range specs {
		if strings.EqualFold(sp.ChainName, string(cur)) {
			return sp, true
		}
	}
	return specs[0], true
}

// FetchUpbitPublicWalletStatuses queries Upbit's unauthenticated public wallet status endpoint
// (https://ccx.upbit.com/api/v1/status/network/wallet) so Upbit's live net_type and deposit/withdraw status
// are available for all 400+ Upbit coins even without an API key.
func FetchUpbitPublicWalletStatuses(ctx context.Context) map[Currency]UpbitPublicWalletStatus {
	globalMetaMu.RLock()
	if len(upbitPublicWallets) > 0 && time.Now().Before(upbitWalletsExpiresAt) {
		out := make(map[Currency]UpbitPublicWalletStatus, len(upbitPublicWallets))
		for k, v := range upbitPublicWallets {
			out[k] = v
		}
		globalMetaMu.RUnlock()
		return out
	}
	globalMetaMu.RUnlock()

	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "GET", "https://ccx.upbit.com/api/v1/status/network/wallet", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "magnolia-feed")
	resp, err := globalMetaHTTPClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}

	var parsed []UpbitPublicWalletStatus
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil || len(parsed) == 0 {
		return nil
	}

	newMap := make(map[Currency]UpbitPublicWalletStatus, len(parsed))
	for _, item := range parsed {
		cur := toUnifiedCurrencyFromUpbit(item.Currency)
		newMap[cur] = item
	}

	globalMetaMu.Lock()
	upbitPublicWallets = newMap
	upbitWalletsExpiresAt = time.Now().Add(2 * time.Minute)
	globalMetaMu.Unlock()

	return newMap
}
