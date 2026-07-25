# Measurement operations

ENRScout reports a non-random sample of identities observed from named crawler
vantages. This runbook defines when a run is warm, how sampling changes are evaluated,
and how independent results are compared. It does not turn identity observations into
machine, operator, validator, or stake estimates.

**Implementation status.** The run-identity, immutable-series, and warm-up mechanics
described here are implemented and in use. The independent-vantage, downstream-merge, and
publication-phase sections describe target operating practice for research-quality
reporting; the current deployment is a single staging vantage and has not yet reached them.

## Run identity and immutable series

Every manifest records the source revision/URL, image digest, sanitized config hash,
run ID, crawler start, schema version, and `methodology_version`. Every successful
publish also writes an immutable point below
`<prefix>/aggregates/<methodology_id>/`, retained for `--keep-aggregates` (default 30 days, swept
across every methodology, and required to exceed the seven-day assessment window), where the ID
binds the methodology version to
the sanitized crawler configuration. A point contains current/stale counts by
layer, membership-evidence and fresh-fingerprint coverage, direction mix, the exact
fork evaluation time, and rolling-distinct estimates with their windows and error.

Changing discovery topology, retention, fingerprint policy, HLL precision/window,
network/fork tables, evidence eligibility, or aggregate definitions requires a new
methodology version. Do not splice the new points into the old series. Superseded
versions stay readable so a bump is an online deployment, but writes are pinned to the
current one, so points land under a new `<methodology_id>` prefix and the previous
series simply ends.

`2026-07-v2` narrowed execution currency to the exact current fork ID. Under `2026-07-v1`
an earlier era carrying its own canonical `Next` counted as current. Measured at the
switchover: mainnet `execution` fell 11607 to 11013 (-5.1%), hoodi 1101 to 1061, sepolia
991 to 909, with each drop appearing in `execution_stale` and consensus counts unchanged.
Do not compare `execution_current`/`execution_stale` across the two. Raw immutable
snapshot generations are the regeneration source for the configured retention horizon.

## Warm-up and steady state

A run is operationally warming up until all of these are true:

1. Seven continuous days have elapsed since cold start or the last methodology change.
2. Snapshots stayed within the configured freshness objective and no publish guard
   fired.
3. Retained counts and rolling-distinct yield are stable across 24-hour, three-day,
   and seven-day views using bands declared before examining the interval.
4. Resolve and fingerprint queues were not persistently saturated, and remote failure
   outcomes did not show a sustained rate-limit regression.
5. Fresh fingerprint coverage and inbound/outbound direction mix remained inside the
   predeclared stability bands.

The UI automatically withholds client percentages for the first 48 hours from the recorded
methodology start, which survives process restarts when the methodology ID is unchanged.
That presentation timer is a minimal disclosure gate, not one of the steady-state
conditions above: it lapses well before the seven-day checklist is satisfied and
certifies none of the other requirements. Operators must continue to label the run
warming in publications until the complete checklist is recorded. Record incidents,
deployments, and methodology changes against the immutable series. A material sample
change restarts the seven-day assessment window.

## Walker experiment

Keep the current traversal set as control. For each candidate walker configuration,
run at least three randomized 24-hour staging intervals from comparable networks and
record, per protocol/family/walker: rolling new-distinct yield, overlap and marginal
yield, candidate and fingerprint queue latency, UDP traffic, bounded remote-error
outcomes, classified coverage, and Status-verified coverage. Confirm non-walking
advertisers remain DHT-reachable.

Adopt the smallest traversal set whose median new-distinct yield is within 5% of the
control, verified coverage is within two percentage points, and queue/remote-error
metrics show no material regression. Lock thresholds before the experiment; archive
the raw points and decision with the methodology version.

## Independent vantages

Research-quality reporting requires at least two crawlers in meaningfully different
regions and networks. Give each vantage a stable crawler ID and a separate object
prefix. Never aim two raw writers at one manifest. Native conditional commits provide
cross-host compare-and-swap; the explicitly configured `verified` mode for partial-S3
backends provides pre/post verification and only a same-identity-directory host lock.

A versioned downstream job may merge the raw generations. For every network/layer it
must publish identity intersection, unique contribution per vantage, fresh client
coverage and direction mix, address-family/ASN/region composition, source run IDs, and
the exact join window. Capture-recapture may be shown only with its independence and
equal-catchability assumptions, uncertainty, and diagnostics; if those assumptions are
visibly false, publish overlap as a bias diagnostic without a population estimate.

## External comparisons

Periodically compare against ethernodes.org, node-crawler-compatible output, ProbeLab,
and current consensus-layer studies. Freeze each source artifact or query timestamp.
Compare definitions and eligibility first, then identity overlap and differences by
protocol, family, direction, ASN, region, and client evidence. External totals are not
ground truth and ENRScout totals must never be tuned to match them.

## Publication checklist

- Beta: Phase 0 correctness/tests are green, health routes are truthful, qualifiers are
  visible, and at least 24 healthy staging hours are recorded.
- Statistics: Phase 1 evidence/totals are green, one stable seven-day window exists,
  two independent vantages have an overlap/bias report, longitudinal points are
  retained, and an outside reviewer has checked the interpretation guide.
- Every publication names its snapshot window, fork evaluation time, vantages,
  numerator/denominator, fingerprint freshness/direction, and methodology version.
