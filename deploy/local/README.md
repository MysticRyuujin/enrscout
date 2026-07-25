# Local stack (docker-compose)

Brings up the full ENRScout pipeline on one machine:

- **minio** - S3-compatible object storage (snapshots). Console: http://localhost:9001 (`minioadmin`/`minioadmin`).
- **crawler** - crawls mainnet, writes Parquet snapshots to MinIO.
- **api** - DuckDB-over-snapshots query API: http://localhost:8080
- **web** - map explorer UI: http://localhost:8081

## Run

```bash
docker compose -f deploy/local/docker-compose.yaml up --build
```

Then open http://localhost:8081. The crawler publishes a snapshot every 30s; the
API refreshes on the same cadence, so the map fills in within a minute.

This local/NAT profile runs discovery over IPv4 and publishes ports 30303-30311 for
the mainnet, Hoodi, and Sepolia advertiser identities. Each network uses EL discovery
UDP/RLPx TCP, CL discovery UDP, and CL libp2p TCP/QUIC UDP in that order. Docker
publication only reaches the host: the host firewall and any upstream router must
allow or forward the same protocol-specific ports for unsolicited traffic to reach
the crawler. Advertiser keys persist in the `crawler-identities` volume. HTTP,
metrics, pprof, and MinIO remain loopback-bound. A dual-stack deployment must use
`--ip-stack=dual`, provide routable IPv6, and publish/allow the IPv6 UDP socket too.

## Geo enrichment (optional)

Country/city/ASN require MaxMind GeoLite2 databases. Place them at:

```text
data/geoip/GeoLite2-City.mmdb
data/geoip/GeoLite2-ASN.mmdb
```

They are mounted read-only into the crawler. Without them the crawler logs a
warning and runs with geo disabled (nodes still discovered, just no location).

To fetch them, sign up for a free MaxMind account
(https://www.maxmind.com/en/geolite2/signup) and run once from the repo root:

```bash
docker run --rm \
  -e GEOIPUPDATE_ACCOUNT_ID=<account-id> \
  -e GEOIPUPDATE_LICENSE_KEY=<license-key> \
  -e "GEOIPUPDATE_EDITION_IDS=GeoLite2-City GeoLite2-ASN" \
  -v "$PWD/data/geoip:/usr/share/GeoIP" \
  ghcr.io/maxmind/geoipupdate@sha256:51e70dd6f16cd3e4d845ac02d09940b10772a75b9d741427d235a78570923c1d
```

## Useful

```bash
docker compose -f deploy/local/docker-compose.yaml logs -f crawler
curl "http://localhost:8080/api/v1/stats?network=mainnet"
docker compose -f deploy/local/docker-compose.yaml down          # stop
docker compose -f deploy/local/docker-compose.yaml down -v       # stop + wipe snapshots
```
