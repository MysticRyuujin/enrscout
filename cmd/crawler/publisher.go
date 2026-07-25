package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/distinct"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
	"github.com/MysticRyuujin/enrscout/internal/snapshot"
	"github.com/MysticRyuujin/enrscout/internal/store"
)

type publishConfig struct {
	nodeMaxIdle     time.Duration
	verifiedMaxIdle time.Duration
	maxCollapsePct  int
	minCurrentNodes int
	keepGenerations int
	keepAggregates  time.Duration
	forcePublish    bool
}

type publisher struct {
	cfg         publishConfig
	store       store.Store
	layout      snapshot.Layout
	set         *nodeset.Set
	distinct    *distinct.State
	run         *snapshot.RunMetadata
	crawlerID   string
	networks    []string
	statePrefix string
	distinctKey string

	prev         *snapshot.Manifest
	quarantines  int
	forceUsed    bool
	forcePending bool
}

type generation struct {
	network, key, sum string
	data              []byte
	count, current    int
}

// Publish returns an error only when this crawler must stop: another writer advanced the manifest, so
// continuing would fight over it. Every other failure - a guard rejection, a serialization error, a
// failed upload - logs, records a metric and returns nil, because a skipped publish only leaves the
// served data stale and the next tick can succeed.
func (p *publisher) Publish(ctx context.Context) error {
	defer func(start time.Time) { mPublishDuration.Observe(time.Since(start).Seconds()) }(time.Now())
	now := p.generationTime()
	p.pruneStale(now)
	p.recordSetMetrics()

	m, gens, byNet, ok := p.build(now)
	if !ok {
		return nil
	}
	admitted, forced := p.admit(m)
	if !admitted {
		return nil
	}
	uploaded, ok := p.upload(ctx, gens)
	if !ok {
		return nil
	}
	err := snapshot.WriteConditional(ctx, p.store, p.layout, p.prev, m)
	if err != nil && !errors.Is(err, snapshot.ErrManifestConflict) && p.storedIsOwn(ctx, m) {
		// The error was only on the response: the stored manifest is this very write.
		slog.Warn("manifest write reported an error but the commit landed", "err", err)
		err = nil
	}
	if err != nil {
		mPublishFailures.Inc()
		cleanupOrphanGenerations(ctx, p.store, p.layout, uploaded)
		if errors.Is(err, snapshot.ErrManifestConflict) {
			return err
		}
		if forced {
			// Still ambiguous (the reconcile read failed too): the forced shrink may have
			// committed. forcePending blocks further forced admits until a successful
			// commit settles it, so the one-shot override cannot be reused.
			p.forcePending = true
		}
		slog.Error("write manifest", "err", err)
		return nil
	}
	p.prev = m
	if forced || p.forcePending {
		p.forceUsed = true
		p.forcePending = false
	}
	p.quarantines = 0
	mConsecutiveQuarantines.Set(0)
	p.recordPublished(ctx, m, gens, byNet, now)
	return nil
}

// storedIsOwn reports whether the stored manifest is exactly the one this publish attempted,
// i.e. an errored PutIfVersion actually committed before the response was lost.
func (p *publisher) storedIsOwn(ctx context.Context, m *snapshot.Manifest) bool {
	stored, err := snapshot.Read(ctx, p.store, p.layout)
	return err == nil && stored.Run.RunID == m.Run.RunID && stored.GeneratedAt.Equal(m.GeneratedAt)
}

// Generation keys and manifests must stay monotonic across a wall-clock step backwards.
func (p *publisher) generationTime() time.Time {
	now := time.Now().UTC()
	if p.prev != nil && !now.After(p.prev.GeneratedAt) {
		return p.prev.GeneratedAt.Add(time.Nanosecond)
	}
	return now
}

