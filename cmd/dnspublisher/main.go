package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/p2p/dnsdisc"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/MysticRyuujin/enrscout/internal/buildinfo"
	"github.com/MysticRyuujin/enrscout/internal/metricsrv"
	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
	"github.com/MysticRyuujin/enrscout/internal/snapshot"
	"github.com/MysticRyuujin/enrscout/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("dnspublisher exited", "err", err)
		os.Exit(1)
	}
}

var (
	mDNSTreeNodes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "enrscout_dns_tree_nodes",
		Help: "Nodes in each built EIP-1459 tree (matches devp2p_discv4_dns_nodes). Written as a local artifact, not pushed to DNS.",
	}, []string{"domain"})
	mDNSPublishTotal = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "enrscout_dns_artifact_nodes_total",
		Help: "Nodes across all trees in the last successful artifact-write cycle. See enrscout_dns_published_* for DNS publication.",
	})
	mDNSPublishTimestamp = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "enrscout_dns_last_artifact_write_timestamp_seconds",
		Help: "Unix time of the last cycle that wrote tree artifacts. Not a DNS publication time.",
	})
	mDNSRecordsChanged = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_dns_published_records_changed_total",
		Help: "TXT records created, updated, or deleted in the zone, by domain.",
	}, []string{"domain"})
	mDNSPublishedTimestamp = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "enrscout_dns_last_publish_timestamp_seconds",
		Help: "Unix time of the last cycle that pushed records to DNS. Stays zero while publishing is unconfigured.",
	})
	mDNSPushErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "enrscout_dns_publish_errors_total",
		Help: "DNS pushes that failed, leaving the zone and the collapse baseline unchanged.",
	})
	mDNSPublishErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "enrscout_dns_artifact_write_errors_total",
		Help: "Artifact-write cycle failures.",
	})
	mDNSPublishSkipped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_dns_artifact_skipped_total",
		Help: "Artifact writes a sanity guard skipped, keeping the last-good tree, by reason.",
	}, []string{"reason"})
	mDNSZoneRecords = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "enrscout_dns_zone_records",
		Help: "TXT record sets in the zone under each tree domain after reconciliation (current plus retained generation). Route53 hosted zones default to a 10000 record-set quota.",
	}, []string{"domain"})
	mDNSGitPushErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "enrscout_dns_git_push_errors_total",
		Help: "Git publishes that failed; DNS is unaffected and the next cycle retries from a fresh clone.",
	})
	mDNSGitPushTimestamp = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "enrscout_dns_last_git_push_timestamp_seconds",
		Help: "Unix time of the last cycle whose trees reached the git remote. Stays zero while git publishing is unconfigured.",
	})
	mDNSBuildInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "enrscout_dns_build_info",
		Help: "Deployed build; the labels carry the revision, value is always 1.",
	}, []string{"revision", "source_url"})
)

