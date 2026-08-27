package query

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"

	"github.com/MysticRyuujin/enrscout/internal/clientname"
	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/snapshot"
	"github.com/MysticRyuujin/enrscout/internal/store"
)

const columns = `id, enode, enr, seq, ip, ip6, tcp, udp, tcp6, udp6, quic, quic6, network, fork_hash, fork_next, layer,
	coalesce(cgc, 0), coalesce(cgc_known, false),
	has_v4, has_v5, score, first_seen, last_seen, last_check,
	coalesce(last_resolved, 0),
	client, client_version, os, lang, capabilities, country, city, coalesce(subdivision, ''), lat, lon, asn, org, hosting, coalesce(hosting_known, false), fp_status, coalesce(fp_at, 0),
	coalesce(geolocated, false), coalesce(geo_accuracy_radius_km, 0),
	coalesce(membership_source, ''), coalesce(membership_verified_at, 0), coalesce(fork_source, ''), coalesce(fork_observed_at, 0),
	coalesce(fp_direction, ''), coalesce(pinned, false), dialable`

const verifiedFingerprintCondition = "fp_status IN ('ok', 'stale')"

// chartMaxFingerprintAge bounds how old a verified fingerprint may be and still count toward client charts; older identifications remain on node detail as last-known state.
const chartMaxFingerprintAge = 7 * 24 * time.Hour

// warmupPeriod gates the disclosure banner only — not the same window as chartMaxFingerprintAge.
const warmupPeriod = 48 * time.Hour

func chartFingerprintConditionAt(at time.Time) (string, int64) {
	return verifiedFingerprintCondition + " AND coalesce(fp_at, 0) >= ?", at.Add(-chartMaxFingerprintAge).Unix()
}

type sortSpec struct {
	columns      []string
	defaultOrder string
}

var sortColumns = map[string]sortSpec{
	"score":      {columns: []string{"score", "last_seen", "id"}, defaultOrder: "desc"},
	"last_seen":  {columns: []string{"last_seen", "score", "id"}, defaultOrder: "desc"},
	"first_seen": {columns: []string{"first_seen", "id"}, defaultOrder: "desc"},
	"client":     {columns: []string{"lower(client)", "lower(client_version)", "id"}, defaultOrder: "asc"},
	// Useful to multi-network API consumers, though the single-network web UI omits it.
	"network": {columns: []string{"network", "score", "id"}, defaultOrder: "asc"},
	// Rows without a decodable cgc sort as zero.
	"cgc": {columns: []string{"coalesce(cgc, 0)", "score", "id"}, defaultOrder: "desc"},
}

func ValidSort(value string) bool {
	if value == "" {
		return true
	}
	_, ok := sortColumns[value]
	return ok
}

func ValidOrder(value string) bool {
	return value == "" || value == "asc" || value == "desc"
}

// where() adds no condition for a value it does not recognize, so an enum value it
// cannot implement has to be rejected here rather than returning unfiltered rows.
var filterEnums = []struct {
	name   string
	get    func(Filter) string
	values map[string]bool
}{
	{"layer", func(f Filter) string { return f.Layer }, map[string]bool{"": true, "el": true, "cl": true}},
	{"protocol", func(f Filter) string { return f.Protocol }, map[string]bool{"": true, "v4": true, "v5": true}},
	{"ipstack", func(f Filter) string { return f.IPStack }, map[string]bool{"": true, "ipv4": true, "ipv6": true, "dual": true}},
	{"hosting", func(f Filter) string { return f.Hosting }, map[string]bool{"": true, "yes": true, "no": true}},
	{"dialable", func(f Filter) string { return f.Dialable }, map[string]bool{"": true, "yes": true, "no": true}},
	{"fork status", func(f Filter) string { return f.ForkStatus }, map[string]bool{"": true, "current": true, "stale": true, "all": true}},
	{"membership", func(f Filter) string { return f.Membership }, map[string]bool{"": true, "verified": true, "claimed": true, "all": true}},
}

func (f Filter) validate() error {
	for _, e := range filterEnums {
		if value := e.get(f); !e.values[value] {
			return fmt.Errorf("invalid %s %q", e.name, value)
		}
	}
	for name, value := range map[string]string{"cgc_min": f.CGCMin, "cgc_max": f.CGCMax} {
		if value == "" {
			continue
		}
		if _, err := parseCGC(value); err != nil {
			return fmt.Errorf("invalid %s %q", name, value)
		}
	}
	return nil
}

func parseCGC(s string) (uint32, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	return uint32(v), err
}

func sortExpression(name, order string) string {
	spec, ok := sortColumns[name]
	if !ok {
		spec = sortColumns["score"]
	}
	if order != "asc" && order != "desc" {
		order = spec.defaultOrder
	}
	direction := strings.ToUpper(order)
	parts := make([]string, len(spec.columns))
	for i, column := range spec.columns {
		parts[i] = column + " " + direction
	}
	return strings.Join(parts, ", ")
}

