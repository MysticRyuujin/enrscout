package main

import (
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/MysticRyuujin/enrscout/internal/clientname"
	"github.com/MysticRyuujin/enrscout/internal/distinct"
	"github.com/MysticRyuujin/enrscout/internal/enrich"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
)

var (
	mBuildInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "enrscout_crawler_build_info",
		Help: "Deployed build; the labels carry the revision, value is always 1.",
	}, []string{"revision", "source_url"})
	mDiscovered = promauto.NewCounter(prometheus.CounterOpts{
		Name: "enrscout_crawler_discovered_total",
		Help: "Nodes admitted to resolution after the per-node attempt gate.",
	})
	mDiscoverySightings = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_crawler_discovery_sightings_total",
		Help: "Nodes yielded by discovery random walks before the per-node attempt gate.",
	}, []string{"protocol", "family"})
	mDiscoveryWalkerSightings = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_crawler_discovery_walker_sightings_total",
		Help: "Nodes yielded by each fixed crawler identity's random walk, before the per-node attempt gate.",
	}, []string{"walker", "protocol", "family"})
	mDiscoveryWalkerDistinct = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "enrscout_crawler_discovery_walker_distinct_identities",
		Help: "Persisted seven-day distinct-identity estimate by fixed walker, protocol, and family.",
	}, []string{"walker", "protocol", "family"})
	mRollingDistinct = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "enrscout_crawler_rolling_distinct_identities",
		Help: "HyperLogLog estimate of distinct identities observed in the rolling seven-day window.",
	}, []string{"protocol", "family"})
	mRollingDistinctWindowStart = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "enrscout_crawler_rolling_distinct_window_start_timestamp_seconds",
		Help: "Inclusive start of the rolling distinct-identity observation window.",
	}, []string{"protocol", "family"})
	mRollingDistinctWindowEnd = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "enrscout_crawler_rolling_distinct_window_end_timestamp_seconds",
		Help: "Exclusive end of the rolling distinct-identity observation window.",
	}, []string{"protocol", "family"})
	mRollingDistinctRelativeError = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "enrscout_crawler_rolling_distinct_relative_error",
		Help: "Configured one-standard-error relative bound of the HyperLogLog estimate.",
	})
	mDiscoveryNewDistinctYield = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "enrscout_crawler_discovery_new_distinct_yield_ratio",
		Help: "Rolling distinct estimate divided by discovery sightings, by protocol and family.",
	}, []string{"protocol", "family"})
	mDiscoveryReobservation = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "enrscout_crawler_discovery_reobservation_ratio",
		Help: "One minus rolling distinct yield; an approximate share of repeated identity sightings.",
	}, []string{"protocol", "family"})
	mNodesetAdmissions = promauto.NewCounter(prometheus.CounterOpts{
		Name: "enrscout_crawler_nodeset_admissions_total",
		Help: "Node identities admitted to the in-memory set; evicted or aged identities may be admitted again, and the counter resets on process restart.",
	})
	mUnclassifiedNodes = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "enrscout_crawler_nodeset_unclassified",
		Help: "Retained nodes not classified into a tracked network.",
	})
	mNodesetClassSize = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "enrscout_crawler_nodeset_class_size",
		Help: "Retained nodes by capacity class (verified, classified, unclassified, fallback).",
	}, []string{"class"})
	mCapacityEvictions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_crawler_capacity_evictions_total",
		Help: "Nodes evicted at capacity to admit a higher-priority record, by evicted class.",
	}, []string{"class"})
	mIgnoredSelf = promauto.NewCounter(prometheus.CounterOpts{
		Name: "enrscout_crawler_self_records_ignored_total",
		Help: "Current or stale crawler discovery identities ignored.",
	})
	mResolved = promauto.NewCounter(prometheus.CounterOpts{
		Name: "enrscout_crawler_resolved_total",
		Help: "Nodes successfully resolved.",
	})
	mResolveFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "enrscout_crawler_resolve_failures_total",
		Help: "Node resolution failures.",
	})
	mResolveFallbacks = promauto.NewCounter(prometheus.CounterOpts{
		Name: "enrscout_crawler_resolve_fallbacks_total",
		Help: "Unresolvable nodes recorded from their signed DHT-served record.",
	})
	mLegacyCandidates = promauto.NewCounter(prometheus.CounterOpts{
		Name: "enrscout_crawler_legacy_el_candidates_total",
		Help: "TCP-capable candidates without ENR fork evidence queued for RLPx eth Status classification.",
	})
	mLegacyIdentified = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_crawler_legacy_el_identified_total",
		Help: "Execution candidates without ENR fork evidence verified and classified by eth Status.",
	}, []string{"network", "direction"})
	mLegacyFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_crawler_legacy_el_failures_total",
		Help: "Execution-candidate Status classification failures by bounded stage.",
	}, []string{"reason"})
	mLegacyDeferrals = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_crawler_legacy_el_deferrals_total",
		Help: "Legacy EL outbound probes deferred and left to inbound identification, by bounded reason.",
	}, []string{"reason"})
	mCandidateAdmissions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_crawler_el_candidate_admissions_total",
		Help: "Unresolved execution candidates offered to bounded nodeset retention, by outcome (admitted or the bounded rejection reason).",
	}, []string{"outcome"})
	mCandidateNodes = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "enrscout_crawler_el_candidates",
		Help: "Retained unclassified execution candidates awaiting Status classification.",
	})
	mCandidateClients = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_crawler_el_candidate_clients_total",
		Help: "Hello-proven client families observed on unclassified execution candidates, by bounded client and outcome (hello_only|promoted).",
	}, []string{"client", "outcome"})
	mInvalidRecords = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_crawler_invalid_records_total",
		Help: "Observations rejected before retention, by bounded reason.",
	}, []string{"reason"})
	mAdvertiserInbound = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_crawler_advertiser_inbound_total",
		Help: "Inbound advertiser fingerprint connections by network, layer, and bounded outcome.",
	}, []string{"network", "layer", "outcome"})
	mCallbackPanics = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_callback_panics_total",
		Help: "Recovered panics at peer-controlled crawler callback boundaries.",
	}, []string{"network", "layer"})
	mFingerprintFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "enrscout_crawler_fingerprint_failures_total",
		Help: "Fingerprint probe failures.",
	})
	mFingerprintFailureReasons = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_crawler_fingerprint_failures_by_reason_total",
		Help: "Fingerprint failures by layer, direction, network, probe phase (initial|refresh), transport error class, and bounded failure stage.",
	}, []string{"layer", "direction", "network", "phase", "class", "reason"})
	mFingerprintRecoveries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_crawler_fingerprint_recoveries_total",
		Help: "Fingerprint probes that succeeded after one or more failures.",
	}, []string{"layer"})
	mFingerprintHelloOnly = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_crawler_fingerprint_hello_only_total",
		Help: "Outbound EL probes that identified the client from the RLPx Hello but could not verify membership (Status exchange failed).",
	}, []string{"network"})
	mFingerprintAttempts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_crawler_fingerprint_attempts_total",
		Help: "Completed fingerprint attempts by layer, direction, and bounded outcome.",
	}, []string{"layer", "direction", "outcome"})
	mFingerprintSuccesses = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_crawler_fingerprint_successes_total",
		Help: "Successful fingerprints by layer and active or passive direction.",
	}, []string{"layer", "direction"})
	mFingerprintDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "enrscout_crawler_fingerprint_duration_seconds",
		Help:    "Fingerprint attempt latency by layer, direction, and bounded outcome.",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"layer", "direction", "outcome"})
	mFingerprintDisconnectReasons = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_crawler_fingerprint_disconnect_reasons_total",
		Help: "Decoded devp2p disconnects by layer, direction, and bounded reason.",
	}, []string{"layer", "direction", "reason"})
	mFingerprintNodes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "enrscout_crawler_fingerprint_nodes",
		Help: "Current classified nodes by layer and fingerprint status.",
	}, []string{"layer", "status"})
	mFingerprintAttemptNodes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "enrscout_crawler_fingerprint_nodes_by_attempts",
		Help: "Current classified nodes by layer and completed consecutive attempt count.",
	}, []string{"layer", "attempts"})
	mFingerprintQueueDepth = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "enrscout_crawler_fingerprint_queue_depth",
		Help: "Current queued outbound fingerprint work by layer.",
	}, []string{"layer"})
	mFingerprintQueueDeferrals = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_crawler_fingerprint_queue_deferrals_total",
		Help: "Due probes deferred because the bounded layer queue was full.",
	}, []string{"layer"})
	mActiveDialDeferrals = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_crawler_active_dial_deferrals_total",
		Help: "Active fingerprint dials deferred by a bounded shared budget, by layer and reason.",
	}, []string{"layer", "reason"})
	mPublishFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "enrscout_crawler_publish_failures_total",
		Help: "Snapshot publish failures.",
	})
	mSnapshotCleanupFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "enrscout_crawler_snapshot_cleanup_failures_total",
		Help: "Failed or unsafe cleanup attempts for uncommitted snapshot generations.",
	})
	mQuarantine = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_crawler_quarantine_total",
		Help: "Publishes rejected by snapshot population guards, by bounded reason.",
	}, []string{"reason"})
	mConsecutiveQuarantines = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "enrscout_crawler_consecutive_quarantines",
		Help: "Publishes rejected by any population guard since the last successful publish; a sustained non-zero value means the manifest is frozen and needs investigation.",
	})
	mPublishDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "enrscout_crawler_publish_duration_seconds",
		Help:    "Time to build and commit a snapshot.",
		Buckets: prometheus.DefBuckets,
	})
	mLastPublish = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "enrscout_crawler_last_publish_timestamp_seconds",
		Help: "Time of the last successfully committed manifest.",
	})
	mSnapshotNodes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "enrscout_crawler_snapshot_nodes",
		Help: "Nodes in the last committed snapshot per network.",
	}, []string{"network"})
	mSnapshotCurrentNodes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "enrscout_crawler_snapshot_current_nodes",
		Help: "Current-fork nodes in the last committed snapshot per network. Alert on sustained decline here rather than blocking publishes on it.",
	}, []string{"network"})
	mSnapshotBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "enrscout_crawler_snapshot_bytes",
		Help: "Bytes of the last committed snapshot per network.",
	}, []string{"network"})
	mNodesetSize = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "enrscout_crawler_nodeset_size",
		Help: "Current in-memory node count across all networks.",
	})
	mCLStatus = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_crawler_cl_status_total",
		Help: "Consensus Status exchanges accompanying successful identifies, by direction and bounded outcome.",
	}, []string{"direction", "outcome"})
	mWalkIterators = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "enrscout_crawler_walk_iterators",
		Help: "Discovery random-walk iterators feeding the resolve pipeline.",
	})
	mResolveThrottleWait = promauto.NewCounter(prometheus.CounterOpts{
		Name: "enrscout_crawler_resolve_throttle_wait_seconds_total",
		Help: "Time discovery readers spent waiting on the global resolve rate budget.",
	})
)

