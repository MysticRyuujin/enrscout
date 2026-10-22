package netconf

import (
	"encoding/hex"
	"math"
	"strings"
	"time"
)

// ForkTrackingGrace keeps an activated fork tracked, so the cutover stays visible after it lands.
const ForkTrackingGrace = 14 * 24 * time.Hour

const (
	PhaseNone      = "none"
	PhaseScheduled = "scheduled"
	PhaseActivated = "activated"
	// PhaseCancelled tracks a fork that was called off: ready means a node no longer schedules it.
	PhaseCancelled = "cancelled"
)

type Readiness string

const (
	Ready    Readiness = "ready"
	Pending  Readiness = "pending"
	NotReady Readiness = "not_ready"
	Mismatch Readiness = "mismatch"
	Unknown  Readiness = "unknown"
	Stale    Readiness = "stale"
)

var Readinesses = []Readiness{Ready, Pending, NotReady, Mismatch, Unknown, Stale}

type ELForkTarget struct {
	Name     string `json:"name"`
	Time     uint64 `json:"time"`
	Phase    string `json:"phase"`
	PreHash  string `json:"pre_hash"`
	PostHash string `json:"post_hash"`
}

type CLForkTarget struct {
	Name       string    `json:"name"`
	Epoch      uint64    `json:"epoch"`
	Version    string    `json:"version"`
	Phase      string    `json:"phase"`
	Time       time.Time `json:"time"`
	PreDigest  string    `json:"pre_digest"`
	PostDigest string    `json:"post_digest"`
}

// ForkTarget is the fork a readiness view tracks on one network. A layer is nil when it has no
// scheduled or recently activated fork.
type ForkTarget struct {
	Name      string        `json:"name"`
	Cancelled bool          `json:"cancelled,omitempty"`
	EL        *ELForkTarget `json:"el,omitempty"`
	CL        *CLForkTarget `json:"cl,omitempty"`
}

func (t ForkTarget) Phase() string {
	if (t.EL != nil && t.EL.Phase == PhaseCancelled) || (t.CL != nil && t.CL.Phase == PhaseCancelled) {
		return PhaseCancelled
	}
	if (t.EL != nil && t.EL.Phase == PhaseScheduled) || (t.CL != nil && t.CL.Phase == PhaseScheduled) {
		return PhaseScheduled
	}
	if t.EL != nil || t.CL != nil {
		return PhaseActivated
	}
	return PhaseNone
}

// Schedule identifies the tracked activation by each layer's own coordinate, so a rescheduled fork
// reads as a different series.
func (t ForkTarget) Schedule() (elTime, clEpoch uint64) {
	if t.EL != nil {
		elTime = t.EL.Time
	}
	if t.CL != nil {
		clEpoch = t.CL.Epoch
	}
	return elTime, clEpoch
}

// Activation is the tracked instant; ForkTargetAt keeps only layers that share it.
func (t ForkTarget) Activation() time.Time {
	switch {
	case t.EL != nil:
		return time.Unix(int64(t.EL.Time), 0).UTC()
	case t.CL != nil:
		return t.CL.Time
	}
	return time.Time{}
}

var combinedForkNames = map[[2]string]string{
	{"shanghai", "capella"}: "Shapella",
	{"cancun", "deneb"}:     "Dencun",
	{"prague", "electra"}:   "Pectra",
	{"osaka", "fulu"}:       "Fusaka",
	{"amsterdam", "gloas"}:  "Glamsterdam",
}

// ForkTargetAt picks each layer's next scheduled fork, or else the one activated within
// ForkTrackingGrace. A CL target includes blob-parameter-only forks, because the ENR's
// next_fork_epoch advertises them (Fulu p2p spec) while next_fork_version stays unchanged.
func ForkTargetAt(network string, at time.Time) (ForkTarget, error) {
	t, err := LayerForkTargetsAt(network, at)
	if err != nil {
		return ForkTarget{}, err
	}
	if t.EL != nil && t.CL != nil && t.CL.Time.Unix() != int64(t.EL.Time) {
		// Layers with different activation instants are different upgrades and must not share one
		// name. Track the next scheduled one, or else the latest activated one.
		elScheduled, clScheduled := t.EL.Phase != PhaseActivated, t.CL.Phase != PhaseActivated
		elFirst := int64(t.EL.Time) < t.CL.Time.Unix()
		switch {
		case elScheduled && (!clScheduled || elFirst), !elScheduled && !clScheduled && !elFirst:
			t.CL = nil
		default:
			t.EL = nil
		}
	}
	var el, cl string
	if t.EL != nil {
		el = t.EL.Name
	}
	if t.CL != nil {
		cl = t.CL.Name
	}
	t.Name = upgradeName(el, cl)
	t.Cancelled = t.Phase() == PhaseCancelled
	return t, nil
}

