package query

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/clientname"
	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/snapshot"
	"github.com/MysticRyuujin/enrscout/internal/store"
)

type ReadinessCounts map[netconf.Readiness]int

func newReadinessCounts() ReadinessCounts {
	c := ReadinessCounts{}
	for _, r := range netconf.Readinesses {
		c[r] = 0
	}
	return c
}

type VersionReadiness struct {
	Version string          `json:"version"`
	Total   int             `json:"total"`
	Counts  ReadinessCounts `json:"counts"`
	// Release is the curated-release status shared by every raw version in the bucket, or "mixed".
	Release string `json:"release,omitempty"`
}

type ClientReadiness struct {
	Client   string                 `json:"client"`
	Total    int                    `json:"total"`
	Counts   ReadinessCounts        `json:"counts"`
	Release  *netconf.ClientRelease `json:"release,omitempty"`
	Versions []VersionReadiness     `json:"versions"`
}

type LayerReadiness struct {
	Total  int             `json:"total"`
	Counts ReadinessCounts `json:"counts"`
	Sync   map[string]int  `json:"sync"`
	// Unidentified holds the non-stale rows outside the client-chart population, so Clients plus
	// Unidentified add up to Counts less the stale rows.
	Unidentified ReadinessCounts   `json:"unidentified"`
	Clients      []ClientReadiness `json:"clients"`
}

type ForkReadiness struct {
	Network             string                     `json:"network"`
	ForkEvaluatedAt     string                     `json:"fork_evaluated_at"`
	SnapshotGeneratedAt string                     `json:"snapshot_generated_at,omitempty"`
	FingerprintWindow   int64                      `json:"fingerprint_window_seconds"`
	Phase               string                     `json:"phase"`
	Fork                netconf.ForkTarget         `json:"fork"`
	ReleasesUpdated     string                     `json:"releases_updated"`
	Releases            []netconf.ClientRelease    `json:"releases"`
	Layers              map[string]*LayerReadiness `json:"layers"`
	History             *snapshot.ReadinessHistory `json:"history,omitempty"`
}

const readinessVersionRows = 15

