package netconf

import (
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/forkid"
)

// freshNetwork swaps a registered network for an unloaded copy, so a test can apply cancellations
// that other tests' use of the shared entry would otherwise forbid.
func freshNetwork(t *testing.T, name string) *Network {
	t.Helper()
	i := slices.IndexFunc(registry, func(n *Network) bool { return n.Name == name })
	if i < 0 {
		t.Fatalf("no network %s", name)
	}
	orig := registry[i]
	fresh := &Network{Name: orig.Name, NetworkID: orig.NetworkID, ChainConfig: orig.ChainConfig, genesisFn: orig.genesisFn,
		cl: orig.cl.clone(), bootnodes: orig.bootnodes, clBootnodes: orig.clBootnodes, extraELForks: orig.extraELForks}
	registry[i] = fresh
	t.Cleanup(func() { registry[i] = orig })
	return fresh
}

func hoodiGlamsterdamCancellation() CancelledFork {
	return CancelledFork{Network: "hoodi", Fork: "Glamsterdam",
		EL: &CancelledELFork{Name: "Amsterdam", Time: hoodiGlamsterdam},
		CL: &CancelledCLFork{Name: "gloas", Epoch: 132352, Version: "80000910"}}
}

func TestCancelHoodiGlamsterdam(t *testing.T) {
	n := freshNetwork(t, "hoodi")
	absent, err := ApplyCancellations([]CancelledFork{hoodiGlamsterdamCancellation()})
	if err != nil || len(absent) != 0 {
		t.Fatalf("ApplyCancellations = %v, %v", absent, err)
	}
	before, activation := time.Unix(hoodiGlamsterdam-86400, 0), time.Unix(hoodiGlamsterdam, 0)

	if id := n.CurrentForkIDAt(before); fmt.Sprintf("%x", id.Hash) != "23aa1351" || id.Next != 0 {
		t.Fatalf("advertised fork id before = %x/%d, want 23aa1351/0", id.Hash, id.Next)
	}
	if id := n.CurrentForkIDAt(activation.Add(time.Hour)); fmt.Sprintf("%x", id.Hash) != "23aa1351" {
		t.Fatalf("canonical fork id after the cancelled time = %x, want 23aa1351", id.Hash)
	}
	state, err := CLForkStateAt("hoodi", before)
	if err != nil || hex.EncodeToString(state.Digest[:]) != "c6ecb76c" || state.NextForkEpoch != math.MaxUint64 ||
		hex.EncodeToString(state.NextForkVersion[:]) != "70000910" {
		t.Fatalf("CL state = %+v, %v; want digest c6ecb76c and no next fork", state, err)
	}

	target, err := ForkTargetAt("hoodi", before)
	if err != nil {
		t.Fatal(err)
	}
	if target.Name != "Glamsterdam" || !target.Cancelled || target.Phase() != PhaseCancelled ||
		target.EL.PreHash != "23aa1351" || target.EL.PostHash != "3d068b59" ||
		target.CL.PreDigest != "c6ecb76c" || target.CL.PostDigest != "5ad30129" || target.Activation() != activation.UTC() {
		t.Fatalf("target = %+v (EL %+v, CL %+v)", target, target.EL, target.CL)
	}
	if expired, _ := ForkTargetAt("hoodi", activation.Add(ForkTrackingGrace)); expired.Phase() != PhaseNone {
		t.Fatalf("target after grace = %+v, want none", expired)
	}

	if _, ok := n.gatherHashes(before)[[4]byte{0x3d, 0x06, 0x8b, 0x59}]; !ok {
		t.Error("a node on the cancelled fork no longer classifies as hoodi")
	}
	if got := ClassifyCL([4]byte{0x5a, 0xd3, 0x01, 0x29}); got != "hoodi" {
		t.Errorf("cancelled Gloas digest classifies as %q", got)
	}
	if IsCurrentCLForkAt("hoodi", "5ad30129", activation.Add(time.Hour)) || IsCurrentForkAt("hoodi", "3d068b59", activation.Add(time.Hour)) {
		t.Error("the cancelled fork reads as current")
	}

	for _, tc := range []struct {
		name string
		ev   ReadinessEvidence
		at   time.Time
		want Readiness
	}{
		{"el still scheduled", ReadinessEvidence{Layer: "el", ForkHash: "23aa1351", ForkNext: hoodiGlamsterdam}, before, NotReady},
		{"el still scheduled, unrefreshed after", ReadinessEvidence{Layer: "el", ForkHash: "23aa1351", ForkNext: hoodiGlamsterdam}, activation.Add(time.Hour), NotReady},
		{"el unscheduled", ReadinessEvidence{Layer: "el", ForkHash: "23aa1351"}, before, Ready},
		{"el unscheduled after", ReadinessEvidence{Layer: "el", ForkHash: "23aa1351"}, activation.Add(time.Hour), Ready},
		{"el on the cancelled fork", ReadinessEvidence{Layer: "el", ForkHash: "3d068b59"}, activation.Add(time.Hour), NotReady},
		{"el older fork", ReadinessEvidence{Layer: "el", ForkHash: "deadbeef"}, before, Stale},
		{"cl still scheduled", ReadinessEvidence{Layer: "cl", ForkHash: "c6ecb76c", ENRForkDigest: "c6ecb76c", ENRNextForkVersion: "80000910", ENRNextForkEpoch: 132352}, before, NotReady},
		{"cl unscheduled", ReadinessEvidence{Layer: "cl", ForkHash: "c6ecb76c", ENRForkDigest: "c6ecb76c", ENRNextForkVersion: "70000910", ENRNextForkEpoch: math.MaxUint64}, before, Ready},
		{"cl on the cancelled fork", ReadinessEvidence{Layer: "cl", ForkHash: "5ad30129"}, activation.Add(time.Hour), NotReady},
		{"cl without ENR", ReadinessEvidence{Layer: "cl", ForkHash: "c6ecb76c"}, before, Unknown},
	} {
		tt, err := ForkTargetAt("hoodi", tc.at)
		if err != nil {
			t.Fatal(err)
		}
		if got := ReadinessAt(tt, "hoodi", tc.ev, tc.at); got != tc.want {
			t.Errorf("%s: ReadinessAt = %q, want %q", tc.name, got, tc.want)
		}
	}

	schedule, err := ScheduleAt("hoodi", before)
	if err != nil {
		t.Fatal(err)
	}
	last := schedule[len(schedule)-1]
	if last.Name != "Glamsterdam" || !last.Cancelled || last.Activated || !last.Tracked || slices.ContainsFunc(schedule[:len(schedule)-1], func(f ScheduledFork) bool { return f.Name == "Glamsterdam" }) {
		t.Fatalf("schedule = %+v", schedule)
	}

	tokenBefore, next, err := ForkEraTokenAt(activation.Add(-time.Minute), "hoodi")
	if err != nil || !next.Equal(activation.UTC()) {
		t.Fatalf("next transition = %s, %v; want the cancelled time", next, err)
	}
	if tokenAfter, _, _ := ForkEraTokenAt(activation, "hoodi"); tokenAfter == tokenBefore {
		t.Error("the era token does not change at the cancelled time, so caches would keep pre-time bodies")
	}
	if !CancelledForkActivatedBetween("hoodi", "el", activation.Add(-time.Minute), activation) ||
		CancelledForkActivatedBetween("hoodi", "el", activation, activation.Add(time.Minute)) {
		t.Error("CancelledForkActivatedBetween is not (from, to]")
	}
}

