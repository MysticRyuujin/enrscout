package netconf

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	forkHeadBlock      = uint64(30_000_000)
	timestampThreshold = uint64(1_438_269_973)
)

type EthEntry struct {
	ForkID forkid.ID
	Rest   []rlp.RawValue `rlp:"tail"`
}

func (EthEntry) ENRKey() string { return "eth" }

type Eth2Entry []byte

func (Eth2Entry) ENRKey() string { return "eth2" }

type AttnetsEntry []byte

func (AttnetsEntry) ENRKey() string { return "attnets" }

type SyncnetsEntry []byte

func (SyncnetsEntry) ENRKey() string { return "syncnets" }

type CGCEntry []byte

func (CGCEntry) ENRKey() string { return "cgc" }

type NFDEntry []byte

func (NFDEntry) ENRKey() string { return "nfd" }

type Network struct {
	Name        string
	NetworkID   uint64
	ChainConfig *params.ChainConfig
	genesisFn   func() *core.Genesis

	once    sync.Once
	genesis *types.Block
	times   []uint64
	hashMu  sync.RWMutex
	hashes  map[[4]byte]struct{}
	hashAt  time.Time
	fork    atomic.Pointer[forkMemo]
}

// Racing writers compute the same id for the same era, so the store is benign.
type forkMemo struct {
	era int64
	id  forkid.ID
}

func (n *Network) load() {
	n.once.Do(func() {
		n.genesis = n.genesisFn().ToBlock()
		n.times = forkTimes(n.ChainConfig)
	})
}

// Sound cache key: the fork evaluators vary with at only across scheduled fork
// times, and constant forkHeadBlock keeps block-activated forks out of it.
func (n *Network) forkEraAt(at time.Time) int64 {
	unix := clampUnix(at)
	var era int64
	for _, ft := range n.times {
		if t := int64(ft); t <= unix && t > era {
			era = t
		}
	}
	return era
}

// Recent-window fork ids only (not back to genesis) so genesis-sharing forks like PulseChain, which reuse mainnet's ancient Frontier hash, don't misclassify.
func (n *Network) gatherHashes(at time.Time) map[[4]byte]struct{} {
	const (
		step     = int64(15 * 24 * 3600)
		lookback = int64(730 * 24 * 3600)
		ahead    = int64(365 * 24 * 3600)
	)
	set := make(map[[4]byte]struct{})
	now := at.Unix()
	start := int64(n.genesis.Time())
	if lb := now - lookback; lb > start {
		start = lb
	}
	for t := start; t <= now+ahead; t += step {
		set[forkid.NewID(n.ChainConfig, n.genesis, forkHeadBlock, uint64(t)).Hash] = struct{}{}
	}
	// Exact boundaries too: step sampling misses fork ids active for under one step.
	for _, ft := range n.times {
		if t := int64(ft); t >= start && t <= now+ahead {
			set[forkid.NewID(n.ChainConfig, n.genesis, forkHeadBlock, ft).Hash] = struct{}{}
		}
	}
	return set
}

// RefreshClassifyWindowAt updates the recent membership hash window. It is safe
// to call from the hourly advertiser refresh and from concurrent classifiers.
func (n *Network) RefreshClassifyWindowAt(at time.Time) {
	n.load()
	hashes := n.gatherHashes(at)
	n.hashMu.Lock()
	n.hashes, n.hashAt = hashes, at
	n.hashMu.Unlock()
}

// Reflection over *Time fields, not a hand-kept list, so geth upgrades add forks for free.
func forkTimes(cfg *params.ChainConfig) []uint64 {
	v := reflect.ValueOf(cfg).Elem()
	t := v.Type()
	var out []uint64
	for i := 0; i < t.NumField(); i++ {
		if !strings.HasSuffix(t.Field(i).Name, "Time") {
			continue
		}
		if p, ok := v.Field(i).Interface().(*uint64); ok && p != nil {
			out = append(out, *p)
		}
	}
	return out
}

func (n *Network) CurrentForkID() forkid.ID {
	return n.CurrentForkIDAt(time.Now())
}

