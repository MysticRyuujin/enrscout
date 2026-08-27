# Nethermind underrepresentation investigation

## Summary

ENRScout reports Nethermind at about 4% of identified mainnet execution nodes.
This share is lower than estimates from client teams, operators, and other crawlers.

The investigation found a crawler selection bias against nodes with missing or
unusable ENRs. Nethermind releases through 1.39.3 commonly enter this path.
ENRScout can classify these nodes through authenticated RLPx Status. However,
most candidates do not receive the retry treatment used for valid-ENR nodes.

Do not wait for all Nethermind operators to upgrade. Retain unresolved candidates
in an unpublished quarantine and retry Status with the existing bounded schedule.
Promote a candidate only after Status identifies a configured network.

## Reported node

The investigation started with this mainnet node:

- Node ID: `12b2a420b3202938035de9b14404e4ec9486fe1bf7751da91dde6fb4c6066200`
- Operator host: `eth02`
- Client: Nethermind 1.39.3
- Public endpoint: `47.186.76.213:30302`
- ENRScout record: no ENR, sequence zero, discv4 only

The node was healthy, synchronized, listening, and connected to peers. Its UDP
socket received normal discv4 traffic. The host firewall did not block discovery.

ENRScout last discovered the node on 2026-08-23. It completed a later inbound
RLPx fingerprint and Status exchange on 2026-08-25. The row therefore had newer
`last_resolved`, `fingerprint_at`, and `membership_verified_at` values.

The page's `last_seen` field records discovery activity. It does not record the
latest authenticated contact. This distinction made the node appear less active
than it was.

## Nethermind behavior

Nethermind 1.39.3 has two relevant discovery defects.

First, its discv4 ENR response uses a hash derived from the request payload.
EIP-868 requires the hash of the complete ENR request packet. Geth's discovery
implementation rejects the response because the request token does not match.

Second, the local node record contains the IP address, ports, and public key.
It does not contain the `eth` fork entry. The source has an explicit fork ID TODO.

Nethermind `master` already fixes both behaviors:

- [`9a79c716`](https://github.com/NethermindEth/nethermind/commit/9a79c71648966540c4eb96aa7ec9ea1162833cbb) uses the request packet hash.
- [`6d6e14e5`](https://github.com/NethermindEth/nethermind/commit/6d6e14e5e77624485eabf91afbd2a7482ee2f994) publishes and refreshes the fork ID and ENR sequence.

The focused Nethermind `master` tests for both behaviors pass. A future release
with these commits will improve discovery. It will not correct the existing
measurement bias until operators deploy it.

## Existing ENRScout fallback

ENRScout already has a safe fallback for execution nodes without usable ENRs.

1. The RLPx handshake authenticates control of the node key.
2. Hello supplies the self-reported client name and version.
3. ETH Status supplies the network ID, genesis hash, and live fork ID.
4. ENRScout matches the network ID and genesis against configured networks.
5. Current views also require the exact current fork hash and accepted `next` value.

The fallback works for inbound and outbound connections. It does not trust a
client name, IP address, or network ID alone. See
[`resolverpool.go`](../cmd/crawler/resolverpool.go),
[`fingerprint.go`](../internal/enrich/fingerprint.go), and
[`nodeset.go`](../internal/nodeset/nodeset.go).

Verified nodes use `last_resolved` for idle pruning. The default verified-node
retention is seven days. Client charts use fingerprints from the last seven days.

## Production evidence

The following counters came from the production crawler on 2026-08-26. The
deployed source revision was `647a75557086a8ba3950eea9f5c3a31773f965f7`.

### Current population

- Current mainnet execution fingerprints: 10,745
- Current Nethermind fingerprints: about 400, or 3.7%
- Nethermind rows with no ENR and sequence zero: 182
- Nethermind rows verified through Status: 388
- Nethermind rows that remained ENR-claimed: 12
- Nethermind fingerprints from inbound connections: 247
- Nethermind fingerprints from outbound connections: 153

The fallback supplies almost all observed Nethermind membership evidence. Almost
half of the observed Nethermind nodes have no ENR.

### Legacy candidate outcomes

- Queued legacy execution probes: 303,872
- Successful outbound classifications across all networks: 1,211
- Outbound success rate: 0.4%
- Mainnet outbound classifications: 939
- Mainnet inbound legacy classifications: 2,037
- Deferrals caused by the six-hour attempt window: 879,932

All queued outbound attempts have a recorded outcome. The failures were:

| Failure | Count |
| --- | ---: |
| Dial | 150,209 |
| RLPx handshake | 113,063 |
| Peer disconnect | 18,368 |
| Status network classification | 13,338 |
| Status exchange | 4,426 |
| Status decode | 1,797 |
| Status protocol | 852 |
| Status read | 549 |
| Hello read | 50 |
| Endpoint | 9 |

About 93% of attempts fail before ENRScout receives a usable Status message.
Many endpoints are stale, unreachable, at peer capacity, or protected by recent
connection limits. These failures are normal for public peer crawling.

## Root cause

The crawler treats valid-ENR and legacy candidates differently after a failed
fingerprint attempt.

A valid-ENR node enters the node set before fingerprinting. A failed connection
keeps the node available for retries after 1 minute, 5 minutes, 15 minutes,
1 hour, 3 hours, and 6 hours.

A sequence-zero legacy node does not enter the node set before Status succeeds.
It remains in a pending map for 30 minutes. A separate six-hour gate blocks the
next outbound attempt. A failed first attempt therefore loses the candidate's
normal retry state for most of the gate period.

This difference favors clients with working ENRs. Nethermind dominates the
missing-ENR group, so the difference depresses its measured share.

There is also an inbound admission defect. The eligibility rule requires a
second authenticated sighting for an unsigned inbound-only node. The first
sighting creates the node with score one. Later Status exchanges update its
fingerprint but do not increment the evidence score. A node without later DHT
provenance can remain unpublished after many authenticated connections.

## Recommended course of action

### 1. Retain unresolved execution candidates

After ENR resolution fails, admit a TCP-capable candidate to the bounded node
set with these properties:

- Layer: execution
- Network: empty
- Evidence: discovery candidate only
- Snapshot eligibility: false
- Capacity class: unclassified

Do not expose these candidates through per-network snapshots or the public API.
Prune them with the existing unverified idle policy. Evict them before verified
or classified nodes when capacity is constrained.

### 2. Use the normal fingerprint retry schedule

Run authenticated RLPx Status probes through the existing retry state. Preserve
the global handshake limit and per-target dial budget. Do not reset a retry only
because discovery reports the same endpoint again.

This gives valid-ENR and missing-ENR candidates equivalent connection treatment.
It does not increase the maximum concurrent load on remote nodes.

### 3. Keep classification strict

Promote a candidate only after a complete Status exchange identifies a configured
network by both network ID and genesis hash. Record its live fork ID separately.

Keep these publication rules:

- Unknown network: never publish
- Known network with unknown or invalid fork: audit only
- Known network with stale fork: stale audit view only
- Exact current fork: current node views and client charts
- Missing or invalid ENR: never publish through EIP-1459 DNS

These rules prevent foreign chains and malformed candidates from increasing
headline mainnet counts.

### 4. Correct repeated inbound confirmation

Count each successful authenticated Status exchange as an evidence sighting for
an unsigned node. Make the second matching sighting satisfy the existing admission
policy. Require the same node key and network classification for both sightings.

### 5. Preserve diagnostic Hello results

When Hello succeeds but Status fails, retain the bounded client family on the
unpublished candidate. Add metrics for candidate client, failure stage, retry
state, and promotion outcome. Keep the metric client label canonical and bounded.

This data will show whether failed candidates are disproportionately Nethermind.
It must not affect network counts before Status succeeds.

### 6. Clarify liveness in the UI

Rename `Last seen` to `Last discovered`. Add `Last contacted` as the newest of
`last_resolved`, `membership_verified_at`, and `fingerprint_at`. This change does
not alter admission or counts.

## Validation and rollout

Add regression tests for these cases:

- A sequence-zero discovery candidate survives its first failed fingerprint.
- The candidate follows the bounded retry schedule.
- Repeated discovery observations do not bypass retry backoff.
- A matching Status promotes the candidate to mainnet.
- A foreign network Status never promotes the candidate to mainnet.
- A stale fork remains outside current views.
- Two authenticated inbound sightings publish an unsigned identity.
- One inbound sighting remains unpublished.
- Capacity eviction removes unclassified candidates before verified nodes.
- Missing-ENR nodes never enter DNS output.

Run the existing Nethermind 1.39.3 integration test in both directions. Add a
case that reproduces the broken ENR response and confirms Status promotion.

Deploy the change to staging first. Observe it for at least 48 hours. Compare:

- Quarantined candidate count
- Candidate client distribution
- Promotion rate by client and direction
- Dial and handshake rates
- Mainnet current-node growth
- Foreign and stale promotion rejections
- Node-set capacity classes

Deploy to production after the staging rates remain bounded. Evaluate the
Nethermind share after 48 hours and again after seven days. Keep the methodology
version distinct so the measurement change is visible in longitudinal data.

Upgrading Nethermind remains useful, but it is not the corrective action for
the current dataset. The crawler must remove the asymmetric retry policy first.