type Node struct {
	ID             string `json:"id"`
	Enode          string `json:"enode"`
	ENR            string `json:"enr"`
	Seq            uint64 `json:"seq,string"`
	IP             string `json:"ip"`
	IP6            string `json:"ip6"`
	TCP            int    `json:"tcp"`
	UDP            int    `json:"udp"`
	TCP6           int    `json:"tcp6"`
	UDP6           int    `json:"udp6"`
	QUIC           int    `json:"quic"`
	QUIC6          int    `json:"quic6"`
	Network        string `json:"network"`
	ForkHash       string `json:"fork_hash"`
	ForkNext       uint64 `json:"fork_next,string"`
	ForkCompatible bool   `json:"fork_compatible"`
	Layer          string `json:"layer"`
	CGC            uint32 `json:"cgc"`
	CGCKnown       bool   `json:"cgc_known"`
	HasV4          bool   `json:"has_v4"`
	HasV5          bool   `json:"has_v5"`
	Score          int    `json:"score"`

	FirstSeen    int64 `json:"first_seen"`
	LastSeen     int64 `json:"last_seen"`
	LastCheck    int64 `json:"last_check"`
	LastResolved int64 `json:"last_resolved"`

	Client               string  `json:"client"`
	ClientVersion        string  `json:"client_version"`
	OS                   string  `json:"os"`
	Lang                 string  `json:"lang"`
	Capabilities         string  `json:"capabilities"`
	Country              string  `json:"country"`
	City                 string  `json:"city"`
	Subdivision          string  `json:"subdivision"`
	Lat                  float64 `json:"lat"`
	Lon                  float64 `json:"lon"`
	ASN                  uint    `json:"asn"`
	Org                  string  `json:"org"`
	Hosting              bool    `json:"hosting"`
	HostingKnown         bool    `json:"hosting_known"`
	FPStatus             string  `json:"fp_status"`
	FingerprintAt        int64   `json:"fingerprint_at"`
	MembershipSource     string  `json:"membership_source"`
	MembershipVerifiedAt int64   `json:"membership_verified_at"`
	ForkSource           string  `json:"fork_source"`
	ForkObservedAt       int64   `json:"fork_observed_at"`
	FPDirection          string  `json:"fp_direction"`
	Dialable             bool    `json:"dialable"`
	Pinned               bool    `json:"pinned"`
	Geolocated           bool    `json:"geolocated"`
	GeoAccuracyRadiusKM  uint16  `json:"geo_accuracy_radius_km"`
}

type Filter struct {
	Network     string
	Client      string
	ClientExact bool
	Country     string
	Layer       string
	Protocol    string
	IPStack     string
	Hosting     string
	Dialable    string
	ForkStatus  string
	Membership  string
	IP          string
	Q           string
	CGCMin      string
	CGCMax      string
	Sort        string
	Order       string
	Limit       int
	Offset      int
	ForkAt      time.Time
}

type NodesResult struct {
	Total int    `json:"total"`
	Count int    `json:"count"`
	Nodes []Node `json:"nodes"`
}

type Stats struct {
	ForkEvaluatedAt          string   `json:"fork_evaluated_at"`
	SnapshotGeneratedAt      string   `json:"snapshot_generated_at,omitempty"`
	SnapshotAgeSeconds       int      `json:"snapshot_age_seconds"`
	FingerprintWindowSeconds int64    `json:"fingerprint_window_seconds"`
	ELIdentified             int      `json:"el_identified"`
	CLIdentified             int      `json:"cl_identified"`
	WarmingUp                bool     `json:"warming_up"`
	WarmupEndsAt             string   `json:"warmup_ends_at,omitempty"`
	WarmupBasis              string   `json:"warmup_basis,omitempty"`
	WarmupReasons            []string `json:"warmup_reasons,omitempty"`

	Total          int `json:"total"`
	Execution      int `json:"execution"`
	ExecutionStale int `json:"execution_stale"`
	ConsensusStale int `json:"consensus_stale"`
	Consensus      int `json:"consensus"`
	Discv4         int `json:"discv4"`
	Discv5         int `json:"discv5"`
	Geolocated     int `json:"geolocated"`
	IPv6           int `json:"ipv6"`
	DualStack      int `json:"dualstack"`
	Hosting        int `json:"hosting"`
	Dialable       int `json:"dialable"`
	// IPv6 reachability, which IPv6 alone does not imply. IPv6ExplicitPort is the subset carrying
	// their own tcp6/quic6: sigp's enr crate reads tcp6 with no fallback to tcp, so a discv5-based
	// client can only dial these over v6.
	IPv6Dialable     int `json:"ipv6_dialable"`
	IPv6ExplicitPort int `json:"ipv6_explicit_port"`

	ELIdentifiedStale  int `json:"el_identified_stale"`
	CLIdentifiedStale  int `json:"cl_identified_stale"`
	MembershipVerified int `json:"membership_verified"`
	MembershipClaimed  int `json:"membership_claimed"`

	ByNetwork     map[string]int `json:"by_network"`
	ByClient      map[string]int `json:"by_client"`
	ByClientEL    map[string]int `json:"by_client_el"`
	ByClientCL    map[string]int `json:"by_client_cl"`
	ByDirectionEL map[string]int `json:"by_direction_el"`
	ByDirectionCL map[string]int `json:"by_direction_cl"`
	// Generation identifies the loaded table this aggregate was computed against, so a caller
	// pairing it with a separately cached VersionsForMembershipAt result can tell that the two
	// halves straddled a refresh. A caller's own LastRefresh reading cannot: it is taken before
	// the publication barrier and may already be stale.
	Generation time.Time `json:"-"`

	ByCountry map[string]int `json:"by_country"`
	ByOrg     map[string]int `json:"by_org"`
	ByOS      map[string]int `json:"by_os"`
	ByLayer   map[string]int `json:"by_layer"`
	ByVersion map[string]int `json:"by_version"`
}

