package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/dnsdisc"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/rlp"
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
	mDNSBuildInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "enrscout_dns_build_info",
		Help: "Deployed build; the labels carry the revision, value is always 1.",
	}, []string{"revision", "source_url"})
)

type output struct {
	SchemaVersion int               `json:"schema_version"`
	URL           string            `json:"url"`
	Domain        string            `json:"domain"`
	Network       string            `json:"network"`
	Capability    string            `json:"capability"`
	Nodes         int               `json:"nodes"`
	Seq           uint64            `json:"seq"`
	Records       map[string]string `json:"records"`
}

const outputSchemaVersion = 1

const publishedSuffix = ".published"

const maxKeyPassphraseBytes = 8 << 10

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
		maxPerCl     = flag.Int("max-per-client", 0, "cap nodes per client for diversity (0 = no cap)")
		dryRun       = flag.Bool("dry-run", false, "allow an ephemeral key and never require --key-file")
		validate     = flag.Bool("validate", false, "parse the signed tree URL back to verify it")
		prefix       = flag.String("prefix", "snapshots", "object key prefix")
		dataDir      = flag.String("data", "data", "filesystem snapshot dir (used when --s3-endpoint is empty)")
		s3Endpoint   = flag.String("s3-endpoint", "", "S3-compatible endpoint (host:port); empty uses filesystem")
		s3Bucket     = flag.String("s3-bucket", "enrscout", "S3 bucket")
		s3Region     = flag.String("s3-region", "us-east-1", "S3 region")
		s3SSL        = flag.Bool("s3-ssl", true, "use TLS for the S3 endpoint (set false only for a trusted local endpoint)")
		cfZone       = flag.String("cloudflare-zone-id", "", "Cloudflare zone to publish TXT records into (empty = write artifacts only)")
		cfTokenFile  = flag.String("cloudflare-token-file", "", "file holding a Cloudflare API token scoped to Zone:DNS:Edit on that zone")
	)
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	mDNSBuildInfo.WithLabelValues(buildinfo.Revision, buildinfo.SourceURL).Set(1)

	sel := selectOpts{
		minScore: *minScore, maxAge: *maxAge, protocol: *protocol,
		layer: *layer, limit: *limit, maxPerClient: *maxPerCl,
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
	// Publishing needs the durable published-state artifact to hold its collapse baseline and the
	// record set retained for clients still on the previous root.
	if *cfZone != "" && *outDir == "" {
		return errors.New("--cloudflare-zone-id requires --out: the published-state artifact carries the collapse baseline and the retained record set")
	}
	// One generation of records is retained, so a generation survives roughly two intervals. Below the
	// client root-recheck interval that grace is shorter than the window clients hold a stale root in.
	if *cfZone != "" && *publishEvery > 0 && *publishEvery < minPublishInterval {
		return fmt.Errorf("--publish-interval must be at least %s when publishing to DNS: retained records must outlive a client's cached root", minPublishInterval)
	}
	nets, err := parseNetworks(*networks)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	st, err := store.Open(ctx, store.S3Config{
		Endpoint: *s3Endpoint, Region: *s3Region, Bucket: *s3Bucket,
		AccessKey: os.Getenv("S3_ACCESS_KEY"), SecretKey: os.Getenv("S3_SECRET_KEY"), UseSSL: *s3SSL,
	}, *dataDir)
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
		if err := metricsrv.Start(*metricsAddr); err != nil {
			return err
		}
	}
	key, err := loadKey(*keyFile, keyPass, *dryRun)
	if err != nil {
		return err
	}
	var publisher recordPublisher
	if *cfZone != "" {
		token, err := readPrivateFile(*cfTokenFile, "cloudflare token", maxCloudflareTokenBytes)
		if err != nil {
			return err
		}
		publisher = newCloudflareDNS(*cfZone, strings.TrimSpace(string(token)))
	}
	return runMultiTree(ctx, st, snapshot.Layout{Prefix: *prefix}, multiConfig{
		networks: nets, baseDomain: *baseDomain, outDir: *outDir, key: key, sel: sel,
		publishEvery: *publishEvery, maxSnapshotAge: *maxSnapAge,
		minTreeNodes: *minTreeNodes, maxDropPct: *maxDropPct, validate: *validate,
		publisher: publisher,
	})
}

