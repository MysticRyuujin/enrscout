package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// config is the crawler's validated configuration. Parsing and every constraint check live here,
// so run() receives settled values rather than forty-odd flag pointers whose rules are enforced
// somewhere else entirely.
type config struct {
	devnetDir           string
	devnetOnly          bool
	devnetForce         bool
	allowPrivate        bool
	advertiserNets      string
	advertiserBase      int
	elIdentities        int
	walkerELIds         int
	resolveRate         float64
	identityDir         string
	ipStack             string
	advertiseIP4        string
	advertiseIP6        string
	nodeDB              string
	netrestrict         string
	workers             int
	maxNodes            int
	maxLegacyCandidates int
	snapInterval        time.Duration
	nodeMaxIdle         time.Duration
	verifiedMaxIdle     time.Duration
	prefix              string
	maxCollapse         int
	forcePublish        bool
	minCurrentNodes     int
	keepGens            int
	keepAggregates      time.Duration
	crawlerID           string
	out                 string
	s3Endpoint          string
	s3Bucket            string
	s3Region            string
	s3SSL               bool
	s3Create            bool
	s3Conditional       string
	pprofAddr           string
	metricsAddr         string
	probeAddr           string
	probeNoAuth         bool
	geoCity             string
	geoASN              string
	fingerprint         bool
	fpWorkers           int
	fpMaxInflight       int
	fpTimeout           time.Duration
	targetDialRate      float64
	targetDialBurst     int
	level               slog.Level
	probeTokenValue     string
}

