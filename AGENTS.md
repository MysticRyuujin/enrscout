# AGENTS.md

Guidance for coding agents working in this repository.

ENRScout crawls the Ethereum devp2p discovery network (discv5 + discv4, IPv4 + IPv6),
classifies and enriches nodes, and serves a map-centric explorer. See README.md for
the product overview and HTTP API.

## Commands

```bash
make build                 # crawler + api binaries (dnspublisher: go build ./cmd/dnspublisher)
make test                  # all Go tests (incl. DuckDB integration in internal/query)
go test ./internal/query/ -run TestEngineEndToEnd   # a single test
make lint                  # go vet + gofmt -l
docker compose -f deploy/local/docker-compose.yaml up --build   # full local stack
node web/e2e/browse.mjs    # Playwright browse test (needs: cd web && npm ci && npx playwright install chromium)
```

The local stack serves web on :8081, API on :8080, MinIO on :9000/:9001, and
publishes the crawler advertiser endpoints on :30303-:30311.

## Architecture (the parts that span files)

- **One crawler discovers everything.** The discv4/discv5 DHTs are a single global
  keyspace, so `cmd/crawler` runs all protocols on one process and finds every
  network's nodes. Per-network EL and CL advertiser identities provide compatible
  fork records but feed one resolver, nodeset, fingerprint queue, and publisher. Do
  not split them into independent per-network crawler processes.
- **Walkers ≠ advertisers.** Only `--walker-el-identities` EL identities per network
  (default 1) plus each CL identity (discv5 only) consume random-walk iterators;
  the rest are advertisers that stay DHT-findable without walking (discovery tables
  self-maintain and answer FINDNODE without `RandomNodes` consumption). A global
  `--resolve-rate` token bucket (default 100/s) backpressures the walk readers.
  Advertisers deliberately accept inbound RLPx/libp2p connections and complete
  Status - that inbound traffic is the primary EL identification path.
- **Capacity is class-prioritized, not FCFS.** At `--max-nodes` the set evicts from
  the lowest class present (fallback → unclassified → classified) before rejecting a
  higher-class candidate; verified/pinned nodes are never evicted
  (`nodeset.capacityClass`, `nodeset_class_size` metric).
- **Evidence tiers are recorded, not implied.** `membership_source` (`enr|status`)
  and `fp_direction` (`inbound|outbound`) are Row columns; client charts count only
  fingerprints fresher than 7 days (`chartMaxFingerprintAge`); fork currency is
  query-derived (`netconf.CLForkStateAt` / `IsCurrentCLForkAt`), surfaced via `consensus_stale`,
  `fork=stale`, and `fork_compatible`. Headline EL and CL views are current-only;
  `fork=all` preserves audit access to stale rows.
- **Currency is exact, and symmetric across layers.** EL requires the current fork-id hash
  (`netconf.IsCanonicalForkCompatibleAt`) and CL the current digest (`IsCurrentCLForkAt`).
  EIP-2124's acceptance of an earlier era carrying its own canonical `Next` is a
  connection-admission rule for peers that may still be syncing - the advertised id tracks a
  node's synced head, not its capability - so it is deliberately not honoured for a
  current-fork view. Membership stays separate and hash-only (`Matches`/`gatherHashes`): a
  resyncing mainnet node is still mainnet, just not current. Expect `execution_stale` and
  `consensus_stale` to spike for about one snapshot interval at each transition.
- **Advertiser ports are deterministic.** Starting at `--advertiser-port-base`, each
  network consumes three ports: EL discovery UDP/RLPx TCP, CL discovery UDP, and CL
  libp2p TCP/QUIC UDP. Identities are persistent under `--identity-dir`.
- **Classification, not filtering, separates networks.** Each node is bucketed by
  `internal/netconf`: EL by EIP-2124 fork id (`Classify`), CL by fork digest
  (`ClassifyCL`, `consensus.go`). Unclassified observations may remain temporarily in
  the bounded crawler set for retry, but `nodeset.SnapshotNetworks` excludes them from
  atomically published per-network snapshots.
- **Serving never touches the crawler.** Crawler → Parquet in S3-compatible storage
  (`internal/store`) → `cmd/api` reads snapshots with embedded DuckDB
  (`internal/query`) → static `web/` SPA. A dead crawler = stale data, not downtime.