func TestApplyCancellationsRejects(t *testing.T) {
	for name, mutate := range map[string]func(*CancelledFork){
		"el wrong time":      func(c *CancelledFork) { c.EL.Time++ },
		"cl wrong epoch":     func(c *CancelledFork) { c.CL.Epoch++ },
		"cl wrong version":   func(c *CancelledFork) { c.CL.Version = "80000911" },
		"no layer":           func(c *CancelledFork) { c.EL, c.CL = nil, nil },
		"unknown network":    func(c *CancelledFork) { c.Network = "nowhere" },
		"different instants": func(c *CancelledFork) { c.EL = &CancelledELFork{Name: "Bogota", Time: hoodiGlamsterdam + 1} },
	} {
		t.Run(name, func(t *testing.T) {
			freshNetwork(t, "hoodi")
			c := hoodiGlamsterdamCancellation()
			mutate(&c)
			if _, err := ApplyCancellations([]CancelledFork{c}); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	t.Run("duplicate", func(t *testing.T) {
		freshNetwork(t, "hoodi")
		c := hoodiGlamsterdamCancellation()
		if _, err := ApplyCancellations([]CancelledFork{c, c}); err == nil {
			t.Fatal("accepted")
		}
	})
	t.Run("network in use", func(t *testing.T) {
		n := freshNetwork(t, "hoodi")
		n.load()
		if _, err := ApplyCancellations([]CancelledFork{hoodiGlamsterdamCancellation()}); err == nil {
			t.Fatal("accepted after the network was loaded")
		}
	})
}

func TestCancelledBPOKeepsLaterNames(t *testing.T) {
	const genesis = 1700000000
	epochTime := func(e int) int { return genesis + e*384 }
	cfg := devnetConfig()
	cfg.ELGenesisJSON = []byte(strings.Replace(devnetGenesisJSON, `"osakaTime": 0`,
		fmt.Sprintf(`"osakaTime": 0, "bpo1Time": %d, "bpo2Time": %d`, epochTime(100), epochTime(200)), 1))
	cfg.BlobSchedule = append(cfg.BlobSchedule, BlobParams{Epoch: 100, MaxBlobs: 21}, BlobParams{Epoch: 200, MaxBlobs: 30})
	t.Cleanup(func() {
		registry = slices.DeleteFunc(registry, func(n *Network) bool { return n.Name == "devnet" })
	})
	if err := RegisterDevnet(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyCancellations([]CancelledFork{{Network: "devnet", Fork: "BPO1",
		EL: &CancelledELFork{Name: "BPO1", Time: uint64(epochTime(100))},
		CL: &CancelledCLFork{Name: "BPO1", Epoch: 100, Version: "70000038", MaxBlobs: 21}}}); err != nil {
		t.Fatal(err)
	}
	n, _ := Get("devnet")
	forks, err := n.cl.transitions()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(forks, func(f clFork) bool { return f.name == "BPO2" && f.epoch == 200 }) ||
		slices.ContainsFunc(forks, func(f clFork) bool { return f.name == "BPO1" }) {
		t.Fatalf("transitions = %+v, want BPO2 kept and BPO1 gone", forks)
	}
	pre, _, _ := n.cl.digestAt(99)
	mid, _, _ := n.cl.digestAt(150)
	if pre != mid {
		t.Error("the cancelled BPO still changes the digest")
	}
	at := time.Unix(int64(epochTime(50)), 0)
	target, err := ForkTargetAt("devnet", at)
	if err != nil || target.Name != "BPO1" || !target.Cancelled || target.EL == nil || target.CL == nil {
		t.Fatalf("target = %+v, %v; want the cancelled BPO1 on both layers", target, err)
	}
	if id := n.CurrentForkIDAt(at); id.Next != uint64(epochTime(200)) {
		t.Fatalf("advertised Next = %d, want BPO2 at %d", id.Next, epochTime(200))
	}
}

func TestCancelAbsentForkTracksOnly(t *testing.T) {
	n := freshNetwork(t, "hoodi")
	const later = hoodiGlamsterdam + 384*7000
	absent, err := ApplyCancellations([]CancelledFork{{Network: "hoodi", Fork: "Hegota",
		EL: &CancelledELFork{Name: "Bogota", Time: later},
		CL: &CancelledCLFork{Name: "heze", Epoch: 132352 + 7000, Version: "90000910"}}})
	if err != nil || len(absent) != 2 {
		t.Fatalf("ApplyCancellations = %v, %v; want both layers reported absent", absent, err)
	}
	at := time.Unix(hoodiGlamsterdam+3600, 0)
	if id := n.CurrentForkIDAt(at); id != (forkid.ID{Hash: [4]byte{0x3d, 0x06, 0x8b, 0x59}}) {
		t.Fatalf("canonical schedule changed: %x/%d", id.Hash, id.Next)
	}
	target, err := ForkTargetAt("hoodi", time.Unix(later-60, 0))
	if err != nil || target.Name != "Bogota-Heze" || !target.Cancelled {
		t.Fatalf("target = %+v, %v", target, err)
	}
}