func parseFlags() (*config, error) {
	var c config
	var logLevel, probeToken string

	flag.StringVar(&c.devnetDir, "devnet-dir", "", "load a Kurtosis devnet definition from this dir (genesis.json, config.yaml, genesis_validators_root.txt, bootnodes.txt) and register it as network \"devnet\"")
	flag.BoolVar(&c.devnetOnly, "devnet-only", false, "seed discovery only from the devnet bootnodes (confines the crawl to the isolated devnet instead of the global DHT)")
	flag.BoolVar(&c.devnetForce, "devnet-force-unclassified", false, "in --devnet-only mode, classify in-range TCP records without fork metadata as devnet EL (isolated interoperability tests only)")
	flag.BoolVar(&c.allowPrivate, "allow-private-ips", false, "keep nodes with private IPs (needed to crawl an isolated devnet)")
	flag.StringVar(&c.advertiserNets, "advertiser-networks", "mainnet,hoodi,sepolia", "comma-separated networks receiving EL and CL advertiser identities")
	flag.IntVar(&c.advertiserBase, "advertiser-port-base", 30303, "first port in the contiguous advertiser identity range")
	flag.IntVar(&c.elIdentities, "el-identities-per-network", 1, "independently keyed EL discovery and RLPx advertiser identities per network")
	flag.IntVar(&c.walkerELIds, "walker-el-identities", 1, "EL identities per network whose DHT random walks feed resolution (0 = all; non-walkers stay findable advertisers)")
	flag.Float64Var(&c.resolveRate, "resolve-rate", 100, "global cap on discovery candidates fed to resolution per second (0 = unlimited)")
	flag.StringVar(&c.identityDir, "identity-dir", "data/identities", "directory for persistent <network>-<layer>.key advertiser identities")
	flag.StringVar(&c.ipStack, "ip-stack", "auto", "IP families: auto|ipv4|ipv6|dual")
	flag.StringVar(&c.advertiseIP4, "advertise-ip4", "", "public IPv4 address to pin in the crawler ENR (skips endpoint prediction; set from known deployment inventory)")
	flag.StringVar(&c.advertiseIP6, "advertise-ip6", "", "public IPv6 address to pin in the crawler ENR (endpoint prediction never converges for v6 behind a v4-only bootstrap)")
	flag.StringVar(&c.nodeDB, "nodedb", "", "path to persistent node DB (empty = in-memory)")
	flag.StringVar(&c.netrestrict, "netrestrict", "", "comma-separated CIDRs; when set, discovery only accepts nodes within these networks")
	flag.IntVar(&c.workers, "workers", 16, "concurrent ENR-resolution workers")
	flag.IntVar(&c.maxNodes, "max-nodes", 200000, "maximum retained nodes (0 disables the safety limit)")
	flag.IntVar(&c.maxLegacyCandidates, "max-legacy-candidates", 50000, "maximum retained unclassified execution candidates awaiting Status classification (0 disables retention and falls back to one outbound attempt per window)")
	flag.DurationVar(&c.snapInterval, "snapshot-interval", 60*time.Second, "how often to publish a snapshot")
	flag.DurationVar(&c.nodeMaxIdle, "node-max-idle", 24*time.Hour, "remove unpinned nodes not observed within this period (0 = disable age-based removal)")
	flag.DurationVar(&c.verifiedMaxIdle, "verified-node-max-idle", 7*24*time.Hour, "remove previously fingerprinted nodes not observed within this period (0 = disable age-based removal)")
	flag.StringVar(&c.prefix, "prefix", "snapshots", "object key prefix")
	flag.IntVar(&c.maxCollapse, "max-collapse-pct", 50, "refuse to publish when a network shrank by more than this percent since the last publish")
	flag.BoolVar(&c.forcePublish, "force-publish", false, "accept the first publish after startup even if a network shrank past --max-collapse-pct; use when the reduction is intended")
	flag.IntVar(&c.minCurrentNodes, "min-current-nodes", 1, "reject every publish with fewer current-fork nodes in any tracked network (0 disables)")
	flag.IntVar(&c.keepGens, "keep-generations", 48, "immutable snapshot generations to retain per network (0 = keep all)")
	flag.DurationVar(&c.keepAggregates, "keep-aggregates", 30*24*time.Hour, "delete longitudinal aggregate objects older than this, across every methodology (0 = keep all); must exceed the seven-day assessment window")
	flag.StringVar(&c.crawlerID, "crawler-id", "", "identity recorded in the manifest (empty = hostname)")
	flag.StringVar(&c.out, "out", "data", "filesystem output dir (used when --s3-endpoint is empty)")
	flag.StringVar(&c.s3Endpoint, "s3-endpoint", "", "S3-compatible endpoint (host:port); empty uses filesystem")
	flag.StringVar(&c.s3Bucket, "s3-bucket", "enrscout", "S3 bucket")
	flag.StringVar(&c.s3Region, "s3-region", "us-east-1", "S3 region")
	flag.BoolVar(&c.s3SSL, "s3-ssl", true, "use TLS for the S3 endpoint (set false only for a trusted local endpoint)")
	flag.BoolVar(&c.s3Create, "s3-create-bucket", false, "create the S3 bucket if absent (crawler role only; requires elevated storage permissions)")
	flag.StringVar(&c.s3Conditional, "s3-conditional-mode", "native", "manifest write mode: native (atomic PutObject If-Match) or verified (non-atomic precheck/write/verify for partial-S3 services; single-host writer lock required)")
	flag.StringVar(&c.pprofAddr, "pprof", "", "serve net/http/pprof on this address, e.g. 127.0.0.1:6060 (empty = off)")
	flag.StringVar(&c.metricsAddr, "metrics-addr", "", "serve Prometheus /metrics on this address (empty = off)")
	flag.StringVar(&c.probeAddr, "probe-addr", "", "serve the on-demand /probe API on this address (empty = off); authentication required by default")
	flag.StringVar(&probeToken, "probe-token-file", "", "file containing the /probe bearer token (required with --probe-addr unless unsafe opt-in is set)")
	flag.BoolVar(&c.probeNoAuth, "probe-allow-unauthenticated", false, "UNSAFE: allow unauthenticated /probe; local isolated devnets only")
	flag.StringVar(&c.geoCity, "geolite-city", "", "path to GeoLite2-City.mmdb (empty = no geo)")
	flag.StringVar(&c.geoASN, "geolite-asn", "", "path to GeoLite2-ASN.mmdb (empty = no ASN)")
	flag.BoolVar(&c.fingerprint, "fingerprint", true, "enable outbound and inbound EL/CL client fingerprinting")
	flag.IntVar(&c.fpWorkers, "fingerprint-workers", 32, "concurrent outbound fingerprint probes per layer (the EL and CL pools each get this many)")
	flag.IntVar(&c.fpMaxInflight, "fingerprint-max-inflight", 8, "max concurrent RLPx frame reads (bounds worst-case memory from hostile peers)")
	flag.DurationVar(&c.fpTimeout, "fingerprint-timeout", 5*time.Second, "per-probe timeout")
	flag.Float64Var(&c.targetDialRate, "target-dial-rate", 1, "maximum active fingerprint dials per second to one IPv4 address or IPv6 /64 (0 disables)")
	flag.IntVar(&c.targetDialBurst, "target-dial-burst", 2, "burst allowance for each active-dial target")
	flag.StringVar(&logLevel, "log-level", "info", "log verbosity: debug|info|warn|error")
	flag.Parse()

	level, err := parseLogLevel(logLevel)
	if err != nil {
		return nil, err
	}
	c.level = level

	if c.workers < 1 {
		return nil, fmt.Errorf("--workers must be >= 1, got %d", c.workers)
	}
	if c.maxNodes < 0 {
		return nil, fmt.Errorf("--max-nodes must be >= 0, got %d", c.maxNodes)
	}
	if c.minCurrentNodes < 0 {
		return nil, fmt.Errorf("--min-current-nodes must be >= 0, got %d", c.minCurrentNodes)
	}
	if c.devnetForce && !c.devnetOnly {
		return nil, errors.New("--devnet-force-unclassified requires --devnet-only")
	}
	if c.devnetOnly && strings.TrimSpace(c.devnetDir) == "" {
		return nil, errors.New("--devnet-only requires --devnet-dir")
	}
	if c.snapInterval <= 0 {
		return nil, fmt.Errorf("--snapshot-interval must be positive, got %s", c.snapInterval)
	}
	if c.nodeMaxIdle < 0 {
		return nil, fmt.Errorf("--node-max-idle must not be negative, got %s", c.nodeMaxIdle)
	}
	if c.verifiedMaxIdle < 0 {
		return nil, fmt.Errorf("--verified-node-max-idle must not be negative, got %s", c.verifiedMaxIdle)
	}
	if c.maxCollapse < 0 || c.maxCollapse > 100 {
		return nil, fmt.Errorf("--max-collapse-pct must be between 0 and 100, got %d", c.maxCollapse)
	}
	if c.keepGens < 0 {
		return nil, fmt.Errorf("--keep-generations must be >= 0, got %d", c.keepGens)
	}
	if c.keepAggregates < 0 || (c.keepAggregates > 0 && c.keepAggregates < aggregateRetentionFloor) {
		return nil, fmt.Errorf("--keep-aggregates must be 0 or at least %s, got %s", aggregateRetentionFloor, c.keepAggregates)
	}
	if c.fingerprint && c.fpWorkers < 1 {
		return nil, fmt.Errorf("--fingerprint-workers must be >= 1, got %d", c.fpWorkers)
	}
	if c.fingerprint && c.fpMaxInflight < 1 {
		return nil, fmt.Errorf("--fingerprint-max-inflight must be >= 1, got %d", c.fpMaxInflight)
	}
	if c.fingerprint && c.fpTimeout <= 0 {
		return nil, fmt.Errorf("--fingerprint-timeout must be positive, got %s", c.fpTimeout)
	}
	if c.targetDialRate < 0 || (c.targetDialRate > 0 && c.targetDialBurst < 1) {
		return nil, fmt.Errorf("target dial rate must be non-negative and burst positive when enabled")
	}
	if strings.TrimSpace(c.identityDir) == "" {
		return nil, errors.New("--identity-dir must not be empty")
	}
	if c.elIdentities < 1 {
		return nil, fmt.Errorf("--el-identities-per-network must be >= 1, got %d", c.elIdentities)
	}
	if c.walkerELIds < 0 {
		return nil, fmt.Errorf("--walker-el-identities must be >= 0, got %d", c.walkerELIds)
	}
	if c.resolveRate < 0 {
		return nil, fmt.Errorf("--resolve-rate must be >= 0, got %g", c.resolveRate)
	}
	if probeToken != "" && c.probeNoAuth {
		return nil, errors.New("--probe-token-file and --probe-allow-unauthenticated are mutually exclusive")
	}
	if c.probeAddr == "" && (probeToken != "" || c.probeNoAuth) {
		return nil, errors.New("--probe-token-file and --probe-allow-unauthenticated require --probe-addr")
	}
	token, err := readToken(probeToken)
	if err != nil {
		return nil, err
	}
	if c.probeAddr != "" && token == "" && !c.probeNoAuth {
		return nil, errors.New("--probe-addr requires --probe-token-file or explicit --probe-allow-unauthenticated")
	}
	c.probeTokenValue = token

	return &c, nil
}