func (n *Network) CurrentForkIDAt(at time.Time) forkid.ID {
	n.load()
	era := n.forkEraAt(at)
	if memo := n.fork.Load(); memo != nil && memo.era == era {
		return memo.id
	}
	id := n.currentForkIDAt(at)
	n.fork.Store(&forkMemo{era: era, id: id})
	return id
}

func (n *Network) currentForkIDAt(at time.Time) forkid.ID {
	return forkid.NewID(n.ChainConfig, n.genesis, forkHeadBlock, uint64(clampUnix(at)))
}

// Every fork evaluator clamps: a pre-1970 evaluation time would otherwise wrap into a
// far-future head and read as the latest era rather than genesis.
func clampUnix(at time.Time) int64 {
	if unix := at.Unix(); unix > 0 {
		return unix
	}
	return 0
}

func (n *Network) IsCurrentForkAt(raw string, at time.Time) bool {
	want := n.CurrentForkIDAt(at).Hash
	got, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(raw)), "0x"))
	return err == nil && bytes.Equal(got, want[:])
}

func IsCurrentForkAt(network, hash string, at time.Time) bool {
	n, err := Get(network)
	return err == nil && n.IsCurrentForkAt(hash, at)
}

// ForkEraTokenAt identifies every EL and CL fork state used by a request and
// returns the earliest scheduled wall-clock transition among those networks.
func ForkEraTokenAt(at time.Time, requested ...string) (string, time.Time, error) {
	if len(requested) == 0 {
		requested = append([]string(nil), Names()...)
	} else {
		requested = append([]string(nil), requested...)
	}
	sort.Strings(requested)
	parts := make([]string, 0, len(requested))
	var next time.Time
	for _, name := range requested {
		n, err := Get(name)
		if err != nil {
			return "", time.Time{}, err
		}
		el := n.CurrentForkIDAt(at).Hash
		state, err := CLForkStateAt(name, at)
		if err != nil {
			return "", time.Time{}, err
		}
		parts = append(parts, name+":"+hex.EncodeToString(el[:])+":"+hex.EncodeToString(state.Digest[:]))
		if !state.NextTransition.IsZero() && (next.IsZero() || state.NextTransition.Before(next)) {
			next = state.NextTransition
		}
		for _, unix := range n.times {
			transition := time.Unix(int64(unix), 0).UTC()
			if transition.After(at) && (next.IsZero() || transition.Before(next)) {
				next = transition
			}
		}
	}
	return strings.Join(parts, ","), next, nil
}

func (n *Network) GenesisHash() common.Hash {
	n.load()
	return n.genesis.Hash()
}

// GenesisDifficulty is the total difficulty at genesis — the correct eth/68 Status TD for a not-yet-synced node.
func (n *Network) GenesisDifficulty() *big.Int {
	n.load()
	return n.genesis.Difficulty()
}

func (n *Network) Matches(id forkid.ID) bool {
	n.load()
	n.hashMu.RLock()
	refresh := n.hashes == nil || time.Since(n.hashAt) >= time.Hour
	n.hashMu.RUnlock()
	if refresh {
		n.RefreshClassifyWindowAt(time.Now())
	}
	n.hashMu.RLock()
	_, ok := n.hashes[id.Hash]
	n.hashMu.RUnlock()
	return ok
}

// CanonicalCurrentNextRanges is the Next values a row already on the current fork hash
// may carry, as inclusive ranges: absent, a block-numbered hint above our head, or a
// timestamp still in the future. Both the Go matcher and the query engine's SQL predicate
// must derive from this one place or they can disagree with no compile error.
func CanonicalCurrentNextRanges(at time.Time) [3][2]uint64 {
	unix := clampUnix(at)
	future := timestampThreshold
	if uint64(unix) > future {
		future = uint64(unix)
	}
	return [3][2]uint64{
		{0, 0},
		{forkHeadBlock + 1, timestampThreshold},
		{future + 1, math.MaxUint64},
	}
}

