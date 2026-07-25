package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	gethlog "github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/p2p/netutil"
	"golang.org/x/sys/unix"
	"golang.org/x/time/rate"

	"github.com/MysticRyuujin/enrscout/internal/buildinfo"
	"github.com/MysticRyuujin/enrscout/internal/debugsrv"
	"github.com/MysticRyuujin/enrscout/internal/devnetconfig"
	"github.com/MysticRyuujin/enrscout/internal/discovery"
	"github.com/MysticRyuujin/enrscout/internal/distinct"
	"github.com/MysticRyuujin/enrscout/internal/enrich"
	"github.com/MysticRyuujin/enrscout/internal/metricsrv"
	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
	"github.com/MysticRyuujin/enrscout/internal/probesrv"
	"github.com/MysticRyuujin/enrscout/internal/snapshot"
	"github.com/MysticRyuujin/enrscout/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("crawler exited", "err", err)
		os.Exit(1)
	}
}

func generateRunID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func acquireProcessLock(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing symlink process lock %q", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open crawler process lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf("another crawler holds process lock %q", path)
		}
		return nil, fmt.Errorf("lock crawler process: %w", err)
	}
	return f, nil
}

func releaseProcessLock(f *os.File) {
	if f == nil {
		return
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	_ = f.Close()
}

func sanitizedConfigSHA256(args []string) string {
	sanitized := append([]string(nil), args...)
	redactNext := false
	for i, arg := range sanitized {
		if redactNext {
			sanitized[i] = "<redacted>"
			redactNext = false
			continue
		}
		name, _, hasValue := strings.Cut(arg, "=")
		lower := strings.ToLower(strings.TrimLeft(name, "-"))
		sensitive := strings.Contains(lower, "secret") || strings.Contains(lower, "password") ||
			strings.Contains(lower, "token") || strings.HasSuffix(lower, "key-file")
		if !sensitive {
			continue
		}
		if hasValue {
			sanitized[i] = name + "=<redacted>"
		} else {
			redactNext = true
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(sanitized, "\x00")))
	return hex.EncodeToString(sum[:])
}

type legacyFingerprintDisposition uint8

const (
	legacyFingerprintUnavailable legacyFingerprintDisposition = iota
	legacyFingerprintDeferred
	legacyFingerprintQueued
)

// legacyInboundCandidate returns the best record to promote after an inbound
// RLPx Status exchange authenticated an otherwise unclassified EL peer. Some
// peers advertise a zero Hello listen port, so ListenInbound cannot always
// reconstruct result.Node even when discovery already supplied a usable record.
func legacyInboundCandidate(set *nodeset.Set, pending *pendingLegacyNodes, result enrich.InboundFingerprint, now time.Time) (*enode.Node, string) {
	if candidate, ok := pending.Take(result.NodeID, now); ok {
		return candidate.node, candidate.via
	}
	if result.Node != nil {
		return result.Node, "inbound"
	}
	if set != nil {
		return set.CurrentNode(result.NodeID), "inbound"
	}
	return nil, ""
}

// consensusInboundCandidate recovers the crawler's canonical record when
// libp2p identify and Status succeed but the peer advertises no matching TCP
// listen address from which the inbound path can synthesize a node.
func consensusInboundCandidate(set *nodeset.Set, result enrich.InboundCLFingerprint) *enode.Node {
	if result.Node != nil {
		return result.Node
	}
	if set != nil {
		return set.CurrentNode(result.NodeID)
	}
	return nil
}

type targetLimiterEntry struct {
	limiter *rate.Limiter
	last    time.Time
}

type targetDialBudget struct {
	mu      sync.Mutex
	entries map[string]targetLimiterEntry
	rate    rate.Limit
	burst   int
	limit   int
	ttl     time.Duration
}

func newTargetDialBudget(perSecond float64, burst, limit int, ttl time.Duration) *targetDialBudget {
	return &targetDialBudget{entries: make(map[string]targetLimiterEntry), rate: rate.Limit(perSecond), burst: burst, limit: limit, ttl: ttl}
}

func (b *targetDialBudget) Allow(ip net.IP, now time.Time) bool {
	if b == nil || b.rate <= 0 || ip == nil {
		return true
	}
	key := ip.String()
	if ip4 := ip.To4(); ip4 != nil {
		key = ip4.String() + "/32"
	} else if ip16 := ip.To16(); ip16 != nil {
		key = fmt.Sprintf("%x/64", ip16[:8])
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.entries[key]
	if !ok {
		if len(b.entries) >= b.limit {
			for existing, candidate := range b.entries {
				if now.Sub(candidate.last) >= b.ttl {
					delete(b.entries, existing)
				}
			}
		}
		if len(b.entries) >= b.limit {
			return false
		}
		entry.limiter = rate.NewLimiter(b.rate, b.burst)
	}
	entry.last = now
	b.entries[key] = entry
	return entry.limiter.AllowN(now, 1)
}

func recoverPeerCallback(network, layer string) {
	if recovered := recover(); recovered != nil {
		mCallbackPanics.WithLabelValues(network, layer).Inc()
		slog.Error("recovered panic in peer callback", "network", network, "layer", layer, "panic", recovered)
	}
}

func run() error {
	crawlerStartedAt := time.Now().UTC()
	conf, err := parseFlags()
	if err != nil {
		return err
	}
	// go-ethereum's log.SetDefault also hijacks the global slog default, so ours must come after it.
	gethlog.SetDefault(gethlog.NewLogger(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: conf.level})))
	if err := debugsrv.Start(conf.pprofAddr); err != nil {
		return err
	}

	if conf.devnetDir != "" {
		dcfg, err := devnetconfig.Load(conf.devnetDir)
		if err != nil {
			return fmt.Errorf("load devnet: %w", err)
		}
		if err := netconf.RegisterDevnet(dcfg); err != nil {
			return fmt.Errorf("register devnet: %w", err)
		}
		slog.Info("registered devnet", "bootnodes", len(dcfg.BootnodeRecords), "cl-forks", len(dcfg.CLForks))
	}
	nodeset.AllowPrivateIPs = conf.allowPrivate || conf.devnetOnly

	advertisedNetworks, err := parseAdvertiserNetworks(conf.advertiserNets, conf.devnetOnly)
	if err != nil {
		return err
	}
	for _, name := range advertisedNetworks {
		if _, err := netconf.Get(name); err != nil {
			return fmt.Errorf("--advertiser-networks: %w", err)
		}
	}
	specs, err := identitySpecs(advertisedNetworks, conf.advertiserBase, conf.elIdentities, conf.walkerELIds, conf.fingerprint)
	if err != nil {
		return err
	}
	families, err := resolveFamilies(conf.ipStack, conf.allowPrivate && conf.devnetOnly)
	if err != nil {
		return err
	}
	initializeDiscoveryMetrics(specs, families)
	mBuildInfo.WithLabelValues(buildinfo.Revision, buildinfo.SourceURL).Set(1)
	if err := metricsrv.Start(conf.metricsAddr); err != nil {
		return err
	}
	var restrict *netutil.Netlist
	if conf.netrestrict != "" {
		if restrict, err = netutil.ParseNetlist(conf.netrestrict); err != nil {
			return fmt.Errorf("--netrestrict: %w", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := os.MkdirAll(conf.identityDir, 0o700); err != nil {
		return fmt.Errorf("create identity directory: %w", err)
	}
	processLock, err := acquireProcessLock(filepath.Join(conf.identityDir, ".crawler.lock"))
	if err != nil {
		return err
	}
	defer releaseProcessLock(processLock)

	st, err := store.Open(ctx, store.S3Config{
		Endpoint: conf.s3Endpoint, Region: conf.s3Region, Bucket: conf.s3Bucket,
		AccessKey: os.Getenv("S3_ACCESS_KEY"), SecretKey: os.Getenv("S3_SECRET_KEY"), UseSSL: conf.s3SSL, CreateBucket: conf.s3Create,
		ConditionalMode: conf.s3Conditional,
	}, conf.out)
	if err != nil {
		return err
	}

	// No public vouchers on an isolated devnet: without a pinned IP the ENR stays IP-less.
	var staticIP net.IP
	var staticIPs []net.IP
	if conf.devnetOnly {
		staticIP, err = enclaveIP()
		if err != nil {
			return err
		}
		staticIPs = append(staticIPs, staticIP)
	} else if staticIPs, err = advertiseIPs(conf.advertiseIP4, conf.advertiseIP6); err != nil {
		return err
	}
	geo, err := enrich.OpenGeo(geoPathOrWarn(conf.geoCity), geoPathOrWarn(conf.geoASN))
	if err != nil {
		return err
	}
	if geo != nil {
		defer geo.Close()
	}

	set := nodeset.NewWithLimit(conf.maxNodes)
	layout := snapshot.Layout{Prefix: conf.prefix}
	id := conf.crawlerID
	if id == "" {
		if h, err := os.Hostname(); err == nil {
			id = h
		} else {
			id = "crawler"
		}
	}
	runID, err := generateRunID()
	if err != nil {
		return fmt.Errorf("generate run id: %w", err)
	}
	configSum := sanitizedConfigSHA256(os.Args[1:])
	// The methodology is the hand-bumped MethodologyVersion, not a hash of the configuration. A
	// crawler flag change does not invalidate "distinct identities seen in seven days"; a deliberate
	// change to how measurement works does, and that is what bumping the constant records.
	methodologyID := snapshot.MethodologyVersion
	crawlerHash := sha256.Sum256([]byte(id))
	statePrefix := strings.TrimSuffix(conf.prefix, "/")
	if statePrefix == "" {
		statePrefix = "snapshots"
	}
	distinctStateKey := fmt.Sprintf("%s/state/distinct/%s/%s.json.gz", statePrefix, hex.EncodeToString(crawlerHash[:8]), methodologyID)
	distinctMethodology := methodologyID
	distinctState := distinct.New(distinctMethodology, distinct.DefaultPrecision)
	if raw, stateErr := st.Get(ctx, distinctStateKey); stateErr == nil {
		if restored, restoreErr := distinct.Restore(raw, distinctMethodology, distinct.DefaultPrecision); restoreErr != nil {
			slog.Warn("discarding invalid rolling-distinct state", "err", restoreErr)
		} else {
			distinctState = restored
		}
	} else if !errors.Is(stateErr, store.ErrNotFound) {
		return fmt.Errorf("read rolling-distinct state: %w", stateErr)
	}
	// Expose restored rolling state immediately. Otherwise the per-walker gauges
	// disappear after restart until the next (potentially multi-minute) publish.
	updateDistinctMetrics(distinctState, time.Now())
	runMetadata := &snapshot.RunMetadata{
		RunID:                runID,
		SourceRevision:       buildinfo.Revision,
		SourceURL:            buildinfo.SourceURL,
		ImageDigest:          strings.TrimSpace(os.Getenv("ENRSCOUT_IMAGE_DIGEST")),
		ConfigSHA256:         configSum,
		CrawlerStartedAt:     crawlerStartedAt,
		MethodologyStartedAt: crawlerStartedAt,
		MethodologyVersion:   snapshot.MethodologyVersion,
		MethodologyID:        methodologyID,
	}

	prev, err := restore(ctx, st, layout, set, geo, advertisedNetworks)
	if err != nil {
		return err
	}
	pruneGenerationsForNetworks(ctx, st, layout, conf.keepGens, prev, advertisedNetworks)
	if prev != nil && prev.Run.MethodologyID == methodologyID && !prev.Run.MethodologyStartedAt.IsZero() {
		runMetadata.MethodologyStartedAt = prev.Run.MethodologyStartedAt
	}
	pending := newPendingFingerprints(30*time.Minute, 10000)
	pendingLegacy := newPendingLegacyNodes(30*time.Minute, 10000)
	// One resolve attempt per node per window, including nodes that never resolve:
	// unthrottled retries trip peers' per-IP rate limits and lock the crawler out.
	const attemptWindow = 30 * time.Second
	const legacyAttemptWindow = 6 * time.Hour
	cr := &crawler{
		conf: conf, set: set, geo: geo,
		pending: pending, pendingLegacy: pendingLegacy,
		targetBudget:    newTargetDialBudget(conf.targetDialRate, conf.targetDialBurst, 65536, time.Hour),
		attempted:       newExpiringMap[struct{}](attemptWindow, 65536),
		legacyAttempted: newExpiringMap[struct{}](legacyAttemptWindow, 65536),
		pool:            newFingerprintPool(conf.fpWorkers * 4),
	}

	rt, identityCtx := newIdentityRuntime(ctx, cr, families, restrict, staticIPs)
	defer rt.Close()
	if err := rt.start(specs); err != nil {
		return err
	}
	identities := rt.identities
	rt.startRefresh(identityCtx)

	probes, err := probesrv.Start(probesrv.Options{
		Addr: conf.probeAddr, Token: conf.probeTokenValue, AllowUnauthenticated: conf.probeNoAuth,
	}, cr.fp, cr.clfp, geo, set, conf.fpTimeout, conf.devnetOnly)
	if err != nil {
		return err
	}

	slog.Info("crawler started", "networks", advertisedNetworks, "identities", len(identities), "store", st.Backend(),
		"families", families, "geo", geo != nil, "fingerprint", conf.fingerprint, "crawler-id", id, "restored", set.Len())

	resolvers := newResolverPool(cr, identities, restrict, staticIP, distinctState)
	resolvers.Start(ctx)

	if cr.fp != nil {
		// Outbound legacy probes use mainnet status because the global discv4
		// population is predominantly mainnet. Other networks can still be
		// classified from their authoritative Status reply or an inbound dial.
		legacyStatusName := "mainnet"
		if len(advertisedNetworks) == 1 {
			legacyStatusName = advertisedNetworks[0]
		}
		legacyStatusNetwork, _ := netconf.Get(legacyStatusName)
		cr.pool.startEL(ctx, cr, conf.fpWorkers, legacyStatusNetwork)
	}

	if cr.clfp != nil {
		cr.pool.startCL(ctx, cr, conf.fpWorkers)
	}

	loops := newBackgroundLoops(ctx)
	defer loops.Close()
	if conf.devnetOnly {
		boot, err := netconf.Bootnodes("devnet")
		if err != nil {
			return err
		}
		observeSeeds := func(now time.Time) {
			for _, s := range boot {
				if observed := set.ObserveSeedResult(s, "devnet", now); observed.Applied {
					cr.enqueueFingerprint(s, now)
				}
			}
		}
		observeSeeds(time.Now())
		loops.every(30*time.Second, observeSeeds)
	}

	// Retry retained nodes independently of discovery. A quiet or temporarily
	// disconnected peer should recover without needing to reappear in a DHT walk.
	if cr.fp != nil {
		loops.every(5*time.Second, func(now time.Time) {
			for _, cached := range pending.DrainKnown(now, set.LayerOf) {
				cr.applyInbound(cached.id, cached.layer, cached.value)
			}
			for _, n := range set.FingerprintCandidates(now, conf.fpWorkers*4) {
				cr.enqueueFingerprint(n, now)
			}
		})
	}

	pub := &publisher{
		cfg: publishConfig{
			nodeMaxIdle: conf.nodeMaxIdle, verifiedMaxIdle: conf.verifiedMaxIdle,
			maxCollapsePct: conf.maxCollapse, minCurrentNodes: conf.minCurrentNodes,
			keepGenerations: conf.keepGens, keepAggregates: conf.keepAggregates, forcePublish: conf.forcePublish,
		},
		store: st, layout: layout, set: set, distinct: distinctState, run: runMetadata,
		crawlerID: id, networks: advertisedNetworks, statePrefix: statePrefix, distinctKey: distinctStateKey,
		prev: prev,
	}
	ticker := time.NewTicker(conf.snapInterval)
	defer ticker.Stop()

	// fatal is only ever a lost manifest race: another writer advanced it, so this crawler stops
	// rather than fighting over the pointer.
	var fatal error
	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down", "nodes", set.Len())
			return shutdown(resolvers, loops, cr.pool, probes, probeShutdownGrace(conf.fpTimeout), rt, pub, fatal)
		case <-ticker.C:
			if err := pub.Publish(ctx); err != nil {
				fatal = err
				slog.Error("snapshot writer leadership lost; stopping crawler", "err", err)
				stop()
			}
		}
	}
}

// probeShutdownGrace budgets the graceful half of the probe shutdown. An RLPx probe re-arms the
// socket deadline per stage rather than running under one clock: up to two dials (v4 then v6), the
// handshake, the read-capacity re-arm, and the Status exchange, so a healthy probe can legitimately
// take about five times --fingerprint-timeout. Budgeting less makes a normal probe look like a
// stuck one. Correctness does not rest on this value; probesrv waits for handlers regardless.
func probeShutdownGrace(fpTimeout time.Duration) time.Duration {
	return 5*fpTimeout + 15*time.Second
}

// shutdown quiesces every producer before the final publish, outermost first: discovery stops
// feeding resolution, the periodic loops stop mutating the nodeset, queued probes drain, and the
// advertisers close. Each Close is idempotent, so the deferred ones are harmless afterwards.
func shutdown(resolvers *resolverPool, loops *backgroundLoops, pool *fingerprintPool, probes *probesrv.Server, probeGrace time.Duration, rt *identityRuntime, pub *publisher, fatal error) error {
	resolvers.Close()
	loops.Close()
	pool.Close()
	// Probes come before the advertisers they borrow fingerprinters from, and before the publish
	// their handlers write into.
	probeCtx, cancelProbes := context.WithTimeout(context.Background(), probeGrace)
	if err := probes.Shutdown(probeCtx); err != nil {
		slog.Warn("probe server did not drain gracefully; waited for handlers anyway", "err", err)
	}
	cancelProbes()
	rt.Close()
	if fatal != nil {
		return fatal
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return pub.Publish(ctx)
}

// A manifest read error is fatal: continuing with prev=nil would disable the
// collapse guard and let the first publish overwrite the manifest with a near-empty set.
func restore(ctx context.Context, st store.Store, layout snapshot.Layout, set *nodeset.Set, geo *enrich.Geo, networks []string) (*snapshot.Manifest, error) {
	const subdivisionSchemaVersion = 2
	m, err := snapshot.Read(ctx, st, layout)
	if errors.Is(err, snapshot.ErrNoManifest) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	configured := make(map[string]bool, len(networks))
	for _, network := range networks {
		configured[network] = true
	}
	for network := range m.Networks {
		if !configured[network] {
			slog.Warn("skipping restore of network no longer configured; its rows leave the manifest at the next publish", "network", network)
		}
	}
	for _, network := range networks {
		if _, ok := m.Networks[network]; !ok {
			continue
		}
		rows, err := snapshot.LoadNetworkRows(ctx, st, layout, m, network)
		if err != nil {
			return nil, fmt.Errorf("restore snapshot %s: %w", network, err)
		}
		if m.SchemaVersion < subdivisionSchemaVersion && geo != nil {
			for i := range rows {
				ip := net.ParseIP(rows[i].IP)
				if ip == nil {
					ip = net.ParseIP(rows[i].IP6)
				}
				if result := geo.Lookup(ip); result.Geolocated {
					rows[i].Subdivision = result.Subdivision
				}
			}
		}
		for i := range rows {
			rows[i].Hosting = enrich.ClassifyHosting(rows[i].Org)
		}
		before := set.Len()
		dropped, evicted := set.Ingest(rows)
		if added := set.Len() - before; added+dropped+evicted != len(rows) {
			return nil, fmt.Errorf("restore snapshot %s: retained %d of %d rows (increase --max-nodes)", network, added, len(rows))
		}
		if dropped > 0 {
			slog.Warn("dropped restored rows (undecodable id or failing current address policy)", "network", network, "dropped", dropped)
		}
	}
	slog.Info("restored from snapshot", "generated-at", m.GeneratedAt)
	return m, nil
}

func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("--log-level must be debug, info, warn, or error, got %q", raw)
	}
}

