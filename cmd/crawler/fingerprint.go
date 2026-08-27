package main

import (
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"

	"github.com/MysticRyuujin/enrscout/internal/enrich"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
)

type elFingerprintTask struct {
	n      *enode.Node
	via    string
	legacy bool
	// claimed marks a set-resident candidate reserved through ClaimFingerprintAt,
	// so completion must go through the claim machinery (retry schedule, stale-result
	// protection) rather than the fire-and-forget legacy path.
	claimed bool
}

// crawler is the long-lived state the fingerprint paths share. It exists so those paths are
// methods with one owner rather than closures over a dozen locals in run().
type crawler struct {
	conf *config
	set  *nodeset.Set
	geo  *enrich.Geo

	fp   *enrich.Fingerprinter
	clfp *enrich.CLFingerprinter

	pending       *pendingFingerprints
	pendingLegacy *pendingLegacyNodes
	targetBudget  *targetDialBudget

	// attempted bounds resolve attempts per node; legacyAttempted bounds outbound legacy EL probes.
	attempted       *expiringMap[struct{}]
	legacyAttempted *expiringMap[struct{}]

	pool *fingerprintPool
}

func (c *crawler) applyLegacyFingerprint(n *enode.Node, via, direction string, r enrich.Fingerprint) bool {
	now := time.Now()
	observed := c.set.ObserveAuthenticatedEL(n, via, r.Network, hex.EncodeToString(r.ForkID.Hash[:]), r.ForkID.Next, now)
	if !observed.Accepted {
		mInvalidRecords.WithLabelValues(rejectReason(observed)).Inc()
		return false
	}
	if observed.Changed && c.geo != nil {
		addr := n.IP()
		g := c.geo.Lookup(addr)
		c.set.SetGeo(n.ID(), addr, g.Country, g.City, g.Subdivision, g.Lat, g.Lon, g.ASN, g.Org, g.Hosting, g.HostingKnown, g.Geolocated, g.AccuracyRadiusKM)
	}
	c.set.SetFingerprint(n.ID(), r.Client, r.Version, r.OS, r.Lang, r.Caps, direction)
	mLegacyIdentified.WithLabelValues(r.Network, direction).Inc()
	slog.Info("legacy EL node identified", "node", n.ID(), "network", r.Network, "client", r.Client, "direction", direction, "via", via)
	return true
}

func (c *crawler) applyInbound(nid enode.ID, layer string, r enrich.Fingerprint) {
	if c.set.LayerOf(nid) != layer {
		if layer == layerEL && r.Network != "" {
			if candidate, ok := c.pendingLegacy.Take(nid, time.Now()); ok {
				c.applyLegacyFingerprint(candidate.node, candidate.via, "inbound", r)
				return
			}
		}
		c.pending.Put(nid, layer, r, time.Now())
		return
	}
	prev := c.set.NetworkOf(nid)
	if layer == layerEL && prev == "" && r.Network == "" {
		// A Hello without a verified Status must not mark an unclassified candidate
		// verified: fpDone would park it in the never-evicted class and stop its
		// Status retries.
		if c.set.SetCandidateClient(nid, r.Client, r.Version, r.OS, r.Lang, r.Caps, "inbound") {
			mCandidateClients.WithLabelValues(candidateClientBucket(r.Client), "hello_only").Inc()
		}
		return
	}
	if failures := c.set.SetFingerprint(nid, r.Client, r.Version, r.OS, r.Lang, r.Caps, "inbound"); failures > 0 {
		mFingerprintRecoveries.WithLabelValues(layer).Inc()
		slog.Info("fingerprint recovered from inbound connection", "node", nid, "layer", layer, "prior-failures", failures, "client", r.Client)
	}
	if layer == layerEL && r.Network != "" {
		c.set.SetExecutionStatus(nid, r.Network, r.ForkID)
	}
	if layer == layerCL && r.Network != "" {
		c.set.SetConsensusStatus(nid, r.Network, r.ForkHash)
	}
	if layer == layerEL && prev == "" && r.Network != "" && c.set.NetworkOf(nid) == r.Network {
		mLegacyIdentified.WithLabelValues(r.Network, "inbound").Inc()
		mCandidateClients.WithLabelValues(candidateClientBucket(r.Client), "promoted").Inc()
		slog.Info("candidate promoted from inbound connection", "node", nid, "network", r.Network, "client", r.Client)
	}
}