// IsCanonicalForkCompatibleAt applies the persisted-row policy used by public
// current-fork views: the exact current hash, with a Next that is absent or still
// ahead. EIP-2124's acceptance of an earlier era carrying its own canonical Next is a
// connection-admission rule for peers that may still be syncing, so it is deliberately
// not honoured here - the advertised id tracks a node's synced head, not its
// capability, and an earlier era means the node is behind.
func IsCanonicalForkCompatibleAt(network, raw string, next uint64, at time.Time) bool {
	if !IsCurrentForkAt(network, raw, at) {
		return false
	}
	for _, r := range CanonicalCurrentNextRanges(at) {
		if next >= r[0] && next <= r[1] {
			return true
		}
	}
	return false
}

// RowForkCurrentAt is the single fork-currency rule for a persisted row. Query predicates,
// snapshot counts, measurement aggregates and DNS selection must agree exactly, so none of
// them may repeat this layer switch.
func RowForkCurrentAt(layer, network, forkHash string, forkNext uint64, at time.Time) bool {
	switch layer {
	case "el":
		return IsCanonicalForkCompatibleAt(network, forkHash, forkNext, at)
	case "cl":
		return IsCurrentCLForkAt(network, forkHash, at)
	default:
		return false
	}
}

var networks = map[string]*Network{
	"mainnet": {Name: "mainnet", NetworkID: 1, ChainConfig: params.MainnetChainConfig, genesisFn: core.DefaultGenesisBlock},
	"sepolia": {Name: "sepolia", NetworkID: 11155111, ChainConfig: params.SepoliaChainConfig, genesisFn: core.DefaultSepoliaGenesisBlock},
	"hoodi":   {Name: "hoodi", NetworkID: 560048, ChainConfig: params.HoodiChainConfig, genesisFn: core.DefaultHoodiGenesisBlock},
}

var bootnodes = map[string][]string{
	"mainnet": params.MainnetBootnodes,
	"sepolia": params.SepoliaBootnodes,
	"hoodi":   params.HoodiBootnodes,
}