func run() error {
	var (
		baseDomain   = flag.String("base-domain", "", "publish the full set under <cap>.<net>.<base-domain>")
		networks     = flag.String("networks", "mainnet,hoodi,sepolia", "comma-separated networks")
		publishEvery = flag.Duration("publish-interval", 6*time.Hour, "re-publish every interval (0 = one-shot)")
		maxSnapAge   = flag.Duration("max-snapshot-age", time.Hour, "skip the cycle if the snapshot is older than this (0 = no check)")
		minTreeNodes = flag.Int("min-tree-nodes", 50, "skip a network whose all-tree has fewer nodes than this")
		maxDropPct   = flag.Int("max-drop-pct", 50, "skip a network whose all-tree dropped more than this percent vs the last publish (0 = no check)")
		metricsAddr  = flag.String("metrics-addr", "", "serve Prometheus /metrics on this address (empty = off)")
		keyFile      = flag.String("key-file", "", "signing key: hex secp256k1 or a devp2p Web3 keystore JSON")
		keyPassFile  = flag.String("key-passphrase-file", "", "file holding the keystore passphrase (empty = no passphrase, matching devp2p)")
		outDir       = flag.String("out", "", "directory to write artifacts (empty = stdout)")
		minScore     = flag.Int("min-score", 1, "minimum crawler-local resolution score")
		maxAge       = flag.Duration("max-age", time.Hour, "drop nodes whose last_seen is older than this (0 = no limit)")
		protocol     = flag.String("protocol", "any", "require discovery protocol: any|v4|v5")
		layer        = flag.String("layer", "el", "restrict to a layer: el|cl|any (EL-only matches discv4-crawl trees and excludes un-peerable beacon nodes)")
		limit        = flag.Int("limit", 0, "maximum nodes to include (0 = all)")
		balance      = flag.String("client-balance", balanceProportional, "how --limit is shared across clients: proportional (every identified client gets a slot, the rest by pool share) or none (highest scoring first)")
		dryRun       = flag.Bool("dry-run", false, "allow an ephemeral key and never require --key-file")
		validate     = flag.Bool("validate", false, "parse the signed tree URL back to verify it")
		prefix       = flag.String("prefix", "snapshots", "object key prefix")
		storeFlags   = store.BindFlags(flag.CommandLine, "data", "filesystem snapshot dir")
		cfZone       = flag.String("cloudflare-zone-id", "", "Cloudflare zone to publish TXT records into (empty = write artifacts only)")
		cfTokenFile  = flag.String("cloudflare-token-file", "", "file holding a Cloudflare API token scoped to Zone:DNS:Edit on that zone")
		r53Zone      = flag.String("route53-zone-id", "", "Route53 hosted zone to publish TXT records into (empty = write artifacts only)")
		r53CredsFile = flag.String("route53-credentials-file", "", "file holding aws_access_key_id and aws_secret_access_key for that zone")
		r53Region    = flag.String("route53-region", "us-east-1", "AWS region used to sign Route53 requests (the service endpoint itself is global)")
		entryTTLFlag = flag.Duration("entry-ttl", entryTTL*time.Second, "DNS TTL for tree entries (content-addressed and immutable, so a long TTL only trades cache churn for query volume)")
		rootTTLFlag  = flag.Duration("root-ttl", rootTTL*time.Second, "DNS TTL for the tree root (adds to how long a client can serve a superseded tree)")
		gitRepoURL   = flag.String("git-repo-url", "", "SSH git remote to publish each tree's nodes.json and enrtree-info.json to (empty = off)")
		gitBranch    = flag.String("git-branch", "master", "branch to clone and push")
		gitKeyFile   = flag.String("git-ssh-key-file", "", "PEM SSH deploy key with write access to that remote")
		gitHostsFile = flag.String("git-known-hosts-file", "", "OpenSSH known_hosts file holding the remote's host keys")
		gitDir       = flag.String("git-dir", "", "writable directory holding the git checkout")
		gitName      = flag.String("git-committer-name", "enrscout-dnspublisher", "git commit author name")
		gitEmail     = flag.String("git-committer-email", "enrscout@localhost", "git commit author email")
	)
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	mDNSBuildInfo.WithLabelValues(buildinfo.Revision, buildinfo.SourceURL).Set(1)

	sel := selectOpts{
		minScore: *minScore, maxAge: *maxAge, protocol: *protocol,
		layer: *layer, limit: *limit, balance: *balance,
	}
	if err := sel.Validate(); err != nil {
		return err
	}
	if err := validateDomain(*baseDomain); err != nil {
		return fmt.Errorf("--base-domain: %w", err)
	}
	if *publishEvery < 0 {
		return errors.New("--publish-interval must not be negative")
	}
	if *maxSnapAge < 0 {
		return errors.New("--max-snapshot-age must not be negative")
	}
	if *minTreeNodes < 0 {
		return errors.New("--min-tree-nodes must not be negative")
	}
	if *maxDropPct < 0 || *maxDropPct > 100 {
		return errors.New("--max-drop-pct must be between 0 and 100")
	}
	// Recurring publication needs durable artifacts: they carry both the collapse baseline and
	// the per-domain sequence floor, and an in-process floor does not survive a restart, so a
	// stdout-only service would reuse a sequence for a tree whose age filtering changed it.
	if *publishEvery > 0 && *outDir == "" {
		return errors.New("--publish-interval requires --out: the written artifacts carry the collapse baseline and sequence floor across cycles")
	}
	if (*cfZone == "") != (*cfTokenFile == "") {
		return errors.New("--cloudflare-zone-id and --cloudflare-token-file must be set together")
	}
	if (*r53Zone == "") != (*r53CredsFile == "") {
		return errors.New("--route53-zone-id and --route53-credentials-file must be set together")
	}
	if *cfZone != "" && *r53Zone != "" {
		return errors.New("--cloudflare-zone-id and --route53-zone-id are mutually exclusive: a tree is published into one zone")
	}
	if *r53Zone != "" && *r53Region == "" {
		return errors.New("--route53-region must not be empty")
	}
	publishToDNS := *cfZone != "" || *r53Zone != ""
	// Publishing needs the durable published-state artifact to hold its collapse baseline and the
	// record set retained for clients still on the previous root.
	if publishToDNS && *outDir == "" {
		return errors.New("publishing to DNS requires --out: the published-state artifact carries the collapse baseline and the retained record set")
	}
	// One generation of records is retained, so a generation survives roughly two intervals. Below the
	// client root-recheck interval that grace is shorter than the window clients hold a stale root in.
	if publishToDNS && *publishEvery > 0 && *publishEvery < minPublishInterval {
		return fmt.Errorf("--publish-interval must be at least %s when publishing to DNS: retained records must outlive a client's cached root", minPublishInterval)
	}
	// The same invariant: a resolver caches the root for its TTL on top of the client recheck.
	if publishToDNS && *publishEvery > 0 && *publishEvery < *rootTTLFlag {
		return fmt.Errorf("--publish-interval must be at least --root-ttl (%s): retained records must outlive a client's cached root", *rootTTLFlag)
	}
	ttls, err := validateTTLs(*entryTTLFlag, *rootTTLFlag, *cfZone != "")
	if err != nil {
		return err
	}
	gitSet := 0
	for _, v := range []string{*gitRepoURL, *gitKeyFile, *gitHostsFile, *gitDir} {
		if v != "" {
			gitSet++
		}
	}
	if gitSet != 0 && gitSet != 4 {
		return errors.New("--git-repo-url, --git-ssh-key-file, --git-known-hosts-file, and --git-dir must be set together")
	}
	if *gitRepoURL != "" && (*gitBranch == "" || *gitName == "" || *gitEmail == "") {
		return errors.New("--git-branch, --git-committer-name, and --git-committer-email must not be empty")
	}
	nets, err := netconf.ParseNetworkList(*networks)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	st, err := storeFlags.Open(ctx)
	if err != nil {
		return err
	}

	keyPass := ""
	if *keyPassFile != "" {
		keyPass, err = readKeyPassphrase(*keyPassFile)
		if err != nil {
			return err
		}
	}
	if *metricsAddr != "" {
		if err := metricsrv.Start(*metricsAddr, "dnspublisher"); err != nil {
			return err
		}
	}
	key, err := loadKey(*keyFile, keyPass, *dryRun)
	if err != nil {
		return err
	}
	var publisher recordPublisher
	switch {
	case *cfZone != "":
		token, err := readPrivateFile(*cfTokenFile, "cloudflare token", maxCloudflareTokenBytes)
		if err != nil {
			return err
		}
		publisher = newCloudflareDNS(*cfZone, strings.TrimSpace(string(token)), ttls)
	case *r53Zone != "":
		creds, err := readPrivateFile(*r53CredsFile, "route53 credentials", maxRoute53CredentialsBytes)
		if err != nil {
			return err
		}
		keyID, secret, err := parseRoute53Credentials(creds)
		if err != nil {
			return err
		}
		publisher = newRoute53DNS(*r53Zone, *r53Region, keyID, secret, ttls)
	}
	var gitPub *gitPublisher
	if *gitRepoURL != "" {
		gitPub, err = newGitPublisher(*gitRepoURL, *gitBranch, *gitDir, *gitKeyFile, *gitHostsFile, *gitName, *gitEmail)
		if err != nil {
			return err
		}
	}
	return runMultiTree(ctx, st, snapshot.Layout{Prefix: *prefix}, multiConfig{
		networks: nets, baseDomain: *baseDomain, outDir: *outDir, key: key, sel: sel,
		publishEvery: *publishEvery, maxSnapshotAge: *maxSnapAge,
		minTreeNodes: *minTreeNodes, maxDropPct: *maxDropPct, validate: *validate,
		publisher: publisher, git: gitPub,
	})
}

