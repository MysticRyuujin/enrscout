package netconf

import (
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/params"
)

func TestClassifyCurrentForkMatchesOwnNetwork(t *testing.T) {
	at := time.Now()
	for _, name := range Names() {
		n, err := Get(name)
		if err != nil {
			t.Fatalf("Get(%q): %v", name, err)
		}
		if got := Classify(n.CurrentForkIDAt(at)); got != name {
			t.Errorf("Classify(current %s fork) = %q, want %q", name, got, name)
		}
		current := n.CurrentForkIDAt(at).Hash
		if !n.IsCurrentForkAt(fmt.Sprintf("0x%X", current), at) || !IsCurrentForkAt(name, fmt.Sprintf("%x", current), at) {
			t.Errorf("current %s fork hash was not recognized", name)
		}
	}
	if IsCurrentForkAt("mainnet", "fc64ec04", at) || IsCurrentForkAt("unknown", "07c9462e", at) {
		t.Error("stale or unknown network fork was recognized as current")
	}
}

func TestGenesisDifficulty(t *testing.T) {
	mainnet, err := Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	if got := mainnet.GenesisDifficulty(); got.Cmp(big.NewInt(17179869184)) != 0 {
		t.Fatalf("mainnet genesis difficulty = %s, want 17179869184", got)
	}
	for _, name := range Names() {
		n, err := Get(name)
		if err != nil {
			t.Fatalf("Get(%q): %v", name, err)
		}
		if got := n.GenesisDifficulty(); got == nil || got.Sign() < 0 {
			t.Fatalf("%s genesis difficulty = %v, want non-negative", name, got)
		}
	}
}

func TestNetworksDoNotCrossClassify(t *testing.T) {
	for _, a := range Names() {
		na, _ := Get(a)
		for _, b := range Names() {
			if a == b {
				continue
			}
			nb, _ := Get(b)
			if nb.Matches(na.CurrentForkID()) {
				t.Errorf("%s current fork id matched %s", a, b)
			}
		}
	}
}

func TestAncientForkDoesNotClassify(t *testing.T) {
	frontier := forkid.ID{Hash: [4]byte{0xfc, 0x64, 0xec, 0x04}}
	if got := Classify(frontier); got != "" {
		t.Errorf("mainnet Frontier fork id classified as %q; genesis-sharing forks (PulseChain) would leak in", got)
	}
}

func TestMembershipIgnoresForkNext(t *testing.T) {
	n, _ := Get("mainnet")
	at := time.Now()
	current := n.CurrentForkIDAt(at)
	if !n.Matches(current) {
		t.Fatal("canonical current fork ID was not a member")
	}
	current.Next = 1 // already passed, so the currency rule rejects it
	if !n.Matches(current) {
		t.Fatal("membership classification incorrectly depended on fork_next")
	}
	if IsCanonicalForkCompatibleAt("mainnet", fmt.Sprintf("%x", current.Hash), current.Next, at) {
		t.Fatal("current hash with an already-passed fork_next was accepted as current")
	}
}

// previousEraForkID is what a node synced to just before the most recent scheduled fork
// advertises: an earlier hash carrying that fork as its exact canonical Next.
func previousEraForkID(n *Network, at time.Time) (forkid.ID, bool) {
	var last uint64
	for _, ft := range forkTimes(n.ChainConfig) {
		if ft <= uint64(at.Unix()) && ft > last {
			last = ft
		}
	}
	if last == 0 {
		return forkid.ID{}, false
	}
	return n.CurrentForkIDAt(time.Unix(int64(last)-1, 0).UTC()), true
}

// geth's EIP-2124 filter accepts an earlier era advertising its own exact canonical Next,
// because the remote may still be syncing. That is a connection-admission rule, not a
// readiness assertion, so a persisted current-fork view must reject it.
func TestPersistedRowCurrencyRequiresTheCurrentHash(t *testing.T) {
	n, _ := Get("mainnet")
	at := time.Now().UTC()
	current := n.CurrentForkIDAt(at)
	if !IsCanonicalForkCompatibleAt("mainnet", fmt.Sprintf("%x", current.Hash), current.Next, at) {
		t.Fatal("canonical current fork ID was rejected")
	}
	if IsCanonicalForkCompatibleAt("mainnet", "9f3d2254", 0, at) {
		t.Fatal("non-canonical historical fork hint was accepted for a persisted current view")
	}
	prev, ok := previousEraForkID(n, at)
	if !ok {
		t.Fatal("mainnet has no scheduled time fork to derive an earlier era from")
	}
	if prev.Hash == current.Hash {
		t.Fatal("previous era resolved to the current hash; the fixture is not exercising the rule")
	}
	if IsCanonicalForkCompatibleAt("mainnet", fmt.Sprintf("%x", prev.Hash), prev.Next, at) {
		t.Errorf("historical era %x with exact canonical Next %d was accepted as current", prev.Hash, prev.Next)
	}
	// Frontier is the genesis era a node resyncing from scratch advertises, and the hash
	// genesis-sharing chains like PulseChain reuse.
	if IsCanonicalForkCompatibleAt("mainnet", "fc64ec04", 1150000, at) {
		t.Error("mainnet Frontier with its canonical Next was accepted as current")
	}
}