func buildTree(rows []nodeset.Row, opt selectOpts, seq uint, domain, network string, key *ecdsa.PrivateKey, now time.Time) (output, error) {
	nodes := selectNodes(rows, opt, now)
	tree, err := dnsdisc.MakeTree(seq, nodes, nil)
	if err != nil {
		return output{}, fmt.Errorf("make tree %s: %w", domain, err)
	}
	url, err := tree.Sign(key, domain)
	if err != nil {
		return output{}, fmt.Errorf("sign tree %s: %w", domain, err)
	}
	return output{
		SchemaVersion: outputSchemaVersion, URL: url, Domain: domain, Network: network,
		Capability: opt.capability, Nodes: len(nodes), Seq: uint64(seq), Records: tree.ToTXT(domain),
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
				if err := emitTree(out, cfg.outDir); err != nil {
					return err
				}
				issued[out.Domain] = out.Seq
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
func publishNetwork(ctx context.Context, cfg multiConfig, network string, trees []output) error {
	if cfg.publisher == nil {
		return nil
	}
	for _, out := range trees {
		prev, err := publishedArtifact(cfg.outDir, out.Domain, network, out.Capability)
		if err != nil {
			return fmt.Errorf("read published state %s: %w", out.Domain, err)
		}
		var retain map[string]string
		if prev != nil {
			retain = prev.Records
		}
		changed, err := cfg.publisher.Sync(ctx, out.Domain, out.Records, retain)
		if err != nil {
			return fmt.Errorf("publish %s: %w", out.Domain, err)
		}
		mDNSRecordsChanged.WithLabelValues(out.Domain).Add(float64(changed))
		// Committed per tree rather than after the whole network: once a tree is in DNS its state must
		// be recorded, or a later failure leaves it serving records the next cycle would prune.
		if _, err := emitArtifact(out, cfg.outDir, out.Domain+publishedSuffix); err != nil {
			return fmt.Errorf("commit published state %s: %w", out.Domain, err)
		}
		slog.Info("published tree to DNS", "domain", out.Domain, "nodes", out.Nodes, "seq", out.Seq, "records_changed", changed)
	}
	mDNSPublishedTimestamp.SetToCurrentTime()
	return nil
}

// baselineFor reads the last built artifact for domain. A zero-node artifact predates the
// empty-tree guard, so it yields no collapse baseline rather than wedging the domain forever, but
// its sequence still has to be exceeded.
func baselineFor(outDir, domain, network, capability string) (nodes int, seq uint64, err error) {
	prev, err := ownArtifact(outDir, domain, domain, network, capability)
	if err != nil || prev == nil {
		return 0, 0, err
	}
	return prev.Nodes, prev.Seq, nil
}

// publishedArtifact reads what a domain last got into DNS, which is nil until a push succeeds.
func publishedArtifact(outDir, domain, network, capability string) (*output, error) {
	return ownArtifact(outDir, domain+publishedSuffix, domain, network, capability)
}

func ownArtifact(outDir, name, domain, network, capability string) (*output, error) {
	if outDir == "" {
		return nil, nil
	}
	prev, exists, err := readPrevious(filepath.Join(outDir, name+".json"))
	if err != nil || !exists {
		return nil, err
	}
	if err := validateBaseline(prev, domain, network, capability); err != nil {
		return nil, err
	}
	return prev, nil
}

// Sequence floor tracks the last build, or a reused sequence leaves resolvers on a cached root for
// changed content. Collapse baseline tracks the last successful publish, or a build a failed push
// never delivered becomes the number the next cycle measures its drop against.
func baselinesFor(cfg multiConfig, domain, network, capability string) (nodes int, seq uint64, err error) {
	nodes, seq, err = baselineFor(cfg.outDir, domain, network, capability)
	if err != nil || cfg.publisher == nil {
		return nodes, seq, err
	}
	published, err := publishedArtifact(cfg.outDir, domain, network, capability)
	if err != nil {
		return 0, 0, err
	}
	if published == nil {
		return 0, seq, nil
	}
	return published.Nodes, seq, nil
}

// issued carries the sequence this process last published per domain. Without --out there is no
// artifact to read a floor from, yet selection still filters on evaluatedAt, so successive cycles
// over one manifest can change a tree that would otherwise reuse its sequence.
func buildNetworkTrees(rows []nodeset.Row, network string, generatedAt, evaluatedAt time.Time, cfg multiConfig, issued map[string]uint64) ([]output, skipDecision, error) {
	var trees []output
	// CL ENRs never carry the snap entry, so a CL-layer snap tree would be permanently
	// empty and its empty-tree guard would gate the valid all tree every cycle.
	capabilities := []string{"all"}
	if cfg.sel.layer != "cl" {
		capabilities = append(capabilities, "snap")
	}
	for _, capability := range capabilities {
		domain := capability + "." + network + "." + cfg.baseDomain
		previousNodes, previousSeq, err := baselinesFor(cfg, domain, network, capability)
		if err != nil {
			return nil, skipDecision{"baseline_unreadable", "collapse baseline is unusable, keeping the last published trees",
				[]any{"domain", domain, "err", err}}, nil
		}
		opt := cfg.sel
		opt.capability = capability
		out, err := buildTree(rows, opt, treeSequence(generatedAt, max(previousSeq, issued[domain])), domain, network, cfg.key, evaluatedAt)
		if err != nil {
			return nil, skipDecision{}, err
		}
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

func parseNetworks(csv string) ([]string, error) {
	var out []string
	for _, n := range strings.Split(csv, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if !snapshot.ValidComponent(n) {
			return nil, fmt.Errorf("--networks entry %q must contain only letters, digits, underscores, or hyphens", n)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, errors.New("--networks must list at least one network")
	}
	return out, nil
}

func emitArtifact(out output, outDir, name string) (string, error) {
	buf, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	if outDir == "" {
		_, err := os.Stdout.Write(append(buf, '\n'))
		return "", err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(outDir, name+".json")
	return path, atomicWriteFile(path, buf, 0o644)
}

func emitTree(out output, outDir string) error {
	_, err := emitArtifact(out, outDir, out.Domain)
	return err
}

type selectOpts struct {
	minScore     int
	maxAge       time.Duration
	protocol     string
	layer        string
	capability   string
	limit        int
	maxPerClient int
}

// capability is set per tree by buildNetworkTrees, so it is not validated here.
func (o selectOpts) Validate() error {
	if o.minScore < 0 || int64(o.minScore) > int64(1<<31-1) {
		return errors.New("--min-score must be between 0 and 2147483647")
	}
	if o.maxAge < 0 {
		return errors.New("--max-age must not be negative")
	}
	if o.protocol != "any" && o.protocol != "v4" && o.protocol != "v5" {
		return errors.New("--protocol must be any, v4, or v5")
	}
	if o.layer != "el" && o.layer != "cl" && o.layer != "any" {
		return errors.New("--layer must be el, cl, or any")
	}
	if o.limit < 0 {
		return errors.New("--limit must not be negative")
	}
	if o.maxPerClient < 0 {
		return errors.New("--max-per-client must not be negative")
	}
	return nil
}

// enrHasEntry reports ENR-entry presence, matching devp2p nodeset filter's snap/les selection (presence, not fingerprinted caps).
func enrHasEntry(n *enode.Node, key string) bool {
	var raw rlp.RawValue
	return n.Record().Load(enr.WithEntry(key, &raw)) == nil
}

func selectNodes(rows []nodeset.Row, opt selectOpts, now time.Time) []*enode.Node {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Score != rows[j].Score {
			return rows[i].Score > rows[j].Score
		}
		if rows[i].LastSeen != rows[j].LastSeen {
			return rows[i].LastSeen > rows[j].LastSeen
		}
		return rows[i].ID < rows[j].ID
	})
	perClient := map[string]int{}
	var out []*enode.Node
	for _, r := range rows {
		if opt.layer != "" && opt.layer != "any" && r.Layer != opt.layer {
			continue
		}
		if !netconf.RowForkCurrentAt(r.Layer, r.Network, r.ForkHash, r.ForkNext, now) {
			continue
		}
		if !r.Dialable() {
			continue
		}
		if int(r.Score) < opt.minScore || r.ENR == "" {
			continue
		}
		if opt.maxAge > 0 && now.Sub(time.Unix(r.LastSeen, 0)) > opt.maxAge {
			continue
		}
		switch opt.protocol {
		case "v4":
			if !r.HasV4 {
				continue
			}
		case "v5":
			if !r.HasV5 {
				continue
			}
		}
		clientBucket := r.Client
		if clientBucket == "" {
			clientBucket = "<unknown>"
		}
		if opt.maxPerClient > 0 {
			if perClient[clientBucket] >= opt.maxPerClient {
				continue
			}
		}
		n, err := enode.Parse(enode.ValidSchemes, r.ENR)
		if err != nil {
			continue
		}
		if opt.capability == "snap" && !enrHasEntry(n, "snap") {
			continue
		}
		out = append(out, n)
		perClient[clientBucket]++
		if opt.limit > 0 && len(out) >= opt.limit {
			break
		}
	}
	return out
}

// readKeyPassphrase strips only the first line's CR/LF terminator, preserving a passphrase's own surrounding spaces.
func readKeyPassphrase(path string) (string, error) {
	data, err := readPrivateFile(path, "key passphrase", maxKeyPassphraseBytes)
	if err != nil {
		return "", err
	}
	line := string(data)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return strings.TrimSuffix(line, "\r"), nil
}

// readPrivateFile validates and reads the same opened file descriptor so a
// path replacement cannot change the file after its type and mode checks.
func readPrivateFile(path, label string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", label, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", label)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s %q is accessible by group or others; require mode 0600 or stricter", label, path)
	}
	if maxBytes <= 0 {
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", label, err)
		}
		return data, nil
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("%s file exceeds %d bytes", label, maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", label, err)
	}
	// The descriptor can grow after fstat (and some virtual files report size
	// zero), so the bounded read and post-read check are intentionally retained.
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s file exceeds %d bytes", label, maxBytes)
	}
	return data, nil
}

func loadKey(keyFile, keyPass string, dryRun bool) (*ecdsa.PrivateKey, error) {
	if keyFile != "" {
		data, err := readPrivateFile(keyFile, "signing key", 0)
		if err != nil {
			return nil, err
		}
		trimmed := bytes.TrimSpace(data)
		// devp2p dns sign uses a Web3 keystore JSON; accept it so the reused key yields identical enrtree URLs.
		if len(trimmed) > 0 && trimmed[0] == '{' {
			key, err := keystore.DecryptKey(trimmed, keyPass)
			if err != nil {
				return nil, fmt.Errorf("decrypt keystore signing key: %w", err)
			}
			return key.PrivateKey, nil
		}
		return crypto.HexToECDSA(string(trimmed))
	}
	if !dryRun {
		return nil, errors.New("--key-file is required unless --dry-run")
	}
	slog.Warn("no --key-file; generating an ephemeral key (dry-run only, not stable)")
	return crypto.GenerateKey()
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".enrscout-dns-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func readPrevious(path string) (*output, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, store.MaxObjectBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(raw) > store.MaxObjectBytes {
		return nil, false, fmt.Errorf("previous artifact exceeds %d bytes", store.MaxObjectBytes)
	}
	var prev output
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&prev); err != nil {
		return nil, false, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, false, errors.New("previous artifact contains multiple JSON values")
		}
		return nil, false, err
	}
	return &prev, true, nil
}