// builtTree carries what one cycle knows about a tree beyond the artifact schema - the root
// signature and the selected nodes - for transports (git) that publish more than TXT records.
// The on-disk artifact stays the embedded output alone.
type builtTree struct {
	output
	signature string
	nodes     []publishedNode
	// retain is the last published generation's records, nil until a push has succeeded.
	retain map[string]string
}

type publishedNode struct {
	id           string
	record       string
	seq          uint64
	score        int32
	firstSeen    int64
	lastResolved int64
	lastCheck    int64
}

func buildTree(cands []candidate, opt selectOpts, seq uint, domain, network string, key *ecdsa.PrivateKey) (builtTree, error) {
	picked := pick(cands, opt)
	nodes := make([]*enode.Node, len(picked))
	for i, c := range picked {
		nodes[i] = c.node
	}
	tree, err := dnsdisc.MakeTree(seq, nodes, nil)
	if err != nil {
		return builtTree{}, fmt.Errorf("make tree %s: %w", domain, err)
	}
	url, err := tree.Sign(key, domain)
	if err != nil {
		return builtTree{}, fmt.Errorf("sign tree %s: %w", domain, err)
	}
	published := make([]publishedNode, 0, len(picked))
	for _, c := range picked {
		// record is the re-encoded form the tree carries, not the row's ENR verbatim, so the git
		// output stays byte-identical to the published TXT leaves.
		published = append(published, publishedNode{
			id: c.node.ID().String(), record: c.node.String(), seq: c.node.Seq(),
			score: c.row.Score, firstSeen: c.row.FirstSeen, lastResolved: c.row.LastResolved, lastCheck: c.row.LastCheck,
		})
	}
	return builtTree{
		output: output{
			SchemaVersion: outputSchemaVersion, URL: url, Domain: domain, Network: network,
			Capability: opt.capability, Nodes: len(nodes), Seq: uint64(seq), Records: tree.ToTXT(domain),
		},
		signature: tree.Signature(),
		nodes:     published,
	}, nil
}