type Point struct {
	ID          string  `json:"id"`
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	Client      string  `json:"client"`
	Network     string  `json:"network"`
	Country     string  `json:"country"`
	City        string  `json:"city"`
	Subdivision string  `json:"subdivision"`
	Layer       string  `json:"layer"`
	Hosting     bool    `json:"hosting"`
	IPv6        bool    `json:"ipv6"`
	Verified    bool    `json:"verified"`
	AccuracyKM  uint16  `json:"accuracy_km"`
	CGC         uint32  `json:"cgc"`
}

type Engine struct {
	db       *sql.DB
	store    store.Store
	networks []string
	dir      string
	ownedDir bool
	layout   snapshot.Layout
	// refreshMu serialises refreshes; publishMu is the narrow publication barrier readers take so
	// they cannot straddle the table swap. Splitting them keeps object-store reads and the staging
	// load, which dominate a refresh, from blocking requests.
	refreshMu sync.Mutex
	publishMu sync.RWMutex

	mu          sync.RWMutex
	loaded      int
	generatedAt time.Time
	lastRefresh time.Time
	manifestID  string
	run         snapshot.RunMetadata
	crawlerID   string
	schema      int
}

func New(st store.Store, networks []string, tmpDir, prefix string) (*Engine, error) {
	ownedDir := false
	if tmpDir == "" {
		var err error
		tmpDir, err = os.MkdirTemp("", "enrscout-api-")
		if err != nil {
			return nil, err
		}
		ownedDir = true
	} else if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("duckdb", "")
	if err != nil {
		if ownedDir {
			os.RemoveAll(tmpDir)
		}
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	if _, err := db.Exec(emptyTableDDL); err != nil {
		db.Close()
		if ownedDir {
			os.RemoveAll(tmpDir)
		}
		return nil, err
	}
	return &Engine{db: db, store: st, networks: networks, dir: tmpDir, ownedDir: ownedDir, layout: snapshot.Layout{Prefix: prefix}}, nil
}

const emptyTableDDL = `CREATE TABLE IF NOT EXISTS nodes (
	id VARCHAR, enode VARCHAR, enr VARCHAR, seq UBIGINT,
	ip VARCHAR, ip6 VARCHAR, tcp INTEGER, udp INTEGER,
	tcp6 INTEGER, udp6 INTEGER, quic INTEGER, quic6 INTEGER,
	network VARCHAR, fork_hash VARCHAR, fork_next UBIGINT, layer VARCHAR,
	cgc UINTEGER, cgc_known BOOLEAN,
	has_v4 BOOLEAN, has_v5 BOOLEAN, score INTEGER,
	first_seen BIGINT, last_seen BIGINT, last_check BIGINT,
	last_resolved BIGINT,
	client VARCHAR, client_version VARCHAR, os VARCHAR, lang VARCHAR, capabilities VARCHAR,
	country VARCHAR, city VARCHAR, subdivision VARCHAR, lat DOUBLE, lon DOUBLE, asn UINTEGER, org VARCHAR, hosting BOOLEAN, hosting_known BOOLEAN,
	fp_status VARCHAR, fp_at BIGINT, geolocated BOOLEAN, geo_accuracy_radius_km USMALLINT,
	membership_source VARCHAR, membership_verified_at BIGINT, fork_source VARCHAR, fork_observed_at BIGINT,
	fp_direction VARCHAR, pinned BOOLEAN, dialable BOOLEAN
)`

// migrateStagingSchema supplies defaults for additive columns that older,
// still-supported snapshot generations do not contain.
func migrateStagingSchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, "ALTER TABLE nodes_staging ADD COLUMN IF NOT EXISTS subdivision VARCHAR DEFAULT ''"); err != nil {
		return fmt.Errorf("add subdivision snapshot column: %w", err)
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE nodes_staging ADD COLUMN IF NOT EXISTS cgc UINTEGER DEFAULT 0"); err != nil {
		return fmt.Errorf("add cgc snapshot column: %w", err)
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE nodes_staging ADD COLUMN IF NOT EXISTS cgc_known BOOLEAN DEFAULT false"); err != nil {
		return fmt.Errorf("add cgc_known snapshot column: %w", err)
	}
	return nil
}

func (e *Engine) Close() error {
	err := e.db.Close()
	if e.ownedDir {
		if removeErr := os.RemoveAll(e.dir); err == nil {
			err = removeErr
		}
	}
	return err
}

func (e *Engine) Loaded() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.loaded
}

func (e *Engine) GeneratedAt() time.Time {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.generatedAt
}

func (e *Engine) LastRefresh() time.Time {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastRefresh
}

func (e *Engine) RunMetadata() snapshot.RunMetadata {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.run
}

func (e *Engine) CrawlerID() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.crawlerID
}

func (e *Engine) SchemaVersion() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.schema
}