// Consensus bootnodes mirror Lighthouse's built-in network configuration:
// https://github.com/sigp/lighthouse/tree/stable/common/eth2_network_config/built_in_network_configs
// Keep the signed ENRs current: an updated record can retain its node ID while
// changing address, so a stale entry silently points discovery at a dead endpoint.
var clBootnodes = map[string][]string{
	"mainnet": {
		"enr:-Iu4QLm7bZGdAt9NSeJG0cEnJohWcQTQaI9wFLu3Q7eHIDfrI4cwtzvEW3F3VbG9XdFXlrHyFGeXPn9snTCQJ9bnMRABgmlkgnY0gmlwhAOTJQCJc2VjcDI1NmsxoQIZdZD6tDYpkpEfVo5bgiU8MGRjhcOmHGD2nErK0UKRrIN0Y3CCIyiDdWRwgiMo",
		"enr:-Iu4QEDJ4Wa_UQNbK8Ay1hFEkXvd8psolVK6OhfTL9irqz3nbXxxWyKwEplPfkju4zduVQj6mMhUCm9R2Lc4YM5jPcIBgmlkgnY0gmlwhANrfESJc2VjcDI1NmsxoQJCYz2-nsqFpeEj6eov9HSi9QssIVIVNr0I89J1vXM9foN0Y3CCIyiDdWRwgiMo",
		"enr:-Ku4QImhMc1z8yCiNJ1TyUxdcfNucje3BGwEHzodEZUan8PherEo4sF7pPHPSIB1NNuSg5fZy7qFsjmUKs2ea1Whi0EBh2F0dG5ldHOIAAAAAAAAAACEZXRoMpD1pf1CAAAAAP__________gmlkgnY0gmlwhBLf22SJc2VjcDI1NmsxoQOVphkDqal4QzPMksc5wnpuC3gvSC8AfbFOnZY_On34wIN1ZHCCIyg",
		"enr:-Ku4QP2xDnEtUXIjzJ_DhlCRN9SN99RYQPJL92TMlSv7U5C1YnYLjwOQHgZIUXw6c-BvRg2Yc2QsZxxoS_pPRVe0yK8Bh2F0dG5ldHOIAAAAAAAAAACEZXRoMpD1pf1CAAAAAP__________gmlkgnY0gmlwhBLf22SJc2VjcDI1NmsxoQMeFF5GrS7UZpAH2Ly84aLK-TyvH-dRo0JM1i8yygH50YN1ZHCCJxA",
		"enr:-Ku4QPp9z1W4tAO8Ber_NQierYaOStqhDqQdOPY3bB3jDgkjcbk6YrEnVYIiCBbTxuar3CzS528d2iE7TdJsrL-dEKoBh2F0dG5ldHOIAAAAAAAAAACEZXRoMpD1pf1CAAAAAP__________gmlkgnY0gmlwhBLf22SJc2VjcDI1NmsxoQMw5fqqkw2hHC4F5HZZDPsNmPdB1Gi8JPQK7pRc9XHh-oN1ZHCCKvg",
		"enr:-Le4QPUXJS2BTORXxyx2Ia-9ae4YqA_JWX3ssj4E_J-3z1A-HmFGrU8BpvpqhNabayXeOZ2Nq_sbeDgtzMJpLLnXFgAChGV0aDKQtTA_KgEAAAAAIgEAAAAAAIJpZIJ2NIJpcISsaa0Zg2lwNpAkAIkHAAAAAPA8kv_-awoTiXNlY3AyNTZrMaEDHAD2JKYevx89W0CcFJFiskdcEzkH_Wdv9iW42qLK79ODdWRwgiMohHVkcDaCI4I",
		"enr:-Le4QLHZDSvkLfqgEo8IWGG96h6mxwe_PsggC20CL3neLBjfXLGAQFOPSltZ7oP6ol54OvaNqO02Rnvb8YmDR274uq8ChGV0aDKQtTA_KgEAAAAAIgEAAAAAAIJpZIJ2NIJpcISLosQxg2lwNpAqAX4AAAAAAPA8kv_-ax65iXNlY3AyNTZrMaEDBJj7_dLFACaxBfaI8KZTh_SSJUjhyAyfshimvSqo22WDdWRwgiMohHVkcDaCI4I",
		"enr:-Le4QH6LQrusDbAHPjU_HcKOuMeXfdEB5NJyXgHWFadfHgiySqeDyusQMvfphdYWOzuSZO9Uq2AMRJR5O4ip7OvVma8BhGV0aDKQtTA_KgEAAAAAIgEAAAAAAIJpZIJ2NIJpcISLY9ncg2lwNpAkAh8AgQIBAAAAAAAAAAmXiXNlY3AyNTZrMaECDYCZTZEksF-kmgPholqgVt8IXr-8L7Nu7YrZ7HUpgxmDdWRwgiMohHVkcDaCI4I",
		"enr:-Le4QIqLuWybHNONr933Lk0dcMmAB5WgvGKRyDihy1wHDIVlNuuztX62W51voT4I8qD34GcTEOTmag1bcdZ_8aaT4NUBhGV0aDKQtTA_KgEAAAAAIgEAAAAAAIJpZIJ2NIJpcISLY04ng2lwNpAkAh8AgAIBAAAAAAAAAA-fiXNlY3AyNTZrMaEDscnRV6n1m-D9ID5UsURk0jsoKNXt1TIrj8uKOGW6iluDdWRwgiMohHVkcDaCI4I",
		"enr:-Ku4QHqVeJ8PPICcWk1vSn_XcSkjOkNiTg6Fmii5j6vUQgvzMc9L1goFnLKgXqBJspJjIsB91LTOleFmyWWrFVATGngBh2F0dG5ldHOIAAAAAAAAAACEZXRoMpC1MD8qAAAAAP__________gmlkgnY0gmlwhAMRHkWJc2VjcDI1NmsxoQKLVXFOhp2uX6jeT0DvvDpPcU8FWMjQdR4wMuORMhpX24N1ZHCCIyg",
		"enr:-Ku4QG-2_Md3sZIAUebGYT6g0SMskIml77l6yR-M_JXc-UdNHCmHQeOiMLbylPejyJsdAPsTHJyjJB2sYGDLe0dn8uYBh2F0dG5ldHOIAAAAAAAAAACEZXRoMpC1MD8qAAAAAP__________gmlkgnY0gmlwhBLY-NyJc2VjcDI1NmsxoQORcM6e19T1T9gi7jxEZjk_sjVLGFscUNqAY9obgZaxbIN1ZHCCIyg",
		"enr:-Ku4QPn5eVhcoF1opaFEvg1b6JNFD2rqVkHQ8HApOKK61OIcIXD127bKWgAtbwI7pnxx6cDyk_nI88TrZKQaGMZj0q0Bh2F0dG5ldHOIAAAAAAAAAACEZXRoMpC1MD8qAAAAAP__________gmlkgnY0gmlwhDayLMaJc2VjcDI1NmsxoQK2sBOLGcUb4AwuYzFuAVCaNHA-dy24UuEKkeFNgCVCsIN1ZHCCIyg",
		"enr:-Ku4QEWzdnVtXc2Q0ZVigfCGggOVB2Vc1ZCPEc6j21NIFLODSJbvNaef1g4PxhPwl_3kax86YPheFUSLXPRs98vvYsoBh2F0dG5ldHOIAAAAAAAAAACEZXRoMpC1MD8qAAAAAP__________gmlkgnY0gmlwhDZBrP2Jc2VjcDI1NmsxoQM6jr8Rb1ktLEsVcKAPa08wCsKUmvoQ8khiOl_SLozf9IN1ZHCCIyg",
		"enr:-LK4QA8FfhaAjlb_BXsXxSfiysR7R52Nhi9JBt4F8SPssu8hdE1BXQQEtVDC3qStCW60LSO7hEsVHv5zm8_6Vnjhcn0Bh2F0dG5ldHOIAAAAAAAAAACEZXRoMpC1MD8qAAAAAP__________gmlkgnY0gmlwhAN4aBKJc2VjcDI1NmsxoQJerDhsJ-KxZ8sHySMOCmTO6sHM3iCFQ6VMvLTe948MyYN0Y3CCI4yDdWRwgiOM",
		"enr:-LK4QKWrXTpV9T78hNG6s8AM6IO4XH9kFT91uZtFg1GcsJ6dKovDOr1jtAAFPnS2lvNltkOGA9k29BUN7lFh_sjuc9QBh2F0dG5ldHOIAAAAAAAAAACEZXRoMpC1MD8qAAAAAP__________gmlkgnY0gmlwhANAdd-Jc2VjcDI1NmsxoQLQa6ai7y9PMN5hpLe5HmiJSlYzMuzP7ZhwRiwHvqNXdoN0Y3CCI4yDdWRwgiOM",
		"enr:-IS4QPi-onjNsT5xAIAenhCGTDl4z-4UOR25Uq-3TmG4V3kwB9ljLTb_Kp1wdjHNj-H8VVLRBSSWVZo3GUe3z6k0E-IBgmlkgnY0gmlwhKB3_qGJc2VjcDI1NmsxoQMvAfgB4cJXvvXeM6WbCG86CstbSxbQBSGx31FAwVtOTYN1ZHCCIyg",
		"enr:-KG4QPUf8-g_jU-KrwzG42AGt0wWM1BTnQxgZXlvCEIfTQ5hSmptkmgmMbRkpOqv6kzb33SlhPHJp7x4rLWWiVq5lSECgmlkgnY0gmlwhFPlR9KDaXA2kCoGxcAJAAAVAAAAAAAAABCJc2VjcDI1NmsxoQLdUv9Eo9sxCt0tc_CheLOWnX59yHJtkBSOL7kpxdJ6GYN1ZHCCIyiEdWRwNoIjKA",
	},
	"sepolia": {
		"enr:-Ku4QDZ_rCowZFsozeWr60WwLgOfHzv1Fz2cuMvJqN5iJzLxKtVjoIURY42X_YTokMi3IGstW5v32uSYZyGUXj9Q_IECh2F0dG5ldHOIAAAAAAAAAACEZXRoMpCo_ujukAAAaf__________gmlkgnY0gmlwhIpEe5iJc2VjcDI1NmsxoQNHTpFdaNSCEWiN_QqT396nb0PzcUpLe3OVtLph-AciBYN1ZHCCIy0",
		"enr:-Ku4QHRyRwEPT7s0XLYzJ_EeeWvZTXBQb4UCGy1F_3m-YtCNTtDlGsCMr4UTgo4uR89pv11uM-xq4w6GKfKhqU31hTgCh2F0dG5ldHOIAAAAAAAAAACEZXRoMpCo_ujukAAAaf__________gmlkgnY0gmlwhIrFM7WJc2VjcDI1NmsxoQI4diTwChN3zAAkarf7smOHCdFb1q3DSwdiQ_Lc_FdzFIN1ZHCCIy0",
		"enr:-Ku4QOkvvf0u5Hg4-HhY-SJmEyft77G5h3rUM8VF_e-Hag5cAma3jtmFoX4WElLAqdILCA-UWFRN1ZCDJJVuEHrFeLkDh2F0dG5ldHOIAAAAAAAAAACEZXRoMpCo_ujukAAAaf__________gmlkgnY0gmlwhJK-AWeJc2VjcDI1NmsxoQLFcT5VE_NMiIC8Ll7GypWDnQ4UEmuzD7hF_Hf4veDJwIN1ZHCCIy0",
		"enr:-Ku4QH6tYsHKITYeHUu5kdfXgEZWI18EWk_2RtGOn1jBPlx2UlS_uF3Pm5Dx7tnjOvla_zs-wwlPgjnEOcQDWXey51QCh2F0dG5ldHOIAAAAAAAAAACEZXRoMpCo_ujukAAAaf__________gmlkgnY0gmlwhIs7Mc6Jc2VjcDI1NmsxoQIET4Mlv9YzhrYhX_H9D7aWMemUrvki6W4J2Qo0YmFMp4N1ZHCCIy0",
		"enr:-Ku4QDmz-4c1InchGitsgNk4qzorWMiFUoaPJT4G0IiF8r2UaevrekND1o7fdoftNucirj7sFFTTn2-JdC2Ej0p1Mn8Ch2F0dG5ldHOIAAAAAAAAAACEZXRoMpCo_ujukAAAaf__________gmlkgnY0gmlwhKpA-liJc2VjcDI1NmsxoQMpHP5U1DK8O_JQU6FadmWbE42qEdcGlllR8HcSkkfWq4N1ZHCCIy0",
		// Lighthouse's current Sepolia Teku record is invalid under go-ethereum
		// (duplicate ENR key), so retain its previous valid signed record until fixed.
		"enr:-Iu4QKvMF7Ne_RSQoZGvavTuZ1QA5_Pgeb0nq_hrjhU8s0UDV3KhcMXJkGwOWhsDGZL3ISjL0CTP-hfoTjZtEtCEwR4BgmlkgnY0gmlwhAOAaySJc2VjcDI1NmsxoQNta5b_bexSSwwrGW2Re24MjfMntzFd0f2SAxQtMj3ueYN0Y3CCIyiDdWRwgiMo",
		"enr:-KG4QJejf8KVtMeAPWFhN_P0c4efuwu1pZHELTveiXUeim6nKYcYcMIQpGxxdgT2Xp9h-M5pr9gn2NbbwEAtxzu50Y8BgmlkgnY0gmlwhEEVkQCDaXA2kCoBBPnAEJg4AAAAAAAAAAGJc2VjcDI1NmsxoQLEh_eVvk07AQABvLkTGBQTrrIOQkzouMgSBtNHIRUxOIN1ZHCCIyiEdWRwNoIjKA",
		"enr:-Iq4QMCTfIMXnow27baRUb35Q8iiFHSIDBJh6hQM5Axohhf4b6Kr_cOCu0htQ5WvVqKvFgY28893DHAg8gnBAXsAVqmGAX53x8JggmlkgnY0gmlwhLKAlv6Jc2VjcDI1NmsxoQK6S-Cii_KmfFdUJL2TANL3ksaKUnNXvTCv1tLwXs0QgIN1ZHCCIyk",
		"enr:-L64QC9Hhov4DhQ7mRukTOz4_jHm4DHlGL726NWH4ojH1wFgEwSin_6H95Gs6nW2fktTWbPachHJ6rUFu0iJNgA0SB2CARqHYXR0bmV0c4j__________4RldGgykDb6UBOQAABx__________-CaWSCdjSCaXCEA-2vzolzZWNwMjU2azGhA17lsUg60R776rauYMdrAz383UUgESoaHEzMkvm4K6k6iHN5bmNuZXRzD4N0Y3CCIyiDdWRwgiMo",
	},
	"hoodi": {
		"enr:-Mq4QLkmuSwbGBUph1r7iHopzRpdqE-gcm5LNZfcE-6T37OCZbRHi22bXZkaqnZ6XdIyEDTelnkmMEQB8w6NbnJUt9GGAZWaowaYh2F0dG5ldHOIABgAAAAAAACEZXRoMpDS8Zl_YAAJEAAIAAAAAAAAgmlkgnY0gmlwhNEmfKCEcXVpY4IyyIlzZWNwMjU2azGhA0hGa4jZJZYQAS-z6ZFK-m4GCFnWS8wfjO0bpSQn6hyEiHN5bmNuZXRzAIN0Y3CCIyiDdWRwgiMo",
		"enr:-Ku4QLVumWTwyOUVS4ajqq8ZuZz2ik6t3Gtq0Ozxqecj0qNZWpMnudcvTs-4jrlwYRQMQwBS8Pvtmu4ZPP2Lx3i2t7YBh2F0dG5ldHOIAAAAAAAAAACEZXRoMpBd9cEGEAAJEP__________gmlkgnY0gmlwhNEmfKCJc2VjcDI1NmsxoQLdRlI8aCa_ELwTJhVN8k7km7IDc3pYu-FMYBs5_FiigIN1ZHCCIyk",
		"enr:-LK4QAYuLujoiaqCAs0-qNWj9oFws1B4iy-Hff1bRB7wpQCYSS-IIMxLWCn7sWloTJzC1SiH8Y7lMQ5I36ynGV1ASj4Eh2F0dG5ldHOIYAAAAAAAAACEZXRoMpDS8Zl_YAAJEAAIAAAAAAAAgmlkgnY0gmlwhIbRilSJc2VjcDI1NmsxoQOmI5MlAu3f5WEThAYOqoygpS2wYn0XS5NV2aYq7T0a04N0Y3CCIyiDdWRwgiMo",
		"enr:-Ku4QIC89sMC0o-irosD4_23lJJ4qCGOvdUz7SmoShWx0k6AaxCFTKviEHa-sa7-EzsiXpDp0qP0xzX6nKdXJX3X-IQBh2F0dG5ldHOIAAAAAAAAAACEZXRoMpBd9cEGEAAJEP__________gmlkgnY0gmlwhIbRilSJc2VjcDI1NmsxoQK_m0f1DzDc9Cjrspm36zuRa7072HSiMGYWLsKiVSbP34N1ZHCCIyk",
		"enr:-Ku4QNkWjw5tNzo8DtWqKm7CnDdIq_y7xppD6c1EZSwjB8rMOkSFA1wJPLoKrq5UvA7wcxIotH6Usx3PAugEN2JMncIBh2F0dG5ldHOIAAAAAAAAAACEZXRoMpBd9cEGEAAJEP__________gmlkgnY0gmlwhIbHuBeJc2VjcDI1NmsxoQP3FwrhFYB60djwRjAoOjttq6du94DtkQuaN99wvgqaIYN1ZHCCIyk",
		"enr:-OS4QMJGE13xEROqvKN1xnnt7U-noc51VXyM6wFMuL9LMhQDfo1p1dF_zFdS4OsnXz_vIYk-nQWnqJMWRDKvkSK6_CwDh2F0dG5ldHOIAAAAADAAAACGY2xpZW502IpMaWdodGhvdXNljDcuMC4wLWJldGEuM4RldGgykNLxmX9gAAkQAAgAAAAAAACCaWSCdjSCaXCEhse4F4RxdWljgiMqiXNlY3AyNTZrMaECef77P8k5l3PC_raLw42OAzdXfxeQ-58BJriNaqiRGJSIc3luY25ldHMAg3RjcIIjKIN1ZHCCIyg",
		"enr:-LK4QDwhXMitMbC8xRiNL-XGMhRyMSOnxej-zGifjv9Nm5G8EF285phTU-CAsMHRRefZimNI7eNpAluijMQP7NDC8kEMh2F0dG5ldHOIAAAAAAAABgCEZXRoMpDS8Zl_YAAJEAAIAAAAAAAAgmlkgnY0gmlwhAOIT_SJc2VjcDI1NmsxoQMoHWNL4MAvh6YpQeM2SUjhUrLIPsAVPB8nyxbmckC6KIN0Y3CCIyiDdWRwgiMo",
		"enr:-LK4QPYl2HnMPQ7b1es6Nf_tFYkyya5bj9IqAKOEj2cmoqVkN8ANbJJJK40MX4kciL7pZszPHw6vLNyeC-O3HUrLQv8Mh2F0dG5ldHOIAAAAAAAAAMCEZXRoMpDS8Zl_YAAJEAAIAAAAAAAAgmlkgnY0gmlwhAMYRG-Jc2VjcDI1NmsxoQPQ35tjr6q1qUqwAnegQmYQyfqxC_6437CObkZneI9n34N0Y3CCIyiDdWRwgiMo",
		"enr:-KG4QKRSUi4IOAIK_xt5ERrwW_J47wmNCLWFh7Jo0hFE69drZsiZ5Pb5CEcM_njFTTLlIR6SCf67HTcSV1g6hCXdhWkCgmlkgnY0gmlwhLkvrBODaXA2kCoGxcAWAAAYAAAAAAAAABCJc2VjcDI1NmsxoQPU7g2jQGTz8BYbB2vLTb39S_PrcZAehwMM0b3bWsM5rIN1ZHCCIyiEdWRwNoIjKA",
	},
}

