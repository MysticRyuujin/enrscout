# ENRScout

An open Ethereum network explorer and crawler. ENRScout continuously discovers
Ethereum nodes over the devp2p discovery protocols - **discv5 (primary) and discv4** -
across **IPv4 and IPv6**, enriches them (network/fork classification, client
fingerprint, geo, hosting), and publishes a current snapshot that powers a
map-centric web explorer.

DHT-served fallback records remain retryable in crawler memory but do not enter
public snapshots until direct discovery resolution or an authenticated transport
fingerprint verifies the identity. This keeps transient or injected discovery leads
from distorting network and client-coverage totals.

Networks: **Mainnet, Hoodi, Sepolia**. See [Definitions](DEFINITIONS.md) for the
precise meanings of active, identified, dialable, and coverage. ENRScout observes
from a single crawler vantage point: its figures are an observed, non-random identity
sample - not a census, machine count, operator count, validator count, or
stake-weighted client-diversity estimate. Coverage gaps can reduce the sample, while
Sybil identities, key rotation, and multiple identities per machine can increase it.

## Architecture

```text
 crawler (one process, all protocols)          object storage (S3/MinIO)        serving
 ┌──────────────────────────────────┐          ┌───────────────────────┐   ┌──────────────┐
 │ discv5 + discv4  (IPv4 + IPv6)    │ Parquet  │ snapshots/mainnet/     │   │ api          │
 │ dedup by node id                 │─────────▶│ snapshots/hoodi/       │◀──│ DuckDB query │
 │ classify EL (forkid) / CL (digest)│  per net │ snapshots/sepolia/     │   │ JSON/GeoJSON │
 │ enrich: RLPx + libp2p, geo, ASN  │          └───────────────────────┘   └──────┬───────┘
 └──────────────────────────────────┘                                        CDN  │
                                                                             ┌─────▼──────┐
                                                                             │ web (SPA)  │
                                                                             │ deck.gl map│
                                                                             └────────────┘
```

- **One crawler** runs discv4 + discv5 on IPv4 + IPv6 simultaneously. The discovery
  DHTs are a single global keyspace, so one process discovers every network's nodes;
  they are deduped by node id (`has_v4`/`has_v5` OR'd together). Per-network EL and
  CL identities advertise compatible fork records and feed the same crawl pipeline.
- Each identity is **classified** by network: EL by EIP-2124 fork id, CL by fork
  digest. Records that don't match one of the tracked networks (other chains -
  Polygon, Berachain, PulseChain, devnets - sharing the DHT) may remain temporarily
  in bounded crawler memory for retries, but are excluded from published snapshots.
- The crawler is **never on the request path**. It writes per-network Parquet
  generations to S3-compatible storage and atomically advances a checksum-protected
  manifest; the API reads committed generations with embedded **DuckDB**; the website
  is static. A crashed crawler means stale data, not downtime.

## Components

| Path                 | What it is                                                       |
| -------------------- | ---------------------------------------------------------------- |
| `cmd/crawler`        | Continuous discovery worker; writes Parquet snapshots.           |
| `cmd/api`            | Stateless query API over the snapshots (DuckDB).                 |
| `cmd/dnspublisher`   | Validates a snapshot and builds a signed EIP-1459 DNS tree.      |
| `web/`               | Static SPA (React + deck.gl map + charts).                       |
| `internal/discovery` | dual-stack discv4/discv5 listeners, ENR resolution.              |
| `internal/enrich`    | RLPx (EL) + libp2p identify (CL) fingerprint, GeoLite2, hosting. |
| `internal/nodeset`   | current-set model, decay scoring, Parquet serialization.         |
| `internal/netconf`   | bootnodes, EL fork-id + CL fork-digest classification.           |
| `internal/store`     | filesystem + S3-compatible (minio-go) object storage.            |
| `internal/query`     | DuckDB query engine over the snapshots.                          |

## Quick start (local)

```bash
docker compose -f deploy/local/docker-compose.yaml up --build
```

Open **http://localhost:8081** (map explorer). API on **:8080**, MinIO console on
**:9001** (`minioadmin`/`minioadmin`). This is an IPv4 development profile: HTTP/admin
ports are loopback-bound, while crawler TCP/UDP 30303 remains subject to the host
firewall and upstream NAT. See the [local deployment guide](deploy/local/README.md)
for credentials, GeoIP, port, and dual-stack details.

## HTTP API

Data endpoints use the `/api/v1` prefix and accept
`?network=mainnet|hoodi|sepolia`. An omitted network returns all configured networks
where that is meaningful.