func outputSequence(prev *output) (uint64, error) {
	if prev.SchemaVersion != outputSchemaVersion {
		return 0, fmt.Errorf("unsupported DNS artifact schema version %d", prev.SchemaVersion)
	}
	if prev.Seq == 0 {
		return 0, errors.New("previous artifact has no sequence")
	}
	return prev.Seq, nil
}

func validateBaseline(prev *output, domain, network, capability string) error {
	if _, err := outputSequence(prev); err != nil {
		return err
	}
	if prev.Domain != domain {
		return fmt.Errorf("artifact domain %q does not match %q", prev.Domain, domain)
	}
	if prev.Network != network {
		return fmt.Errorf("artifact network %q does not match %q", prev.Network, network)
	}
	if prev.Capability != capability {
		return fmt.Errorf("artifact capability %q does not match %q", prev.Capability, capability)
	}
	// Sequences are unix seconds and node counts small; implausible values from a corrupted
	// artifact would wrap treeSequence to 0 or overflow the collapse-guard arithmetic.
	const maxBaselineSeq = uint64(1) << 40
	const maxBaselineNodes = 1 << 24
	if prev.Seq > maxBaselineSeq {
		return fmt.Errorf("artifact sequence %d is implausible", prev.Seq)
	}
	if prev.Nodes < 0 || prev.Nodes > maxBaselineNodes {
		return fmt.Errorf("artifact reports %d nodes", prev.Nodes)
	}
	if len(prev.Records) == 0 {
		return errors.New("artifact has no TXT records")
	}
	return nil
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
