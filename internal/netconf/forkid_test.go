package netconf

import (
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/params"
)

func TestForkIDMatchesGeth(t *testing.T) {
	for _, n := range registry {
		plain := &Network{Name: n.Name + "-without-overrides", ChainConfig: n.ChainConfig, genesisFn: n.genesisFn}
		plain.load()
		times := []uint64{0, plain.genesis.Time(), math.MaxUint64}
		for _, f := range plain.elForks {
			times = append(times, f.time-1, f.time, f.time+1)
		}
		for _, at := range times {
			if got, want := plain.forkIDAt(at), forkid.NewID(n.ChainConfig, plain.genesis, forkHeadBlock, at); got != want {
				t.Errorf("%s at %d: fork id %x/%d, geth %x/%d", n.Name, at, got.Hash, got.Next, want.Hash, want.Next)
			}
		}
	}
}

func TestELScheduleOverridesAreStillNeeded(t *testing.T) {
	for network, forks := range elScheduleOverrides {
		n, err := Get(network)
		if err != nil {
			t.Errorf("override for %s: %v", network, err)
			continue
		}
		for _, f := range forks {
			field := reflect.ValueOf(n.ChainConfig).Elem().FieldByName(f.name + "Time")
			if field.IsValid() && !field.IsNil() {
				t.Errorf("go-ethereum now schedules %s on %s at %d; delete this override", f.name, network, field.Elem().Uint())
			}
		}
	}
}

// An extra fork must yield the id that go-ethereum gives once it has a field for the fork.
func TestForkIDWithExtraFork(t *testing.T) {
	const future = hoodiGlamsterdam + 1_000_000
	extra := &Network{Name: "extra", ChainConfig: params.HoodiChainConfig, genesisFn: registry[1].genesisFn,
		extraELForks: []elForkTime{{name: "Futurefork", time: future}}}
	if registry[1].Name != "hoodi" {
		t.Fatalf("registry[1] = %s, want hoodi", registry[1].Name)
	}
	cfg := *params.HoodiChainConfig
	bogota := uint64(future)
	cfg.BogotaTime = &bogota
	extra.load()
	for _, at := range []uint64{hoodiGlamsterdam, future - 1, future, future + 1} {
		if got, want := extra.forkIDAt(at), forkid.NewID(&cfg, extra.genesis, forkHeadBlock, at); got != want {
			t.Errorf("at %d: fork id %x/%d, want %x/%d", at, got.Hash, got.Next, want.Hash, want.Next)
		}
	}
	target, ok := extra.elTargetAt(time.Unix(future-86400, 0))
	if !ok || target.Name != "Futurefork" || target.Time != future {
		t.Fatalf("EL target = %+v, %v; want Futurefork at %d", target, ok, future)
	}
}

func TestRegisterDevnetUnknownELFork(t *testing.T) {
	const futurefork, bpo6 = 1700050000, 1700060000
	cfg := devnetConfig()
	cfg.ELGenesisJSON = []byte(strings.Replace(devnetGenesisJSON, `"osakaTime": 0`,
		`"osakaTime": 0, "futureforkTime": 1700050000, "bpo6Time": 1700060000, "laterTime": null`, 1))
	t.Cleanup(func() {
		registry = slices.DeleteFunc(registry, func(n *Network) bool { return n.Name == "devnet" })
	})
	if err := RegisterDevnet(cfg); err != nil {
		t.Fatal(err)
	}
	n, err := Get("devnet")
	if err != nil {
		t.Fatal(err)
	}
	before := n.CurrentForkIDAt(time.Unix(futurefork-1, 0))
	after := n.CurrentForkIDAt(time.Unix(futurefork, 0))
	if before.Next != futurefork || after.Hash == before.Hash || after.Next != bpo6 {
		t.Fatalf("fork ids before %x/%d, after %x/%d; want Next to name each extra fork", before.Hash, before.Next, after.Hash, after.Next)
	}
	if _, ok := n.gatherHashes(time.Unix(bpo6, 0))[after.Hash]; !ok {
		t.Errorf("post-fork id %x is outside the classify window", after.Hash)
	}
	el, ok := n.elTargetAt(time.Unix(futurefork-60, 0))
	if !ok || el.Name != "Futurefork" {
		t.Errorf("EL target = %+v, want Futurefork", el)
	}
	el, ok = n.elTargetAt(time.Unix(bpo6-60, 0))
	if !ok || el.Name != "BPO6" {
		t.Errorf("EL target = %+v, want BPO6", el)
	}
}

func TestRegisterDevnetRejectsMalformedForkTime(t *testing.T) {
	cfg := devnetConfig()
	cfg.ELGenesisJSON = []byte(strings.Replace(devnetGenesisJSON, `"osakaTime": 0`, `"osakaTime": 0, "futureforkTime": "soon"`, 1))
	t.Cleanup(func() {
		registry = slices.DeleteFunc(registry, func(n *Network) bool { return n.Name == "devnet" })
	})
	if err := RegisterDevnet(cfg); err == nil {
		t.Fatal("non-numeric fork time accepted")
	}
}

// A mistyped override or CL epoch splits one upgrade into two targets, so every paired fork must
// activate both layers at one instant.
func TestELAndCLForksActivateTogether(t *testing.T) {
	for _, n := range registry {
		n.load()
		forks, err := n.cl.transitions()
		if err != nil {
			t.Fatal(err)
		}
		cl := map[string]int64{}
		for _, f := range forks {
			if f.epoch != 0 && f.epoch != math.MaxUint64 {
				cl[strings.ToLower(f.name)] = n.cl.timeAtEpoch(f.epoch).Unix()
			}
		}
		paired := 0
		for _, f := range n.elForks {
			if f.time <= n.genesis.Time() {
				continue
			}
			name := strings.ToLower(f.name)
			for pair := range combinedForkNames {
				if pair[0] == name {
					name = pair[1]
				}
			}
			if at, ok := cl[name]; ok {
				paired++
				if at != int64(f.time) {
					t.Errorf("%s: EL %s at %d, CL %s at %d", n.Name, f.name, f.time, name, at)
				}
			}
		}
		if paired == 0 {
			t.Errorf("%s: no EL fork paired with a CL fork", n.Name)
		}
	}
}
