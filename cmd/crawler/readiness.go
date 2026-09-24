package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/clientname"
	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
	"github.com/MysticRyuujin/enrscout/internal/snapshot"
	"github.com/MysticRyuujin/enrscout/internal/store"
)

// readinessPointAt applies the same rule and client-chart population as /api/v1/forks, so a history
// point and the live response agree for the same rows.
func readinessPointAt(target netconf.ForkTarget, network string, rows []nodeset.Row, at time.Time) snapshot.ReadinessPoint {
	point := snapshot.ReadinessPoint{At: at.Unix()}
	freshAfter := at.Add(-7 * 24 * time.Hour).Unix()
	for _, row := range rows {
		var layer map[string]int
		var clients map[string][2]int
		switch {
		case row.Layer == "el" && target.EL != nil:
			if point.EL == nil {
				point.EL, point.ClientsEL = map[string]int{}, map[string][2]int{}
			}
			layer, clients = point.EL, point.ClientsEL
		case row.Layer == "cl" && target.CL != nil:
			if point.CL == nil {
				point.CL, point.ClientsCL = map[string]int{}, map[string][2]int{}
			}
			layer, clients = point.CL, point.ClientsCL
		default:
			continue
		}
		readiness := netconf.ReadinessAt(target, network, netconf.ReadinessEvidence{
			Layer: row.Layer, ForkHash: row.ForkHash, ForkNext: row.ForkNext,
			ENRForkDigest: row.ENRForkDigest, ENRNextForkVersion: row.ENRNextForkVersion, ENRNextForkEpoch: row.ENRNextForkEpoch,
		}, at)
		layer[string(readiness)]++
		if readiness == netconf.Stale || row.Client == "" || (row.FPStatus != "ok" && row.FPStatus != "stale") || row.FPAt < freshAfter {
			continue
		}
		client := row.Client
		if !clientname.Recognized(client) {
			client = "Other"
		}
		c := clients[client]
		if readiness == netconf.Ready {
			c[0]++
		}
		c[1]++
		clients[client] = c
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
		if err != nil || target.Phase() == "" || target.Name == "" {
			continue
		}
		key := p.layout.ReadinessHistoryKey(network, target.Name)
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
		var elTime, clEpoch uint64
		if target.EL != nil {
			elTime = target.EL.Time
		}
		if target.CL != nil {
			clEpoch = target.CL.Epoch
		}
		if history.ELTime != elTime || history.CLEpoch != clEpoch {
			// A rescheduled fork is a different series; mixing them would draw one false curve.
			history.ELTime, history.CLEpoch, history.Points = elTime, clEpoch, nil
		}
		if !history.Append(readinessPointAt(target, network, byNet[network], now)) {
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
