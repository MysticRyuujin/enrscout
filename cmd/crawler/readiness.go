package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
	"github.com/MysticRyuujin/enrscout/internal/snapshot"
	"github.com/MysticRyuujin/enrscout/internal/store"
)

// readinessPointAt applies the same rule as /api/v1/forks, so a history point and the live counts
// agree for the same rows.
func readinessPointAt(target netconf.ForkTarget, network string, rows []nodeset.Row, at time.Time) snapshot.ReadinessPoint {
	point := snapshot.ReadinessPoint{At: at.Unix()}
	if target.EL != nil {
		point.EL = map[string]int{}
	}
	if target.CL != nil {
		point.CL = map[string]int{}
	}
	for _, row := range rows {
		var layer map[string]int
		switch row.Layer {
		case "el":
			layer = point.EL
		case "cl":
			layer = point.CL
		}
		if layer == nil {
			continue
		}
		readiness := netconf.ReadinessAt(target, network, netconf.ReadinessEvidence{
			Layer: row.Layer, ForkHash: row.ForkHash, ForkNext: row.ForkNext,
			ENRForkDigest: row.ENRForkDigest, ENRNextForkVersion: row.ENRNextForkVersion, ENRNextForkEpoch: row.ENRNextForkEpoch,
		}, at)
		layer[string(readiness)]++
	}
	return point
}

// recordReadiness appends one history point per tracked network. It reads the stored object back
// each time rather than trusting memory, so a restart or a host move cannot drop earlier points.
// Failures only warn: history is a side output and must never block a publish.
func (p *publisher) recordReadiness(ctx context.Context, byNet map[string][]nodeset.Row, now time.Time) {
	if !p.readinessAt.IsZero() && now.Sub(p.readinessAt) < snapshot.ReadinessInterval {
		return
	}
	p.readinessAt = now
	for _, network := range p.networks {
		target, err := netconf.ForkTargetAt(network, now)
		if err != nil || target.Phase() == netconf.PhaseNone {
			continue
		}
		key, err := p.layout.ReadinessHistoryKey(network, target.Name)
		if err != nil {
			slog.Warn("readiness history key", "network", network, "err", err)
			continue
		}
		history := &snapshot.ReadinessHistory{Version: snapshot.ReadinessHistoryVersion, Network: network, Fork: target.Name}
		if data, err := p.store.Get(ctx, key); err == nil {
			stored, err := snapshot.DecodeReadinessHistory(data)
			if err != nil {
				// Overwriting an object this binary cannot read could destroy a newer writer's history.
				slog.Warn("readiness history unreadable; not overwriting it", "key", key, "err", err)
				continue
			}
			history = stored
		} else if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("read readiness history", "key", key, "err", err)
			continue
		}
		if elTime, clEpoch := target.Schedule(); history.ELTime != elTime || history.CLEpoch != clEpoch {
			// A rescheduled fork is a different series; mixing them would draw one false curve.
			history.ELTime, history.CLEpoch, history.Points = elTime, clEpoch, nil
		}
		if !history.Append(readinessPointAt(target, network, byNet[network], now)) {
			if last := history.Points[len(history.Points)-1].At; last > now.Unix() {
				slog.Warn("readiness history has a point after now; not appending until the clock passes it", "key", key, "last", time.Unix(last, 0).UTC())
			}
			continue
		}
		data, err := history.Encode()
		if err != nil {
			slog.Warn("encode readiness history", "key", key, "err", err)
			continue
		}
		if err := p.store.Put(ctx, key, data, "application/json"); err != nil {
			slog.Warn("persist readiness history", "key", key, "err", err)
		}
	}
}