func (p *publisher) pruneStale(now time.Time) {
	if p.cfg.nodeMaxIdle <= 0 && p.cfg.verifiedMaxIdle <= 0 {
		return
	}
	cutoff, verifiedCutoff := time.Time{}, time.Time{}
	if p.cfg.nodeMaxIdle > 0 {
		cutoff = now.Add(-p.cfg.nodeMaxIdle)
	}
	if p.cfg.verifiedMaxIdle > 0 {
		verifiedCutoff = now.Add(-p.cfg.verifiedMaxIdle)
	}
	if removed := p.set.PruneStaleWithVerified(cutoff, verifiedCutoff); removed > 0 {
		slog.Info("pruned stale nodes", "removed", removed,
			"max-idle", p.cfg.nodeMaxIdle, "verified-max-idle", p.cfg.verifiedMaxIdle)
	}
}

func (p *publisher) recordSetMetrics() {
	mNodesetSize.Set(float64(p.set.Len()))
	mUnclassifiedNodes.Set(float64(p.set.CountUnclassified()))
	for i, count := range p.set.ClassCounts() {
		mNodesetClassSize.WithLabelValues(nodeset.ClassName(i)).Set(float64(count))
	}
}

func (p *publisher) build(now time.Time) (*snapshot.Manifest, []generation, map[string][]nodeset.Row, bool) {
	m := &snapshot.Manifest{
		SchemaVersion: snapshot.SchemaVersion,
		GeneratedAt:   now,
		CrawlerID:     p.crawlerID,
		Run:           *p.run,
		Networks:      make(map[string]snapshot.NetworkSnapshot, len(p.networks)),
	}
	byNet := p.set.SnapshotNetworks(p.networks)
	updateFingerprintStateMetrics(byNet)
	gens := make([]generation, 0, len(p.networks))
	for _, network := range p.networks {
		rows := byNet[network]
		data, err := nodeset.ParquetFromRows(rows)
		if err != nil {
			slog.Error("serialize snapshot", "network", network, "err", err)
			return nil, nil, nil, false
		}
		sum := sha256.Sum256(data)
		g := generation{
			network: network, key: p.layout.GenerationKey(network, now), sum: hex.EncodeToString(sum[:]),
			data: data, count: len(rows), current: currentForkCount(network, rows, now),
		}
		gens = append(gens, g)
		m.Networks[network] = snapshot.NetworkSnapshot{
			GenerationKey: g.key, NodeCount: g.count, CurrentNodeCount: g.current,
			SHA256: g.sum, Bytes: len(g.data),
		}
	}
	if err := m.Validate(p.layout); err != nil {
		mPublishFailures.Inc()
		slog.Error("validate manifest before upload", "err", err)
		return nil, nil, nil, false
	}

	return m, gens, byNet, true
}

// admit runs the guards before anything is uploaded, so a rejected publish cannot orphan generation
// objects in the store. forced reports that --force-publish overrode the shrink guard; the caller
// marks it used only after the manifest commits, so a failed or benign publish does not consume it.
func (p *publisher) admit(m *snapshot.Manifest) (admitted, forced bool) {
	if reason := belowFloor(m, p.networks, p.cfg.minCurrentNodes); reason != "" {
		p.quarantine(reason, "network has too few current-fork nodes to publish",
			"min-current-nodes", p.cfg.minCurrentNodes)
		return false, false
	}
	if reason := shrankTooMuch(p.prev, m, p.cfg.maxCollapsePct); reason != "" {
		if !p.cfg.forcePublish || p.forceUsed || p.forcePending {
			p.quarantine(reason, "network shrank too much since the last publish; restart with --force-publish if the reduction is intended",
				"max-collapse-pct", p.cfg.maxCollapsePct)
			return false, false
		}
		slog.Warn("accepting a shrunken publish because --force-publish was set", "reason", reason)
		forced = true
	}
	return true, forced
}

func (p *publisher) quarantine(reason, message string, args ...any) {
	p.quarantines++
	mConsecutiveQuarantines.Set(float64(p.quarantines))
	mQuarantine.WithLabelValues(reason).Inc()
	slog.Error(message, append([]any{"reason", reason, "consecutive", p.quarantines}, args...)...)
}

