# Operations

Deployment and runtime reference for running ENRScout: common crawler tuning flags,
the production checklist, and the fork-upgrade runbook. For measurement methodology
(warm-up, walker experiments, independent vantages, external comparisons) see
[measurement-operations.md](measurement-operations.md).

## Crawler flags (common)

`--advertiser-networks`, `--advertiser-port-base`, `--el-identities-per-network`, `--identity-dir`,
`--ip-stack auto|ipv4|ipv6|dual`, `--workers`, `--nodedb`, `--resolve-rate`,
`--fingerprint` / `--fingerprint-workers` / `--fingerprint-timeout`,
`--geolite-city` / `--geolite-asn`, `--snapshot-interval`, `--node-max-idle`,
`--target-dial-rate` / `--target-dial-burst`, `--verified-node-max-idle`,
`--max-collapse-pct` / `--min-current-nodes` / `--force-publish`, `--s3-endpoint`/`--s3-bucket`
(or `--out` for filesystem), `--log-level`, `--pprof 127.0.0.1:6060`. Isolated
interoperability tests can pair `--devnet-only` with `--devnet-force-unclassified`;
the latter must never be used for a public/global crawl. Run `--help` for the full set.

Each configured network uses `--el-identities-per-network + 2` ports starting at
`--advertiser-port-base`: one discovery UDP plus RLPx TCP port per EL identity, CL
discovery UDP, then CL libp2p TCP plus QUIC UDP. Advertiser keys persist as
`<network>-<layer>.key` and numbered copies in `--identity-dir`. Fingerprint
retries use jittered one-minute-to-six-hour backoff independent of DHT rediscovery.
Successful fingerprints are retained stale-while-revalidate: daily refresh failures
do not erase the last verified client, and only another successful probe replaces it.
Unverified leads age out after 24 hours by default, while verified identities use the
separate seven-day `--verified-node-max-idle` lifetime.
The API's bounded 0-10 `score` is discovery history, not fingerprint confidence: a
successful repeated resolution raises it, while a resolution failure reduces it.
Metrics expose bounded layer, direction, outcome,
disconnect-reason, status, queue, and attempt-count dimensions; use
`enrscout_crawler_fingerprint_nodes` for coverage and
`enrscout_crawler_advertiser_inbound_total` plus
`enrscout_crawler_fingerprint_disconnect_reasons_total` for admission diagnostics.
`--resolve-rate` limits discovery candidates fed to resolution, not packets. Active
EL and CL fingerprint dials additionally share per-target IPv4 `/32` and IPv6 `/64`
token buckets. Rolling seven-day HyperLogLog metrics are persisted in 169 hourly
buckets per crawler and methodology, and expose window bounds, configured error,
distinct yield, and re-observation ratios by protocol/family.

Manifest advancement is tied to the restored generation and `crawler_id`. Filesystem
storage and S3 services that implement conditional `PutObject` use atomic compare-and-swap.
Partial-S3 services may use `--s3-conditional-mode=verified`, which prechecks, writes,
and reads back the exact bytes; this mode is intentionally documented as non-atomic
across hosts and relies on the process-lifetime lock in the persistent identity directory.
Independent vantages must publish separate raw prefixes and be combined by an explicitly
versioned downstream merge.

Filesystem storage assumes its root is
trusted against concurrent local directory replacement; symlink checks are defense in depth.

`--probe-addr` enables a POST-only on-demand probe endpoint that dials the supplied
caller-provided ENR or enode. Normal address policy rejects private and special-use
targets, and `/probe` is not restricted to records already stored by the crawler. It requires a bearer token from `--probe-token-file` by default and must
not be exposed directly to the internet. `--probe-allow-unauthenticated` is an explicit
unsafe exception for an isolated local devnet. With `--devnet-only`,
`POST /probe?network=devnet` can force a TCP-capable unclassified test record into
the devnet matrix.

## Production checklist