func observeFingerprintAttempt(layer, direction string, duration time.Duration, err error) {
	outcome := "success"
	if err != nil {
		outcome = "failure"
	} else {
		mFingerprintSuccesses.WithLabelValues(layer, direction).Inc()
	}
	mFingerprintAttempts.WithLabelValues(layer, direction, outcome).Inc()
	if duration > 0 {
		mFingerprintDuration.WithLabelValues(layer, direction, outcome).Observe(duration.Seconds())
	}
	if reason := enrich.ProbeDisconnectReason(err); reason != "" {
		mFingerprintDisconnectReasons.WithLabelValues(layer, direction, reason).Inc()
	}
}

func clStatusOutcome(fp enrich.Fingerprint) string {
	switch {
	case fp.Network != "":
		return "ok"
	case fp.ForkHash != "":
		return "classify_miss"
	default:
		return "none"
	}
}

func rejectReason(obs nodeset.Observation) string {
	if obs.Reject != "" {
		return obs.Reject
	}
	return "other"
}

func recordEvictions(obs nodeset.Observation) {
	if obs.Evicted > 0 {
		mCapacityEvictions.WithLabelValues(obs.EvictedClass).Add(float64(obs.Evicted))
	}
}

// observeFingerprintFailure feeds the one consolidated failure family and returns the bounded stage for logging.
func observeFingerprintFailure(layer, direction, network, phase string, err error) string {
	reason := enrich.ProbeFailureKind(err)
	mFingerprintFailures.Inc()
	mFingerprintFailureReasons.WithLabelValues(layer, direction, network, phase, enrich.ProbeErrorClass(err), reason).Inc()
	return reason
}