func (e *Engine) Refresh(ctx context.Context) error {
	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()

	m, err := snapshot.Read(ctx, e.store, e.layout)
	if errors.Is(err, snapshot.ErrNoManifest) {
		return errors.New("no snapshots available")
	}
	if err != nil {
		return err
	}
	manifestID := e.manifestIdentity(m)
	e.mu.RLock()
	alreadyLoaded := e.manifestID == manifestID
	e.mu.RUnlock()
	if alreadyLoaded {
		return nil
	}

	var files []string
	expected := make(map[string]int, len(e.networks))
	for i, net := range e.networks {
		ns, ok := m.Networks[net]
		if !ok {
			return fmt.Errorf("manifest has no snapshot for configured network %q", net)
		}
		data, err := e.store.Get(ctx, ns.GenerationKey)
		if err != nil {
			return fmt.Errorf("get %s: %w", ns.GenerationKey, err)
		}
		if err := e.layout.VerifyGeneration(net, ns, data); err != nil {
			return fmt.Errorf("verify %s: %w", net, err)
		}
		f := filepath.Join(e.dir, fmt.Sprintf("network-%d.parquet", i))
		if err := os.WriteFile(f, data, 0o600); err != nil {
			return err
		}
		files = append(files, f)
		expected[net] = ns.NodeCount
	}
	if len(files) == 0 {
		return errors.New("no snapshots available")
	}

	for i, f := range files {
		files[i] = strings.ReplaceAll(f, "'", "''")
	}
	list := "['" + strings.Join(files, "','") + "']"
	// The row_number dedup is defence in depth, not a working path: SnapshotNetworks assigns each
	// node to one network, and the per-network count assertion below deliberately fails the whole
	// refresh if it ever fires, keeping the previously committed table rather than serving a
	// silently deduplicated one.
	q := fmt.Sprintf(`CREATE OR REPLACE TABLE nodes_staging AS
		SELECT * EXCLUDE (rn, fp_attempts, fp_next),
			((ip <> '' AND (tcp <> 0 OR quic <> 0)) OR (ip6 <> '' AND (tcp6 <> 0 OR tcp <> 0 OR quic6 <> 0 OR quic <> 0))) AS dialable
		FROM (
			SELECT *, row_number() OVER (PARTITION BY id ORDER BY last_seen DESC, seq DESC, network ASC) AS rn
			FROM read_parquet(%s)
		) WHERE rn = 1`, list)
	if _, err := e.db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("load snapshots: %w", err)
	}
	swapped := false
	defer func() {
		if !swapped {
			_, _ = e.db.Exec("DROP TABLE IF EXISTS nodes_staging")
		}
	}()
	if err := migrateStagingSchema(ctx, e.db); err != nil {
		return err
	}

	actual := make(map[string]int, len(expected))
	rows, err := e.db.QueryContext(ctx, "SELECT network, count(*) FROM nodes_staging GROUP BY network")
	if err != nil {
		return err
	}
	for rows.Next() {
		var network string
		var count int
		if err := rows.Scan(&network, &count); err != nil {
			rows.Close()
			return err
		}
		actual[network] = count
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for network, count := range expected {
		if actual[network] != count {
			return fmt.Errorf("snapshot %s row count mismatch: got %d want %d", network, actual[network], count)
		}
		delete(actual, network)
	}
	if len(actual) != 0 {
		return fmt.Errorf("snapshots contain unexpected networks: %v", actual)
	}

	e.publishMu.Lock()
	defer e.publishMu.Unlock()
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "CREATE OR REPLACE TABLE nodes AS SELECT * FROM nodes_staging"); err != nil {
		return fmt.Errorf("commit snapshots: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DROP TABLE nodes_staging"); err != nil {
		return fmt.Errorf("drop snapshot staging table: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit snapshots: %w", err)
	}
	swapped = true

	n := 0
	for _, count := range expected {
		n += count
	}
	e.mu.Lock()
	e.loaded = n
	e.generatedAt = m.GeneratedAt
	e.lastRefresh = time.Now()
	e.manifestID = manifestID
	e.schema = m.SchemaVersion
	e.crawlerID = m.CrawlerID
	e.run = m.Run
	e.mu.Unlock()
	return nil
}

func (e *Engine) manifestIdentity(m *snapshot.Manifest) string {
	var b strings.Builder
	b.WriteString(m.GeneratedAt.UTC().Format(time.RFC3339Nano))
	for _, network := range e.networks {
		ns := m.Networks[network]
		fmt.Fprintf(&b, "\x00%s\x00%s\x00%s\x00%d\x00%d", network, ns.GenerationKey, ns.SHA256, ns.Bytes, ns.NodeCount)
	}
	return b.String()
}

// forkNetworks bounds the fork predicate to the networks a request can actually match,
// so one network's broken fork configuration cannot fail requests for a healthy one. An
// unrequested or unserved network yields no candidates, which must stay an empty result
// rather than becoming an error.
func (f Filter) forkNetworks(served []string) []string {
	if f.Network == "" {
		return served
	}
	for _, name := range served {
		if name == f.Network {
			return []string{f.Network}
		}
	}
	return nil
}