- Terminate TLS and enforce request limits at an ingress or CDN.
- Isolate crawler egress from private control planes; keep pprof and `/probe` private.
- Use prefix-scoped object permissions, durable versioned storage, and one writer per
  manifest prefix.
- When increasing the schema version, deploy snapshot readers before the crawler writer.
  Supported older generations are upgraded at read time; routine additive changes do
  not require deleting the manifest or Parquet history.
- Route dual-stack discovery UDP and the passive RLPx TCP port, then alert on readiness,
  snapshot age, publish failures, and count collapses.

See [SECURITY.md](../SECURITY.md) for the complete trust-boundary model.
Long-running warm-up, walker, multi-vantage, longitudinal, and external-comparison
procedures are defined in [measurement-operations.md](measurement-operations.md).

## Fork-upgrade runbook

Before every scheduled execution fork, upgrade go-ethereum so its chain configuration
contains the activation. Before every consensus fork or blob-parameter-only transition,
update and verify `internal/netconf/consensus.go`. Deploy before activation and confirm
the advertiser `eth`/`eth2`/`nfd` values and API `fork_evaluated_at` boundary tests.
If these tables lag, current classification collapses toward lagging peers and the
current-node floor and collapse guard freeze the manifest rather than publishing the bad generation.
Because currency requires the exact current fork ID, a lagging table now reads the *entire*
layer as stale rather than a subset, so deploying late is a hard failure, not a degradation.
Expect a transient dip at every activation even when the deploy is correct: peers advertise
their synced head, so all of them read stale until they re-publish past the boundary. One or
more publishes may quarantine with `collapse_current` and then recover on their own;
`--force-publish` forces a single cycle through if a stale snapshot is worse than the dip.
`TestForkHeadBlockCoversConfiguredBlockForks` deliberately fails when a geth upgrade
adds a configured block-numbered fork beyond `netconf.ForkHeadBlock`; it does not track
the live chain head. Advance that constant with the dependency upgrade. Timestamp
forks use the request evaluation time and need no head guess.

### Fork tracker and client release table

The fork tracker (`/forks`, `GET /api/v1/forks`) follows each network's next scheduled fork on its
own, and keeps an activated fork for 14 days (`netconf.ForkTrackingGrace`). Readiness counts come
from the fork schedule that each node advertises. The client release table only labels versions.

When a fork is scheduled:

1. Add one `internal/netconf/releases.go` entry per client for that network and fork, with no
   release yet. Ship it with the fork schedule update.
2. If the upgrade pairs an EL and a CL fork under a new common name, add the pair to
   `combinedForkNames` in `internal/netconf/readiness.go`.

As each client ships a release that carries the schedule:

1. Confirm the schedule in the tagged source or the release's chain configuration. A release note
   alone is not proof.
2. Set `fork_time` to the activation you checked against, and add the version to `min_versions`.
   A release candidate goes in `prerelease` instead. A backport to an older release line gets its
   own floor in `min_versions`; a later release on a line already listed needs no change.
3. For a fast fix, edit the YAML file named by `--client-releases-file` (format in the README,
   "Client release table"). Comment each entry with where you verified the schedule. The API reloads it on its next snapshot refresh. The file replaces the whole
   built-in table, so keep every entry you still want in it.
4. Fold the change into `releases.go` at the next ENRScout release. Once the built-in table
   matches the file, remove `--client-releases-file` from the API's configuration and restart it,
   then delete the file. The API refuses to start when the flag names a missing or invalid file.
   A file removed while the API runs is only logged; the table already loaded stays in use.

If a fork is rescheduled, update the fork schedule first. Entries verified against the old time then
show as outdated, and their labels stay hidden until each one is checked again and its `fork_time`
is updated. The trend chart restarts too: the API withholds history recorded against the old
time, and the crawler starts a new series on its next point.

The trend chart reads `<prefix>/state/readiness/<network>/<fork>.json`, which the crawler appends to
every 15 minutes. History starts when a crawler that knows the fork first publishes, so deploy the
fork schedule early. The object inherits the single-writer rule of its prefix.