func upgradeName(el, cl string) string {
	switch {
	case el != "" && cl != "":
		if name := combinedForkNames[[2]string{strings.ToLower(el), strings.ToLower(cl)}]; name != "" {
			return name
		}
		if el == cl {
			return el
		}
		return el + "-" + capitalize(cl)
	case el != "":
		return el
	}
	return capitalize(cl)
}

// LayerForkTargetsAt is each layer's target on its own, unnamed and never dropped for activating at a
// different instant than the other layer. Per-row consumers need it: ForkTargetAt keeps one upgrade
// per view, which would leave a layer unranked while the other layer's earlier fork is scheduled.
func LayerForkTargetsAt(network string, at time.Time) (ForkTarget, error) {
	n, err := Get(network)
	if err != nil {
		return ForkTarget{}, err
	}
	var t ForkTarget
	if el, ok := n.elTargetAt(at); ok {
		t.EL = &el
	}
	if n.cl != nil {
		if cl, ok, err := n.cl.targetAt(at); err != nil {
			return ForkTarget{}, err
		} else if ok {
			t.CL = &cl
		}
	}
	n.cancelledTargetsAt(at, &t)
	t.Cancelled = t.Phase() == PhaseCancelled
	return t, nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (n *Network) elTargetAt(at time.Time) (ELForkTarget, bool) {
	n.load()
	unix := uint64(clampUnix(at))
	var next, last *elForkTime
	for i := range n.elForks {
		f := &n.elForks[i]
		if f.time <= n.genesis.Time() {
			continue
		}
		// Fields are in activation order, so on a tie the last one names the era, before and after it
		// activates alike; otherwise the target's name, and the history key it selects, would change at
		// activation.
		if f.time > unix && (next == nil || f.time <= next.time) {
			next = f
		}
		if f.time <= unix && (last == nil || f.time >= last.time) {
			last = f
		}
	}
	chosen, phase := next, PhaseScheduled
	if chosen == nil {
		if last == nil || at.Sub(time.Unix(int64(last.time), 0)) >= ForkTrackingGrace {
			return ELForkTarget{}, false
		}
		chosen, phase = last, PhaseActivated
	}
	pre := n.forkIDAt(chosen.time - 1).Hash
	post := n.forkIDAt(chosen.time).Hash
	return ELForkTarget{
		Name: chosen.name, Time: chosen.time, Phase: phase,
		PreHash: hex.EncodeToString(pre[:]), PostHash: hex.EncodeToString(post[:]),
	}, true
}

func (c *clNetwork) targetAt(at time.Time) (CLForkTarget, bool, error) {
	epoch := c.epochAt(at)
	forks, err := c.transitions()
	if err != nil {
		return CLForkTarget{}, false, err
	}
	var chosen *clFork
	phase := PhaseScheduled
	for i := range forks {
		if forks[i].epoch > epoch && forks[i].epoch != math.MaxUint64 {
			chosen = &forks[i]
			// Forks sharing an epoch resolve to the last one, the same fork the activated branch below
			// picks, so the target name and its history key do not change at activation.
			for j := i + 1; j < len(forks) && forks[j].epoch == chosen.epoch; j++ {
				chosen = &forks[j]
			}
			break
		}
	}
	if chosen == nil {
		for i := len(forks) - 1; i >= 0; i-- {
			f := &forks[i]
			if f.epoch == 0 || f.epoch > epoch {
				continue
			}
			if at.Sub(c.timeAtEpoch(f.epoch)) < ForkTrackingGrace {
				chosen, phase = f, PhaseActivated
			}
			break
		}
	}
	if chosen == nil {
		return CLForkTarget{}, false, nil
	}
	pre, _, err := c.digestAt(chosen.epoch - 1)
	if err != nil {
		return CLForkTarget{}, false, err
	}
	post, _, err := c.digestAt(chosen.epoch)
	if err != nil {
		return CLForkTarget{}, false, err
	}
	return CLForkTarget{
		Name: chosen.name, Epoch: chosen.epoch, Version: hex.EncodeToString(chosen.version[:]), Phase: phase,
		Time: c.timeAtEpoch(chosen.epoch).UTC(), PreDigest: hex.EncodeToString(pre[:]), PostDigest: hex.EncodeToString(post[:]),
	}, true, nil
}

// ReadinessEvidence is what one persisted row says about the tracked fork. The ENR fields are the
// record's own eth2 schedule, never overwritten by Status, so the next-fork claim is always read
// against the digest it was advertised with.
type ReadinessEvidence struct {
	Layer              string
	ForkHash           string
	ForkNext           uint64
	ENRForkDigest      string
	ENRNextForkVersion string
	ENRNextForkEpoch   uint64
}

// ReadinessAt is the single readiness rule. Before activation a current-fork row is ready when it
// advertises the target itself: the EL fork id's Next equals the fork time, or the CL record's next
// fork version and epoch equal the target's. After activation a row on the new fork is ready. A row
// still on the pre-fork hash or digest is pending when it carries that same pre-activation claim,
// because it was last seen before the fork or has not synced to it, unknown when a CL row has no
// schedule under that digest, and not ready otherwise. Anything else not current is stale. A row whose layer has no tracked fork has no readiness (""). The query
// engine's SQL mirror (readinessConditionAt) must agree; a Go-vs-SQL test pins the two.
func ReadinessAt(t ForkTarget, network string, ev ReadinessEvidence, at time.Time) Readiness {
	current := RowForkCurrentAt(ev.Layer, network, ev.ForkHash, ev.ForkNext, at)
	switch {
	case ev.Layer == "el" && t.EL != nil:
		el := t.EL
		if el.Phase == PhaseCancelled {
			return cancelledReadiness(current, strings.EqualFold(ev.ForkHash, el.PostHash) ||
				(strings.EqualFold(ev.ForkHash, el.PreHash) && ev.ForkNext == el.Time), true)
		}
		if el.Phase == PhaseActivated {
			return activatedReadiness(current, ev.ForkHash, el.PreHash, true, ev.ForkNext == el.Time)
		}
		switch {
		case !current:
			return Stale
		case ev.ForkNext == el.Time:
			return Ready
		case ev.ForkNext == 0:
			return NotReady
		default:
			return Mismatch
		}
	case ev.Layer == "cl" && t.CL != nil:
		cl := t.CL
		if cl.Phase == PhaseCancelled {
			affected := strings.EqualFold(ev.ForkHash, cl.PostDigest) || (strings.EqualFold(ev.ENRForkDigest, cl.PreDigest) &&
				ev.ENRNextForkEpoch == cl.Epoch && strings.EqualFold(ev.ENRNextForkVersion, cl.Version))
			return cancelledReadiness(current, affected, ev.ENRForkDigest != "" && strings.EqualFold(ev.ENRForkDigest, ev.ForkHash))
		}
		if cl.Phase == PhaseActivated {
			readable := strings.EqualFold(ev.ENRForkDigest, cl.PreDigest)
			scheduled := ev.ENRNextForkEpoch == cl.Epoch && strings.EqualFold(ev.ENRNextForkVersion, cl.Version)
			return activatedReadiness(current, ev.ForkHash, cl.PreDigest, readable, scheduled)
		}
		switch {
		case !current:
			return Stale
		case ev.ENRForkDigest == "" || !strings.EqualFold(ev.ENRForkDigest, ev.ForkHash):
			return Unknown
		case ev.ENRNextForkEpoch == cl.Epoch && strings.EqualFold(ev.ENRNextForkVersion, cl.Version):
			return Ready
		case ev.ENRNextForkEpoch == math.MaxUint64:
			return NotReady
		default:
			return Mismatch
		}
	}
	return ""
}

// cancelledReadiness inverts the meaning of ready: a node is good when it no longer schedules the
// cancelled fork. A node that still schedules it, or has already moved onto it, counts as not ready
// whether or not it is current, so a record left unrefreshed after the would-be activation stays
// counted as affected instead of falling out as stale.
func cancelledReadiness(current, affected, readable bool) Readiness {
	switch {
	case affected:
		return NotReady
	case !current:
		return Stale
	case !readable:
		return Unknown
	default:
		return Ready
	}
}

func activatedReadiness(current bool, forkHash, pre string, readable, scheduled bool) Readiness {
	switch {
	case current:
		return Ready
	case !strings.EqualFold(forkHash, pre):
		return Stale
	case !readable:
		return Unknown
	case scheduled:
		return Pending
	default:
		return NotReady
	}
}

// HistoryName keys a target's readiness history. A cancelled fork gets its own series, because its
// ready means unscheduled, the opposite of the scheduled series recorded before the cancellation.
func (t ForkTarget) HistoryName() string {
	if t.Cancelled {
		return t.Name + "-cancelled"
	}
	return t.Name
}