func (f Filter) where(networks []string) (string, []any, error) {
	var conds []string
	var args []any
	if f.Network != "" {
		conds = append(conds, "network = ?")
		args = append(args, f.Network)
	}
	if f.Client != "" {
		if f.ClientExact {
			conds = append(conds, "lower(client) = lower(?)")
			args = append(args, f.Client)
		} else {
			conds = append(conds, "lower(client) LIKE lower(?) ESCAPE '$'")
			args = append(args, "%"+escapeLike(f.Client)+"%")
		}
	}
	if f.Country != "" {
		conds = append(conds, "lower(country) LIKE lower(?) ESCAPE '$'")
		args = append(args, "%"+escapeLike(f.Country)+"%")
	}
	if f.Layer != "" {
		conds = append(conds, "layer = ?")
		args = append(args, f.Layer)
	}
	switch f.Protocol {
	case "v4":
		conds = append(conds, "has_v4")
	case "v5":
		conds = append(conds, "has_v5")
	}
	switch f.IPStack {
	case "ipv4":
		conds = append(conds, "ip <> '' AND ip6 = ''")
	case "ipv6":
		conds = append(conds, "ip6 <> '' AND ip = ''")
	case "dual":
		conds = append(conds, "ip <> '' AND ip6 <> ''")
	}
	switch f.Hosting {
	case "yes":
		conds = append(conds, "hosting")
	case "no":
		conds = append(conds, "hosting_known AND NOT hosting")
	}
	switch f.Dialable {
	case "yes":
		conds = append(conds, "dialable")
	case "no":
		conds = append(conds, "NOT dialable")
	}
	switch f.Membership {
	case "verified":
		conds = append(conds, "membership_source = 'status'")
	case "claimed":
		conds = append(conds, "membership_source = 'enr'")
	}
	// Only current and stale need the predicate; building it for "all" would make the
	// audit view fail exactly when fork configuration is broken.
	if f.ForkStatus == "current" || f.ForkStatus == "stale" {
		at := f.ForkAt
		if at.IsZero() {
			at = time.Now()
		}
		condition, currentArgs, err := currentForkConditionAt(at, f.forkNetworks(networks))
		if err != nil {
			return "", nil, err
		}
		if f.ForkStatus == "stale" {
			condition = "NOT " + condition
		}
		conds = append(conds, condition)
		args = append(args, currentArgs...)
	}
	if f.IP != "" {
		pattern := "%" + escapeLike(f.IP) + "%"
		conds = append(conds, "(lower(ip) LIKE lower(?) ESCAPE '$' OR lower(ip6) LIKE lower(?) ESCAPE '$')")
		args = append(args, pattern, pattern)
	}
	if f.Q != "" {
		literal := escapeLike(f.Q)
		pattern := "%" + literal + "%"
		conds = append(conds, "(lower(id) LIKE lower(?) ESCAPE '$' OR lower(ip) LIKE lower(?) ESCAPE '$' OR lower(ip6) LIKE lower(?) ESCAPE '$' OR lower(enode) LIKE lower(?) ESCAPE '$' OR lower(enr) LIKE lower(?) ESCAPE '$')")
		args = append(args, pattern, pattern, pattern, pattern, pattern)
	}
	if f.CGCMin != "" {
		if v, err := parseCGC(f.CGCMin); err == nil {
			conds = append(conds, "coalesce(cgc_known, false) AND coalesce(cgc, 0) >= ?")
			args = append(args, v)
		}
	}
	if f.CGCMax != "" {
		if v, err := parseCGC(f.CGCMax); err == nil {
			conds = append(conds, "coalesce(cgc_known, false) AND coalesce(cgc, 0) <= ?")
			args = append(args, v)
		}
	}
	if len(conds) == 0 {
		return "", args, nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args, nil
}

func currentForkConditionAt(at time.Time, networks []string) (string, []any, error) {
	// The accepted Next bands depend only on at, so the fragment is identical for every
	// network; only the bound hashes differ. The top band is open-ended and renders without
	// an upper bound, because database/sql cannot bind its math.MaxUint64 limit.
	ranges := netconf.CanonicalCurrentNextRanges(at)
	tests := make([]string, 0, len(ranges))
	bounds := make([]any, 0, len(ranges)*2)
	for _, r := range ranges {
		if r[1] == math.MaxUint64 {
			tests = append(tests, "coalesce(fork_next, 0) >= ?")
			bounds = append(bounds, r[0])
			continue
		}
		tests = append(tests, "(coalesce(fork_next, 0) >= ? AND coalesce(fork_next, 0) <= ?)")
		bounds = append(bounds, r[0], r[1])
	}
	part := "(network = ? AND ((layer = 'el' AND lower(fork_hash) = ? AND (" +
		strings.Join(tests, " OR ") + ")) OR (layer = 'cl' AND lower(fork_hash) = ?)))"

	parts := make([]string, 0, len(networks))
	args := make([]any, 0, len(networks)*(3+len(bounds)))
	for _, name := range networks {
		nw, err := netconf.Get(name)
		if err != nil {
			return "", nil, err
		}
		state, err := netconf.CLForkStateAt(name, at)
		if err != nil {
			return "", nil, err
		}
		current := nw.CurrentForkIDAt(at)
		args = append(args, name, hex.EncodeToString(current.Hash[:]))
		args = append(args, bounds...)
		args = append(args, hex.EncodeToString(state.Digest[:]))
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "FALSE", nil, nil
	}
	// coalesce so a NULL column cannot make the condition NULL: "NOT condition" is only
	// the exact complement of fork=current while it is strictly true or false, otherwise
	// a row falls out of both the current and stale views.
	return "coalesce(" + strings.Join(parts, " OR ") + ", FALSE)", args, nil
}

func escapeLike(s string) string {
	return strings.NewReplacer("$", "$$", "%", "$%", "_", "$_").Replace(s)
}

func (e *Engine) Nodes(ctx context.Context, f Filter) (NodesResult, error) {
	if err := f.validate(); err != nil {
		return NodesResult{}, err
	}
	if f.ForkAt.IsZero() {
		f.ForkAt = time.Now()
	}
	if f.ForkStatus == "" {
		f.ForkStatus = "current"
	}
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 100
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	order := sortExpression(f.Sort, f.Order)
	clause, args, err := f.where(e.networks)
	if err != nil {
		return NodesResult{}, err
	}

	var res NodesResult
	// One transaction so a concurrent Refresh table swap can't split Total and the page across generations.
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM nodes"+clause, args...).Scan(&res.Total); err != nil {
		return res, err
	}

	q := fmt.Sprintf("SELECT %s FROM nodes%s ORDER BY %s LIMIT ? OFFSET ?", columns, clause, order)
	rows, err := tx.QueryContext(ctx, q, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return res, err
	}
	defer rows.Close()
	res.Nodes, err = scanNodes(rows, f.ForkAt)
	res.Count = len(res.Nodes)
	return res, err
}

