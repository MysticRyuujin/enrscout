// Package netconftest names one scheduled fork that activates both layers at one instant on a
// built-in network, so tests outside netconf can pin to it instead of copying its schedule.
package netconftest

import (
	"testing"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
)

const (
	Network   = "sepolia"
	Fork      = "Glamsterdam"
	CLEpoch   = 353024
	CLVersion = "90000076"
	ELPreHash = "268956b6"
)

// Activation is Sepolia Glamsterdam: EL Amsterdam and CL Gloas at CLEpoch.
var Activation = time.Unix(1791294816, 0).UTC()

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
	t.Cleanup(netconf.ResetForTest(Network))
	if _, err := netconf.ApplyCancellations([]netconf.CancelledFork{{Network: Network, Fork: Fork,
		EL: &netconf.CancelledELFork{Name: "Amsterdam", Time: uint64(Activation.Unix())},
		CL: &netconf.CancelledCLFork{Name: "gloas", Epoch: CLEpoch, Version: CLVersion}}}); err != nil {
		t.Fatal(err)
	}
}