func allowedByNetrestrict(n *enode.Node, restrict *netutil.Netlist) bool {
	if restrict == nil {
		return true
	}
	found := false
	var ip4 enr.IPv4
	if n.Load(&ip4) == nil {
		found = true
		if !restrict.Contains(net.IP(ip4[:])) {
			return false
		}
	}
	var ip6 enr.IPv6
	if n.Load(&ip6) == nil {
		found = true
		if !restrict.Contains(net.IP(ip6[:])) {
			return false
		}
	}
	return found
}

// isCrawlerRecord rejects the process's current discovery identities everywhere.
// An isolated devnet additionally rejects records on its own container IP: crawler
// keys are intentionally ephemeral there, and prior identities remain in clients'
// DHT caches after a restart. Production mode must not apply the IP rule because
// unrelated nodes can legitimately share a public/NAT address.
func isCrawlerRecord(n *enode.Node, current map[enode.ID]bool, devnetOnly bool, advertisedIP net.IP) bool {
	if current[n.ID()] {
		return true
	}
	if !devnetOnly || advertisedIP == nil {
		return false
	}
	var ip4 enr.IPv4
	if n.Load(&ip4) == nil && net.IP(ip4[:]).Equal(advertisedIP) {
		return true
	}
	var ip6 enr.IPv6
	return n.Load(&ip6) == nil && net.IP(ip6[:]).Equal(advertisedIP)
}