| Endpoint                          | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| --------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `GET /livez`                      | Public nginx/ingress liveness; it does not assert API or data readiness.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `GET /readyz`                     | Proxied API and snapshot readiness; returns 503 without a snapshot or when it exceeds `--max-snapshot-age`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `GET /healthz`                    | Proxied API/data diagnostics with loaded node count, snapshot generation, and age.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| Private metrics listener          | Prometheus API/process metrics on `--metrics-addr` (default `127.0.0.1:9101`); it is not routed through the public web ingress.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `GET /api/v1/meta`                | Snapshot generation, age, loaded node count, schema/methodology version, run ID, and source revision/URL.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `GET /api/v1/nodes`               | Filter/search/sort/paginate. Text filters use case-insensitive partial matching. Params: `network, client, country, ip, layer(el\|cl), protocol(v4\|v5), ipstack(dual\|ipv6\|ipv4), hosting(yes\|no), dialable(yes\|no), membership(verified\|claimed\|all), fork(current\|stale\|all), q, sort(score\|last_seen\|first_seen\|client\|network), order(asc\|desc), limit, offset`. Rows carry separate membership, fork, fingerprint, liveness, pin, and geolocation evidence fields. Returns `{total,count,nodes[]}`. JSON serializes unsigned 64-bit `seq` and `fork_next` as strings to preserve exact values in JavaScript. |
| `GET /api/v1/nodes/{key}`         | One node by ID (or a hexadecimal ID prefix of at least 16 characters), IP, enode, or ENR.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `GET /api/v1/stats`               | Current-fork-only scalars (current fork ID exactly; earlier eras count as stale) including dialability and separate `execution_stale`/`consensus_stale` audit counts, plus `fork_evaluated_at`, network, EL/CL client (identifications fresher than 7 days, with `el/cl_identified_stale` exclusion counts and `by_direction_el/cl` inbound-outbound mixes), country, ASN organization, OS, layer, and optional client-version breakdowns.                                                                                                                                                                                                                                            |
| `GET /api/v1/map`                 | GeoJSON FeatureCollection of geolocated nodes. `format=compact` returns Web UI tuples `[id_prefix,lon,lat,client,country,city,layer,hosting,ipv6,verified,accuracy_km,subdivision]` (`hosting`/`ipv6`/`verified` as `0\|1`) without repeated GeoJSON keys.                                                                                                                                                                                                                                                                                                                                                                     |

`--cors-origin` controls whether browser JavaScript can read cross-origin API responses;
it is not an authorization or CSRF control.

## DNS tree publisher

`cmd/dnspublisher` reads and verifies the committed manifest and Parquet generation,
selects fresh dialable nodes, and emits signed EIP-1459 tree artifacts. It optionally
pushes those records into a hosted zone. It never shares credentials with the crawler.

Each cycle emits an `all` and a `snap` tree per network, under
`<all|snap>.<network>.<base-domain>`:

```bash
go build -o bin/enrscout-dnspublisher ./cmd/dnspublisher
chmod 600 /secure/path/dns-tree.key
bin/enrscout-dnspublisher \
  --base-domain=nodes.example.org \
  --networks=mainnet,hoodi,sepolia \
  --publish-interval=6h \
  --key-file=/secure/path/dns-tree.key \
  --out=artifacts
```

Sequence numbers are derived from the snapshot generation and stepped past the
previous artifact's, so they always increase without being managed by hand. That
makes `--out` mandatory for recurring publication: the written artifacts carry both
the sequence floor and the collapse baseline across restarts. Pass
`--publish-interval=0` for a one-shot run, which may write to stdout instead.

A network's two trees are gated together and any guard keeps both last-good copies:
a stale snapshot (`--max-snapshot-age`), a tree that selected no nodes, an all-tree
below `--min-tree-nodes`, or a drop past `--max-drop-pct` against that domain's own
last publish. Keep the signing key in a secret manager or offline signing workflow;
the output JSON contains only public tree records and the signed URL.

### Publishing to a hosted zone

Without zone flags the publisher only writes artifacts. Add one provider — never both —
to push the TXT records as well:

```bash
# Cloudflare: an API token scoped to Zone:DNS:Edit on that zone.
  --cloudflare-zone-id=<zone> --cloudflare-token-file=/secure/path/cf-token

# AWS Route53: aws_access_key_id and aws_secret_access_key for that hosted zone.
  --route53-zone-id=<Z...> --route53-credentials-file=/secure/path/aws-credentials
```

The credentials file accepts either `KEY=VALUE` lines or a single-profile AWS shared
credentials file, and must be mode 0600 or stricter. The Route53 identity needs
`route53:ListResourceRecordSets` and `route53:ChangeResourceRecordSets` on the hosted
zone, plus `route53:GetChange`. `--route53-region` (default `us-east-1`) only signs the
request; the service endpoint is global.

