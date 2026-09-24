package netconf

import (
	"math"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/params"
)

const sepoliaAmsterdam = 1791294816

func TestForkTargetSepoliaGlamsterdam(t *testing.T) {
	before := time.Unix(sepoliaAmsterdam-86400, 0)
	target, err := ForkTargetAt("sepolia", before)
	if err != nil {
		t.Fatal(err)
	}
	if target.Name != "Glamsterdam" || target.Phase() != PhaseScheduled {
		t.Fatalf("target = %q phase %q, want Glamsterdam scheduled", target.Name, target.Phase())
	}
	if el := target.EL; el == nil || el.Name != "Amsterdam" || el.Time != sepoliaAmsterdam || el.PreHash != "268956b6" || el.PostHash != "6c1d9423" {
		t.Fatalf("EL target = %+v", target.EL)
	}
	if cl := target.CL; cl == nil || cl.Name != "gloas" || cl.Epoch != 353024 || cl.Version != "90000076" ||
		cl.PreDigest != "74d01459" || cl.PostDigest != "669e6c11" || cl.Time.Unix() != sepoliaAmsterdam {
		t.Fatalf("CL target = %+v", target.CL)
	}

	after, err := ForkTargetAt("sepolia", time.Unix(sepoliaAmsterdam, 0))
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "Glamsterdam" || after.Phase() != PhaseActivated || after.EL.Phase != PhaseActivated || after.CL.Phase != PhaseActivated {
		t.Fatalf("at activation target = %+v", after)
	}

	expired, err := ForkTargetAt("sepolia", time.Unix(sepoliaAmsterdam, 0).Add(ForkTrackingGrace))
	if err != nil {
		t.Fatal(err)
	}
	if expired.CL != nil || (expired.EL != nil && expired.EL.Name == "Amsterdam") {
		t.Fatalf("grace expired but target still tracks Glamsterdam: %+v", expired)
	}
}