func geoPathOrWarn(p string) string {
	if p == "" {
		return ""
	}
	if _, err := os.Stat(p); err != nil {
		slog.Warn("geolite db not found; disabling this geo source", "path", p)
		return ""
	}
	return p
}

func resolveFamilies(stack string, allowPrivate bool) ([]string, error) {
	switch stack {
	case "auto":
		return discovery.DetectFamilies(allowPrivate)
	case "ipv4":
		return []string{"udp4"}, nil
	case "ipv6":
		return []string{"udp6"}, nil
	case "dual":
		return []string{"udp4", "udp6"}, nil
	default:
		return nil, fmt.Errorf("invalid --ip-stack %q (want auto|ipv4|ipv6|dual)", stack)
	}
}

func advertiseIPs(v4, v6 string) ([]net.IP, error) {
	specs := []struct {
		flag string
		val  string
		is4  bool
	}{
		{"--advertise-ip4", v4, true},
		{"--advertise-ip6", v6, false},
	}
	var out []net.IP
	for _, s := range specs {
		if s.val == "" {
			continue
		}
		ip := net.ParseIP(s.val)
		if ip == nil {
			return nil, fmt.Errorf("%s: invalid IP %q", s.flag, s.val)
		}
		if (ip.To4() != nil) != s.is4 {
			return nil, fmt.Errorf("%s: %q is the wrong address family", s.flag, s.val)
		}
		out = append(out, ip)
	}
	return out, nil
}

func loadOrCreateNodeKey(path string) (*ecdsa.PrivateKey, error) {
	info, err := os.Stat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", path)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("%s permissions are %04o; want 0600 or stricter", path, info.Mode().Perm())
		}
		key, loadErr := crypto.LoadECDSA(path)
		if loadErr != nil {
			return nil, fmt.Errorf("load %s: %w", path, loadErr)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	key, err := crypto.GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	if err := crypto.SaveECDSA(path, key); err != nil {
		return nil, fmt.Errorf("create %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("secure %s: %w", path, err)
	}
	return key, nil
}

func readToken(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat probe token: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("probe token must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("probe token %q is accessible by group or others; require mode 0600 or stricter", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read probe token: %w", err)
	}
	if len(b) > 8<<10 {
		return "", errors.New("probe token file exceeds 8 KiB")
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", errors.New("probe token file is empty")
	}
	return token, nil
}