func (c *crawler) enqueueFingerprint(n *enode.Node, now time.Time) bool {
	if c.fp == nil || !c.set.ClaimFingerprintAt(n.ID(), now) {
		return false
	}
	layer := c.set.LayerOf(n.ID())
	if !c.targetBudget.Allow(c.set.IPOf(n.ID()), now) {
		c.set.UnclaimFingerprint(n.ID())
		mActiveDialDeferrals.WithLabelValues(layer, "target_rate").Inc()
		return false
	}
	var accepted bool
	switch layer {
	case "el":
		task := elFingerprintTask{n: n}
		if c.set.NetworkOf(n.ID()) == "" {
			if c.pool.elPressure() {
				c.set.UnclaimFingerprint(n.ID())
				mLegacyDeferrals.WithLabelValues("queue_pressure").Inc()
				return false
			}
			task.legacy, task.claimed = true, true
		}
		accepted = c.pool.enqueueEL(task)
	case "cl":
		accepted = c.pool.enqueueCL(n)
	default:
		c.set.UnclaimFingerprint(n.ID())
		return false
	}
	if !accepted {
		c.set.UnclaimFingerprint(n.ID())
		mFingerprintQueueDeferrals.WithLabelValues(layer).Inc()
	}
	return accepted
}

// retainCandidate admits a TCP-capable unresolved endpoint to the bounded nodeset
// so the normal fingerprint schedule drives its Status probes.
// enqueueLegacyFingerprint stays the fallback when retention is disabled or full,
// so every rejection degrades to the previous one-attempt-per-window behavior.
func (c *crawler) retainCandidate(n *enode.Node, now time.Time) bool {
	if c.conf.maxLegacyCandidates <= 0 || c.fp == nil || !nodeset.HasExecutionTCP(n) {
		return false
	}
	observed := c.set.ObserveCandidateEL(n, now, c.conf.maxLegacyCandidates)
	if !observed.Accepted {
		mCandidateAdmissions.WithLabelValues(rejectReason(observed)).Inc()
		return false
	}
	if observed.Changed && !observed.Applied {
		// A promoted seq-0 row was re-sighted at a different endpoint. Decline the
		// absorb so the caller's legacy fallback dials the fresh sighting; only an
		// authenticated Status may move a published row's endpoint.
		return false
	}
	if observed.New {
		mNodesetAdmissions.Inc()
		mCandidateAdmissions.WithLabelValues("admitted").Inc()
	}
	return true
}

func (c *crawler) enqueueLegacyFingerprint(n *enode.Node, via string) legacyFingerprintDisposition {
	if c.fp == nil || n.TCP() == 0 {
		return legacyFingerprintUnavailable
	}
	now := time.Now()
	c.pendingLegacy.Put(n, via, now)
	if c.pool.elPressure() {
		mLegacyDeferrals.WithLabelValues("queue_pressure").Inc()
		return legacyFingerprintDeferred
	}
	// Claim the window before the budget and queue checks so two workers cannot both enqueue
	// the same node, then release it on either rejection: only a dial actually spends the window.
	if !c.legacyAttempted.Allow(n.ID(), now) {
		mLegacyDeferrals.WithLabelValues("attempt_window").Inc()
		return legacyFingerprintDeferred
	}
	if !c.targetBudget.Allow(n.IP(), now) {
		c.legacyAttempted.Take(n.ID(), now)
		mLegacyDeferrals.WithLabelValues("target_rate").Inc()
		mActiveDialDeferrals.WithLabelValues("el", "target_rate").Inc()
		return legacyFingerprintDeferred
	}
	if !c.pool.enqueueEL(elFingerprintTask{n: n, via: via, legacy: true}) {
		c.legacyAttempted.Take(n.ID(), now)
		mLegacyDeferrals.WithLabelValues("queue_full").Inc()
		return legacyFingerprintDeferred
	}
	mLegacyCandidates.Inc()
	return legacyFingerprintQueued
}

