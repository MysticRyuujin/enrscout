# Definitions

Precise meanings for the terms ENRScout uses in its API, UI, and metrics. These are
product definitions, not implementation notes.

## Node

A distinct devp2p identity, keyed by its **node ID** (the hash of the node's public
key), not by IP address. Multiple nodes behind one IP are counted separately, and a
node that changes IP keeps its identity. A node is included in a network's snapshot
only if it is **classified** into that network (see _Coverage_). A node found only
through legacy discv4 has no signed ENR; once an authenticated RLPx Status exchange
provides a live network-membership claim it is published with its enode URL and an
empty `enr` field; ENRScout never fabricates an ENR.

## Active

A node the crawler retains in the current public snapshot. Each node carries
`first_seen`, `last_seen`, and a score. New directly resolved records start at 1;
repeated successful ENR resolutions raise the score to a maximum of 10, while a
failed resolution halves it and subtracts 2. The score is crawler-local resolution
history from a single vantage point, not confidence, sync status, or
application-protocol reachability; repeated sightings are not independent evidence. At the retention limit, lower-value records (score-zero fallback
leads, then unclassified leads, then classified leads) are evicted before a
higher-value record is rejected; verified or pinned nodes are never evicted. A score-zero DHT-served fallback remains in crawler memory for bounded
resolution and fingerprint retries but enters the snapshot only after direct
resolution or an authenticated transport fingerprint. Unpinned nodes age out after
`--node-max-idle` (24 hours by default). Previously fingerprinted nodes use the
separate `--verified-node-max-idle` retention (7 days by default), measured from their
last successful direct resolution rather than a fallback sighting.

## Identified

A node whose client was confirmed by a successful RLPx or libp2p identity handshake
(`fp_status=ok`). Fingerprints are collected in both directions: outbound probes to
discovered nodes, and inbound connections from peers that dial the crawler's
advertised per-network identities. Inbound is often the only path for peers at their
peer limit or rate-limiting unknown dialers, so identification is not restricted to
nodes that accept our dials, though clients that dial out aggressively may still be
identified sooner than quieter ones. Each row records the direction of its last
successful fingerprint (`fp_direction`), and the per-layer inbound/outbound mix is
shown with the charts so this selection bias stays measurable.

Client-distribution charts count only identifications refreshed within the last
**7 days**, including recent last-known identifications marked `fp_status=stale`.
A previously identified node that stops answering revalidation keeps its last-known
client on its detail page and in charts only until that successful fingerprint exceeds
the seven-day window; the excluded count is shown with each chart's coverage.
Charts are not estimates of the full network's client share. ENR-advertised client
metadata may still appear on node details before an active fingerprint succeeds.

## Network membership and fork readiness

Every row records how its network membership was established
(`membership_source`): **`status`** means the peer made a live membership claim over
an authenticated Status exchange (RLPx `eth` Status for execution nodes, libp2p
consensus Status for consensus nodes), so the peer's network ID and genesis, or its fork
digest, came from the holder of that node identity during a real connection;
**`enr`** means the network is claimed only by the identity's self-signed discovery
record. Authentication binds either statement to the node key; it cannot prove that a
malicious or experimental peer honestly follows the claimed chain. A
directly resolved node can appear in the snapshot as ENR-classified before any
Status exchange succeeds; the field keeps the two evidence tiers distinguishable.
Client names and versions are self-reported. A successful transport handshake ties
the report to the peer identity but does not independently prove the software name;
these strings are never used as a trust boundary.

Execution classification accepts every fork ID in the rolling membership window, so EL
membership is broader than EL currency. An execution observation is **current-fork
compatible** only when its reported EIP-2124 `Hash` equals the single fork ID active at
the request's `fork_evaluated_at` time, and its `Next` is absent or still ahead. A geth
node's advertised fork ID is derived from its synced head rather than its software
version, so an earlier era means the peer is syncing, stalled, or on a chain that shares
this one's genesis. EIP-2124 accepts such a peer for connection admission, but that is
not a statement that it is on the current fork, and it is not honoured here.

Network membership remains hash-based so lagging peers within the rolling classification
window remain auditable; the EL window covers roughly two years to avoid ancient
genesis-sharing fork-ID collisions, and older execution fork IDs become unclassified
rather than `execution_stale`. Current totals, client distributions, map points, and DNS
trees include only current-fork execution observations. Peers on an earlier fork remain
available through the API for forensic completeness, but are counted separately as
`execution_stale` and are not presented in the website's current-network views.
Current-fork compatibility means the peer reports the current fork; it does not prove it
is fully synced to the chain head.

Consensus classification accepts every historical fork digest of a tracked network,
so CL membership is broader than CL currency. A consensus observation is
current-fork compatible only when its digest equals the single digest active at the
request's `fork_evaluated_at` time, including the active blob-parameter era. Older
recognized digests are counted as `consensus_stale`, match `fork=stale`, and remain
available with `fork=all`, but are excluded from headline totals, charts, maps, and
default node results just like stale execution observations.

## Dialability

- **Dialable**: the node advertises an application transport for an address family it
  has: a TCP port (`tcp`/`tcp6`, RLPx for EL, libp2p-TCP for CL) or a QUIC port
  (`quic`/`quic6`, used by consensus-layer clients) matching a present IPv4/IPv6
  address. Surfaced as the `dialable` field and filter. The DNS publisher only includes
  dialable nodes, since an EIP-1459 tree is meant to hand out connectable peers.
  Per the ENR spec, when `tcp6`/`udp6` is absent the `tcp`/`udp` port applies to the
  IPv6 address too, so an IPv6-only node advertising only `tcp` is still dialable.
- **Discovery-only**: the node was found over discv4/discv5 but advertises no dialable
  transport. It is a real network participant and is **included** in snapshots and the
  explorer; it is not directly connectable.

All ports are validated as `uint16`; out-of-range or malformed port entries are dropped
at ENR decode time. Nodes with no globally-routable IP (unspecified, loopback,
multicast, link-local, or private) are never recorded, so they cannot be published.

`dialable` is about _advertised_ capability, not a live connection check. ENRScout
does not currently publish a separate transport-reachability result.

## Coverage

There is no crawl "cycle" with a completion condition: discovery consumes continuous
random walks of the DHT and each snapshot publishes whatever is currently retained
and eligible. A snapshot therefore contains every node the crawler has classified
and verified as active _so far, from one vantage point_, never a provably complete
population. Nodes that share the DHT but belong to other chains, plus unverified DHT
fallback leads, are excluded. The measurable coverage signals are the exported
crawler metrics: `discovery_sightings_total` (walk yield by protocol/family),
`nodeset_admissions_total` (set admissions; evicted or aged identities may be
re-admitted, and the counter resets on process restart), `discovered_total`,
`resolved_total`, `nodeset_class_size`,
`snapshot_nodes`, `rolling_distinct_identities` (a persisted seven-day HyperLogLog
estimate with explicit window/error metrics), and `last_publish_timestamp_seconds` for freshness.
Admission-counter flattening is not a distinct-cardinality or saturation signal.

## Snapshot freshness

Every snapshot manifest records a `generated_at`. The API exposes it via `/api/v1/meta`
and `/healthz`; `/readyz` fails once the loaded snapshot exceeds `--max-snapshot-age`.
The UI shows "Updated N ago" and flags staleness. A dead crawler yields stale data, not
downtime.

The RLPx Status exchange is in scope and is used for fork readiness. Precise sync lag
is not yet exposed: newer `eth` Status versions provide a latest block number, while
older versions provide only a head hash, so a comparable head-distance metric requires
an additional trusted head reference and explicit semantics for partial sync states.
