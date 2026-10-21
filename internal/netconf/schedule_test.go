package netconf

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestScheduleHoodi(t *testing.T) {
	got, err := ScheduleAt("hoodi", time.Unix(hoodiGlamsterdam-86400, 0))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range got {
		names = append(names, f.Name)
	}
	if want := []string{"Pectra", "Fusaka", "BPO1", "BPO2", "Glamsterdam"}; !slices.Equal(names, want) {
		t.Fatalf("schedule = %v, want %v", names, want)
	}
	last := got[len(got)-1]
	if last.EL != "Amsterdam" || last.CL != "gloas" || last.CLEpoch != 132352 || last.Time != hoodiGlamsterdam || last.Activated || !last.Tracked {
		t.Fatalf("Glamsterdam = %+v", last)
	}
	for _, f := range got[:len(got)-1] {
		if !f.Activated || f.Tracked {
			t.Errorf("%s = %+v, want activated and untracked", f.Name, f)
		}
	}
}

func TestScheduleDevnetLaunchedWithForks(t *testing.T) {
	const genesis = 1700000000
	epoch := func(e int) int { return genesis + e*384 }
	cfg := devnetConfig()
	cfg.ELGenesisJSON = []byte(strings.Replace(devnetGenesisJSON, `"pragueTime": 0, "osakaTime": 0`,
		fmt.Sprintf(`"pragueTime": %d, "osakaTime": %d, "bpo1Time": %d, "amsterdamTime": %d, "futureforkTime": %d`,
			genesis, epoch(5), epoch(6), epoch(20), epoch(40)), 1))
	cfg.ELGenesisJSON = []byte(strings.Replace(string(cfg.ELGenesisJSON), `"timestamp": "0x0"`, fmt.Sprintf(`"timestamp": "0x%x"`, genesis), 1))
	cfg.CLForks = []CLForkConfig{
		{Name: "genesis", Epoch: 0, Version: "0x10000038"},
		{Name: "electra", Epoch: 0, Version: "0x60000038"},
		{Name: "fulu", Epoch: 5, Version: "0x70000038"},
		{Name: "gloas", Epoch: 20, Version: "0x80000038"},
	}
	cfg.FuluForkEpoch = 5
	cfg.BlobSchedule = []BlobParams{{Epoch: 0, MaxBlobs: 9}, {Epoch: 6, MaxBlobs: 15}, {Epoch: math.MaxUint64, MaxBlobs: 21}}
	t.Cleanup(func() {
		registry = slices.DeleteFunc(registry, func(n *Network) bool { return n.Name == "devnet" })
	})
	if err := RegisterDevnet(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := ScheduleAt("devnet", time.Unix(genesis, 0))
	if err != nil {
		t.Fatal(err)
	}
	want := []ScheduledFork{
		{Name: "Fusaka", EL: "Osaka", CL: "fulu", Time: int64(epoch(5)), CLEpoch: 5, Tracked: true},
		{Name: "BPO1", EL: "BPO1", CL: "BPO1", Time: int64(epoch(6)), CLEpoch: 6},
		{Name: "Glamsterdam", EL: "Amsterdam", CL: "gloas", Time: int64(epoch(20)), CLEpoch: 20},
		{Name: "Futurefork", EL: "Futurefork", Time: int64(epoch(40))},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("schedule =\n%+v\nwant\n%+v", got, want)
	}
}

func TestScheduleMatchesForkTarget(t *testing.T) {
	for _, n := range registry {
		schedule, err := ScheduleAt(n.Name, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range schedule {
			for _, at := range []time.Time{time.Unix(f.Time-1, 0), time.Unix(f.Time, 0), time.Unix(f.Time+1, 0)} {
				target, err := ForkTargetAt(n.Name, at)
				if err != nil {
					t.Fatal(err)
				}
				got, err := ScheduleAt(n.Name, at)
				if err != nil {
					t.Fatal(err)
				}
				var tracked []ScheduledFork
				for _, g := range got {
					if g.Tracked {
						tracked = append(tracked, g)
					}
				}
				if target.Name == "" {
					if len(tracked) != 0 {
						t.Errorf("%s at %d: tracked %+v with no target", n.Name, at.Unix(), tracked)
					}
					continue
				}
				if len(tracked) != 1 || tracked[0].Name != target.Name || tracked[0].Time != target.Activation().Unix() {
					t.Errorf("%s at %d: tracked %+v, target %s at %s", n.Name, at.Unix(), tracked, target.Name, target.Activation())
				}
			}
		}
	}
}

// An EL fork at a positive genesis timestamp is the genesis ruleset, like a CL fork at epoch 0, so
// neither the schedule nor the tracker shows it.
func TestGenesisForksAreNotUpgrades(t *testing.T) {
	const genesis = 1700000000
	cfg := devnetConfig()
	cfg.ELGenesisJSON = []byte(strings.Replace(strings.Replace(devnetGenesisJSON, `"osakaTime": 0`,
		fmt.Sprintf(`"osakaTime": %d`, genesis), 1), `"timestamp": "0x0"`, fmt.Sprintf(`"timestamp": "0x%x"`, genesis), 1))
	t.Cleanup(func() {
		registry = slices.DeleteFunc(registry, func(n *Network) bool { return n.Name == "devnet" })
	})
	if err := RegisterDevnet(cfg); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(genesis+3600, 0)
	got, err := ScheduleAt("devnet", at)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("schedule = %+v, want no upgrades", got)
	}
	target, err := ForkTargetAt("devnet", at)
	if err != nil {
		t.Fatal(err)
	}
	if target.Phase() != PhaseNone {
		t.Errorf("target = %+v, want none", target)
	}
}