func TestForkTargetNoneScheduled(t *testing.T) {
	// Well after Fusaka and its blob-parameter forks, with nothing newer configured.
	target, err := ForkTargetAt("mainnet", time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if target.EL != nil || target.CL != nil || target.Phase() != "" {
		t.Fatalf("mainnet target = %+v, want none", target)
	}
}

func TestReadinessAt(t *testing.T) {
	before := time.Unix(sepoliaAmsterdam-86400, 0)
	target, err := ForkTargetAt("sepolia", before)
	if err != nil {
		t.Fatal(err)
	}
	cur := target.EL.PreHash
	digest := target.CL.PreDigest
	cases := []struct {
		name string
		ev   ReadinessEvidence
		at   time.Time
		want Readiness
	}{
		{"el scheduled", ReadinessEvidence{Layer: "el", ForkHash: cur, ForkNext: sepoliaAmsterdam}, before, Ready},
		{"el not scheduled", ReadinessEvidence{Layer: "el", ForkHash: cur}, before, NotReady},
		{"el other future time", ReadinessEvidence{Layer: "el", ForkHash: cur, ForkNext: sepoliaAmsterdam + 1}, before, Mismatch},
		{"el older fork", ReadinessEvidence{Layer: "el", ForkHash: "deadbeef", ForkNext: sepoliaAmsterdam}, before, Stale},
		{"cl scheduled", ReadinessEvidence{Layer: "cl", ForkHash: digest, ENRForkDigest: digest, ENRNextForkVersion: "90000076", ENRNextForkEpoch: 353024}, before, Ready},
		{"cl right epoch wrong version", ReadinessEvidence{Layer: "cl", ForkHash: digest, ENRForkDigest: digest, ENRNextForkVersion: "90000077", ENRNextForkEpoch: 353024}, before, Mismatch},
		{"cl not scheduled", ReadinessEvidence{Layer: "cl", ForkHash: digest, ENRForkDigest: digest, ENRNextForkVersion: "90000075", ENRNextForkEpoch: math.MaxUint64}, before, NotReady},
		{"cl without ENR", ReadinessEvidence{Layer: "cl", ForkHash: digest}, before, Unknown},
		// Status replaced the digest but the ENR still pairs its epoch with another one.
		{"cl status digest differs from ENR", ReadinessEvidence{Layer: "cl", ForkHash: digest, ENRForkDigest: "12345678", ENRNextForkVersion: "90000076", ENRNextForkEpoch: 353024}, before, Unknown},
		{"cl older digest", ReadinessEvidence{Layer: "cl", ForkHash: "12345678", ENRForkDigest: "12345678", ENRNextForkVersion: "90000076", ENRNextForkEpoch: 353024}, before, Stale},
	}
	for _, tc := range cases {
		if got := ReadinessAt(target, "sepolia", tc.ev, tc.at); got != tc.want {
			t.Errorf("%s: ReadinessAt = %q, want %q", tc.name, got, tc.want)
		}
	}

	activation := time.Unix(sepoliaAmsterdam, 0)
	activated, err := ForkTargetAt("sepolia", activation)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		ev   ReadinessEvidence
		want Readiness
	}{
		{"el upgraded", ReadinessEvidence{Layer: "el", ForkHash: activated.EL.PostHash}, Ready},
		{"el left behind", ReadinessEvidence{Layer: "el", ForkHash: activated.EL.PreHash}, NotReady},
		{"el long stale", ReadinessEvidence{Layer: "el", ForkHash: "deadbeef"}, Stale},
		{"cl upgraded", ReadinessEvidence{Layer: "cl", ForkHash: activated.CL.PostDigest}, Ready},
		{"cl left behind", ReadinessEvidence{Layer: "cl", ForkHash: activated.CL.PreDigest}, NotReady},
	} {
		if got := ReadinessAt(activated, "sepolia", tc.ev, activation); got != tc.want {
			t.Errorf("%s: ReadinessAt = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestForkTargetNeverPairsDifferentInstants(t *testing.T) {
	// A day after mainnet Fusaka: CL Fulu is recently activated while EL already schedules BPO1.
	fulu := mainnetCL.timeAtEpoch(411392)
	target, err := ForkTargetAt("mainnet", fulu.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if target.CL != nil || target.EL == nil || target.EL.Phase != PhaseScheduled || target.Name != target.EL.Name {
		t.Fatalf("target = %+v (EL %+v, CL %+v), want the scheduled EL fork alone", target, target.EL, target.CL)
	}
	// Just before it, both layers schedule the same instant and share the combined name.
	at, err := ForkTargetAt("mainnet", fulu.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if at.Name != "Fusaka" || at.EL == nil || at.CL == nil {
		t.Fatalf("at Fusaka target = %+v", at)
	}
}

func TestReadinessUntrackedLayerHasNoState(t *testing.T) {
	target := ForkTarget{Name: "Gloas", CL: &CLForkTarget{Name: "gloas", Epoch: 1, Phase: PhaseScheduled}}
	if got := ReadinessAt(target, "sepolia", ReadinessEvidence{Layer: "el", ForkHash: "268956b6"}, time.Unix(sepoliaAmsterdam-86400, 0)); got != "" {
		t.Fatalf("EL row under a CL-only target = %q, want no state", got)
	}
}

func TestELTargetKeepsOneNameAcrossTiedActivation(t *testing.T) {
	cfg := *params.SepoliaChainConfig
	tied := uint64(sepoliaAmsterdam)
	cfg.BogotaTime = &tied
	n := &Network{Name: "tied", ChainConfig: &cfg, genesisFn: core.DefaultSepoliaGenesisBlock}
	before, ok := n.elTargetAt(time.Unix(sepoliaAmsterdam-60, 0))
	if !ok {
		t.Fatal("no scheduled target")
	}
	after, ok := n.elTargetAt(time.Unix(sepoliaAmsterdam, 0))
	if !ok {
		t.Fatal("no activated target")
	}
	if before.Name != after.Name || before.Phase != PhaseScheduled || after.Phase != PhaseActivated {
		t.Fatalf("scheduled %q (%s), activated %q (%s): the name, and so the history key, changed at activation",
			before.Name, before.Phase, after.Name, after.Phase)
	}
}

func TestCLTargetKeepsOneNameAcrossTiedActivation(t *testing.T) {
	const genesis = 1_700_000_000
	c := compiledCL("tied", "d8ea171f3c94aea21ebc42a1ed61052acf3f9209c00e4efbaaddac09ed9b8078", genesis,
		[]namedFork{{"phase0", 0, "10000001"}, {"deneb", 0, "40000001"}, {"electra", 5, "50000001"}, {"fulu", 5, "60000001"}}, nil)
	activation := c.timeAtEpoch(5)
	before, ok, err := c.targetAt(activation.Add(-time.Minute))
	if err != nil || !ok {
		t.Fatalf("scheduled target: %v %v", ok, err)
	}
	after, ok, err := c.targetAt(activation)
	if err != nil || !ok {
		t.Fatalf("activated target: %v %v", ok, err)
	}
	if before.Name != "fulu" || after.Name != "fulu" || before.Phase != PhaseScheduled || after.Phase != PhaseActivated {
		t.Fatalf("scheduled %q (%s), activated %q (%s); want fulu for both", before.Name, before.Phase, after.Name, after.Phase)
	}
}
