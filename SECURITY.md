# Security policy

## Reporting a vulnerability

Report suspected vulnerabilities through a private
[GitHub security advisory](https://github.com/MysticRyuujin/enrscout/security/advisories/new).
Do not open a public issue until a fix or coordinated disclosure is available.

Include the affected revision, configuration, impact, reproduction steps, and any
suggested mitigation. Reports about peer-controlled data should identify whether the
behavior crosses a trust boundary or only affects the reporting peer itself.

This project currently supports the latest revision on `main`; no older release line
receives separate security updates yet.

## Trust boundaries operators must preserve

- Discovery records, RLPx/libp2p handshakes, client strings, IPs, and DNS inputs are
  untrusted network data. The crawler is expected to make outbound connections to
  peer-advertised addresses, so isolate it from private control-plane and metadata
  networks. `--netrestrict` limits accepted discovery ranges but is not a general
  process sandbox or egress firewall.
- `POST /probe` is an administrative active-dial capability that accepts a caller-supplied
  ENR or enode and can connect to its advertised public address. Address policy rejects
  private and special-use targets in normal operation, but this is still not a stored-node-only
  endpoint. Keep it private and use a high-entropy bearer token from `--probe-token-file`.
  The unauthenticated option is only for an isolated devnet.
- pprof can expose sensitive runtime details. Do not expose it publicly.
- Snapshot SHA-256 values detect corruption and inconsistent object reads; they do not
  authenticate a malicious storage administrator. Protect the bucket, manifest prefix,
  and transport with IAM and TLS. Run exactly one writer per manifest prefix.
- The filesystem store requires a trusted root that other processes cannot mutate; its
  symlink checks are defense in depth rather than a local-user sandbox.
- Most of the API is read-only and intentionally unauthenticated. Put public deployments
  behind TLS, request throttling, and response-size/timeout controls.
- CORS controls whether browser JavaScript can read API responses; it does not authorize
  requests.
- The EIP-1459 signing key is a long-lived authority. `dnspublisher` rejects key files
  readable by group or others; production workflows should use isolated or offline
  signing and publish only the resulting public records.

The local Compose credentials and plaintext MinIO connection are development defaults,
not production-safe examples.