func (e *Engine) NodeByKey(ctx context.Context, keyval string) (*Node, error) {
	at := time.Now()
	isPrefix := isNodeIDPrefix(keyval)
	// Persisted ids are lowercase hex; fold the key so uppercase ids and prefixes still match.
	idKey := strings.ToLower(keyval)
	q := fmt.Sprintf(`SELECT %s FROM nodes
		WHERE id = ? OR (? AND starts_with(id, ?)) OR ip = ? OR ip6 = ? OR enode = ? OR enr = ?
		ORDER BY CASE WHEN id = ? THEN 0 WHEN ? AND starts_with(id, ?) THEN 1 WHEN enode = ? THEN 2 WHEN enr = ? THEN 3 ELSE 4 END,
			score DESC, last_seen DESC, id ASC LIMIT 1`, columns)
	rows, err := e.db.QueryContext(ctx, q,
		idKey, isPrefix, idKey, keyval, keyval, keyval, keyval,
		idKey, isPrefix, idKey, keyval, keyval)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ns, err := scanNodes(rows, at)
	if err != nil {
		return nil, err
	}
	if len(ns) == 0 {
		return nil, nil
	}
	return &ns[0], nil
}

func isNodeIDPrefix(value string) bool {
	if len(value) < 16 || len(value) >= 64 {
		return false
	}
	for _, c := range value {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// StatsForMembershipAt is deliberately client-independent: a per-client breakdown would make the
// whole aggregate uncacheable across the caller-supplied client filter. Use
// VersionsForMembershipAt for that breakdown, and pair results by the returned generation - a
// caller's own LastRefresh reading can already be stale by the time the barrier is taken.
func (e *Engine) StatsForMembershipAt(ctx context.Context, network, membership string, at time.Time) (Stats, error) {
	e.publishMu.RLock()
	defer e.publishMu.RUnlock()
	generatedAt := e.GeneratedAt()
	run := e.RunMetadata()
	s := Stats{
		Generation:      e.LastRefresh(),
		ForkEvaluatedAt: at.UTC().Format(time.RFC3339Nano), FingerprintWindowSeconds: int64(chartMaxFingerprintAge.Seconds()),
		ByNetwork: map[string]int{}, ByClient: map[string]int{}, ByClientEL: map[string]int{}, ByClientCL: map[string]int{},
		ByDirectionEL: map[string]int{}, ByDirectionCL: map[string]int{},
		ByCountry: map[string]int{}, ByOrg: map[string]int{}, ByOS: map[string]int{}, ByLayer: map[string]int{}, ByVersion: map[string]int{},
	}
	if !generatedAt.IsZero() {
		s.SnapshotGeneratedAt = generatedAt.UTC().Format(time.RFC3339Nano)
		if age := at.Sub(generatedAt); age > 0 {
			s.SnapshotAgeSeconds = int(age.Seconds())
		}
	}
	warmupStart := run.MethodologyStartedAt
	if warmupStart.IsZero() {
		warmupStart = run.CrawlerStartedAt
	}
	if !warmupStart.IsZero() {
		s.WarmupBasis = "methodology_age"
		warmupEnds := warmupStart.Add(warmupPeriod)
		if at.Before(warmupEnds) {
			s.WarmingUp = true
			s.WarmupEndsAt = warmupEnds.UTC().Format(time.RFC3339Nano)
			s.WarmupReasons = []string{"methodology_age_lt_48h"}
		}
	}
	f := Filter{Network: network, ForkAt: at, Membership: membership}
	if err := f.validate(); err != nil {
		return Stats{}, err
	}
	rawClause, rawArgs, err := f.where(e.networks)
	if err != nil {
		return Stats{}, err
	}
	currentCondition, currentArgs, err := currentForkConditionAt(at, f.forkNetworks(e.networks))
	if err != nil {
		return Stats{}, err
	}
	clause := appendWhereCondition(rawClause, currentCondition)
	args := appendArgs(rawArgs, currentArgs...)
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return s, err
	}
	defer tx.Rollback()

	scalars := fmt.Sprintf(`SELECT
		count(*),
		count(*) FILTER (WHERE layer = 'el'),
		count(*) FILTER (WHERE layer = 'cl'),
		count(*) FILTER (WHERE has_v4),
		count(*) FILTER (WHERE has_v5),
		count(*) FILTER (WHERE geolocated),
		count(*) FILTER (WHERE ip6 <> ''),
		count(*) FILTER (WHERE ip <> '' AND ip6 <> ''),
		count(*) FILTER (WHERE hosting),
		count(*) FILTER (WHERE dialable),
		-- Equivalent to the v6 half of dialable: if ip6 is set and no v6 port applies, no v4 port
		-- applies either, so dialable is already false. Reusing the column keeps one definition.
		count(*) FILTER (WHERE dialable AND ip6 <> ''),
		count(*) FILTER (WHERE ip6 <> '' AND (tcp6 <> 0 OR quic6 <> 0))
		FROM nodes%s`, clause)
	if err := tx.QueryRowContext(ctx, scalars, args...).Scan(
		&s.Total, &s.Execution, &s.Consensus, &s.Discv4, &s.Discv5, &s.Geolocated, &s.IPv6, &s.DualStack, &s.Hosting, &s.Dialable,
		&s.IPv6Dialable, &s.IPv6ExplicitPort); err != nil {
		return s, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FILTER (WHERE membership_source = 'status'), count(*) FILTER (WHERE membership_source = 'enr') FROM nodes"+clause, args...).Scan(&s.MembershipVerified, &s.MembershipClaimed); err != nil {
		return s, err
	}
	staleClause := appendWhereCondition(rawClause, "layer = 'el' AND NOT "+currentCondition)
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM nodes"+staleClause, appendArgs(rawArgs, currentArgs...)...).Scan(&s.ExecutionStale); err != nil {
		return s, err
	}
	clStaleClause := appendWhereCondition(rawClause, "layer = 'cl' AND NOT "+currentCondition)
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM nodes"+clStaleClause, appendArgs(rawArgs, currentArgs...)...).Scan(&s.ConsensusStale); err != nil {
		return s, err
	}

	groups := map[string]map[string]int{
		"network": s.ByNetwork, "country": s.ByCountry,
		"org": s.ByOrg, "layer": s.ByLayer,
	}
	for col, dst := range groups {
		q := fmt.Sprintf("SELECT %s, count(*) FROM nodes%s GROUP BY 1", col, clause)
		if err := scanCounts(ctx, tx, q, args, dst); err != nil {
			return s, err
		}
	}
	chartCond, chartCutoff := chartFingerprintConditionAt(at)
	identifiedClause := appendWhereCondition(clause, chartCond+" AND client <> ''")
	if err := scanCounts(ctx, tx, "SELECT client, count(*) FROM nodes"+identifiedClause+" GROUP BY 1", appendArgs(args, chartCutoff), s.ByClient); err != nil {
		return s, err
	}
	osClause := appendWhereCondition(clause, chartCond+" AND os <> ''")
	if err := scanCounts(ctx, tx, "SELECT os, count(*) FROM nodes"+osClause+" GROUP BY 1", appendArgs(args, chartCutoff), s.ByOS); err != nil {
		return s, err
	}

	for _, lg := range []struct {
		layer string
		dst   map[string]int
		dir   map[string]int
	}{{"el", s.ByClientEL, s.ByDirectionEL}, {"cl", s.ByClientCL, s.ByDirectionCL}} {
		lf := Filter{Network: network, Layer: lg.layer, ForkStatus: "current", ForkAt: at, Membership: membership}
		lc, la, err := lf.where(e.networks)
		if err != nil {
			return Stats{}, err
		}
		lc = appendWhereCondition(lc, chartCond+" AND client <> ''")
		la = appendArgs(la, chartCutoff)
		q := fmt.Sprintf("SELECT client, count(*) FROM nodes%s GROUP BY 1", lc)
		if err := scanCounts(ctx, tx, q, la, lg.dst); err != nil {
			return s, err
		}
		q = fmt.Sprintf("SELECT fp_direction, count(*) FROM nodes%s GROUP BY 1", lc)
		if err := scanCounts(ctx, tx, q, la, lg.dir); err != nil {
			return s, err
		}
	}

	staleIdentClause := appendWhereCondition(clause, verifiedFingerprintCondition+" AND client <> '' AND coalesce(fp_at, 0) < ?")
	staleIdent := fmt.Sprintf(`SELECT
		count(*) FILTER (WHERE layer = 'el'),
		count(*) FILTER (WHERE layer = 'cl')
		FROM nodes%s`, staleIdentClause)
	if err := tx.QueryRowContext(ctx, staleIdent, appendArgs(args, chartCutoff)...).Scan(&s.ELIdentifiedStale, &s.CLIdentifiedStale); err != nil {
		return s, err
	}

	collapseUnrecognizedClients(s.ByClient)
	collapseUnrecognizedClients(s.ByClientEL)
	collapseUnrecognizedClients(s.ByClientCL)
	for _, count := range s.ByClientEL {
		s.ELIdentified += count
	}
	for _, count := range s.ByClientCL {
		s.CLIdentified += count
	}
	if err := tx.Commit(); err != nil {
		return s, err
	}
	return s, nil
}

