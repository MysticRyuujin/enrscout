package netconf

import (
	"cmp"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/core/forkid"
)

// CancelledFork names an upgrade that was scheduled and then called off before it activated. The
// canonical schedule drops it, but nodes on releases that still schedule it are tracked against it
// until ForkTrackingGrace after the time it would have activated.
type CancelledFork struct {
	Network string           `json:"network" yaml:"network"`
	Fork    string           `json:"fork" yaml:"fork"`
	EL      *CancelledELFork `json:"el,omitempty" yaml:"el,omitempty"`
	CL      *CancelledCLFork `json:"cl,omitempty" yaml:"cl,omitempty"`
}

type CancelledELFork struct {
	Name string `json:"name" yaml:"name"`
	Time uint64 `json:"time" yaml:"time"`
}

// CancelledCLFork names a regular fork (its version) or a blob-parameter-only fork (BPOn, the version
// active at its epoch, and its blob limit, which EIP-7892 mixes into the digest).
type CancelledCLFork struct {
	Name     string `json:"name" yaml:"name"`
	Epoch    uint64 `json:"epoch" yaml:"epoch"`
	Version  string `json:"version" yaml:"version"`
	MaxBlobs uint64 `json:"max_blobs,omitempty" yaml:"max_blobs,omitempty"`
}

type cancellation struct {
	fork         string
	elName       string
	elTime       uint64
	clName       string
	clEpoch      uint64
	clVersion    [4]byte
	clPreDigest  [4]byte
	clPostDigest [4]byte
	// What the canonical CL schedule lost, so the schedule nodes still running the fork follow can
	// be rebuilt once every cancellation is applied.
	clRemovedFork  *clFork
	clRemovedBlobs []blobParams
	clBPOEpoch     uint64
}

func (c cancellation) hasEL() bool { return c.elTime != 0 }
func (c cancellation) hasCL() bool { return c.clName != "" }

// ApplyCancellations removes cancelled forks from the global schedule. Like RegisterDevnet it must run
// at startup, before any network is used and before concurrent users start; it returns the forks that
// the compiled schedule no longer carries, which are kept for tracking only.
func ApplyCancellations(list []CancelledFork) (absent []string, err error) {
	return applyCancellations(list, func(name string) (*Network, error) {
		n, err := Get(name)
		if err == nil && n.genesis != nil {
			return nil, errors.New("network already in use; apply cancellations at startup")
		}
		return n, err
	})
}

// ValidateCancellations checks a cancellation list against the compiled schedules without applying it,
// so a reloaded file cannot ask for a restart that would then fail.
func ValidateCancellations(list []CancelledFork) error {
	copies := map[string]*Network{}
	_, err := applyCancellations(list, func(name string) (*Network, error) {
		if n, ok := copies[name]; ok {
			return n, nil
		}
		n, err := Get(name)
		if err != nil {
			return nil, err
		}
		copies[name] = n.unloadedCopy()
		return copies[name], nil
	})
	return err
}

func applyCancellations(list []CancelledFork, get func(string) (*Network, error)) (absent []string, err error) {
	seen := map[string]bool{}
	touched := map[*Network]bool{}
	for _, c := range list {
		key := c.Network + "/" + strings.ToLower(c.Fork)
		if seen[key] {
			return nil, fmt.Errorf("cancelled fork %s: duplicate entry", key)
		}
		seen[key] = true
		n, err := get(c.Network)
		if err != nil {
			return nil, fmt.Errorf("cancelled fork %s: %w", key, err)
		}
		touched[n] = true
		if n.originalCL == nil && n.cl != nil {
			n.originalCL = n.cl.clone()
		}
		got, missing, err := n.cancel(c)
		if err != nil {
			return nil, fmt.Errorf("cancelled fork %s: %w", key, err)
		}
		absent = append(absent, missing...)
		n.cancellations = append(n.cancellations, got)
		slices.SortFunc(n.cancellations, func(a, b cancellation) int { return cmp.Compare(a.at(n), b.at(n)) })
	}
	for n := range touched {
		if err := n.cancelledDigests(); err != nil {
			return nil, fmt.Errorf("cancelled forks on %s: %w", n.Name, err)
		}
	}
	return absent, nil
}

func (c cancellation) at(n *Network) int64 {
	if c.hasEL() {
		return int64(c.elTime)
	}
	return n.cl.timeAtEpoch(c.clEpoch).Unix()
}