func Get(name string) (*Network, error) {
	n, ok := networks[name]
	if !ok {
		return nil, fmt.Errorf("unknown network %q", name)
	}
	return n, nil
}

var names = []string{"mainnet", "hoodi", "sepolia"}

func Names() []string {
	return names
}

func Bootnodes(name string) ([]*enode.Node, error) {
	urls, ok := bootnodes[name]
	if !ok {
		return nil, fmt.Errorf("no bootnodes for network %q", name)
	}
	nodes := make([]*enode.Node, 0, len(urls))
	for _, u := range urls {
		n, err := enode.Parse(enode.ValidSchemes, u)
		if err != nil {
			return nil, fmt.Errorf("parse bootnode %q: %w", u, err)
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

func CLBootnodes(name string) ([]*enode.Node, error) {
	urls, ok := clBootnodes[name]
	if !ok {
		return nil, fmt.Errorf("no consensus bootnodes for network %q", name)
	}
	nodes := make([]*enode.Node, 0, len(urls))
	for _, u := range urls {
		n, err := enode.Parse(enode.ValidSchemes, u)
		if err == nil {
			nodes = append(nodes, n)
		}
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no valid consensus bootnodes for network %q", name)
	}
	return nodes, nil
}

func Classify(id forkid.ID) string {
	for _, name := range Names() {
		if networks[name].Matches(id) {
			return name
		}
	}
	return ""
}

// ClassifyStatus classifies an eth Status handshake. Genesis is authoritative;
// network ID is checked as an additional guard against malformed peers.
func ClassifyStatus(networkID uint64, genesis common.Hash) string {
	for _, name := range Names() {
		n := networks[name]
		if n.NetworkID == networkID && n.GenesisHash() == genesis {
			return name
		}
	}
	return ""
}