func appendWhereCondition(clause, condition string) string {
	if clause == "" {
		return " WHERE " + condition
	}
	return clause + " AND " + condition
}

func appendArgs(args []any, extra ...any) []any {
	out := make([]any, 0, len(args)+len(extra))
	out = append(out, args...)
	return append(out, extra...)
}

// VersionsForMembershipAt is the only client-specific part of the stats response, split out so the
// caller-supplied client filter cannot force the whole aggregate to be recomputed per value. It
// must apply the same membership filter as StatsForMembershipAt or the breakdown would count a
// population the aggregate excluded.
func (e *Engine) VersionsForMembershipAt(ctx context.Context, network, client, membership string, at time.Time) (map[string]int, time.Time, error) {
	e.publishMu.RLock()
	defer e.publishMu.RUnlock()
	generation := e.LastRefresh()
	out := map[string]int{}
	f := Filter{Network: network, Client: client, ClientExact: true, ForkStatus: "current", ForkAt: at, Membership: membership}
	if err := f.validate(); err != nil {
		return nil, generation, err
	}
	clause, args, err := f.where(e.networks)
	if err != nil {
		return nil, generation, err
	}
	chartCond, chartCutoff := chartFingerprintConditionAt(at)
	clause = appendWhereCondition(clause, chartCond)
	q := fmt.Sprintf("SELECT %s, count(*) FROM nodes%s GROUP BY 1", normalizedClientVersionSQL, clause)
	rows, err := e.db.QueryContext(ctx, q, appendArgs(args, chartCutoff)...)
	if err != nil {
		return nil, generation, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var count int
		if err := rows.Scan(&key, &count); err != nil {
			return nil, generation, err
		}
		out[key] = count
	}
	return out, generation, rows.Err()
}

