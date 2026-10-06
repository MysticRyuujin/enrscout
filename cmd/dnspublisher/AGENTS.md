# AGENTS.md

Guidance for coding agents working in `cmd/dnspublisher`. Repo-wide rules are in the root `AGENTS.md`.

- **`cmd/dnspublisher`** builds EIP-1459 trees from a validated snapshot using
  go-ethereum's LGPL `p2p/dnsdisc` (`MakeTree`/`Sign`/`ToTXT`); output is byte-compatible
  with `devp2p dns` / discv4-crawl. It never gets crawler or signing creds inline; the key
  comes from `--key-file` (hex, or a `devp2p`-format Web3 keystore JSON so the exact
  discv4-crawl key can be reused, which keeps the published `enrtree://` URLs identical
  across the migration; empty passphrase by default, `--key-passphrase-file` otherwise;
  both files must be mode 0600 or stricter).
  It publishes only _reachable_ nodes, and only records that are themselves well-formed: `enrWellFormed`
  drops a row whose ENR carries a present-but-undecodable `eth`, address or port entry. Both cases are
  real: a node was observed publishing `eth` as the fork-id list wrapped in an extra RLP string, and a
  port above `uint16` is unrepresentable in the typed entry. go-ethereum signs and parses such a record
  and reports the problem only when that one entry is loaded, so a node can stay dialable through
  another transport and reach the tree. The crawler stays deliberately tolerant of these nodes,
  because dropping them would cost coverage and fingerprints: classification is sticky in
  `nodeset.Observe`, so the ENR blob is refreshed while `Layer`/`Network`/`ForkHash` are not. A
  published record is held to the stricter bar, because a peer that decodes entries strictly rejects the
  whole record (ethrex does) and the tree slot is wasted. Measured against the live EF trees, this
  excludes 0 of 3683 records. The record must also self-describe fork currency (`enrForkCurrentAt`):
  the row currency rule evaluated on the ENR's own `eth` fork id (EL) or `eth2` digest (CL), not the
  row columns. A Status-classified row can be current while its ENR advertises no fork entry or a
  stale one; consumers pre-qualify peers from the record alone, and discv4-crawl's `devp2p nodeset
  filter -eth-network` applied the same bar: every record in the EF production trees carries a
  parseable `eth` entry.
  **Fork readiness ranks, it never filters by itself.** `rankCandidates` ranks each record by
  `netconf.ReadinessAt` on the record's own schedule (ready, neutral, mismatch), against
  `netconf.LayerForkTargetsAt`, which keeps both layers even when their forks activate at different
  instants. The order inside a rank is unchanged (score, `last_seen`, ID). `balanceClients` and
  `reserveIPv6` consume input order, so ready records fill each client's quota first; the reserved
  IPv6 tier still takes explicit `tcp6`/`quic6` before readiness, and which client holds a reserved
  slot can shift per-client counts by up to the reserved slots. Within two publish intervals of a
  scheduled activation, `buildNetworkTrees` publishes a ready-only tree when it passes every guard,
  and otherwise the full ranked tree. The choice is per domain, so `all` and `snap` can differ.
  Under `--layer any`, a layer with no fork near keeps all its records: a tree built then is served across the fork, and EIP-2124
  makes post-fork clients reject a past-fork record whose `Next` does not name the following fork.
  The limit does not bind on small networks, so ranking alone would not change their trees.
  After activation, the first cycle usually skips on the collapse guard and keeps serving the
  ready pre-fork tree. From one publish interval after activation, the collapse guard is exempt
  (the empty and floor guards still apply), dated by the `.published` artifact's sequence, never
  the build floor: a failed exempt push advances the build floor past the fork, and reading that
  would close the exemption and wedge the domain. Only a fork of the tree's own layer counts: an EL
  tree's records stay current across a CL-only fork. Under `--layer any`, a fork of either layer
  exempts the whole tree, which is conservative for the other layer's half. The sequence stands in
  for the publish time without an artifact schema change (strict unmarshal would break rollback,
  and the zone already serves the sequence as `enrtree-root seq=`). It is the snapshot's generation
  time, so it can predate the publish by up to `--max-snapshot-age`. That only widens the window,
  and a post-fork publish from a pre-fork snapshot never passes the guards, because no record in it
  is current. `enrscout_dns_tree_nodes_by_readiness` and the
  `ready`/`ready_only` log fields show what each tree holds.
  When `--limit` binds, IPv6 also gets a reserved share: address family is not a client-balance
  dimension, so a family holding a few percent of the pool otherwise rounds away to nothing (at
  `--limit=25` a 2.4% IPv6 share expects 0.6 nodes). `reserveIPv6` gives it its proportional share and
  never less than one slot, preferring records with an explicit `tcp6`/`quic6`: sigp's `enr` crate
  reads `tcp6` with no fallback to `tcp`, so those are the ones a discv5-based client can dial over v6.
  There is one mode: `--base-domain`, deployed as a scheduled
  service via `--publish-interval`, emitting every `<all|snap>.<net>.<base>` tree per cycle. Both
  capabilities are always built: `snap` is selected by ENR-entry presence like `devp2p nodeset
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
  **Git output** (`git.go`) completes the discv4-dns-lists contract discv4-crawl served: when
  `--git-repo-url` is set (with `--git-ssh-key-file`, `--git-known-hosts-file`, and `--git-dir`,
  all four together), each cycle pushes every gated tree's `nodes.json` (devp2p nodeset format,
  clean-room reimplementation, never code from GPL `cmd/devp2p`; `lastResponse` maps to the row's
  `LastResolved`, the last *successful* resolution) and `enrtree-info.json` into
  `<capability>.<network>.<base-domain>/`. Unlike the legacy crawler, git is output-only (state
  lives in S3 snapshots and `.published` artifacts), so a failed push only warns
  (`enrscout_dns_git_push_errors_total`; freshness via `enrscout_dns_last_git_push_timestamp_seconds`)
  and never blocks DNS. Every cycle re-clones fresh at depth 1, so a rewritten or diverged remote
  can never wedge pushes; directories for trees not in the cycle keep the remote's content and are
  never deleted. Host keys are pinned via the known_hosts file (full OpenSSH key lines, not
  fingerprints); the deploy key must be mode 0600. Expect one commit per publish cycle: the
  sequence advances each cycle, so the root signature always changes. Deploy a single writer per
  repo branch.
- **The DNS push is one interface with two providers.** `recordPublisher.Sync` reconciles one domain's
  TXT records; `cloudflare.go` and `route53.go` implement it and are mutually exclusive
  (`--cloudflare-zone-id` or `--route53-zone-id`, never both). Both write entries before the root, so
  a resolver never follows a new root into a subtree that does not exist yet, and both keep the
  previous generation's records (`retain`, read from the `.published` artifact) so a client still
  holding the old root can finish its walk. A nil `retain` means nothing is known to have been
  published and pruning is skipped entirely: pointing a fresh process at a live zone must not delete
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