Publishing requires `--out`, because the `.published` artifact carries both the collapse
baseline and the records retained for clients still resolving the previous root. It also
floors `--publish-interval` at 30 minutes, the interval at which EIP-1459 clients
re-check a root: one retained generation has to outlive a cached root. Entries are
written and confirmed propagated before the root is replaced, so a resolver never
follows a new root into a subtree that does not exist yet.

### Publishing to a git repository

The publisher can also push each tree's node list to a git repo, continuing the
[ethereum/discv4-dns-lists](https://github.com/ethereum/discv4-dns-lists) format: per tree
directory, `nodes.json` (devp2p nodeset format) and `enrtree-info.json` (signed root metadata).
Set all four flags together:

```bash
  --git-repo-url=git@github.com:org/repo.git \
  --git-branch=master \
  --git-ssh-key-file=/secure/path/deploy-key \
  --git-known-hosts-file=/etc/enrscout/git-known-hosts \
  --git-dir=/var/lib/enrscout/dns/git
```

The deploy key must be mode 0600 or stricter. The known_hosts file pins the remote's host keys
(full OpenSSH key lines). Each cycle clones fresh at depth 1 and pushes one commit with the trees
that passed gating; a push failure only warns and never blocks DNS publishing. Directories for
trees not built in a cycle keep the remote's previous content.

## Container images

Released images are published to GHCR for `linux/amd64` and `linux/arm64`, each
carrying build provenance and an SBOM attestation:

```
ghcr.io/mysticryuujin/enrscout-crawler:<tag>   # also ships enrscout-dnspublisher
ghcr.io/mysticryuujin/enrscout-api:<tag>
ghcr.io/mysticryuujin/enrscout-web:<tag>
```

Each tag points to a multi-platform image, so Docker automatically selects the native
variant. On an Apple Silicon Mac, Docker Desktop runs the `linux/arm64` variant in its
Linux VM; these are container images, not native macOS executables.

Every image is signed keylessly by the release workflow, with the signing identity
recorded in Sigstore's public transparency log. Signing is recursive — the multi-platform
index and each per-architecture child carry their own signature, so pinning either digest
verifies. Verify before deploying:

```bash
cosign verify \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/MysticRyuujin/enrscout/\.github/workflows/release-images\.yml@refs/tags/v' \
  ghcr.io/mysticryuujin/enrscout-crawler:v0.0.1
```

**Requires cosign v3 or newer.** Signatures live at the OCI 1.1 referrers fallback tag
(`sha256-<digest>`), not the pre-v3 `sha256-<digest>.sig` convention, so v1/v2-era
tooling reports these images as *unsigned* rather than as unverifiable. That includes
admission controllers pinned to the old layout. `cosign triangulate` is deprecated and
still prints the pre-v3 path; it is not where the signature is.

Pin by digest rather than tag for anything long-lived. Builds are not reproducible —
BuildKit records per-run provenance, so rebuilding a tag yields a different digest.

## Operations

Deployment and runtime reference lives in [docs/operations.md](docs/operations.md):

- [Crawler flags](docs/operations.md#crawler-flags-common) — common tuning flags and ports.
- [Production checklist](docs/operations.md#production-checklist) — TLS, egress isolation, storage, schema-upgrade order.
- [Fork-upgrade runbook](docs/operations.md#fork-upgrade-runbook) — pre-activation steps before each EL/CL fork.

See [SECURITY.md](SECURITY.md) for the trust-boundary model and
[docs/measurement-operations.md](docs/measurement-operations.md) for measurement methodology.

## Development

```bash
make build        # crawler + api binaries
make test         # unit + DuckDB integration tests
make lint         # go vet + gofmt
make run-crawler ADVERTISER_NETWORKS=mainnet,hoodi,sepolia
cd web && npm ci && npm run build
```

For the Vite development server, `npm run dev` proxies `/api` to
`http://localhost:8080`. Set `VITE_API_PROXY` to change that proxy target, or set
`VITE_API_BASE` when the browser should call an API base URL directly.

### End-to-end browse test

`web/e2e/browse.mjs` drives the running site with Playwright across all networks and
pages, asserts data populates, and writes screenshots to `web/e2e/screenshots/`:

```bash
cd web && npm ci && npx playwright install chromium
node e2e/browse.mjs        # WEB_BASE defaults to http://localhost:8081
```

See [CONTRIBUTING.md](CONTRIBUTING.md) before opening a change.

## License

[AGPL-3.0-or-later](LICENSE). See [NOTICE](NOTICE) for third-party attributions (go-ethereum
LGPL, go-libp2p MIT, MaxMind GeoLite2).

The binaries statically link LGPL-3.0 go-ethereum library packages. You may exercise the
LGPL relinking right by rebuilding from source (`make build`) against a modified copy of
those packages.