func (n *Network) cancel(c CancelledFork) (cancellation, []string, error) {
	if c.Fork == "" {
		return cancellation{}, nil, errors.New("fork name is required")
	}
	if c.EL == nil && c.CL == nil {
		return cancellation{}, nil, errors.New("an el or cl entry is required")
	}
	out := cancellation{fork: c.Fork}
	var absent []string
	if c.EL != nil {
		if c.EL.Name == "" || c.EL.Time == 0 {
			return out, nil, errors.New("el needs name and time")
		}
		out.elName, out.elTime = c.EL.Name, c.EL.Time
		if !n.compiledELFork(c.EL.Name, c.EL.Time) {
			if n.compiledELName(c.EL.Name) {
				return out, nil, fmt.Errorf("el %s is scheduled at another time than %d", c.EL.Name, c.EL.Time)
			}
			absent = append(absent, fmt.Sprintf("%s el %s", n.Name, c.EL.Name))
		}
		n.cancelledEL = append(n.cancelledEL, elForkTime{name: c.EL.Name, time: c.EL.Time})
	}
	if c.CL != nil {
		if n.cl == nil {
			return out, nil, errors.New("network has no consensus schedule")
		}
		if c.CL.Name == "" || c.CL.Epoch == 0 || c.CL.Epoch == math.MaxUint64 {
			return out, nil, errors.New("cl needs name and a scheduled epoch")
		}
		version, err := decodeVersion(c.CL.Version)
		if err != nil {
			return out, nil, fmt.Errorf("cl: %w", err)
		}
		out.clName, out.clEpoch, out.clVersion = c.CL.Name, c.CL.Epoch, version
		canonical, present, err := n.cl.withoutFork(*c.CL, version, &out)
		if err != nil {
			return out, nil, fmt.Errorf("cl: %w", err)
		}
		if !present {
			absent = append(absent, fmt.Sprintf("%s cl %s", n.Name, c.CL.Name))
		}
		n.cl = canonical
	}
	if out.hasEL() && out.hasCL() && int64(out.elTime) != n.cl.timeAtEpoch(out.clEpoch).Unix() {
		return out, nil, fmt.Errorf("el time %d and cl epoch %d are different instants", out.elTime, out.clEpoch)
	}
	return out, absent, nil
}

func (n *Network) compiledELForks() []elForkTime {
	return mergeELForks(forkTimes(n.ChainConfig), elScheduleOverrides[n.Name], n.extraELForks)
}

func (n *Network) compiledELFork(name string, at uint64) bool {
	return slices.ContainsFunc(n.compiledELForks(), func(f elForkTime) bool { return f.name == name && f.time == at })
}

func (n *Network) compiledELName(name string) bool {
	return slices.ContainsFunc(n.compiledELForks(), func(f elForkTime) bool { return f.name == name })
}

// withoutFork returns the canonical schedule without the fork and records in out what it removed. A
// named fork takes the blob entries at its epoch with it, since they are part of that upgrade. A BPO
// is matched by epoch, not name, because BPO names are positions that shift when the compiled schedule
// drops one; it stays in the numbering, so later BPOs keep the names that pair with geth's BPOn.
func (c *clNetwork) withoutFork(f CancelledCLFork, version [4]byte, out *cancellation) (canonical *clNetwork, present bool, err error) {
	canonical = c.clone()
	if canonical.cancelledBPOs == nil {
		canonical.cancelledBPOs = map[uint64]bool{}
	}
	isBPO := strings.HasPrefix(strings.ToUpper(f.Name), "BPO")
	if !isBPO {
		if strings.EqualFold(f.Name, "electra") || strings.EqualFold(f.Name, "fulu") {
			return nil, false, fmt.Errorf("cancelling %s is not supported", f.Name)
		}
		if i := slices.IndexFunc(c.forks, func(x clFork) bool { return strings.EqualFold(x.name, f.Name) }); i >= 0 {
			x := c.forks[i]
			if x.epoch != f.Epoch || x.version != version {
				return nil, false, fmt.Errorf("%s is scheduled at epoch %d with version %x", x.name, x.epoch, x.version)
			}
			canonical.forks = slices.Delete(canonical.forks, i, i+1)
			out.clRemovedFork = &x
			for _, b := range c.blobSchedule {
				if b.epoch == x.epoch {
					out.clRemovedBlobs = append(out.clRemovedBlobs, b)
				}
			}
			canonical.blobSchedule = slices.DeleteFunc(canonical.blobSchedule, func(b blobParams) bool { return b.epoch == x.epoch })
			return canonical, true, nil
		}
		out.clRemovedFork = &clFork{name: strings.ToLower(f.Name), epoch: f.Epoch, version: version}
		if f.MaxBlobs > 0 {
			out.clRemovedBlobs = []blobParams{{epoch: f.Epoch, maxBlobs: f.MaxBlobs}}
		}
		return canonical, false, nil
	}
	forks, err := c.transitions()
	if err != nil {
		return nil, false, err
	}
	out.clBPOEpoch = f.Epoch
	if i := slices.IndexFunc(forks, func(x clFork) bool { return x.epoch == f.Epoch && strings.HasPrefix(x.name, "BPO") }); i >= 0 {
		x := forks[i]
		bp := slices.IndexFunc(c.blobSchedule, func(b blobParams) bool { return b.epoch == x.epoch })
		if x.version != version || c.blobSchedule[bp].maxBlobs != f.MaxBlobs {
			return nil, false, fmt.Errorf("the BPO at epoch %d has version %x and max blobs %d", x.epoch, x.version, c.blobSchedule[bp].maxBlobs)
		}
		canonical.cancelledBPOs[x.epoch] = true
		return canonical, true, nil
	}
	if f.MaxBlobs == 0 {
		return nil, false, errors.New("a blob-parameter-only fork needs max_blobs")
	}
	out.clRemovedBlobs = []blobParams{{epoch: f.Epoch, maxBlobs: f.MaxBlobs}}
	return canonical, false, nil
}

