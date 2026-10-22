// Package netconftest names one scheduled fork that activates both layers at one instant on a
// built-in network, so tests outside netconf can pin to it instead of copying its schedule.
package netconftest

import (
	"testing"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
)

const (
	Network = "hoodi"
	Fork    = "Glamsterdam"
)

// Activation is Hoodi Glamsterdam: EL Amsterdam and CL Gloas (epoch 132352).
var Activation = time.Unix(1793036568, 0).UTC()

// Before is a day ahead of Activation, inside the scheduled phase.
var Before = Activation.Add(-24 * time.Hour)

// Target is the fork as tracked at Before.
func Target(t testing.TB) netconf.ForkTarget {
	t.Helper()
	target, err := netconf.ForkTargetAt(Network, Before)
	if err != nil {
		t.Fatal(err)
	}
	if target.Name != Fork || target.EL == nil || target.CL == nil || target.Activation() != Activation {
		t.Fatalf("%s target at %s = %+v, want both layers at %s", Network, Before, target, Activation)
	}
	return target
}

// Cancel cancels Fork on Network for the rest of the test, as an operator fork file would.
func Cancel(t testing.TB) {
	t.Helper()
	restore, err := netconf.WithCancellations([]netconf.CancelledFork{{Network: Network, Fork: Fork,
		EL: &netconf.CancelledELFork{Name: "Amsterdam", Time: uint64(Activation.Unix())},
		CL: &netconf.CancelledCLFork{Name: "gloas", Epoch: 132352, Version: "80000910"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
}