// ForkReadinessAt groups rows by the raw evidence columns in SQL and classifies each group with
// netconf.ReadinessAt, so the aggregate never needs a second copy of the rule.
func (e *Engine) ForkReadinessAt(ctx context.Context, network string, at time.Time) (ForkReadiness, error) {
	e.publishMu.RLock()
	defer e.publishMu.RUnlock()
	state := e.State()
	out := ForkReadiness{
		Network: network, ForkEvaluatedAt: at.UTC().Format(time.RFC3339Nano),
		FingerprintWindow: int64(chartMaxFingerprintAge.Seconds()),
		Layers:            map[string]*LayerReadiness{},
	}
	if !state.GeneratedAt.IsZero() {
		out.SnapshotGeneratedAt = state.GeneratedAt.UTC().Format(time.RFC3339Nano)
	}
	target, err := netconf.ForkTargetAt(network, at)
	if err != nil {
		return out, err
	}
	out.Fork, out.Phase = target, target.Phase()
	if out.Phase == "" {
		out.Phase = "none"
		return out, nil
	}
	out.ReleasesUpdated, _, out.Releases = netconf.ClientReleasesAt(network, target)
	releases := map[string]*netconf.ClientRelease{}
	for i := range out.Releases {
		r := &out.Releases[i]
		releases[r.Layer+"\x00"+r.Client] = r
	}

	chartCond, chartCutoff := chartFingerprintConditionAt(at)
	q := fmt.Sprintf(`SELECT layer, coalesce(fork_hash, ''), coalesce(fork_next, 0),
		coalesce(enr_fork_digest, ''), coalesce(enr_next_fork_version, ''), coalesce(enr_next_fork_epoch, 0),
		coalesce(%s, false), coalesce(client, ''), coalesce(client_version, ''), %s, coalesce(sync_state, 'unknown'), count(*)
		FROM nodes WHERE network = ? AND layer IN ('el', 'cl') GROUP BY ALL`, chartCond, normalizedClientVersionSQL)
	rows, err := e.db.QueryContext(ctx, q, chartCutoff, network)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	type versionKey struct{ layer, client, version string }
	versions := map[versionKey]*VersionReadiness{}
	clients := map[[2]string]*ClientReadiness{}
	for rows.Next() {
		var ev netconf.ReadinessEvidence
		var chart bool
		var client, rawVersion, version, syncState string
		var count int
		if err := rows.Scan(&ev.Layer, &ev.ForkHash, &ev.ForkNext, &ev.ENRForkDigest, &ev.ENRNextForkVersion, &ev.ENRNextForkEpoch,
			&chart, &client, &rawVersion, &version, &syncState, &count); err != nil {
			return out, err
		}
		if (ev.Layer == "el" && target.EL == nil) || (ev.Layer == "cl" && target.CL == nil) {
			continue
		}
		layer := out.Layers[ev.Layer]
		if layer == nil {
			layer = &LayerReadiness{Counts: newReadinessCounts(), Unidentified: newReadinessCounts(), Sync: map[string]int{}}
			out.Layers[ev.Layer] = layer
		}
		readiness := netconf.ReadinessAt(target, network, ev, at)
		layer.Total += count
		layer.Counts[readiness] += count
		if netconf.RowForkCurrentAt(ev.Layer, network, ev.ForkHash, ev.ForkNext, at) {
			layer.Sync[syncState] += count
		}
		// Stale rows are counted per layer but kept out of the client-chart population, which is
		// current-fork only; after activation "left behind" rows are not stale, so they stay in.
		if readiness == netconf.Stale {
			continue
		}
		if !chart || client == "" {
			layer.Unidentified[readiness] += count
			continue
		}
		if !clientname.Recognized(client) {
			client = "Other"
		}
		ck := [2]string{ev.Layer, client}
		c := clients[ck]
		if c == nil {
			c = &ClientReadiness{Client: client, Counts: newReadinessCounts(), Release: releases[ev.Layer+"\x00"+client]}
			clients[ck] = c
		}
		c.Total += count
		c.Counts[readiness] += count
		vk := versionKey{ev.Layer, client, version}
		v := versions[vk]
		if v == nil {
			v = &VersionReadiness{Version: version, Counts: newReadinessCounts()}
			versions[vk] = v
		}
		v.Total += count
		v.Counts[readiness] += count
		if c.Release != nil && !c.Release.Outdated && len(c.Release.MinVersions) > 0 {
			status := netconf.ReleaseStatus(c.Release.MinVersions, rawVersion)
			if v.Release == "" {
				v.Release = status
			} else if v.Release != status {
				v.Release = "mixed"
			}
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	for vk, v := range versions {
		c := clients[[2]string{vk.layer, vk.client}]
		c.Versions = append(c.Versions, *v)
	}
	for ck, c := range clients {
		sortByTotal(c.Versions, func(v VersionReadiness) (int, string) { return v.Total, v.Version })
		if len(c.Versions) > readinessVersionRows {
			other := VersionReadiness{Version: fmt.Sprintf("Other (%d versions)", len(c.Versions)-readinessVersionRows), Counts: newReadinessCounts()}
			for _, v := range c.Versions[readinessVersionRows:] {
				other.Total += v.Total
				for r, n := range v.Counts {
					other.Counts[r] += n
				}
			}
			c.Versions = append(c.Versions[:readinessVersionRows], other)
		}
		layer := out.Layers[ck[0]]
		layer.Clients = append(layer.Clients, *c)
	}
	for _, layer := range out.Layers {
		sortByTotal(layer.Clients, func(c ClientReadiness) (int, string) { return c.Total, c.Client })
	}
	return out, nil
}

func sortByTotal[T any](items []T, key func(T) (int, string)) {
	sort.Slice(items, func(i, j int) bool {
		ni, si := key(items[i])
		nj, sj := key(items[j])
		if ni != nj {
			return ni > nj
		}
		return si < sj
	})
}

// readinessConditionAt is the SQL mirror of netconf.ReadinessAt for the node filter. It is a second
// definition of the rule, pinned to the Go one by TestReadinessMatchesSQLAndGo.
func readinessConditionAt(network, want string, at time.Time) (string, []any, error) {
	target, err := netconf.ForkTargetAt(network, at)
	if err != nil {
		return "", nil, err
	}
	current, currentArgs, err := currentForkConditionAt(at, []string{network})
	if err != nil {
		return "", nil, err
	}
	cur := func() []any { return append([]any(nil), currentArgs...) }
	var parts []string
	var args []any
	add := func(cond string, a ...any) {
		parts = append(parts, "("+cond+")")
		args = append(args, a...)
	}
	activated := func(layer, pre string) {
		switch netconf.Readiness(want) {
		case netconf.Ready:
			add("layer = '"+layer+"' AND "+current, cur()...)
		case netconf.NotReady:
			add("layer = '"+layer+"' AND NOT "+current+" AND lower(fork_hash) = ?", append(cur(), pre)...)
		case netconf.Stale:
			add("layer = '"+layer+"' AND NOT "+current+" AND lower(coalesce(fork_hash, '')) <> ?", append(cur(), pre)...)
		}
	}
	if el := target.EL; el != nil {
		if el.Phase == netconf.PhaseActivated {
			activated("el", el.PreHash)
		} else {
			switch netconf.Readiness(want) {
			case netconf.Ready:
				add("layer = 'el' AND "+current+" AND coalesce(fork_next, 0) = ?", append(cur(), el.Time)...)
			case netconf.NotReady:
				add("layer = 'el' AND "+current+" AND coalesce(fork_next, 0) = 0", cur()...)
			case netconf.Mismatch:
				add("layer = 'el' AND "+current+" AND coalesce(fork_next, 0) NOT IN (0, ?)", append(cur(), el.Time)...)
			case netconf.Stale:
				add("layer = 'el' AND NOT "+current, cur()...)
			}
		}
	}
	if cl := target.CL; cl != nil {
		if cl.Phase == netconf.PhaseActivated {
			activated("cl", cl.PreDigest)
		} else {
			matched := "coalesce(enr_fork_digest, '') <> '' AND lower(enr_fork_digest) = lower(fork_hash)"
			ready := "coalesce(enr_next_fork_epoch, 0) = ? AND lower(coalesce(enr_next_fork_version, '')) = ?"
			// database/sql cannot bind math.MaxUint64, so FAR_FUTURE_EPOCH is a literal.
			notReady := fmt.Sprintf("coalesce(enr_next_fork_epoch, 0) = %d", uint64(math.MaxUint64))
			switch netconf.Readiness(want) {
			case netconf.Ready:
				add("layer = 'cl' AND "+current+" AND "+matched+" AND "+ready, append(cur(), cl.Epoch, cl.Version)...)
			case netconf.NotReady:
				add("layer = 'cl' AND "+current+" AND "+matched+" AND "+notReady, cur()...)
			case netconf.Mismatch:
				add("layer = 'cl' AND "+current+" AND "+matched+" AND NOT ("+ready+") AND NOT ("+notReady+")", append(cur(), cl.Epoch, cl.Version)...)
			case netconf.Unknown:
				add("layer = 'cl' AND "+current+" AND NOT ("+matched+")", cur()...)
			case netconf.Stale:
				add("layer = 'cl' AND NOT "+current, cur()...)
			}
		}
	}
	if len(parts) == 0 {
		return "FALSE", nil, nil
	}
	return "(" + strings.Join(parts, " OR ") + ")", args, nil
}

const maxHistoryPoints = 480

var (
	historyNetwork  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	historyForkName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9/-]*$`)
)

// ReadinessHistoryFor reads the crawler's rolling history without the publication lock, since it is a
// store read that does not depend on the served table. A missing object is an empty history, and so
// is one recorded against another schedule: after a reschedule it holds measurements of the old
// date until an updated crawler starts a new series.
func (e *Engine) ReadinessHistoryFor(ctx context.Context, network string, target netconf.ForkTarget) (*snapshot.ReadinessHistory, error) {
	if !historyNetwork.MatchString(network) || !historyForkName.MatchString(target.Name) {
		return nil, fmt.Errorf("invalid readiness history key %q/%q", network, target.Name)
	}
	data, err := e.store.Get(ctx, e.layout.ReadinessHistoryKey(network, target.Name))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	h, err := snapshot.DecodeReadinessHistory(data)
	if err != nil {
		return nil, err
	}
	var elTime, clEpoch uint64
	if target.EL != nil {
		elTime = target.EL.Time
	}
	if target.CL != nil {
		clEpoch = target.CL.Epoch
	}
	if h.ELTime != elTime || h.CLEpoch != clEpoch {
		return nil, nil
	}
	if n := len(h.Points); n > maxHistoryPoints {
		step := (n + maxHistoryPoints - 1) / maxHistoryPoints
		kept := make([]snapshot.ReadinessPoint, 0, maxHistoryPoints+1)
		for i := 0; i < n; i += step {
			kept = append(kept, h.Points[i])
		}
		if kept[len(kept)-1].At != h.Points[n-1].At {
			kept = append(kept, h.Points[n-1])
		}
		h.Points = kept
	}
	return h, nil
}