// cancelledDigests runs after every cancellation is applied, so each pre-fork digest reflects the
// final canonical schedule and each post-fork digest adds back only that one fork.
func (n *Network) cancelledDigests() error {
	for i := range n.cancellations {
		c := &n.cancellations[i]
		if !c.hasCL() {
			continue
		}
		var err error
		if c.clPreDigest, _, err = n.cl.digestAt(c.clEpoch - 1); err != nil {
			return err
		}
		shadow := n.cl.clone()
		if c.clRemovedFork != nil {
			shadow.forks = append(shadow.forks, *c.clRemovedFork)
			slices.SortStableFunc(shadow.forks, func(a, b clFork) int { return cmp.Compare(a.epoch, b.epoch) })
		}
		if c.clBPOEpoch != 0 && shadow.cancelledBPOs[c.clBPOEpoch] {
			shadow.cancelledBPOs = maps.Clone(shadow.cancelledBPOs)
			delete(shadow.cancelledBPOs, c.clBPOEpoch)
		}
		if len(c.clRemovedBlobs) > 0 {
			shadow.blobSchedule = append(shadow.blobSchedule, c.clRemovedBlobs...)
			slices.SortStableFunc(shadow.blobSchedule, func(a, b blobParams) int { return cmp.Compare(a.epoch, b.epoch) })
		}
		if c.clPostDigest, _, err = shadow.digestAt(c.clEpoch); err != nil {
			return err
		}
	}
	return nil
}

func (c *clNetwork) clone() *clNetwork {
	return &clNetwork{
		name: c.name, gvr: c.gvr, genesisTime: c.genesisTime, secondsPerSlot: c.secondsPerSlot,
		slotsPerEpoch: c.slotsPerEpoch, forks: slices.Clone(c.forks), fuluEpoch: c.fuluEpoch,
		blobSchedule: slices.Clone(c.blobSchedule), cancelledBPOs: maps.Clone(c.cancelledBPOs),
	}
}

// forkIDWithCancelledAt is the fork id a node still running the cancelled EL fork advertises at at.
func (n *Network) forkIDWithCancelledAt(cancelled, at uint64) forkid.ID {
	n.load()
	times := slices.Clone(n.idTimes)
	if cancelled > n.genesis.Time() {
		times = append(times, cancelled)
		slices.Sort(times)
		times = slices.Compact(times)
	}
	return n.forkIDFrom(times, at)
}

// cancelledTargetsAt replaces a layer's target with a cancelled fork, chosen per layer like a real
// one: the next upcoming cancellation, or else the latest one within grace. It yields only to a real
// fork scheduled before it, since a node that still schedules the cancelled fork is what matters.
func (n *Network) cancelledTargetsAt(at time.Time, t *ForkTarget) {
	pick := func(has func(cancellation) bool) *cancellation {
		var chosen *cancellation
		for i := range n.cancellations {
			c := &n.cancellations[i]
			when := time.Unix(c.at(n), 0)
			switch {
			case !has(*c) || at.Sub(when) >= ForkTrackingGrace:
			case when.After(at):
				return c
			default:
				chosen = c
			}
		}
		return chosen
	}
	if c := pick(cancellation.hasEL); c != nil && (t.EL == nil || t.EL.Phase != PhaseScheduled || t.EL.Time > c.elTime) {
		pre := n.forkIDAt(c.elTime - 1).Hash
		post := n.forkIDWithCancelledAt(c.elTime, c.elTime).Hash
		t.EL = &ELForkTarget{Name: c.elName, Time: c.elTime, Phase: PhaseCancelled,
			PreHash: hex.EncodeToString(pre[:]), PostHash: hex.EncodeToString(post[:])}
	}
	if c := pick(cancellation.hasCL); c != nil && (t.CL == nil || t.CL.Phase != PhaseScheduled || t.CL.Epoch > c.clEpoch) {
		t.CL = &CLForkTarget{Name: c.clName, Epoch: c.clEpoch, Version: hex.EncodeToString(c.clVersion[:]),
			Phase: PhaseCancelled, Time: n.cl.timeAtEpoch(c.clEpoch).UTC(),
			PreDigest: hex.EncodeToString(c.clPreDigest[:]), PostDigest: hex.EncodeToString(c.clPostDigest[:])}
	}
}