func updateFingerprintStateMetrics(byNetwork map[string][]nodeset.Row, candidates []nodeset.FingerprintState) {
	mFingerprintNodes.Reset()
	mFingerprintAttemptNodes.Reset()
	for _, rows := range byNetwork {
		for _, row := range rows {
			if row.Layer != "el" && row.Layer != "cl" {
				continue
			}
			mFingerprintNodes.WithLabelValues(row.Layer, row.FPStatus).Inc()
			mFingerprintAttemptNodes.WithLabelValues(row.Layer, fingerprintAttemptBucket(row.FPAttempts)).Inc()
		}
	}
	for _, c := range candidates {
		mFingerprintNodes.WithLabelValues("el_candidate", c.Status).Inc()
		mFingerprintAttemptNodes.WithLabelValues("el_candidate", fingerprintAttemptBucket(c.Attempts)).Inc()
	}
}

// candidateClientBucket keeps the candidate client label bounded: a peer-chosen
// name earns its own series only when this repository recognizes the family,
// mirroring dnspublisher's clientBucket.
func candidateClientBucket(name string) string {
	canonical := clientname.Canonical("el", name)
	if !clientname.Recognized(canonical) {
		return "unknown"
	}
	return canonical
}

func updateDistinctMetrics(state *distinct.State, now time.Time) {
	for _, estimate := range state.Estimates(now) {
		if strings.HasPrefix(estimate.Key, "walker/") {
			parts := strings.Split(estimate.Key, "/")
			if len(parts) == 4 {
				mDiscoveryWalkerDistinct.WithLabelValues(parts[1], parts[2], parts[3]).Set(float64(estimate.Distinct))
			}
			continue
		}
		protocol, family, ok := strings.Cut(estimate.Key, "/")
		if !ok {
			continue
		}
		mRollingDistinct.WithLabelValues(protocol, family).Set(float64(estimate.Distinct))
		mRollingDistinctWindowStart.WithLabelValues(protocol, family).Set(float64(estimate.WindowStart.Unix()))
		mRollingDistinctWindowEnd.WithLabelValues(protocol, family).Set(float64(estimate.WindowEnd.Unix()))
		mRollingDistinctRelativeError.Set(estimate.Error)
		if estimate.Sightings > 0 {
			yield := float64(estimate.Distinct) / float64(estimate.Sightings)
			if yield > 1 {
				yield = 1
			}
			mDiscoveryNewDistinctYield.WithLabelValues(protocol, family).Set(yield)
			mDiscoveryReobservation.WithLabelValues(protocol, family).Set(1 - yield)
		}
	}
}

// initializeDiscoveryMetrics makes the configured walker series visible at zero
// before the metrics server starts. CounterVec otherwise omits a series until its
// first event, which makes a healthy freshly restarted crawler look uninstrumented.
func initializeDiscoveryMetrics(specs []identitySpec, families []string) {
	for _, spec := range specs {
		walker := walkerName(spec)
		for _, protocol := range []string{"v4", "v5"} {
			if !walkSource(spec, protocol) {
				continue
			}
			for _, family := range families {
				mDiscoverySightings.WithLabelValues(protocol, family).Add(0)
				mDiscoveryWalkerSightings.WithLabelValues(walker, protocol, family).Add(0)
				mDiscoveryWalkerDistinct.WithLabelValues(walker, protocol, family).Set(0)
			}
		}
	}
}

func fingerprintAttemptBucket(attempts int32) string {
	if attempts >= 6 {
		return "6+"
	}
	if attempts < 0 {
		attempts = 0
	}
	return strconv.Itoa(int(attempts))
}