type multiConfig struct {
	networks       []string
	baseDomain     string
	outDir         string
	key            *ecdsa.PrivateKey
	sel            selectOpts
	publishEvery   time.Duration
	maxSnapshotAge time.Duration
	minTreeNodes   int
	maxDropPct     int
	validate       bool
	publisher      recordPublisher
	git            *gitPublisher
}

// recordPublisher reconciles a zone's TXT records under a domain. retain names records kept even
// when absent from want, so a client still holding the previous root can finish traversing it.
type recordPublisher interface {
	Sync(ctx context.Context, domain string, want, retain map[string]string) (changed int, err error)
}

func collapsed(current, previous, maxDropPct int) bool {
	if maxDropPct <= 0 || previous <= 0 {
		return false
	}
	return current*100 < previous*(100-maxDropPct)
}

// runMultiTree publishes every network×capability tree, seq-stamped by the snapshot generation so a replacement always increases seq.
func runMultiTree(ctx context.Context, st store.Store, layout snapshot.Layout, cfg multiConfig) error {
	issued := map[string]uint64{}
	publish := func() error {
		m, err := snapshot.Read(ctx, st, layout)
		if err != nil {
			return fmt.Errorf("read manifest: %w", err)
		}
		if cfg.maxSnapshotAge > 0 && time.Since(m.GeneratedAt) > cfg.maxSnapshotAge {
			mDNSPublishSkipped.WithLabelValues("stale_snapshot").Inc()
			slog.Warn("skip publish cycle: snapshot too old", "generated_at", m.GeneratedAt, "age", time.Since(m.GeneratedAt).Round(time.Second), "max", cfg.maxSnapshotAge)
			return nil
		}
		evaluatedAt := time.Now()
		total, published := 0, 0
		var cycleTrees []builtTree
		for _, net := range cfg.networks {
			rows, err := snapshot.LoadNetworkRows(ctx, st, layout, m, net)
			if err != nil {
				mDNSPublishSkipped.WithLabelValues("load_failed").Inc()
				slog.Error("skip network: snapshot load failed, keeping the last published trees", "network", net, "err", err)
				continue
			}
			trees, skip, err := buildNetworkTrees(rows, net, m.GeneratedAt, evaluatedAt, cfg, issued)
			if err != nil {
				mDNSPublishSkipped.WithLabelValues("build_failed").Inc()
				slog.Error("skip network: tree build failed, keeping the last published trees", "network", net, "err", err)
				continue
			}
			// Both trees are gated before either is written: they come from one snapshot, so a guard
			// that fires on one means the other must not replace its last-good copy either.
			if skip.reason != "" {
				mDNSPublishSkipped.WithLabelValues(skip.reason).Inc()
				slog.Warn("skip network: "+skip.message, append([]any{"network", net}, skip.args...)...)
				continue
			}
			for _, out := range trees {
				if cfg.validate {
					if _, _, err := dnsdisc.ParseURL(out.URL); err != nil {
						return fmt.Errorf("validate %s: %w", out.Domain, err)
					}
				}
				if err := emitArtifact(out.output, cfg.outDir, out.Domain); err != nil {
					return err
				}
				issued[out.Domain] = out.Seq
				cycleTrees = append(cycleTrees, out)
				mDNSTreeNodes.WithLabelValues(out.Domain).Set(float64(out.Nodes))
				total += out.Nodes
				published++
				slog.Info("wrote tree artifact", "domain", out.Domain, "nodes", out.Nodes, "seq", out.Seq)
			}
			if err := publishNetwork(ctx, cfg, net, trees); err != nil {
				mDNSPushErrors.Inc()
				mDNSPublishSkipped.WithLabelValues("push_failed").Inc()
				slog.Error("skip network: DNS push failed, leaving the zone and collapse baseline unchanged", "network", net, "err", err)
				continue
			}
		}
		// Git is a sibling transport of DNS: it carries every tree that passed gating this cycle,
		// and a failure only warns - the next cycle re-clones and self-heals.
		if cfg.git != nil && len(cycleTrees) > 0 {
			if err := cfg.git.Publish(ctx, cycleTrees, evaluatedAt); err != nil {
				mDNSGitPushErrors.Inc()
				slog.Warn("git publish failed; DNS is unaffected", "err", err)
			} else {
				mDNSGitPushTimestamp.SetToCurrentTime()
			}
		}
		if published > 0 {
			mDNSPublishTotal.Set(float64(total))
			mDNSPublishTimestamp.SetToCurrentTime()
		}
		return nil
	}

	if err := publish(); err != nil {
		mDNSPublishErrors.Inc()
		if cfg.publishEvery == 0 {
			return err
		}
		slog.Error("initial publish failed; will retry", "err", err)
	}
	if cfg.publishEvery == 0 {
		return nil
	}
	t := time.NewTicker(cfg.publishEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := publish(); err != nil {
				mDNSPublishErrors.Inc()
				slog.Error("publish cycle failed", "err", err)
			}
		}
	}
}

