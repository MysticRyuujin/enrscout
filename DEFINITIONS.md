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

The client-version chart counts the same population as the client charts, for one client
on one layer. Its bars sum to that client's chart count; versions past the first twelve are
summed in one "Other" row. Versions are grouped by release: a leading `v` and any `-` or `+`
build suffix are dropped, so `v2.0.0+bec830cd-hp` counts as `2.0.0`. A value that does not
start with a version number counts as "Unknown". The node list counts a larger population by
default: its client filter matches a substring, and it includes ENR-claimed client names and
identifications older than seven days. Add `identified=recent` to list only the chart
population.

Some clients let the operator insert a free-text name (`--identity`) into the handshake
string, as in `Geth/<name>/v1.17.6-stable/linux-amd64/go1.26`. The name is not stored.
When the string has more than four `/`-separated parts, the version, OS, and runtime are
read from the last three.

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

## Upcoming fork readiness

The fork tracker evaluates each row of a network against that network's next scheduled fork.
Each layer has its own target: the next EL fork time from the go-ethereum chain configuration,
and the next CL fork version change from `internal/netconf/consensus.go`. When both activate at
the same instant they share one name, such as Glamsterdam (Amsterdam and Gloas). When the two
layers' forks fall at different instants they are different upgrades: the tracker follows the next
scheduled one, or else the most recently activated one. A fork stays tracked for 14 days after
activation unless a later fork is already scheduled.

Readiness is taken from the fork schedule that the node itself advertises, not from its version
string:

| State | Execution row | Consensus row |
| --- | --- | --- |
| `ready` | current fork, and the fork ID `Next` equals the fork time | current fork, and the ENR `eth2` next fork version and epoch equal the target |
| `not_ready` | current fork, and `Next` is 0 | current fork, and the ENR next fork epoch is FAR_FUTURE |
| `mismatch` | current fork, and `Next` is some other value | current fork, and the ENR schedules another version or epoch |
| `unknown` | never | no ENR `eth2` entry, or its digest differs from the row's fork digest |
| `stale` | not on the current fork | not on the current fork |

A consensus row reads its schedule from ENR-only columns (`enr_fork_digest`,
`enr_next_fork_version`, `enr_next_fork_epoch`). A Status exchange replaces the row's fork digest
but says nothing about the next fork, so the next-fork claim is used only while its own digest
still matches. Consensus nodes seen only over libp2p have no ENR and read as `unknown`. A
blob-parameter-only transition changes the digest but not the ENR next-fork fields, so it has no
consensus target.

After activation, a row on the new fork is `ready` (upgraded), a row still on the pre-fork hash or
digest is `not_ready` (left behind), and any other row is `stale`. The tracker therefore counts
stale-fork rows of the network, which the current-network views exclude.

The release table is curated by hand: built into `internal/netconf/releases.go`, and replaceable at
runtime with the API's `--client-releases-file`. For each client it lists the first release of
each release line that ships the schedule (`min_versions`). A reported version is labelled:

- `meets`: a release build at or above the floor of its own major.minor line, or at or above the
  highest floor. Later releases therefore need no table change; a backport line needs its own floor.
- `below`: a release build under those floors.
- `dev_build`: a release candidate, unstable, or development build. It counts only by its advertised
  schedule.
- `mixed`: a version bucket whose raw builds disagree.

An entry also records the fork time it was verified against (`fork_time`). If the tracked fork
activates at another time, the entry is `outdated` and its labels are withheld, because releases
checked against the old date may not carry the new one. A node can advertise the fork while its
fingerprint still shows an older version, because the fingerprint can be up to seven days old.

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

## Custody group count (cgc)

- **cgc**: the custody group count a consensus node advertises in its ENR (Fulu /
  PeerDAS, EIP-7594): how many of the 128 custody groups it stores and serves samples
  from. On mainnet parameters, custody groups map one-to-one to the 128 data columns.
  The honest minimum is 4; nodes with attached validators custody more.
- **Supernode**: a node that advertises `cgc >= 128` and therefore custodies every
  data column.

The value is self-declared in the signed record and is not verified by sampling. It is
surfaced as the `cgc` field (with `cgc_known` marking a decodable entry) and the
`cgc_min`/`cgc_max` filters; execution-layer nodes and pre-Fulu consensus clients have
no `cgc` entry and never match those filters. An entry wider than 32 bits is recorded
as undecodable rather than truncated.

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

## Sync state

Every Status exchange records the head the peer reported (`head`, `head_observed_at`):
a block number from `eth/69` and later, or a head slot from consensus Status. `eth/66`-`68`
Status carries only a head hash, so those peers have no head and their sync state is
`unknown`.

At each snapshot the API compares each head observed in the last 10 minutes with the heads
that other peers of the same network and layer reported in the same window. Each head is
projected to the snapshot time at one block or slot per slot time, so all samples compare at
one instant. The reference is the median, which tolerates any minority of peers that report
a false head or lag behind. It needs at least 5 samples. The state describes the node as of
its last head observation: a node that stops answering stays `synced` until that observation
is 10 minutes old, then reads as `unknown`.
Execution block numbers do not advance on a missed slot, so the projection overstates an older
execution head by the number of slots missed since it was observed. Within the 10-minute window
that is under one block at a typical mainnet missed-slot rate, but a devnet that misses most slots
shows it as a small negative `head_lag`.

- `synced`: at most 32 blocks or slots behind the reference (`head_lag`).
- `lagging`: more than 32 behind.
- `unknown`: no head, a head older than 10 minutes, too few samples, more than 32 ahead, or a
  head too large to compare.

The reference is an observed consensus of peer reports, not a trusted chain head. A peer can
report any head, just as it can report any fork ID: authentication binds a report to the node
key, not to chain state. `sync_state` is therefore display-only, and no publish guard or DNS tree
uses it. A current-fork node that is `unknown`
is not therefore unsynced; it only had no recent comparable head.