// cancelledELHashes are the fork ids of nodes that went onto a cancelled fork. They keep the network's
// membership, so such nodes stay visible as stale instead of disappearing as unclassified.
func (n *Network) cancelledELHashes(set map[[4]byte]struct{}) {
	if len(n.cancellations) > 0 {
		original := n.compiledELForks()
		times := idForkTimes(original, n.genesis.Time())
		for _, f := range original {
			set[n.forkIDFrom(times, f.time).Hash] = struct{}{}
		}
	}
	for _, c := range n.cancellations {
		if !c.hasEL() {
			continue
		}
		set[n.forkIDWithCancelledAt(c.elTime, c.elTime).Hash] = struct{}{}
		for _, f := range n.elForks {
			if f.time > c.elTime {
				set[n.forkIDWithCancelledAt(c.elTime, f.time).Hash] = struct{}{}
			}
		}
	}
}

// cancelledCLDigest also accepts every digest of the original compiled schedule, which a node that ran
// several cancelled forks in a row advertises.
func (n *Network) cancelledCLDigest(digest [4]byte) bool {
	if n.originalCL != nil {
		n.originalCL.compute()
		if _, ok := n.originalCL.digests[digest]; ok {
			return true
		}
	}
	return slices.ContainsFunc(n.cancellations, func(c cancellation) bool { return c.hasCL() && c.clPostDigest == digest })
}

// cancelledEraAt makes a cancelled activation a cache boundary: no fork id changes there, but rows
// still advertising Next equal to that time stop being current.
func (n *Network) cancelledEraAt(at time.Time) (passed string, next time.Time) {
	var b strings.Builder
	for _, c := range n.cancellations {
		when := time.Unix(c.at(n), 0).UTC()
		if !when.After(at) {
			fmt.Fprintf(&b, "~%d", when.Unix())
		} else if next.IsZero() || when.Before(next) {
			next = when
		}
	}
	return b.String(), next
}

// CancelledForkActivatedBetween reports whether a cancelled fork's would-be activation falls in
// (from, to]. Nodes that still schedule it stop being current then, which collapse guards must allow.
func CancelledForkActivatedBetween(network, layer string, from, to time.Time) bool {
	n, err := Get(network)
	if err != nil {
		return false
	}
	for _, c := range n.cancellations {
		if (layer == "el" && !c.hasEL()) || (layer == "cl" && !c.hasCL()) {
			continue
		}
		when := time.Unix(c.at(n), 0)
		if when.After(from) && !when.After(to) {
			return true
		}
	}
	return false
}

// ResetForTest swaps the named networks for fresh, unloaded copies and returns a function that
// restores the originals. Tests in other packages share networks that earlier tests have already
// loaded, and cancellations apply only to unloaded networks.
func ResetForTest(names ...string) (restore func()) {
	saved := slices.Clone(registry)
	registry = slices.Clone(registry)
	for i, n := range registry {
		if slices.Contains(names, n.Name) {
			registry[i] = n.unloadedCopy()
		}
	}
	return func() { registry = saved }
}

// WithCancellations is ResetForTest followed by ApplyCancellations.
func WithCancellations(list []CancelledFork) (restore func(), err error) {
	var names []string
	for _, c := range list {
		names = append(names, c.Network)
	}
	restore = ResetForTest(names...)
	if _, err := ApplyCancellations(list); err != nil {
		restore()
		return nil, err
	}
	return restore, nil
}

func (n *Network) unloadedCopy() *Network {
	c := &Network{Name: n.Name, NetworkID: n.NetworkID, ChainConfig: n.ChainConfig, genesisFn: n.genesisFn,
		bootnodes: n.bootnodes, clBootnodes: n.clBootnodes, extraELForks: n.extraELForks}
	if n.originalCL != nil {
		c.cl = n.originalCL.clone()
	} else if n.cl != nil {
		c.cl = n.cl.clone()
	}
	return c
}
