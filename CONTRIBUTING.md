# Contributing

Thanks for improving ENRScout. Small, focused changes with regression tests are easiest
to review.

## Development checks

Use Go 1.26, Node.js 24, and Docker with Compose when working across the full stack.

```bash
go mod download
make lint
make test
go build ./...

cd web
npm ci
npm audit --audit-level=moderate
npm run build
```

For browser coverage, start the local stack, install the declared Playwright browser,
and run the browse script:

```bash
cd web
npx playwright install chromium
node e2e/browse.mjs
```

Schema changes to `nodeset.Row` must stay online and backward-readable: bump
`snapshot.SchemaVersion`, keep the prior version inside the readable range, and add
read-time defaults in `query.migrateStagingSchema` for additive Parquet columns. Cover
crawler restore and API loading with compatibility tests. Only a breaking change needs
an explicit migration or a new object prefix before old-version support is dropped.
Preserve the recent-fork classification window, MaxMind attribution,
peer-string escaping, the CGO build split, and the address-family fallback rules called
out in [AGENTS.md](AGENTS.md).

## Changes and review

- Add a test that fails before a correctness or security fix whenever practical.
- Keep unrelated formatting or dependency churn out of feature changes.
- Update user and operator documentation when flags, API fields, schemas, or deployment
  assumptions change.
- Do not commit secrets, private keys, GeoLite databases, generated snapshots, browser
  screenshots, or compiled binaries.
- Explain interoperability workarounds separately from ENRScout behavior. Keep dated
  experiments and review notes out of tracked project documentation; `.notes/` is the
  ignored local workspace for those artifacts.

Contributions are accepted under the repository's
[AGPL-3.0-or-later license](LICENSE).