// The three accepted ranges are only disjoint while the block-numbered band ends below
// the timestamp band, and forkHeadBlock has to be advanced before every fork.
func TestCanonicalCurrentNextRangesAreDisjointAndOrdered(t *testing.T) {
	if forkHeadBlock+1 > timestampThreshold {
		t.Fatalf("forkHeadBlock=%d has overtaken timestampThreshold=%d; the accepted Next ranges now overlap",
			forkHeadBlock, timestampThreshold)
	}
	for _, at := range []time.Time{time.Now(), time.Unix(0, 0), time.Unix(-1000, 0), time.Unix(int64(timestampThreshold)-1, 0)} {
		ranges := CanonicalCurrentNextRanges(at)
		for i, r := range ranges {
			if r[0] > r[1] {
				t.Errorf("at %v: range %d is empty: [%d, %d]", at, i, r[0], r[1])
			}
			if i > 0 && r[0] <= ranges[i-1][1] {
				t.Errorf("at %v: range %d [%d, %d] overlaps range %d ending %d",
					at, i, r[0], r[1], i-1, ranges[i-1][1])
			}
		}
	}
}

func TestForkHeadBlockCoversConfiguredBlockForks(t *testing.T) {
	for _, name := range Names() {
		n, _ := Get(name)
		v := reflect.ValueOf(n.ChainConfig).Elem()
		typeOf := v.Type()
		for i := 0; i < typeOf.NumField(); i++ {
			if !strings.HasSuffix(typeOf.Field(i).Name, "Block") {
				continue
			}
			if block, ok := v.Field(i).Interface().(*big.Int); ok && block != nil && block.Sign() >= 0 && block.IsUint64() && block.Uint64() > forkHeadBlock {
				t.Fatalf("%s %s=%d exceeds forkHeadBlock=%d; advance forkHeadBlock before upgrading geth", name, typeOf.Field(i).Name, block.Uint64(), forkHeadBlock)
			}
		}
	}
}

func TestRefreshClassifyWindowContainsCurrentFork(t *testing.T) {
	for _, name := range Names() {
		n, _ := Get(name)
		at := time.Now().Add(180 * 24 * time.Hour)
		n.RefreshClassifyWindowAt(at)
		current := n.CurrentForkIDAt(at)
		n.hashMu.RLock()
		_, ok := n.hashes[current.Hash]
		n.hashMu.RUnlock()
		if !ok {
			t.Fatalf("%s current fork hash absent after classify-window refresh", name)
		}
	}
}

// A fork id active for less than the 15-day sampling step must still enter the match
// set (via exact boundary sampling), or nodes on closely-scheduled forks get dropped.
func TestShortLivedForkIDClassifies(t *testing.T) {
	now := uint64(time.Now().Unix())
	genesisTime := now - 800*24*3600
	shanghai := now - 54*24*3600
	cancun := shanghai + 5*24*3600
	cfg := &params.ChainConfig{
		ChainID:                 big.NewInt(3151908),
		HomesteadBlock:          big.NewInt(0),
		EIP150Block:             big.NewInt(0),
		EIP155Block:             big.NewInt(0),
		EIP158Block:             big.NewInt(0),
		ByzantiumBlock:          big.NewInt(0),
		ConstantinopleBlock:     big.NewInt(0),
		PetersburgBlock:         big.NewInt(0),
		IstanbulBlock:           big.NewInt(0),
		BerlinBlock:             big.NewInt(0),
		LondonBlock:             big.NewInt(0),
		MergeNetsplitBlock:      big.NewInt(0),
		TerminalTotalDifficulty: big.NewInt(0),
		ShanghaiTime:            &shanghai,
		CancunTime:              &cancun,
	}
	g := &core.Genesis{Config: cfg, Timestamp: genesisTime, Difficulty: big.NewInt(1), GasLimit: 30_000_000}
	n := &Network{Name: "shortfork", ChainConfig: cfg, genesisFn: func() *core.Genesis { return g }}
	id := forkid.NewID(cfg, g.ToBlock(), forkHeadBlock, shanghai+1)
	if !n.Matches(id) {
		t.Error("fork id active for only 5 days did not classify; fork boundaries not sampled")
	}
}

func TestBootnodesParse(t *testing.T) {
	for _, name := range Names() {
		nodes, err := Bootnodes(name)
		if err != nil {
			t.Fatalf("Bootnodes(%q): %v", name, err)
		}
		if len(nodes) == 0 {
			t.Errorf("no bootnodes for %q", name)
		}
	}
}

func TestEveryNetworkHasBootnodes(t *testing.T) {
	for _, name := range Names() {
		el, err := Bootnodes(name)
		if err != nil {
			t.Errorf("Bootnodes(%q): %v", name, err)
		} else if len(el) == 0 {
			t.Errorf("%s has no EL bootnodes", name)
		}
		cl, err := CLBootnodes(name)
		if err != nil {
			t.Errorf("CLBootnodes(%q): %v", name, err)
		} else if len(cl) == 0 {
			t.Errorf("%s has no CL bootnodes", name)
		}
	}
}

func TestCLBootnodesParseAndHaveUniqueIDs(t *testing.T) {
	seen := make(map[enode.ID]string)
	for _, name := range Names() {
		for _, record := range clBootnodes[name] {
			n, err := enode.Parse(enode.ValidSchemes, record)
			if err != nil {
				t.Errorf("parse %s CL bootnode: %v", name, err)
				continue
			}
			if previous, ok := seen[n.ID()]; ok {
				t.Errorf("CL bootnode ID %s is duplicated in %s and %s; the later network's record would be unreachable", n.ID(), previous, name)
				continue
			}
			seen[n.ID()] = name
		}
	}
}
