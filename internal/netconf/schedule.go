package netconf

import (
	"cmp"
	"math"
	"slices"
	"time"
)

// ScheduledFork is one upgrade of a network: the EL and CL forks that activate at one instant, named
// as ForkTargetAt names them. Tracked marks the upgrade that the readiness counts refer to.
type ScheduledFork struct {
	Name      string `json:"name"`
	EL        string `json:"el,omitempty"`
	CL        string `json:"cl,omitempty"`
	Time      int64  `json:"time"`
	CLEpoch   uint64 `json:"cl_epoch,omitempty"`
	Activated bool   `json:"activated"`
	Tracked   bool   `json:"tracked"`
	Cancelled bool   `json:"cancelled,omitempty"`
}

// ScheduleAt lists every timestamp-scheduled upgrade after genesis in activation order; block-numbered
// EL forks have no activation time. Forks active at genesis are the genesis ruleset on each layer, not
// upgrades: an EL fork at or before the genesis timestamp (EIP-6122), and a CL fork at epoch 0.
func ScheduleAt(network string, at time.Time) ([]ScheduledFork, error) {
	n, err := Get(network)
	if err != nil {
		return nil, err
	}
	n.load()
	byTime := map[int64]*ScheduledFork{}
	entry := func(unix int64) *ScheduledFork {
		if byTime[unix] == nil {
			byTime[unix] = &ScheduledFork{Time: unix}
		}
		return byTime[unix]
	}
	// On a tie the last fork names the era, as in elTargetAt and clNetwork.targetAt.
	for _, f := range n.elForks {
		if f.time > n.genesis.Time() && f.time <= math.MaxInt64 {
			entry(int64(f.time)).EL = f.name
		}
	}
	if n.cl != nil {
		forks, err := n.cl.transitions()
		if err != nil {
			return nil, err
		}
		for _, f := range forks {
			if f.epoch == 0 || f.epoch == math.MaxUint64 {
				continue
			}
			e := entry(n.cl.timeAtEpoch(f.epoch).Unix())
			e.CL, e.CLEpoch = f.name, f.epoch
		}
	}
	target, err := ForkTargetAt(network, at)
	if err != nil {
		return nil, err
	}
	out := make([]ScheduledFork, 0, len(byTime))
	for _, e := range byTime {
		e.Name = upgradeName(e.EL, e.CL)
		e.Activated = e.Time <= at.Unix()
		e.Tracked = target.Name != "" && e.Name == target.Name && e.Time == target.Activation().Unix()
		out = append(out, *e)
	}
	for _, c := range n.cancellations {
		e := ScheduledFork{Name: c.fork, EL: c.elName, CL: c.clName, Time: c.at(n), Cancelled: true}
		if c.hasCL() {
			e.CLEpoch = c.clEpoch
		}
		e.Tracked = target.Cancelled && e.Time == target.Activation().Unix()
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b ScheduledFork) int { return cmp.Compare(a.Time, b.Time) })
	return out, nil
}