type skipDecision struct {
	reason  string
	message string
	args    []any
}

// treeSequence keeps the published sequence strictly increasing per domain. The generation is only
// second-resolution while manifests are nanosecond, and a shutdown publish can share a second with
// a ticker publish, so the generation alone can repeat across genuinely different trees - and
// EIP-1459 resolvers use the sequence to notice a replacement.
func treeSequence(generatedAt time.Time, previousSeq uint64) uint {
	seq := uint64(generatedAt.Unix())
	if seq <= previousSeq {
		seq = previousSeq + 1
	}
	return uint(seq)
}

// publishNetwork pushes a network's trees, then commits their published state. Committing only
// after every push succeeds is what keeps a failed push from moving the collapse baseline onto a
// tree DNS never served.
func publishNetwork(ctx context.Context, cfg multiConfig, network string, trees []builtTree) error {
	if cfg.publisher == nil {
		return nil
	}
	for _, out := range trees {
		changed, err := cfg.publisher.Sync(ctx, out.Domain, out.Records, out.retain)
		if err != nil {
			return fmt.Errorf("publish %s: %w", out.Domain, err)
		}
		mDNSRecordsChanged.WithLabelValues(out.Domain).Add(float64(changed))
		// Committed per tree rather than after the whole network: once a tree is in DNS its state must
		// be recorded, or a later failure leaves it serving records the next cycle would prune.
		if err := emitArtifact(out.output, cfg.outDir, out.Domain+publishedSuffix); err != nil {
			return fmt.Errorf("commit published state %s: %w", out.Domain, err)
		}
		slog.Info("published tree to DNS", "domain", out.Domain, "nodes", out.Nodes, "seq", out.Seq, "records_changed", changed)
	}
	mDNSPublishedTimestamp.SetToCurrentTime()
	return nil
}

