# Testing ENRScout against a Kurtosis devnet

This is the end-to-end discovery and fingerprinting test. It launches a multi-client
Ethereum devnet with [ethpandaops/ethereum-package] and bootnodoor, seeds ENRScout only
with bootnodoor, and checks that the crawler discovers and identifies the clients that
join the network.

The main procedure below is intentionally independent of client versions, temporary
branches, and open pull requests. Put short-lived image pins, workarounds, and known
interop failures in [Current validation notes](#current-validation-notes), not in the
procedure.

## Ground rule: no prior knowledge of client nodes

The crawler must be seeded with bootnodoor's identities only.

- Do not seed it from nginx `bootstrap_nodes.txt` or `enodes.txt`; those files contain
  client identities and turn the test into direct probing rather than discovery.
- Clients pointing at bootnodoor is normal package wiring.
- Nothing should point at the crawler. Organic inbound connections after discovery are
  part of the behavior under test.
- If a client cannot join discovery or peer, report that separately. Do not add it as a
  crawler seed or static peer merely to increase coverage.

## Scratch layout

Keep devnet state outside the ENRScout repository, for example:

```text
/path/to/enrscout-devnet/
├── network_params.yaml
├── config/
└── docker-compose.yaml
```

- `network_params.yaml` defines the ethereum-package client matrix.
- `config/` is the crawler's generated `--devnet-dir` bundle.
- `docker-compose.yaml` runs MinIO, crawler, and API on the Kurtosis Docker network.

## 1. Define the client matrix

Use the package's normal curated images by default. Add image overrides or extra client
arguments only when testing a particular fix, and record them in the dated notes section.

```yaml
participants:
  - el_type: geth
    cl_type: lighthouse
    count: 1
  - el_type: nethermind
    cl_type: teku
    count: 1
  - el_type: reth
    cl_type: prysm
    count: 1
  - el_type: erigon
    cl_type: caplin
    count: 1
  - el_type: besu
    cl_type: nimbus
    count: 1
  - el_type: nimbus
    cl_type: grandine
    count: 1
  - el_type: ethrex
    cl_type: lodestar
    count: 1

network_params:
  network: kurtosis
  network_id: "3151908"
  seconds_per_slot: 12
  deneb_fork_epoch: 0
  electra_fork_epoch: 0

additional_services:
  - bootnodoor
```

Choose any unused private network ID. Use a local ethereum-package checkout when testing
unmerged package behavior, but do not encode that checkout's branch or commit in this
guide. The generated service commands and the observed runtime behavior are the
authoritative description of what was tested.

## 2. Launch the devnet

```bash
cd /path/to/enrscout-devnet
kurtosis run /path/to/ethereum-package --args-file ./network_params.yaml \
  --enclave enrscout-devnet
```

Success is the `Starlark code successfully run` marker. A pipe through `tee` can hide
Kurtosis's exit status, so either inspect that marker or redirect output to a file.

To replace an earlier run:

```bash
kurtosis enclave rm -f enrscout-devnet
```

## 3. Build the ENRScout devnet bundle

Rebuild `config/` from the live enclave every time. A stale genesis or validator root
causes fork classification failures that resemble discovery bugs.

```bash
WORKDIR=/path/to/enrscout-devnet
CFG="$WORKDIR/config"
NETWORK_ID=3151908

mkdir -p "$CFG"
kurtosis files download enrscout-devnet el_cl_genesis_data /tmp/enrscout-gendata
cp /tmp/enrscout-gendata/{genesis.json,config.yaml,genesis_validators_root.txt} "$CFG/"
printf '%s\n' "$NETWORK_ID" > "$CFG/network_id.txt"

# ENRScout needs the exact beacon genesis time under this key. Some generated
# bundles contain only MIN_GENESIS_TIME.
grep -q '^GENESIS_TIME:' "$CFG/config.yaml" || \
  printf 'GENESIS_TIME: %s\n' \
    "$(grep -oE 'MIN_GENESIS_TIME: [0-9]+' "$CFG/config.yaml" | grep -oE '[0-9]+')" \
    >> "$CFG/config.yaml"

# Seed only bootnodoor's layer-specific identities.
BOOT_HTTP_PORT=$(docker port "$(docker ps -q -f name=bootnodoor)" 8080/tcp \
  | head -1 | sed 's/.*://')
{
  curl -fsS "http://127.0.0.1:$BOOT_HTTP_PORT/cl-enr"; echo
  curl -fsS "http://127.0.0.1:$BOOT_HTTP_PORT/el-enr"; echo
  curl -fsS "http://127.0.0.1:$BOOT_HTTP_PORT/enode"; echo
} > "$CFG/bootnodes.txt"
```

The required files are:

- `genesis.json`
- `network_id.txt`
- `genesis_validators_root.txt`
- `config.yaml`, including `GENESIS_TIME`
- `bootnodes.txt`, containing bootnodoor only

## 4. Run ENRScout on the enclave network

Join the compose services to the enclave's Docker network. The crawler and API both need
the devnet bundle mounted read-only.

The crawler needs these devnet-specific arguments in addition to its storage settings:

```yaml
- --devnet-dir=/devnet-config
- --devnet-only
- --allow-private-ips
- --advertiser-networks=devnet
- --netrestrict=172.16.0.0/12
- --s3-endpoint=minio:9000
- --s3-ssl=false
- --s3-bucket=enrscout
- --s3-create-bucket
- --snapshot-interval=15s
- --probe-addr=:9102
- --probe-allow-unauthenticated
```

`--probe-allow-unauthenticated` is acceptable only on this isolated local devnet. The
API must also receive `--networks=devnet` and `--devnet-dir=/devnet-config`.

```bash
cd /path/to/enrscout-devnet
export KT_NETWORK=kt-enrscout-devnet
docker compose down -v
docker compose up -d
docker logs enrscout-dev-crawler-1 | grep -E 'crawler started|restored'
```

Expect `restored=0` after the volume wipe. Restoring an older snapshot invalidates a
clean-room coverage result.

## 5. Verify discovery and fingerprinting

Watch the identified client sets:

```bash
curl -sS 'http://127.0.0.1:8090/api/v1/stats?network=devnet' \
  | jq '{el: (.by_client_el|keys), cl: (.by_client_cl|keys)}'
```

Do not define success only as a hard-coded count. Compare the result with the participant
matrix and classify each omission:

1. Did the client parse its bootnode and start the intended discovery protocol?
2. Did bootnodoor admit it to the appropriate layer/network table?
3. Did it discover or accept any peers?
4. Did ENRScout observe the node?
5. Did inbound or outbound fingerprinting complete?

For a fully participating matrix, every configured EL and CL implementation should appear.
At steady state,
`enrscout_crawler_nodeset_class_size{class="verified"}` should equal
`enrscout_crawler_nodeset_size`. Inspect individual API rows for `fp_status`,
`fp_direction`, `membership_source`, and `fork_source`; client charts alone do not
show which path succeeded.

A record with no `eth`/`eth2` entry and no TCP port never gains a layer; a
discovery-only devnet seed can produce one. It is kept in the set for retry but deliberately
excluded from snapshots, because publishing it would break the manifest's
`total = execution + consensus` invariant. Expect `enrscout_crawler_nodeset_size` to exceed
the published per-network row count when such a record is present.

## 6. Test outbound fingerprinting directly

The on-demand probe endpoint is the definitive test of ENRScout's outbound path. Obtain a
client's runtime ENR or enode, then submit it from the isolated host:

```bash
curl -sS -X POST 'http://127.0.0.1:9102/probe?network=devnet' \
  --data-binary "$CLIENT_ENR_OR_ENODE" | jq .
```

A successful response includes the client, version, capabilities, network, and
`"registered": true`. After the next snapshot, the node row should report
`fp_direction: "outbound"`. This test bypasses DHT discovery deliberately; it answers
only whether ENRScout can dial, complete the protocol handshake, and classify that target.

## 7. Diagnose discovery separately from fingerprinting

Use evidence from each boundary:

- Client logs/config: bootnode parsing, enabled discovery versions, peer count.
- bootnodoor logs/UI: session establishment, admission/rejection reason, layer table.
- ENRScout crawler logs/metrics: observation, classification, probe direction and failure
  stage.
- ENRScout API: persisted membership and fingerprint evidence.

An empty FINDNODE response from an arbitrary standalone probe does not prove bootnodoor's
table is empty. bootnodoor filters responses by the requester's classified network/fork.
An unclassified or incompatible probe can correctly receive zero nodes while a compatible
devnet client receives the populated table. Test with the devnet's real fork ID and
corroborate it with peer counts and admission logs.

The useful distinction is:

- not admitted by bootnodoor: client bootnode, ENR, fork, or discovery interoperability;
- admitted but not observed by ENRScout: crawler discovery or routing issue;
- observed but not identified: RLPx/libp2p fingerprinting issue;
- identified only inbound or only outbound: direction-specific handshake issue.

## 8. Clean up

```bash
cd /path/to/enrscout-devnet
docker compose down -v
kurtosis enclave rm -f enrscout-devnet
```

## 9. Hard-fork transition testing

The procedure above activates every fork at genesis, so nothing ever transitions
while the daemons run. That hides a whole class of defect: anything that only
executes when a fork activates, and anything whose encoding is only wrong for a
value that a genesis-only network never produces. Schedule post-genesis forks
when you need to exercise those paths.

### Scheduling forks

Fork activation is derived from the CL fork epochs; the EL fork timestamps are
computed from them, so one schedule moves both layers coherently. Set the target
fork to a small non-zero epoch and leave the earlier ones at 0. BPO entries must
land at or after Fulu (the package clamps them up rather than failing).

Kurtosis takes a single `--args-file` and does not merge one file over another,
so copy the whole steady-state file and replace only its `network_params` block.
The fork file must still carry `participants` and
`additional_services: [bootnodoor]`, or the run comes up with default clients
and no bootnodoor to seed ENRScout from.

```bash
cp network_params.yaml network_params.forks.yaml
# then edit the network_params block of the copy:
```

```yaml
network_params:
  network: kurtosis
  network_id: "3151908"
  seconds_per_slot: 12
  deneb_fork_epoch: 0      # chain starts on Deneb
  electra_fork_epoch: 1
  fulu_fork_epoch: 3
  bpo_1_epoch: 5
  bpo_1_max_blobs: 12
```

**Epoch arithmetic.** With the mainnet preset an epoch is 32 slots, so at 12s
slots one epoch is 384s (6.4 min): the schedule above transitions at roughly
T+6m, T+19m and T+32m, and the whole run wants ~50 minutes. The `minimal` preset
uses 8 slots per epoch and is far faster, but it selects a different set of
client images and constrains `seconds_per_slot` (Nimbus rejects anything other
than 6s on minimal, and anything below 12s on mainnet), so prefer the mainnet
preset unless you specifically need short epochs.

**Include a BPO.** A BPO changes the fork digest through the blob schedule
rather than a fork version, so it exercises blob-parameter code that ordinary
forks do not reach. Both bootnodoor and ENRScout's devnet config parse
`BLOB_SCHEDULE`.

**Client support.** Fulu requires EL Osaka support. When a client cannot run the
schedule, Kurtosis rolls back the whole batch; read the failing service's logs,
drop that participant and redeploy rather than weakening the schedule. Dropping
clients is the right trade when the daemons are what is under test; record which
ones and why.

### Computing the transition times

Derive the wall-clock of each activation before you start, so log lines can be
correlated instead of guessed:

```bash
CFG=/path/to/enrscout-devnet/config/config.yaml
G=$(grep -oE '^GENESIS_TIME: [0-9]+' "$CFG" | grep -oE '[0-9]+')
SPS=$(grep -oE '^SECONDS_PER_SLOT: [0-9]+' "$CFG" | grep -oE '[0-9]+')
EPOCH=$((32 * SPS))   # mainnet preset; 8 * SPS for the minimal preset

# GNU date wants -d @<epoch>; BSD/macOS date wants -r <epoch>.
epoch_utc() { date -u -d "@$1" +%H:%M:%S 2>/dev/null || date -u -r "$1" +%H:%M:%S; }

for e in 1 3 5; do
  printf 'epoch %s -> %s UTC\n' "$e" "$(epoch_utc $((G + e*EPOCH)))"
done
```

The devnet bundle also records the schedule itself (`*_FORK_EPOCH` and
`BLOB_SCHEDULE` in `config.yaml`, EL fork timestamps in `genesis.json`), so
verify your arithmetic against it rather than against the args file.

### What to watch on each side

Sample every 30s from before the first fork until several minutes after the
last, and keep the series timestamped.

bootnodoor, per transition:

- **ENR sequence discipline.** The sequence must stay flat between transitions
  and step exactly once per fork. A step every refresh tick means change
  detection is broken; no step at all means the refresh is not firing. Sample
  `LocalENRSeq` from `/?ajax=1` and count the
  `fork transition: re-published ENR fork fields` log lines: they must equal the
  number of scheduled forks.
- **The published records track the era.** Decode `/enr`, `/el-enr` and
  `/cl-enr` at each stage: the `eth` fork hash changes at EL transitions, `Next`
  points at the following scheduled fork and becomes 0 after the last, and the
  `eth2` digest changes at each CL transition. Compare byte-for-byte against a
  client's live ENR: this is how encoding bugs surface, since a self-consistent
  encoder and decoder agree with themselves.
- **Refresh lag.** bootnodoor detects activation on an unaligned one-minute
  ticker, so expect up to ~70s of stale advertisement after each activation.
- **Discovery state survives the re-publish.** Sessions, table sizes and packet
  counters must not reset. The sharp check is bootnodoor's own logs: a node
  should be added to a table once and never re-added after a transition.
- **Grace behaviour.** After a CL transition the previous digest stays accepted
  for the grace period; the accepted-old and accepted-historical counters should
  move.

ENRScout, per transition:

- **Coverage must not drop.** `by_client_el`/`by_client_cl` should hold across
  every boundary, and `nodeset_class_size{class="verified"}` should stay equal
  to `nodeset_size`; nodes get reclassified, not dropped.
- **Stale counts spike and recover, on both layers.** A transition legitimately drives
  the headline `consensus` or `execution` count down for about one snapshot interval
  while peers re-publish; recovery inside ~60s is normal. A permanent collapse is a bug
  and would trip the current-node floor. `execution_stale` now spikes at EL boundaries
  exactly as `consensus_stale` does at CL ones: currency requires the exact current fork
  ID, so a pre-fork EL record is stale even carrying a correct `Next`. At a coordinated
  EL+CL fork both halves dip together, which can trip `collapse_current` and quarantine a
  publish until peers re-publish; `--force-publish` is the one-shot override.
- **`fork=current` and `fork=stale` must complement.** Their counts should sum to
  the total at every sample, including mid-transition, so the audit view stays
  complete while the headline view dips.
- **Advertiser records track the fork.** The crawler arms its refresh at the next
  scheduled activation rather than polling, so its advertiser ENR sequence should
  step within one sample of each boundary.

### Interpreting the results

- Give any alert on the headline consensus count at least two minutes of
  hysteresis; the one-interval dip at a CL fork is expected behaviour.
- Do not read admission rejection counters as fork health. On a dual-layer
  network most records an EL lookup sees are consensus records and vice versa;
  that is `rejected_layer`, not `rejected_fork`.
- Clients that never refresh their own ENR at a fork exist. ENRScout still
  classifies them correctly because it has RLPx Status evidence; anything
  ENR-only will see them as pre-fork indefinitely. Under exact-hash currency those
  rows now read `execution_stale` for as long as the ENR is the only evidence, so a
  residual post-transition stale count is expected rather than a bug.

## Current validation notes

This section is intentionally dated and disposable. Update or remove it as fixes merge;
the procedure above should remain valid.

### State observed on 2026-07-25

The local ethereum-package integration used for this run:

- passes `/cl-enr` to CL clients;
- selects the EL bootnode form per client (see below);
- emits Caplin's bootnode argument and enables its local discovery;
- uses `ethpandaops/bootnodoor:latest`.

No ethereum-package branch or commit is named because that integration was local-only.
Use the package's current curated images first and add overrides only when reproducing
one of the findings below.

Organic discovery reached four of seven EL types and all seven CL types. The three
remaining EL types were each identified outbound through `/probe`, giving seven of seven
EL and seven of seven CL overall, fifteen nodes, with
`nodeset_class_size{class="verified"}` equal to `nodeset_size`.

### EL bootnode form is per client, not global

Feeding `/el-enr` to every EL client fails the whole batch: reth and besu reject a
non-enode `--bootnodes` value outright (`Failed to parse url: no host specified`,
`Invalid enode URL syntax`), and Kurtosis rolls back every service in the batch, so no
client starts. The local checkout now sends the EL ENR only to clients that accept one
(geth, erigon) and bootnodoor's `/enode` to the rest.

### Nethermind

Nethermind 1.39.1 aborts at startup when `--Discovery.Bootnodes` contains an ENR:
`System.ArgumentException: PublicKey should be 64 bytes long`. The previously documented
`--Discovery.DiscoveryVersion=V5` plus ENR workaround therefore no longer applies;
forcing V5 while supplying an enode leaves it at zero peers instead. A master build
(v1.40.0) is expected to restore ENR bootnode parsing; until then Nethermind is
enode-only here and does not join the EL discv5 table.

`/probe?network=devnet` dialed it directly, completed Hello and Status, and returned
`client: Nethermind` with `registered: true`; the persisted row recorded
`fp_direction: outbound` and `membership_source: status`.

### Nimbus-eth1

`executionClient` reads `elBootstrapNodes`, not `bootstrapNodes`, for EL discovery, so
the package must pass `--el-bootstrap-node`. With `--bootstrap-node` the ENR lands in the
wrong list and the node logs `Skipping discovery bootstrap, no bootnodes provided`.

With that corrected it still rejects bootnodoor's discovery-only `enode` because the TCP
port is zero (`enode: incorrect TCP port`). Separately, the tested image populates its
discv5 `eth` entry by RLP-encoding `[ForkId]` and storing those bytes as an ENR byte
string, adding an extra RLP layer, so bootnodoor cannot decode the fork ID and does not
admit it to the EL discv5 table. The upstream fix is to store `eth` as an ENR RLP-list
field with `[ForkId]` as its raw list payload, not as `seq[byte]`.

### Erigon and bootnodoor ENR refresh

Erigon accepts an `enr:` bootnode and comes up discv5-only (`v4=false v5=true`), but held
no peers against `bootnodoor:latest` and stayed outside the EL table. Its startup ENR may
initially lack usable fork data and then advance to a higher sequence;
[bootnodoor PR #41] installs the refreshed distance-zero ENR and re-runs normal
classification. Confirm that image carries the fix before treating this as a new failure.

### Hard-fork transition run on 2026-07-27 (superseded)

Recorded before EL currency required the exact current fork ID. The
`execution_stale` observation below will not reproduce: it now spikes and recovers at
EL boundaries the way `consensus` does at CL ones. Re-run before trusting these numbers.


Deneb at genesis, Electra at epoch 1, Fulu at epoch 3, BPO1 (12 blobs) at epoch
5; mainnet preset, 12s slots, six EL/CL pairs with nimbus-eth1 dropped for the
unrelated tcp=0 enode gap. Every remaining client survived all three
transitions, so Fulu cost no participants. Activation lag on bootnodoor's
one-minute refresh ticker was 68s, 24s and 15s.

ENRScout passed cleanly: coverage held in all 70 samples, `verified` stayed
equal to `nodeset_size`, `execution_stale` never moved, the headline `consensus`
count dipped for a single 30s sample at each CL fork and recovered in 51-67s,
`fork=current` plus `fork=stale` summed to the total throughout, and the
advertiser ENRs stepped once per fork within one sample.

The run found four bootnodoor defects that a genesis-only devnet cannot reach,
all since fixed: `eth2.next_fork_epoch` encoded big-endian where SSZ requires
little-endian (invisible while the epoch is `FAR_FUTURE_EPOCH`, which is
byte-palindromic); an unscheduled fork's version published as
`next_fork_version` after the last fork; records of nodes already in a table
never refreshed, so pre-fork ENRs were served indefinitely; and admission
counters that counted cross-layer records as fork rejections.

It also retired a long-standing misdiagnosis. The `invalid_fork_id` rejections
seen in earlier runs were not fork incompatibility: with rejections split by
cause, every one was `rejected_layer`, a record carrying no `eth`/`eth2` at all.
bootnodoor's fork arithmetic matched what every upgraded client advertised.

### Full-series validation run on 2026-07-28

Both series (steady-state and the Electra/Fulu/BPO1 fork schedule) ran against
a bootnodoor image built from its `develop` working tree, full seven-pair
matrix, no drops. This supersedes several earlier notes:

- **Organic EL coverage is 5/7, not 4/7.** erigon now joins the EL table on
  its own (bootnodoor PR #41's refresh is on `develop`). Nethermind and
  nimbus-eth1 were identified outbound via `/probe` as before.
- **Nethermind's ENR-bootnode abort no longer reproduces.** With the per-client
  bootnode form it parses the enode (`isEnode:true`) and sits at
  `Peers: 0` without erroring; same outcome, different symptom.
- **Fulu costs no participants.** All seven pairs, including nimbus-eth1,
  survive the fork schedule; the 07-27 nimbus-eth1 drop was unnecessary.
- **"Coverage held in all samples" needs qualifying**: `by_client_cl` is
  derived from the fork-filtered headline view, so it legitimately empties or
  shrinks for the one dipped sample at each CL-affecting transition. Only the
  audit invariant (`fork=current` + `fork=stale` = total, `verified` =
  `nodeset_size`) holds at every sample.
- **`accepted_old` can stay at 0 for a transition** when every peer
  re-publishes inside bootnodoor's one-minute ticker window; the
  historical-digest path absorbs the new digest during the lag.
- Refresh lags this run: 68s / 14s / 10s.

The run surfaced a pre-existing bootnodoor defect, since fixed with tests:
**discv4 ENRRequest packets collide on their pending-request key** (the packet
carries only a 1s-granularity expiration and signatures are deterministic, so
same-second requests to different peers are byte-identical and share a hash).
`RequestENR` could install another node's ENR; live effect was most EL table
rows holding the crawler advertiser's record and serving multi-fork-stale
ENRs. The fix keys pending requests by hash + destination node ID, binds
ENRRESPONSE records to the sender's identity before installing, installs
sequence-monotonically, and allows one in-flight FINDNODE per peer (NEIGHBORS
carries no reply token). The same run's fix gated bootnodoor's CL fork-digest
counters on `eth2` presence, mirroring the EL admission-stats fix from 07-27.

**Retraction: the "Prysm advertises `next_fork_epoch` big-endian" observation
from the 07-27 run was an observation error.** A dedicated devnet check
(Prysm v7.1.7 vs Lighthouse v8.2.1, Electra scheduled at epoch 1 so the value
is non-palindromic) showed both clients' `eth2` entries byte-identical:
`edf8b306|60000038|0100000000000000`, little-endian as SSZ requires. Prysm's
encode path (`beacon-chain/p2p/fork.go` → generated SSZ → fastssz
little-endian `MarshalUint`) is correct at every level; no upstream issue is
warranted. Note when decoding Prysm ENRs by hand: its `cgc` entry genuinely is
an RLP big-endian integer, which is an easy source of this confusion.

### Fork readiness run on 2026-09-24 (earlier run)

Seven pairs (erigon paired with lighthouse because Caplin fails to start), Electra at genesis,
Fulu (with EL Osaka) at epoch 5, every BPO at FAR_FUTURE_EPOCH, upstream ethereum-package and the
branch images. `/api/v1/forks?network=devnet` tracked Fusaka from the first snapshot:

- **Scheduled.** EL 8/8 and CL 7/7 `ready` for the whole pre-fork window. EL heads read `synced`
  (7 of 8; the eighth is an ENR-only row with no Status head).
- **Activation.** The first sample after 15:59:08 read `activated` with every row `not_ready` (left
  behind). Thirty seconds later EL was 4/8 and CL 5/7 upgraded; CL was 7/7 within a minute and the
  last EL row flipped after five minutes. `upgraded` equalled `/stats` `execution`/`consensus` and
  `left behind` equalled `execution_stale`/`consensus_stale` at every sample.
- **History and restore.** The history object gained points on its 15-minute cadence across a
  crawler restart (restored 15 rows; the stored point was kept).
- **Sparse chains show the lag projection limit.** The devnet produced about one block per ten
  slots, so older EL heads projected slightly ahead of the median (`head_lag` of -4). See
  DEFINITIONS.md "Sync state".

### Earlier discovery findings

No ENRScout code defect was found in these omissions. Every EL client that could not join
discovery organically was still identified outbound by `/probe`, with both fingerprint
directions represented in the persisted rows (`inbound` for geth, besu, reth and ethrex;
`outbound` for nethermind, erigon and nimbus). Keep future client interoperability
workarounds in this dated section rather than modifying the generic procedure.

### Review validation on 2026-09-24

The full seven-pair matrix runs with bootnodoor and the ENRScout services from the local ethereum-package integration.
Caplin uses `--sentinel.quic.port=4002` because its default QUIC port conflicts with discovery UDP.
The package uses the local MinIO image built from pinned source; its original image cannot be pulled.
The crawler and API use revision `f122a35`; the web image includes the network initialization fix from `80c2aa4`.

The mainnet preset uses 12-second slots and starts on Electra.
Genesis is at 18:22:09 UTC, Fusaka at epoch 2 (18:34:57), and BPO1 at epoch 4 (18:47:45).
BPO1 has a target of 10 blobs and a maximum of 15 blobs.
Later forks remain at `FAR_FUTURE_EPOCH`.

- **Discovery.** The crawler starts with an empty snapshot store and seeds only bootnodoor.
  It discovers all seven EL clients and all seven CL clients before any direct probe.
  The published set contains 15 identities, including the EL bootnode.
- **Fusaka.** Before activation, EL readiness is 8/8 and CL readiness is 6/7.
  The tracker selects BPO1 at Fusaka activation.
  One publication hits `collapse_current`; the next publication succeeds without an override.
  All seven CL records become current by 18:35:48.
  Nimbus EL can return to stale because its ENR retains the earlier fork hash while Status reports the current hash.
- **BPO1.** At 18:47:50 the tracker reports `activated`, and every identity still carries an earlier fork.
  At 18:48:20, five EL identities and six CL identities report the new fork.
  The ready count matches the current count; `not_ready` plus `stale` matches the stale count in each layer.
  All seven CL records become current by 18:48:50; seven EL records become current by 18:50:51.
  Besu temporarily retains an ENR with the Electra fork hash.
  The final sample at 18:53:05 has all eight EL records and all seven CL records current.
- **Caplin evidence.** The beacon API returns `eth2` bytes `1dbb761a700000380000000000000004` after Fusaka.
  The epoch bytes encode 4 in big-endian order.
  SSZ decodes them as `288230376151711744`, so ENRScout correctly reports `mismatch` for the scheduled BPO1.
  The same issue affects its advertised epoch 2 before Fusaka.
- **Restart and history.** The final crawler image restores all 15 devnet rows.
  History retains the 18:19:17 point and adds the next point at 18:34:26, despite a restart at 18:31:41.
  This verifies the fix for a restart that occurs before the next history point is due.
  The crawler stores the first BPO1 point at 18:49:41.
  The API reads it after a restart; its existing cache can delay new history for up to 15 minutes.
- **Status heads.** Separate outbound probes target only previously discovered clients.
  Ten of fourteen probes complete.
  Nethermind returns an RLPx EOF; Caplin, Grandine, and Lighthouse do not complete identify.
  Their existing inbound fingerprints remain available.
  Persisted heads produce `synced` results for seven EL clients and six CL clients at the checked snapshot.
  Grandine has no recorded head and remains `unknown`.
  CL sync labels return to `unknown` when fewer than five observations remain within the ten-minute window.

The local Compose deployment also runs the final images against mainnet, Hoodi, and Sepolia.
The Sepolia check verifies readiness filters, grouped counts, release reloads, history, and snapshot restoration.
At 18:48:38 UTC, the Sepolia audit view contains 358 identities, and all 35 API assertions pass.
The replacement MinIO image reads a populated volume with root ownership, matching the original image.

The devnet produces 61 successful API samples from 18:20:18 through 18:53:05, with 2,251 count assertions.
Every sample retains all 15 identities and all seven client types in each layer.
The final metrics report 15 verified identities and 15 total identities.

The checks pass after the fixes:

- Go tests with the race detector, compilation, vet, formatting, staticcheck, and the reachable vulnerability scan.
- Compatibility tests for earlier snapshot schemas and tests that compare SQL readiness with the Go rule.
- Compose validation, native ARM64 image builds, the web build, and the dependency audit.
- Browser checks: 64/64 across the public networks, 31/31 on Sepolia, and 31/31 on the devnet.
- Targeted browser checks for network links, fork filters, history markers, and mobile layout.
- Activated BPO1 labels and counts match the API, and the stored history appears after the API restart.
- Release reload checks for unchanged file size and timestamp, invalid YAML, and recovery without a restart.

The devnet browser run finds initial requests for unsupported mainnet data before metadata selects the devnet.
The web fix waits for network metadata before it mounts pages; the repeated browser run has no failed requests.
The API monitor misses three samples when Kurtosis assigns a new port during the image update.
It resumes before Fusaka activation.
Docker assigns another port during the later API restart, which causes three more connection failures in the monitor.
The final check uses the stable web proxy port and passes.

Local logs, API samples, image IDs, configurations, and screenshots are retained under `/tmp/enrscout-validation`.
AMD64 image builds remain a CI check; this local run uses ARM64.

[ethpandaops/ethereum-package]: https://github.com/ethpandaops/ethereum-package
[bootnodoor PR #41]: https://github.com/ethpandaops/bootnodoor/pull/41