- **Snapshots are durable and committed as complete generations.** The crawler writes immutable
  per-network generation keys (`snapshots/<net>/<ts>.parquet`) then advances a single
  `snapshots/manifest.json` pointer only after every network's parquet is stored
  (`internal/snapshot`). It restores the nodeset from the manifest on startup, refuses
  to advance the manifest when the total or current-fork count collapses past
  `--max-collapse-pct` or a network falls below `--min-current-nodes`, commits
  against the restored manifest/CrawlerID, and
  prunes old generations (`--keep-generations`). The api reads the manifest, not a
  mutable `latest.parquet`. Both build keys via `snapshot.Layout` so they never diverge.
  Native S3/filesystem conditional writes are atomic. `--s3-conditional-mode=verified`
  exists for partial-S3 backends, but is only precheck/write/read-back plus a same-host
  identity-directory lock; never aim multiple hosts at a prefix in that mode.
- **`cmd/dnspublisher`** builds EIP-1459 trees from a validated snapshot using
  go-ethereum's LGPL `p2p/dnsdisc` (`MakeTree`/`Sign`/`ToTXT`) - output is byte-compatible
  with `devp2p dns` / discv4-crawl. It never gets crawler or signing creds inline; the key
  comes from `--key-file` (hex, or a `devp2p`-format Web3 keystore JSON so the exact
  discv4-crawl key can be reused — empty passphrase by default, `--key-passphrase-file`
  otherwise; both files must be mode 0600 or stricter — which keeps the published
  `enrtree://` URLs identical across the migration).
  It publishes only _reachable_ nodes, and only records that are themselves well-formed: `enrWellFormed`
  drops a row whose ENR carries a present-but-undecodable `eth`, address or port entry. Both cases are
  real: a node was observed publishing `eth` as the fork-id list wrapped in an extra RLP string, and a
  port above `uint16` is unrepresentable in the typed entry. go-ethereum signs and parses such a record
  and reports the problem only when that one entry is loaded, so a node can stay dialable through
  another transport and reach the tree. The crawler stays deliberately tolerant of these nodes -
  classification is sticky in `nodeset.Observe`, so the ENR blob is refreshed while
  `Layer`/`Network`/`ForkHash` are not - because dropping them would cost coverage and fingerprints. A
  published record is held to the stricter bar, because a peer that decodes entries strictly rejects the
  whole record (ethrex does) and the tree slot is wasted. Measured against the live EF trees, this
  excludes 0 of 3683 records. The record must also self-describe fork currency (`enrForkCurrentAt`):
  the row currency rule evaluated on the ENR's own `eth` fork id (EL) or `eth2` digest (CL), not the
  row columns. A Status-classified row can be current while its ENR advertises no fork entry or a
  stale one; consumers pre-qualify peers from the record alone, and discv4-crawl's `devp2p nodeset
  filter -eth-network` applied the same bar - every record in the EF production trees carries a
  parseable `eth` entry. Expect the tree to shed ENR-stale nodes for a cycle at fork transitions,
  bounded by the collapse guards.
  When `--limit` binds, IPv6 also gets a reserved share: address family is not a client-balance
  dimension, so a family holding a few percent of the pool otherwise rounds away to nothing (at
  `--limit=25` a 2.4% IPv6 share expects 0.6 nodes). `reserveIPv6` gives it its proportional share and
  never less than one slot, preferring records with an explicit `tcp6`/`quic6` — sigp's `enr` crate
  reads `tcp6` with no fallback to `tcp`, so those are the ones a discv5-based client can dial over v6.
  There is one mode: `--base-domain`, deployed as a scheduled
  service via `--publish-interval`, emitting every `<all|snap>.<net>.<base>` tree per cycle. Both
  capabilities are always built — `snap` is selected by ENR-entry presence like `devp2p nodeset
  filter -snap`, so it is not a flag; the dead `les` capability is intentionally unsupported.
  `--layer` (default `el`, excluding un-peerable beacon nodes) narrows the population, and every
  selection option is validated by `selectOpts.Validate` before any tree is built. Publishing is
  guarded skip-and-keep-last-good, and **gating** is network-atomic: both of a network's trees are
  built and checked before either is written, so one guard firing keeps both last-good copies. The
  writes themselves are sequential, so an I/O failure between them can leave one tree updated until
  the next cycle rewrites both. It
  skips a stale snapshot (`--max-snapshot-age`), any tree that selected zero nodes (an empty tree
  signs and parses like any other, so nothing downstream would notice it replacing a working one),
  an all-tree below `--min-tree-nodes` (the floor is deliberately not applied to the snap subset,
  which is legitimately smaller), or a `--max-drop-pct` collapse vs that domain's own last publish
  (`enrscout_dns_tree_nodes` per tree, `enrscout_dns_artifact_skipped_total{reason}` on skips).
  Metrics keep the two stages apart: `enrscout_dns_*artifact*` covers building and writing tree JSON,
  `enrscout_dns_published_*` covers what reached DNS. A freshness alert on the artifact timestamp
  would not notice a zone that stopped accepting writes.
  Sequences are per domain and strictly increasing: the generation is only second-resolution while
  manifests are nanosecond, so `treeSequence` steps past the previous artifact's sequence rather than
  reusing it. A zero-node artifact written before that guard existed yields no collapse baseline
  instead of wedging its domain.
  **Remaining before it replaces discv4-crawl:** git output of the tree JSON.
- **The DNS push is one interface with two providers.** `recordPublisher.Sync` reconciles one domain's
  TXT records; `cloudflare.go` and `route53.go` implement it and are mutually exclusive
  (`--cloudflare-zone-id` or `--route53-zone-id`, never both). Both write entries before the root, so
  a resolver never follows a new root into a subtree that does not exist yet, and both keep the
  previous generation's records (`retain`, read from the `.published` artifact) so a client still
  holding the old root can finish its walk. A nil `retain` means nothing is known to have been
  published and pruning is skipped entirely — pointing a fresh process at a live zone must not delete
  the tree it is already serving. The `.published` artifact is committed only after a push succeeds,
  which is what keeps a failed push from moving the collapse baseline onto a tree DNS never served.
  A failed *prune* only warns: it leaves records the current root does not reference, which must not
  fail a publish that already landed.
- **Route53 and Cloudflare differ in more than auth.** Cloudflare keys records by opaque ID and splits
  long TXT content server-side, so a fixed 15s `settle` sleep is the only ordering it can offer.
  Route53 keys by `(name, type)`, needs client-side 253-byte chunking (matching `devp2p` byte-for-byte,
  so a zone it already published needs no rewrite), needs a DELETE to repeat the stored values and TTL
  verbatim (hence `r53RecordSet.values` is kept as returned, never re-rendered), counts an UPSERT
  twice against its 1000-change/32000-byte batch limits, and offers a real ordering guarantee:
  `GetChange` polled to `INSYNC` before the root is written. Change detection unquotes what the zone
  returns (`normalizeTXT`) instead of quoting what we want, so chunk boundaries never read as a
  change. Credentials are static, from a mode-0600 `--route53-credentials-file`, behind a hand-written
  `aws.CredentialsProvider`: the SDK's default chain would pull in SSO, STS, and IMDS for nothing.
- **Health/metrics.** api serves `/livez`, `/readyz` (fails past `--max-snapshot-age`),
  `/healthz` (with `generated_at`/age), `/api/v1/meta`, and `/metrics`. crawler serves
  `/metrics` on `--metrics-addr`. Both use `internal/metricsrv` + prometheus.
- **Fingerprinting is layer-specific.** `internal/enrich` identifies EL clients with
  an outbound RLPx Hello or the bounded passive RLPx listener (`fingerprint.go`), and
  CL clients with go-libp2p identify (`libp2pident.go`). All paths feed
  `nodeset.SetFingerprint`; beacon nodes do not speak RLPx.

## Known and accepted limitations

Reviewed and deliberately not fixed; re-litigate only with new information.

- **`/probe` bypasses `targetDialBudget`.** It is an authenticated operator endpoint; the only thing
  at risk is the operator's own crawler reputation.
- **`fork_evaluated_at` can lag its response by up to a minute.** The stats cache key buckets to the
  minute while the timestamp is full precision. Classification stays correct via the fork-era key.
- **One network below `--min-current-nodes` quarantines the whole publish.** Generations are
  committed atomically as a set, so partial publication is not an option.
- **CORS preflight carries no `Allow-Headers` or `Max-Age`.** Only simple requests are made.
  `Vary: Origin` is unnecessary because `--cors-origin` is static and never reflected.
- **The final publish quiesces every nodeset writer, and each new one must opt in.** `shutdown`
  (`cmd/crawler/main.go`) closes producers outermost-first before publishing: discovery, the
  periodic loops, the outbound probe queues, `/probe`, then the advertisers. Three of those waits
  exist only because their owners were given close-and-wait — `enrich.InboundListener.Close`,
  `enrich.CLFingerprinter.Close` and `probesrv.Server.Shutdown` each retire their event source
  first, then wait for the handlers it spawned. That ordering is the whole trick: the source is
  the only producer of handler `Add`s, so retiring it is what makes the `Wait` safe rather than a
  race. Anything new that writes to the nodeset from a goroutine needs the same shape and a line
  in `shutdown`, or it will silently race the last snapshot again.

## Non-obvious constraints (things that will bite you)

- **Fork-id classification uses a recent ~2-year window, NOT history back to genesis**
  (`netconf.gatherHashes`). Genesis-sharing forks (PulseChain) reuse mainnet's ancient
  Frontier hash `fc64ec04`; an all-history sweep misclassifies them as mainnet.
  Guarded by `TestAncientForkDoesNotClassify` - don't widen the window back to genesis.
- **Fork upgrades are pre-activation work.** Upgrade geth before every EL fork and
  update `internal/netconf/consensus.go` before every CL fork/BPO. The CI head-bound
  assertion tells you when `ForkHeadBlock` must advance. A missed update collapses
  current classification and should trip the current-node floor.
- **Devnet registration is startup-only and singular.** `RegisterDevnet` mutates the
  process-global network registry before crawler goroutines start and reserves the
  literal `devnet` name; one process cannot register two enclaves.
- **Licensing: AGPL-3.0-or-later.** go-ethereum `p2p/*` libs are LGPL (compatible); go-libp2p is
  MIT. Never copy from go-ethereum's GPL `cmd/devp2p`. MaxMind GeoLite2 attribution
  must stay visible (map attribution control + About page + NOTICE).
- **CGO split:** the crawler builds `CGO_ENABLED=0` (pure Go, even with libp2p); the
  api needs CGO (`go-duckdb` bundles a C++ lib) → its Docker image is debian-based.
  That also means **the api image cannot be cross-built**: `Dockerfile.api` deliberately
  omits the `--platform=$BUILDPLATFORM` + `GOOS`/`GOARCH` plumbing that `Dockerfile.crawler`
  and `web/Dockerfile` use, so its arm64 build has to run on the native `ubuntu-24.04-arm`
  runner. Making it match the crawler would need an aarch64 C toolchain in the build stage;
  without one it silently falls back to QEMU, and no workflow installs binfmt.
- **`go.mod` says `go 1.26.0`, not a patch version** - the `golang:1.26` image ships an
  older patch and rejects a patch-pinned directive.
- **go-ethereum's `log.SetDefault` also hijacks the global `slog` default.** Call
  `slog.SetDefault` AFTER it or INFO logs vanish (see `cmd/*/main.go`).
- **Snapshot schema upgrades must be online and backward-readable.** Bump
  `snapshot.SchemaVersion`, keep the prior version within its readable range, and add
  read-time defaults in `query.migrateStagingSchema` for additive Parquet columns.
  Cover crawler restore and API loading with compatibility tests. Breaking changes
  need an explicit migration or a new object prefix before dropping old-version support;
  never make a routine deployment wipe production snapshots or restore an empty set.
- **Reachability is family- and transport-aware.** `dialable` (query + dnspublisher) is
  true when a present IP family has a TCP or QUIC port: `tcp`/`quic` for v4, `tcp6`/`quic6`
  for v6. Per the ENR spec, absent `tcp6`/`udp6` means `tcp`/`udp` also applies to v6, so
  an IPv6-only node with only `tcp` is dialable. TCP-less discovery-only nodes are
  **kept** in snapshots (CL nodes are often QUIC-only), just flagged non-dialable.
- **`nodeset.Observe` drops invalid records** - a node with no globally-routable IP
  (unspecified/loopback/multicast/link-local/private) is never recorded, so it can never
  be published. Ports are `uint16`-typed ENR entries, so out-of-range ports fail decode.
- **`--s3-ssl` defaults to TRUE.** Pointing any binary at a plaintext endpoint (e.g. local
  MinIO) requires `--s3-ssl=false` explicitly - the local compose passes it. Snapshots are
  integrity-checked on read (schema version, byte length, SHA-256, generation-key prefix in
  `snapshot.VerifyGeneration`); a generation that fails is rejected and the API keeps its
  previously committed table. `store.Get`
  caps reads at `MaxObjectBytes`, and the fs store rejects path-traversal keys.
- The RLPx Hello advertises `eth/66..72 + snap` deliberately - narrower caps make
  modern clients (reth/Nethermind/latest Geth) drop us before we read their Hello. The
  range is single-sourced (`min/maxEthVersion` + `ethCaps()` in `internal/enrich/fingerprint.go`);
  eth/66..68 send the TD Status, eth/69..72 the block-range Status (eth/70-72 change only
  non-Status messages). Bump `maxEthVersion` as new eth versions ship, but only after
  confirming the new version leaves the Status message unchanged.