// issued carries the sequence this process last published per domain. Without --out there is no
// artifact to read a floor from, yet selection still filters on evaluatedAt, so successive cycles
// over one manifest can change a tree that would otherwise reuse its sequence.
func buildNetworkTrees(rows []nodeset.Row, network string, generatedAt, evaluatedAt time.Time, cfg multiConfig, issued map[string]uint64) ([]builtTree, skipDecision, error) {
	var trees []builtTree
	// CL ENRs never carry the snap entry, so a CL-layer snap tree would be permanently
	// empty and its empty-tree guard would gate the valid all tree every cycle.
	capabilities := []string{"all"}
	if cfg.sel.layer != "cl" {
		capabilities = append(capabilities, "snap")
	}
	cands := rankCandidates(rows, cfg.sel, evaluatedAt)
	for _, capability := range capabilities {
		domain := capability + "." + network + "." + cfg.baseDomain
		previousNodes, previousSeq, retain, err := baselinesFor(cfg, domain, network, capability)
		if err != nil {
			return nil, skipDecision{"baseline_unreadable", "collapse baseline is unusable, keeping the last published trees",
				[]any{"domain", domain, "err", err}}, nil
		}
		opt := cfg.sel
		opt.capability = capability
		out, err := buildTree(cands, opt, treeSequence(generatedAt, max(previousSeq, issued[domain])), domain, network, cfg.key)
		if err != nil {
			return nil, skipDecision{}, err
		}
		out.retain = retain
		// An empty tree is signable and would silently replace a working one, so it is never a
		// publishable state regardless of the configured floor.
		if out.Nodes == 0 {
			return nil, skipDecision{"empty_tree", "selected no nodes", []any{"domain", domain}}, nil
		}
		if capability == "all" && out.Nodes < cfg.minTreeNodes {
			return nil, skipDecision{"below_floor", "all-tree below floor",
				[]any{"nodes", out.Nodes, "floor", cfg.minTreeNodes}}, nil
		}
		if collapsed(out.Nodes, previousNodes, cfg.maxDropPct) {
			return nil, skipDecision{"collapse", "tree collapsed vs last publish",
				[]any{"domain", domain, "nodes", out.Nodes, "previous", previousNodes, "max_drop_pct", cfg.maxDropPct}}, nil
		}
		trees = append(trees, out)
	}
	return trees, skipDecision{}, nil
}

func validateDomain(domain string) error {
	if domain == "" {
		return errors.New("is required")
	}
	if len(domain) > 253 || strings.HasSuffix(domain, ".") {
		return errors.New("must be an unqualified ASCII domain name of at most 253 bytes")
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("contains an invalid DNS label")
		}
		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return errors.New("contains a non-ASCII or invalid DNS label character")
		}
	}
	return nil
}
