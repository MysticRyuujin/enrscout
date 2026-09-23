package netconf

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

func TestForkDigestsAreDisjointAcrossNetworks(t *testing.T) {
	seen := map[[4]byte]string{}
	for _, c := range builtinCL() {
		c.compute()
		if len(c.digests) < len(c.forks) {
			t.Errorf("%s: %d digests, want at least one per regular fork (%d)", c.name, len(c.digests), len(c.forks))
		}
		for d := range c.digests {
			if other, ok := seen[d]; ok {
				t.Errorf("fork digest %x maps to both %s and %s", d, other, c.name)
			}
			seen[d] = c.name
			if got := ClassifyCL(d); got != c.name {
				t.Errorf("ClassifyCL(%x) = %q, want %q", d, got, c.name)
			}
		}
	}
}

func TestClassifyCLUnknownDigest(t *testing.T) {
	if got := ClassifyCL([4]byte{0xff, 0xff, 0xff, 0xff}); got != "" {
		t.Errorf("unknown digest classified as %q", got)
	}
}

// Post-Fusaka nodes advertise EIP-7892 masked digests; every built-in network must
// classify the digest of its final blob-schedule era (what live nodes advertise today).
func TestBuiltinNetworksClassifyPostFuluDigests(t *testing.T) {
	for _, c := range builtinCL() {
		if len(c.blobSchedule) == 0 {
			t.Errorf("%s: empty blobSchedule; post-Fulu digests are all masked", c.name)
			continue
		}
		fork, err := c.forkAt(c.fuluEpoch)
		if err != nil {
			t.Fatal(err)
		}
		raw := c.rawDigest(fork.version)
		if got := ClassifyCL(raw); got != "" {
			t.Errorf("ClassifyCL(raw Fulu digest %x) = %q, want unknown", raw, got)
		}
		for _, epoch := range append([]uint64{c.fuluEpoch}, c.blobSchedule[1].epoch, c.blobSchedule[2].epoch) {
			d, _, err := c.digestAt(epoch)
			if err != nil {
				t.Fatal(err)
			}
			if got := ClassifyCL(d); got != c.name {
				t.Errorf("ClassifyCL(%x, epoch %d) = %q, want %q", d, epoch, got, c.name)
			}
		}
	}
}

func TestCurrentCLForkDigests(t *testing.T) {
	for _, c := range builtinCL() {
		at := c.timeAtEpoch(lastScheduledEpoch(c) + 1)
		state, err := CLForkStateAt(c.name, at)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		d := hex.EncodeToString(state.Digest[:])
		if !IsCurrentCLForkAt(c.name, d, at) {
			t.Errorf("%s: current digest %s not IsCurrentCLForkAt", c.name, d)
		}
		if got := ClassifyCL(state.Digest); got != c.name {
			t.Errorf("%s: current digest %s classifies as %q", c.name, d, got)
		}
		genesis := c.rawDigest(c.forks[0].version)
		if IsCurrentCLForkAt(c.name, hex.EncodeToString(genesis[:]), at) {
			t.Errorf("%s: genesis-era digest counted as current", c.name)
		}
	}
	if _, err := CLForkStateAt("nope", time.Now()); err == nil {
		t.Fatal("unknown network accepted")
	}
	if IsCurrentCLForkAt("mainnet", "zz", time.Now()) {
		t.Fatal("malformed digest accepted")
	}
}

func TestCurrentCLForkENR(t *testing.T) {
	for _, c := range builtinCL() {
		at := c.timeAtEpoch(lastScheduledEpoch(c) + 1)
		state, err := CLForkStateAt(c.name, at)
		if err != nil {
			t.Fatal(err)
		}
		entry := state.ENRForkID()
		if len(entry) != 16 {
			t.Fatalf("%s entry length = %d, want 16", c.name, len(entry))
		}
		var digest [4]byte
		copy(digest[:], entry[:4])
		if got := ClassifyCL(digest); got != c.name {
			t.Errorf("%s current digest classifies as %q", c.name, got)
		}
		if !bytes.Equal(entry[4:8], state.CurrentVersion[:]) {
			t.Errorf("%s no-next-fork version = %x, want current %x", c.name, entry[4:8], state.CurrentVersion)
		}
		if got := binary.LittleEndian.Uint64(entry[8:16]); got != ^uint64(0) {
			t.Errorf("%s next fork epoch = %d, want FAR_FUTURE_EPOCH", c.name, got)
		}
	}
}

func TestCLForkStateBoundaries(t *testing.T) {
	for _, c := range builtinCL() {
		epochs := make([]uint64, 0, len(c.forks)+len(c.blobSchedule))
		for _, fork := range c.forks {
			if fork.epoch > 0 {
				epochs = append(epochs, fork.epoch)
			}
		}
		for _, blob := range c.blobSchedule {
			if blob.epoch >= c.fuluEpoch && blob.epoch > 0 {
				epochs = append(epochs, blob.epoch)
			}
		}
		for _, epoch := range epochs {
			boundary := c.timeAtEpoch(epoch)
			before, err := CLForkStateAt(c.name, boundary.Add(-time.Duration(c.secondsPerSlot)*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			at, err := CLForkStateAt(c.name, boundary)
			if err != nil {
				t.Fatal(err)
			}
			after, err := CLForkStateAt(c.name, boundary.Add(time.Duration(c.secondsPerSlot)*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if before.Digest == at.Digest {
				t.Errorf("%s digest did not change at epoch %d", c.name, epoch)
			}
			if after.Digest != at.Digest {
				t.Errorf("%s digest changed again one slot after epoch %d", c.name, epoch)
			}
		}
	}
}

func builtinCL() []*clNetwork {
	var out []*clNetwork
	for _, n := range registry {
		if n.cl != nil {
			out = append(out, n.cl)
		}
	}
	return out
}

func lastScheduledEpoch(c *clNetwork) uint64 {
	return max(c.forks[len(c.forks)-1].epoch, c.blobSchedule[len(c.blobSchedule)-1].epoch)
}

// Glamsterdam activates both layers at one instant. The digests are computed
// independently of this package and the fork ids come from geth's forkid tests.
func TestSepoliaGlamsterdamTransition(t *testing.T) {
	const amsterdam = 1791294816
	n, _ := Get("sepolia")
	for _, tc := range []struct {
		unix       int64
		el, cl     string
		clNextFork uint64
	}{
		{amsterdam - 1, "268956b6", "74d01459", 353024},
		{amsterdam, "6c1d9423", "669e6c11", ^uint64(0)},
	} {
		at := time.Unix(tc.unix, 0)
		if got := fmt.Sprintf("%x", n.CurrentForkIDAt(at).Hash); got != tc.el {
			t.Errorf("EL fork id at %d = %s, want %s", tc.unix, got, tc.el)
		}
		state, err := CLForkStateAt("sepolia", at)
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(state.Digest[:]); got != tc.cl {
			t.Errorf("CL digest at %d = %s, want %s", tc.unix, got, tc.cl)
		}
		if state.NextForkEpoch != tc.clNextFork {
			t.Errorf("CL next fork epoch at %d = %d, want %d", tc.unix, state.NextForkEpoch, tc.clNextFork)
		}
	}
	if got := ClassifyCL([4]byte{0x66, 0x9e, 0x6c, 0x11}); got != "sepolia" {
		t.Errorf("Gloas digest classifies as %q", got)
	}
}