// ENR client metadata commonly reports semver without a leading "v", while
// libp2p Identify agent strings from the same client include it. Keep the raw
// value on each node, but fold the two spellings together in version charts and
// collapse missing or placeholder build metadata into one "Unknown" bucket.
// The suffix collapse also groups commit and prerelease decorations.
const normalizedClientVersionSQL = `CASE
	WHEN client_version IS NULL
		OR trim(client_version) = ''
		OR lower(trim(client_version)) = lower(trim(client))
		OR lower(trim(client_version)) IN ('unknown', '<unknown>', 'v<unknown>', 'null', 'nil', 'n/a', 'na', '?', '""', '''')
		OR regexp_matches(lower(trim(client_version)), '^v?unknown($|[-+])')
	THEN 'Unknown'
	WHEN regexp_matches(split_part(trim(client_version), '-', 1), '^[vV][0-9]')
	THEN substr(split_part(trim(client_version), '-', 1), 2)
	ELSE split_part(trim(client_version), '-', 1)
END`

// MaxMapPoints bounds one map response. The non-compact encoding allocates a nested map per
// feature, and the api caches a body per network/format/membership variant, so an unbounded
// population (notably --max-nodes=0) could otherwise exhaust api memory.
const MaxMapPoints = 50_000

// MapPointsForMembershipAt returns at most MaxMapPoints geolocated points plus the full matching
// count, so a caller can disclose that it is rendering a subset rather than silently under-reporting.
func (e *Engine) MapPointsForMembershipAt(ctx context.Context, network, membership string, at time.Time) ([]Point, int, error) {
	e.publishMu.RLock()
	defer e.publishMu.RUnlock()
	f := Filter{Network: network, ForkStatus: "current", ForkAt: at, Membership: membership}
	if err := f.validate(); err != nil {
		return nil, 0, err
	}
	clause, args, err := f.where(e.networks)
	if err != nil {
		return nil, 0, err
	}
	// Unverified ENR-declared client names render as unknown, matching the identified-only methodology.
	chartCond, chartCutoff := chartFingerprintConditionAt(at)
	args = append([]any{chartCutoff}, args...)
	base := "SELECT id, lat, lon, CASE WHEN " + chartCond + " THEN client ELSE '' END AS client, network, country, city, coalesce(subdivision, ''), layer, hosting, (ip6 <> '') AS ipv6, (membership_source = 'status') AS verified, coalesce(geo_accuracy_radius_km, 0) AS accuracy_km, CASE WHEN coalesce(cgc_known, false) THEN coalesce(cgc, 0) ELSE 0 END AS cgc FROM nodes"
	geo := "geolocated"
	if clause == "" {
		clause = " WHERE " + geo
	} else {
		clause += " AND " + geo
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	total := 0
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM nodes"+clause, args[1:]...).Scan(&total); err != nil {
		return nil, 0, err
	}
	// Ordered so a truncated response is a stable subset rather than whatever the scan yielded first.
	rows, err := tx.QueryContext(ctx, base+clause+" ORDER BY id LIMIT ?", append(args, MaxMapPoints)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	pts := make([]Point, 0, min(total, MaxMapPoints))
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.ID, &p.Lat, &p.Lon, &p.Client, &p.Network, &p.Country, &p.City, &p.Subdivision, &p.Layer, &p.Hosting, &p.IPv6, &p.Verified, &p.AccuracyKM, &p.CGC); err != nil {
			return nil, 0, err
		}
		pts = append(pts, p)
	}
	return pts, total, rows.Err()
}

type queryContext interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func scanCounts(ctx context.Context, db queryContext, q string, args []any, dst map[string]int) error {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k sql.NullString
		var c int
		if err := rows.Scan(&k, &c); err != nil {
			return err
		}
		if k.Valid && k.String != "" {
			dst[k.String] = c
		}
	}
	return rows.Err()
}

func collapseUnrecognizedClients(m map[string]int) {
	var other int
	for name, c := range m {
		if !clientname.Recognized(name) {
			other += c
			delete(m, name)
		}
	}
	if other > 0 {
		m[clientname.Other] += other
	}
}

func scanNodes(rows *sql.Rows, at time.Time) ([]Node, error) {
	out := []Node{}
	for rows.Next() {
		var n Node
		if err := rows.Scan(
			&n.ID, &n.Enode, &n.ENR, &n.Seq, &n.IP, &n.IP6, &n.TCP, &n.UDP, &n.TCP6, &n.UDP6, &n.QUIC, &n.QUIC6, &n.Network, &n.ForkHash, &n.ForkNext, &n.Layer,
			&n.CGC, &n.CGCKnown,
			&n.HasV4, &n.HasV5, &n.Score, &n.FirstSeen, &n.LastSeen, &n.LastCheck,
			&n.LastResolved, &n.Client, &n.ClientVersion, &n.OS, &n.Lang, &n.Capabilities, &n.Country, &n.City, &n.Subdivision, &n.Lat, &n.Lon, &n.ASN, &n.Org, &n.Hosting, &n.HostingKnown, &n.FPStatus, &n.FingerprintAt,
			&n.Geolocated, &n.GeoAccuracyRadiusKM, &n.MembershipSource, &n.MembershipVerifiedAt, &n.ForkSource, &n.ForkObservedAt,
			&n.FPDirection, &n.Pinned, &n.Dialable,
		); err != nil {
			return nil, err
		}
		n.ForkCompatible = netconf.RowForkCurrentAt(n.Layer, n.Network, n.ForkHash, n.ForkNext, at)
		out = append(out, n)
	}
	return out, rows.Err()
}