func (p *publisher) upload(ctx context.Context, gens []generation) ([]string, bool) {
	var uploaded []string
	for _, g := range gens {
		// A failed object-store response can be ambiguous: the server may have committed the object
		// before the client saw an error. Include every attempted key in cleanup, not only calls that
		// returned success.
		attempted := append(uploaded, g.key)
		if err := p.store.Put(ctx, g.key, g.data, "application/vnd.apache.parquet"); err != nil {
			mPublishFailures.Inc()
			slog.Error("publish snapshot", "network", g.network, "err", err)
			cleanupOrphanGenerations(ctx, p.store, p.layout, attempted)
			return nil, false
		}
		uploaded = attempted
	}
	return uploaded, true
}

func (p *publisher) recordPublished(ctx context.Context, m *snapshot.Manifest, gens []generation, byNet map[string][]nodeset.Row, now time.Time) {
	if stateData, err := p.distinct.Marshal(); err != nil {
		slog.Warn("encode rolling-distinct state", "err", err)
	} else if err := p.store.Put(ctx, p.distinctKey, stateData, "application/gzip"); err != nil {
		slog.Warn("persist rolling-distinct state", "err", err)
	}
	updateDistinctMetrics(p.distinct, now)
	if pointData, err := json.Marshal(measurementPointAt(now, p.run, byNet, p.distinct)); err != nil {
		slog.Warn("encode longitudinal aggregate", "err", err)
	} else {
		key := fmt.Sprintf("%s/%s/%s.json", aggregatePrefix(p.statePrefix), p.run.MethodologyID, now.Format(aggregateTimeFormat))
		if err := p.store.Put(ctx, key, pointData, "application/json"); err != nil {
			slog.Warn("persist longitudinal aggregate", "key", key, "err", err)
		}
	}
	pruneAggregates(ctx, p.store, p.statePrefix, p.cfg.keepAggregates, now)
	mLastPublish.Set(float64(now.Unix()))
	for _, g := range gens {
		mSnapshotNodes.WithLabelValues(g.network).Set(float64(g.count))
		mSnapshotCurrentNodes.WithLabelValues(g.network).Set(float64(g.current))
		mSnapshotBytes.WithLabelValues(g.network).Set(float64(len(g.data)))
		slog.Info("snapshot published", "network", g.network, "nodes", g.count, "bytes", len(g.data))
	}
	pruneGenerationsForNetworks(ctx, p.store, p.layout, p.cfg.keepGenerations, m, p.networks)
}

// The publish guards, in full. They catch one thing: a crawl that would replace good data with a
// gutted set. Anything gradual or merely surprising is a gauge for the monitoring system to alert on,
// because a skipped publish only makes data stale and staleness is already visible via /readyz.
func shrankTooMuch(prev, current *snapshot.Manifest, maxCollapsePct int) string {
	if prev == nil {
		return ""
	}
	for network, before := range prev.Networks {
		after, stillServed := current.Networks[network]
		if !stillServed {
			continue
		}
		if shrank(after.NodeCount, before.NodeCount, maxCollapsePct) {
			return "collapse_total"
		}
		// A fork-classification regression can leave the row count untouched while every
		// current-fork node disappears, which is the failure that actually reaches the map.
		if shrank(after.CurrentNodeCount, before.CurrentNodeCount, maxCollapsePct) {
			return "collapse_current"
		}
	}
	return ""
}

func belowFloor(current *snapshot.Manifest, networks []string, minCurrent int) string {
	if minCurrent <= 0 {
		return ""
	}
	for _, network := range networks {
		if current.Networks[network].CurrentNodeCount < minCurrent {
			return "current_below_floor"
		}
	}
	return ""
}

func shrank(after, before, maxPct int) bool {
	return before > 0 && int64(after)*100 < int64(before)*int64(100-maxPct)
}

const aggregateTimeFormat = "20060102T150405.000000000Z"

