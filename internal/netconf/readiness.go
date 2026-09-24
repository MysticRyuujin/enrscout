package netconf

import (
	"encoding/hex"
	"math"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/core/forkid"
)

// ForkTrackingGrace keeps an activated fork tracked, so the cutover stays visible after it lands.
const ForkTrackingGrace = 14 * 24 * time.Hour

const (
	PhaseScheduled = "scheduled"
	PhaseActivated = "activated"
)

type Readiness string

const (
	Ready    Readiness = "ready"
	NotReady Readiness = "not_ready"
	Mismatch Readiness = "mismatch"
	Unknown  Readiness = "unknown"
	Stale    Readiness = "stale"
)

var Readinesses = []Readiness{Ready, NotReady, Mismatch, Unknown, Stale}

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
	Name string        `json:"name"`
	EL   *ELForkTarget `json:"el,omitempty"`
	CL   *CLForkTarget `json:"cl,omitempty"`
}

func (t ForkTarget) Phase() string {
	if (t.EL != nil && t.EL.Phase == PhaseScheduled) || (t.CL != nil && t.CL.Phase == PhaseScheduled) {
		return PhaseScheduled
	}
	if t.EL != nil || t.CL != nil {
		return PhaseActivated
	}
	return ""
}

var combinedForkNames = map[[2]string]string{
	{"cancun", "deneb"}:    "Dencun",
	{"prague", "electra"}:  "Pectra",
	{"osaka", "fulu"}:      "Fusaka",
	{"amsterdam", "gloas"}: "Glamsterdam",
}

// ForkTargetAt picks each layer's next scheduled fork, or else the one activated within
// ForkTrackingGrace. A CL target covers regular forks only: a blob-parameter-only transition
// changes the digest but not the ENR's next_fork fields, so its readiness is not advertised.
func ForkTargetAt(network string, at time.Time) (ForkTarget, error) {
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
	if t.EL != nil && t.CL != nil && t.CL.Time.Unix() != int64(t.EL.Time) {
		// Layers with different activation instants are different upgrades and must not share one
		// name. Track the next scheduled one, or else the latest activated one.
		elScheduled, clScheduled := t.EL.Phase == PhaseScheduled, t.CL.Phase == PhaseScheduled
		elFirst := int64(t.EL.Time) < t.CL.Time.Unix()
		switch {
		case elScheduled && (!clScheduled || elFirst), !elScheduled && !clScheduled && !elFirst:
			t.CL = nil
		default:
			t.EL = nil
		}
	}
	switch {
	case t.EL != nil && t.CL != nil:
		t.Name = combinedForkNames[[2]string{strings.ToLower(t.EL.Name), strings.ToLower(t.CL.Name)}]
		if t.Name == "" {
			t.Name = t.EL.Name + "/" + capitalize(t.CL.Name)
		}
	case t.EL != nil:
		t.Name = t.EL.Name
	case t.CL != nil:
		t.Name = capitalize(t.CL.Name)
	}
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
		if last == nil || last.time == 0 || at.Sub(time.Unix(int64(last.time), 0)) >= ForkTrackingGrace {
			return ELForkTarget{}, false
		}
		chosen, phase = last, PhaseActivated
	}
	pre := forkid.NewID(n.ChainConfig, n.genesis, forkHeadBlock, chosen.time-1).Hash
	post := forkid.NewID(n.ChainConfig, n.genesis, forkHeadBlock, chosen.time).Hash
	return ELForkTarget{
		Name: chosen.name, Time: chosen.time, Phase: phase,
		PreHash: hex.EncodeToString(pre[:]), PostHash: hex.EncodeToString(post[:]),
	}, true
}

func (c *clNetwork) targetAt(at time.Time) (CLForkTarget, bool, error) {
	epoch := c.epochAt(at)
	var chosen *clFork
	phase := PhaseScheduled
	for i := range c.forks {
		if c.forks[i].epoch > epoch && c.forks[i].epoch != math.MaxUint64 {
			chosen = &c.forks[i]
			// Forks sharing an epoch resolve to the last one, the same fork the activated branch below
			// picks, so the target name and its history key do not change at activation.
			for j := i + 1; j < len(c.forks) && c.forks[j].epoch == chosen.epoch; j++ {
				chosen = &c.forks[j]
			}
			break
		}
	}
	if chosen == nil {
		for i := len(c.forks) - 1; i >= 0; i-- {
			f := &c.forks[i]
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
// fork version and epoch equal the target's. After activation a row on the new fork is ready and a
// row still on the pre-fork hash or digest is not. Anything else not current is stale. A row whose
// layer has no tracked fork has no readiness (""). The query
// engine's SQL mirror (readinessConditionAt) must agree; a Go-vs-SQL test pins the two.
func ReadinessAt(t ForkTarget, network string, ev ReadinessEvidence, at time.Time) Readiness {
	current := RowForkCurrentAt(ev.Layer, network, ev.ForkHash, ev.ForkNext, at)
	switch {
	case ev.Layer == "el" && t.EL != nil:
		el := t.EL
		if el.Phase == PhaseActivated {
			return activatedReadiness(current, ev.ForkHash, el.PreHash)
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
		if cl.Phase == PhaseActivated {
			return activatedReadiness(current, ev.ForkHash, cl.PreDigest)
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

func activatedReadiness(current bool, forkHash, pre string) Readiness {
	switch {
	case current:
		return Ready
	case strings.EqualFold(forkHash, pre):
		return NotReady
	default:
		return Stale
	}
}