// finishCandidateFingerprint completes a claimed Status probe of a set-resident
// unclassified candidate. Success promotes at today's outbound bar: the dial
// verified the endpoint and key, so one authenticated Status publishes. Failure
// feeds the normal retry schedule instead of the legacy one-shot path.
func (c *crawler) finishCandidateFingerprint(n *enode.Node, r enrich.Fingerprint, probeErr error) {
	now := time.Now()
	if probeErr == nil {
		// Membership first: fpRefresh only arms on endpoint or client changes, so a
		// seq-only record update slips past SetClaimedFingerprint's guard. Marking
		// the row verified before the membership stamp was known to apply would park
		// it as fpDone with no network - never retried, never published.
		observed := c.set.ObserveAuthenticatedEL(n, "dial", r.Network, hex.EncodeToString(r.ForkID.Hash[:]), r.ForkID.Next, now)
		if !observed.Applied {
			if !observed.Accepted {
				mInvalidRecords.WithLabelValues(rejectReason(observed)).Inc()
			}
			c.set.UnclaimFingerprint(n.ID())
			return
		}
		failures, applied := c.set.SetClaimedFingerprint(n.ID(), r.Client, r.Version, r.OS, r.Lang, r.Caps, "outbound")
		if !applied {
			return
		}
		if observed.Changed && c.geo != nil {
			addr := n.IP()
			g := c.geo.Lookup(addr)
			c.set.SetGeo(n.ID(), addr, g.Country, g.City, g.Subdivision, g.Lat, g.Lon, g.ASN, g.Org, g.Hosting, g.HostingKnown, g.Geolocated, g.AccuracyRadiusKM)
		}
		if failures > 0 {
			mFingerprintRecoveries.WithLabelValues(layerEL).Inc()
		}
		c.pendingLegacy.Take(n.ID(), now)
		mLegacyIdentified.WithLabelValues(r.Network, "outbound").Inc()
		mCandidateClients.WithLabelValues(candidateClientBucket(r.Client), "promoted").Inc()
		slog.Info("candidate promoted", "node", n.ID(), "network", r.Network, "client", r.Client, "direction", "outbound")
		return
	}
	if r.Client != "" {
		if c.set.SetCandidateClient(n.ID(), r.Client, r.Version, r.OS, r.Lang, r.Caps, "outbound") {
			mCandidateClients.WithLabelValues(candidateClientBucket(r.Client), "hello_only").Inc()
		}
	}
	retry := c.set.FingerprintFailed(n.ID(), now)
	phase := "initial"
	if retry.Refresh {
		phase = "refresh"
	}
	reason := observeFingerprintFailure(layerEL, "outbound", "", phase, probeErr)
	slog.Debug("candidate classification failed", "node", n.ID(), "attempt", retry.Attempts,
		"retry-at", retry.RetryAt, "reason", reason, "err", probeErr)
}

func (c *crawler) finishFingerprint(layer string, n *enode.Node, r enrich.Fingerprint, probeErr error) {
	if probeErr == nil {
		failures, applied := c.set.SetClaimedFingerprint(n.ID(), r.Client, r.Version, r.OS, r.Lang, r.Caps, "outbound")
		if applied && layer == layerEL && r.Network != "" {
			c.set.SetExecutionStatus(n.ID(), r.Network, r.ForkID)
		}
		if applied && layer == layerCL && r.Network != "" {
			c.set.SetConsensusStatus(n.ID(), r.Network, r.ForkHash)
		}
		if failures > 0 {
			mFingerprintRecoveries.WithLabelValues(layer).Inc()
			slog.Info("fingerprint recovered", "node", n.ID(), "layer", layer, "prior-failures", failures, "client", r.Client)
		}
		return
	}
	// The Hello identifies the client even when the eth Status exchange fails; membership stays ENR-claimed since the network went unverified.
	if layer == layerEL && r.Client != "" {
		network := c.set.NetworkOf(n.ID())
		c.set.SetClaimedFingerprint(n.ID(), r.Client, r.Version, r.OS, r.Lang, r.Caps, "outbound")
		mFingerprintHelloOnly.WithLabelValues(network).Inc()
		slog.Debug("identified client from hello despite status failure", "node", n.ID(), "client", r.Client, "err", probeErr)
		return
	}
	now := time.Now()
	retry := c.set.FingerprintFailed(n.ID(), now)
	phase := "initial"
	if retry.Refresh {
		phase = "refresh"
	}
	reason := observeFingerprintFailure(layer, "outbound", c.set.NetworkOf(n.ID()), phase, probeErr)
	disconnectReason := enrich.ProbeDisconnectReason(probeErr)
	slog.Debug("fingerprint failed", "node", n.ID(), "layer", layer, "attempt", retry.Attempts,
		"retry-at", retry.RetryAt, "reason", reason, "disconnect-reason", disconnectReason, "err", probeErr)
	if retry.BecameFailed {
		slog.Warn("fingerprint repeatedly failed; continuing with slow retries", "node", n.ID(), "layer", layer,
			"attempts", retry.Attempts, "retry-at", retry.RetryAt, "reason", reason, "disconnect-reason", disconnectReason, "err", probeErr)
	}
}