// Aggregates are the immutable points the documented three- and seven-day stability views are read
// from, so retention can never be set below that assessment window. It bounds only object retention,
// not any guard comparison.
const aggregateRetentionFloor = 7 * 24 * time.Hour

func aggregatePrefix(statePrefix string) string { return statePrefix + "/aggregates" }

func aggregateTime(statePrefix, key string) (time.Time, bool) {
	rest, ok := strings.CutPrefix(key, aggregatePrefix(statePrefix)+"/")
	if !ok || !strings.HasSuffix(rest, ".json") {
		return time.Time{}, false
	}
	_, name, ok := strings.Cut(rest, "/")
	if !ok {
		return time.Time{}, false
	}
	at, err := time.Parse(aggregateTimeFormat, strings.TrimSuffix(name, ".json"))
	return at, err == nil
}

// pruneAggregates sweeps every methodology's series, not just the running one: the key embeds the
// methodology id, so pruning only the current one leaves each prior configuration's aggregates
// accumulating forever at one object per publish.
func pruneAggregates(ctx context.Context, st store.Store, statePrefix string, maxAge time.Duration, now time.Time) {
	if maxAge <= 0 {
		return
	}
	keys, err := st.List(ctx, aggregatePrefix(statePrefix)+"/")
	if err != nil {
		slog.Warn("list longitudinal aggregates", "err", err)
		return
	}
	cutoff := now.Add(-maxAge)
	for _, key := range keys {
		at, ok := aggregateTime(statePrefix, key)
		if !ok || !at.Before(cutoff) {
			continue
		}
		if err := st.Delete(ctx, key); err != nil {
			slog.Warn("prune longitudinal aggregate", "key", key, "err", err)
		}
	}
}

func pruneGenerationsForNetworks(ctx context.Context, st store.Store, layout snapshot.Layout, keep int, current *snapshot.Manifest, networks []string) {
	if keep <= 0 {
		return
	}
	for _, net := range networks {
		keys, err := st.List(ctx, layout.NetworkPrefix(net))
		if err != nil {
			slog.Warn("list generations", "network", net, "err", err)
			continue
		}
		var parquets []string
		for _, k := range keys {
			if layout.IsGenerationKey(net, k) {
				parquets = append(parquets, k)
			}
		}
		if len(parquets) <= keep {
			continue
		}
		sort.Slice(parquets, func(i, j int) bool {
			ti, _ := layout.GenerationTime(net, parquets[i])
			tj, _ := layout.GenerationTime(net, parquets[j])
			if ti.Equal(tj) {
				return parquets[i] < parquets[j]
			}
			return ti.Before(tj)
		})
		protected := ""
		if current != nil {
			if ns, ok := current.Networks[net]; ok {
				protected = ns.GenerationKey
			}
		}
		toDelete := len(parquets) - keep
		for _, k := range parquets {
			if toDelete == 0 {
				break
			}
			if k == protected {
				continue
			}
			if err := st.Delete(ctx, k); err != nil {
				slog.Warn("prune generation", "key", k, "err", err)
				continue
			}
			toDelete--
		}
	}
}
func cleanupOrphanGenerations(parent context.Context, st store.Store, layout snapshot.Layout, keys []string) {
	if len(keys) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	protected := make(map[string]bool)
	current, err := snapshot.Read(ctx, st, layout)
	if err == nil {
		for _, network := range current.Networks {
			protected[network.GenerationKey] = true
		}
	} else if !errors.Is(err, snapshot.ErrNoManifest) {
		mSnapshotCleanupFailures.Inc()
		slog.Warn("skip uncommitted generation cleanup: cannot establish current manifest", "err", err)
		return
	}
	for _, key := range keys {
		if protected[key] {
			continue
		}
		if err := st.Delete(ctx, key); err != nil {
			mSnapshotCleanupFailures.Inc()
			slog.Warn("cleanup orphaned generation after publish failure", "key", key, "err", err)
		}
	}
}
