package query

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
	"github.com/MysticRyuujin/enrscout/internal/snapshot"
	"github.com/MysticRyuujin/enrscout/internal/store"
)

type countingStore struct {
	store.Store
	mu   sync.Mutex
	gets int
}

func (s *countingStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	return s.Store.Get(ctx, key)
}

func (s *countingStore) getCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

func testManifest(gen time.Time, crawlerID string, networks map[string]snapshot.NetworkSnapshot) *snapshot.Manifest {
	return &snapshot.Manifest{
		SchemaVersion: snapshot.SchemaVersion, GeneratedAt: gen, CrawlerID: crawlerID,
		Run: snapshot.RunMetadata{
			RunID: "query-test-run", SourceRevision: "query-test-revision", SourceURL: "https://example.com/source",
			ConfigSHA256: strings.Repeat("00", sha256.Size), CrawlerStartedAt: gen.Add(-time.Minute),
			MethodologyStartedAt: gen.Add(-time.Minute), MethodologyVersion: snapshot.MethodologyVersion,
			MethodologyID: "query-test-method",
		},
		Networks: networks,
	}
}

func TestFilterWhere(t *testing.T) {
	clause, args, err := Filter{}.where(nil)
	if err != nil {
		t.Fatal(err)
	}
	if clause != "" || len(args) != 0 {
		t.Errorf("empty filter should yield no clause, got %q / %v", clause, args)
	}

	clause, args, err = Filter{Network: "mainnet", Client: "eth", Country: "u", Protocol: "v5"}.where(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(clause, " WHERE ") {
		t.Errorf("expected WHERE prefix, got %q", clause)
	}
	if !strings.Contains(clause, "network = ?") || !strings.Contains(clause, "lower(client) LIKE lower(?)") ||
		!strings.Contains(clause, "lower(country) LIKE lower(?)") || !strings.Contains(clause, "has_v5") {
		t.Errorf("missing conditions in %q", clause)
	}
	if len(args) != 3 || args[1] != "%eth%" || args[2] != "%u%" {
		t.Errorf("unexpected bound args: %v", args)
	}

	clause, args, err = Filter{Client: "Geth", ClientExact: true}.where(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(clause, "lower(client) = lower(?)") || len(args) != 1 || args[0] != "Geth" {
		t.Errorf("exact client filter = %q / %v", clause, args)
	}

	clause, args, err = Filter{CGCMin: "8", CGCMax: "128"}.where(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(clause, "coalesce(cgc_known, false) AND coalesce(cgc, 0) >= ?") ||
		!strings.Contains(clause, "coalesce(cgc_known, false) AND coalesce(cgc, 0) <= ?") {
		t.Errorf("missing cgc conditions in %q", clause)
	}
	if len(args) != 2 || args[0] != uint32(8) || args[1] != uint32(128) {
		t.Errorf("unexpected cgc bound args: %v", args)
	}
}

func TestFilterEscapesLikeMetacharacters(t *testing.T) {
	clause, args, err := Filter{Q: "a_b%$"}.where(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(clause, "ESCAPE '$'") {
		t.Fatalf("LIKE clause has no explicit escape: %q", clause)
	}
	for i, got := range args {
		if want := "%a$_b$%$$%"; got != want {
			t.Fatalf("escaped search arg %d = %q, want %q", i, got, want)
		}
	}
}

func TestSortExpression(t *testing.T) {
	tests := []struct {
		name, order, want string
	}{
		{"", "", "score DESC, last_seen DESC, id DESC"},
		{"client", "", "lower(client) ASC, lower(client_version) ASC, id ASC"},
		{"first_seen", "asc", "first_seen ASC, id ASC"},
		{"last_seen", "desc", "last_seen DESC, score DESC, id DESC"},
		{"cgc", "", "coalesce(cgc, 0) DESC, score DESC, id DESC"},
	}
	for _, tt := range tests {
		if got := sortExpression(tt.name, tt.order); got != tt.want {
			t.Errorf("sortExpression(%q, %q) = %q, want %q", tt.name, tt.order, got, tt.want)
		}
	}
}

func TestNodeProjectionAndScanContract(t *testing.T) {
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(st, []string{"mainnet"}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	_, err = eng.db.Exec(`INSERT INTO nodes (
		id,enode,enr,seq,ip,ip6,tcp,udp,tcp6,udp6,quic,quic6,network,fork_hash,fork_next,layer,cgc,cgc_known,
		has_v4,has_v5,score,first_seen,last_seen,last_check,client,client_version,os,lang,capabilities,
		country,city,subdivision,lat,lon,asn,org,hosting,fp_status,fp_at,geolocated,membership_source,dialable)
		VALUES ('id-a','enode-a','enr-a',11,'1.1.1.1','2606:4700:4700::1111',21,22,23,24,25,26,
		'mainnet','aabbccdd',31,'el',128,true,true,false,41,51,52,53,'ClientA','VersionB','OSA','LangB','CapsC',
		'US','CityB','WI',61.5,62.5,63,'OrgC',true,'failed',71,true,'enr',false)`)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := eng.db.Query("SELECT " + columns + " FROM nodes")
	if err != nil {
		t.Fatal(err)
	}
	got, err := scanNodes(rows, time.Now())
	rows.Close()
	if err != nil || len(got) != 1 {
		t.Fatalf("scanNodes = %+v, %v", got, err)
	}
	n := got[0]
	if n.Enode != "enode-a" || n.ENR != "enr-a" || n.Seq != 11 || n.IP != "1.1.1.1" ||
		n.TCP != 21 || n.UDP != 22 || n.TCP6 != 23 || n.UDP6 != 24 || n.QUIC != 25 || n.QUIC6 != 26 ||
		n.ForkNext != 31 || !n.HasV4 || n.HasV5 || n.FirstSeen != 51 || n.LastSeen != 52 || n.LastCheck != 53 ||
		n.Client != "ClientA" || n.ClientVersion != "VersionB" || n.OS != "OSA" || n.Lang != "LangB" ||
		n.Country != "US" || n.City != "CityB" || n.Subdivision != "WI" || n.Lat != 61.5 || n.Lon != 62.5 || n.ASN != 63 ||
		n.Org != "OrgC" || !n.Hosting || n.FPStatus != "failed" || n.FingerprintAt != 71 || n.Dialable || n.ForkCompatible ||
		n.CGC != 128 || !n.CGCKnown {
		t.Fatalf("projection/scan fields were misassigned: %+v", n)
	}

	supernodes, err := eng.Nodes(context.Background(), Filter{CGCMin: "128", ForkStatus: "all", Limit: 10})
	if err != nil || supernodes.Total != 1 {
		t.Fatalf("cgc_min filter = %+v, %v", supernodes, err)
	}
	none, err := eng.Nodes(context.Background(), Filter{CGCMax: "8", ForkStatus: "all", Limit: 10})
	if err != nil || none.Total != 0 {
		t.Fatalf("cgc_max filter = %+v, %v", none, err)
	}

	res, err := eng.Nodes(context.Background(), Filter{Client: "ienta", Country: "u", IP: "1.1", ForkStatus: "all", Limit: 10})
	if err != nil || res.Total != 1 || len(res.Nodes) != 1 || res.Nodes[0].ID != "id-a" {
		t.Fatalf("partial filters = %+v, %v", res, err)
	}
	ctx := context.Background()
	stale, err := eng.Nodes(ctx, Filter{Network: "mainnet", ForkStatus: "stale", Limit: 10})
	if err != nil || stale.Total != 1 || len(stale.Nodes) != 1 {
		t.Fatalf("stale fork filter = %+v, %v", stale, err)
	}
	current, err := eng.Nodes(ctx, Filter{Network: "mainnet", ForkStatus: "current", Limit: 10})
	if err != nil || current.Total != 0 {
		t.Fatalf("current fork filter = %+v, %v", current, err)
	}
	stats, err := eng.StatsForMembershipAt(ctx, "mainnet", "", time.Now())
	if err != nil || stats.Total != 0 || stats.ExecutionStale != 1 {
		t.Fatalf("stale fork stats = %+v, %v", stats, err)
	}
	points, _, err := eng.MapPointsForMembershipAt(ctx, "mainnet", "", time.Now())
	if err != nil || len(points) != 0 {
		t.Fatalf("stale fork map points = %+v, %v", points, err)
	}

	nw, _ := netconf.Get("mainnet")
	currentID := nw.CurrentForkID()
	if _, err := eng.db.Exec("UPDATE nodes SET fork_hash = ?, fork_next = ?", hex.EncodeToString(currentID.Hash[:]), currentID.Next); err != nil {
		t.Fatal(err)
	}
	current, err = eng.Nodes(ctx, Filter{Network: "mainnet", ForkStatus: "current", Limit: 10})
	if err != nil || current.Total != 1 || !current.Nodes[0].ForkCompatible {
		t.Fatalf("updated current fork filter = %+v, %v", current, err)
	}
	points, _, err = eng.MapPointsForMembershipAt(ctx, "mainnet", "", time.Now())
	if err != nil || len(points) != 1 || points[0].Subdivision != "WI" {
		t.Fatalf("current map points = %+v, %v", points, err)
	}
	if _, err := eng.db.Exec(`INSERT INTO nodes (network, layer) VALUES ('mainnet', '')`); err != nil {
		t.Fatal(err)
	}
	stats, err = eng.StatsForMembershipAt(ctx, "mainnet", "", time.Now())
	if err != nil || stats.Total != stats.Execution+stats.Consensus {
		t.Fatalf("unclassified layer leaked into current stats: %+v, %v", stats, err)
	}
}

func TestMigrateStagingSchemaAddsSubdivision(t *testing.T) {
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(st, []string{"mainnet"}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if _, err := eng.db.Exec("CREATE TABLE nodes_staging (id VARCHAR); INSERT INTO nodes_staging VALUES ('legacy')"); err != nil {
		t.Fatal(err)
	}
	if err := migrateStagingSchema(context.Background(), eng.db); err != nil {
		t.Fatal(err)
	}
	// The migration is intentionally idempotent because every refresh runs it.
	if err := migrateStagingSchema(context.Background(), eng.db); err != nil {
		t.Fatal(err)
	}
	var subdivision string
	if err := eng.db.QueryRow("SELECT subdivision FROM nodes_staging WHERE id = 'legacy'").Scan(&subdivision); err != nil {
		t.Fatal(err)
	}
	if subdivision != "" {
		t.Fatalf("subdivision = %q, want empty default", subdivision)
	}
	var cgc uint32
	var cgcKnown bool
	if err := eng.db.QueryRow("SELECT cgc, cgc_known FROM nodes_staging WHERE id = 'legacy'").Scan(&cgc, &cgcKnown); err != nil {
		t.Fatal(err)
	}
	if cgc != 0 || cgcKnown {
		t.Fatalf("cgc defaults = (%d, %v), want (0, false)", cgc, cgcKnown)
	}
}

func TestEngineConcurrentRefreshAndQueries(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	set := nodeset.NewWithLimit(0)
	set.Observe(mainnetNode(t), "v5", time.Now())
	data, err := set.ParquetForNetwork("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	layout := snapshot.Layout{}
	gen := time.Now().UTC()
	key := layout.GenerationKey("mainnet", gen)
	if err := st.Put(ctx, key, data, "application/vnd.apache.parquet"); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if err := snapshot.Write(ctx, st, layout, testManifest(gen, "concurrency-test", map[string]snapshot.NetworkSnapshot{
		"mainnet": {GenerationKey: key, NodeCount: 1, Bytes: len(data), SHA256: hex.EncodeToString(sum[:])},
	})); err != nil {
		t.Fatal(err)
	}
	eng, err := New(st, []string{"mainnet"}, "", "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if err := eng.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 1024)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				if _, err := eng.Nodes(ctx, Filter{Network: "mainnet"}); err != nil {
					errCh <- err
				}
				s, err := eng.StatsForMembershipAt(ctx, "mainnet", "", time.Now())
				if err != nil {
					errCh <- err
				} else if s.Total != s.Execution || s.Total != s.ByNetwork["mainnet"] {
					errCh <- fmt.Errorf("stats mixed generations: total=%d execution=%d by_network=%d", s.Total, s.Execution, s.ByNetwork["mainnet"])
				}
			}
		}()
	}
	baseGeneration := time.Now().UTC()
	for i := range 10 {
		next := nodeset.NewWithLimit(0)
		next.Observe(mainnetNode(t), "v5", time.Now())
		count := 1
		if i%2 == 0 {
			next.Observe(mainnetNodeNoTCP(t), "v5", time.Now())
			count = 2
		}
		publishTestSnapshot(t, ctx, st, next, count, baseGeneration.Add(time.Duration(i+1)*time.Nanosecond))
		if err := eng.Refresh(ctx); err != nil {
			errCh <- err
		}
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent operation failed: %v", err)
	}
}

type blockingGenerationStore struct {
	store.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingGenerationStore) Get(ctx context.Context, key string) ([]byte, error) {
	if strings.HasSuffix(key, ".parquet") {
		s.once.Do(func() { close(s.entered) })
		<-s.release
	}
	return s.Store.Get(ctx, key)
}

// A refresh spends nearly all of its time on object-store reads and the staging load, so readers
// must only block for the table swap itself.
func TestMapPointsReportsTheFullCountWhenTruncated(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(st, []string{"mainnet"}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	nw, _ := netconf.Get("mainnet")
	current := nw.CurrentForkID()
	if _, err := eng.db.Exec(`INSERT INTO nodes (id, network, layer, fork_hash, fork_next, lat, lon, geolocated, membership_source, dialable, client, country, city, subdivision, ip6, hosting)
		SELECT format('{:064d}', i), 'mainnet', 'el', ?, ?, 1.0, 2.0, true, 'enr', true, '', 'US', 'Chicago', 'IL', '', false FROM range(0, ?) t(i)`,
		hex.EncodeToString(current.Hash[:]), current.Next, MaxMapPoints+10); err != nil {
		t.Fatal(err)
	}
	pts, total, err := eng.MapPointsForMembershipAt(ctx, "mainnet", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != MaxMapPoints {
		t.Fatalf("returned %d points, want the %d cap", len(pts), MaxMapPoints)
	}
	if total != MaxMapPoints+10 {
		t.Fatalf("total = %d, want the full matching count %d", total, MaxMapPoints+10)
	}
	// Truncation must be a stable prefix, not whatever the scan happened to yield first.
	if pts[0].ID >= pts[len(pts)-1].ID {
		t.Fatalf("points are not id-ordered: %q .. %q", pts[0].ID, pts[len(pts)-1].ID)
	}
}

func TestStatsIsNotBlockedByAnInFlightRefreshRead(t *testing.T) {
	ctx := context.Background()
	base, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	set := nodeset.NewWithLimit(0)
	set.Observe(mainnetNode(t), "v5", time.Now())
	publishTestSnapshot(t, ctx, base, set, 1, time.Now().UTC())

	blocking := &blockingGenerationStore{Store: base, entered: make(chan struct{}), release: make(chan struct{})}
	eng, err := New(blocking, []string{"mainnet"}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	refreshed := make(chan error, 1)
	go func() { refreshed <- eng.Refresh(ctx) }()
	<-blocking.entered

	served := make(chan error, 1)
	go func() {
		_, err := eng.StatsForMembershipAt(ctx, "mainnet", "", time.Now())
		served <- err
	}()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("stats during a refresh read: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("stats blocked on a refresh still reading generations; the publication barrier is too wide")
	}

	close(blocking.release)
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
}

func TestRefreshSkipsUnchangedGeneration(t *testing.T) {
	ctx := context.Background()
	base, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	set := nodeset.NewWithLimit(0)
	set.Observe(mainnetNode(t), "v5", time.Now())
	publishTestSnapshot(t, ctx, base, set, 1, time.Now().UTC())

	st := &countingStore{Store: base}
	eng, err := New(st, []string{"mainnet"}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if err := eng.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	firstGets := st.getCount()
	if err := eng.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := st.getCount() - firstGets; got != 1 {
		t.Fatalf("unchanged refresh performed %d reads, want only the manifest read", got)
	}
}

func TestFailedRefreshPreservesLiveTable(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	set := nodeset.NewWithLimit(0)
	set.Observe(mainnetNode(t), "v5", time.Now())
	publishTestSnapshot(t, ctx, st, set, 1, time.Now().UTC())

	eng, err := New(st, []string{"mainnet"}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if err := eng.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	publishTestSnapshot(t, ctx, st, set, 2, time.Now().UTC().Add(time.Nanosecond))
	if err := eng.Refresh(ctx); err == nil {
		t.Fatal("refresh with a manifest/parquet count mismatch succeeded")
	}
	res, err := eng.Nodes(ctx, Filter{Network: "mainnet"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 {
		t.Fatalf("failed refresh replaced live table: total=%d, want 1", res.Total)
	}
}

func publishTestSnapshot(t *testing.T, ctx context.Context, st store.Store, set *nodeset.Set, count int, gen time.Time) {
	t.Helper()
	data, err := set.ParquetForNetwork("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	layout := snapshot.Layout{}
	key := layout.GenerationKey("mainnet", gen)
	if err := st.Put(ctx, key, data, "application/vnd.apache.parquet"); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if err := snapshot.Write(ctx, st, layout, testManifest(gen, "query-test", map[string]snapshot.NetworkSnapshot{
		"mainnet": {
			GenerationKey: key,
			NodeCount:     count,
			Bytes:         len(data),
			SHA256:        hex.EncodeToString(sum[:]),
		},
	})); err != nil {
		t.Fatal(err)
	}
}

func mainnetNode(t *testing.T) *enode.Node {
	t.Helper()
	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(enr.IPv4{9, 9, 9, 9})
	r.Set(enr.TCP(30303))
	r.Set(netconf.EthEntry{ForkID: nw.CurrentForkID()})
	var id enode.ID
	id[0] = 42
	return enode.SignNull(&r, id)
}

func mainnetNodeNoTCP(t *testing.T) *enode.Node {
	t.Helper()
	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(enr.IPv4{8, 8, 8, 8})
	r.Set(netconf.EthEntry{ForkID: nw.CurrentForkID()})
	var id enode.ID
	id[0] = 43
	return enode.SignNull(&r, id)
}

func TestTCPlessNodesIncluded(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	set := nodeset.NewWithLimit(0)
	set.Observe(mainnetNode(t), "v5", time.Now())
	set.Observe(mainnetNodeNoTCP(t), "v5", time.Now())
	data, err := set.ParquetForNetwork("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	layout := snapshot.Layout{}
	gen := time.Now()
	key := layout.GenerationKey("mainnet", gen)
	if err := st.Put(ctx, key, data, ""); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if err := snapshot.Write(ctx, st, layout, testManifest(gen, "test-crawler", map[string]snapshot.NetworkSnapshot{
		"mainnet": {GenerationKey: key, NodeCount: 2, Bytes: len(data), SHA256: hex.EncodeToString(sum[:])},
	})); err != nil {
		t.Fatal(err)
	}

	eng, err := New(st, []string{"mainnet"}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if err := eng.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	all, err := eng.Nodes(ctx, Filter{Network: "mainnet"})
	if err != nil {
		t.Fatal(err)
	}
	if all.Total != 2 {
		t.Fatalf("expected 2 nodes including TCP-less, got %d", all.Total)
	}

	yes, _ := eng.Nodes(ctx, Filter{Network: "mainnet", Dialable: "yes"})
	if yes.Total != 1 {
		t.Errorf("dialable=yes total = %d, want 1", yes.Total)
	}
	no, _ := eng.Nodes(ctx, Filter{Network: "mainnet", Dialable: "no"})
	if no.Total != 1 || len(no.Nodes) != 1 || no.Nodes[0].Dialable {
		t.Errorf("dialable=no wrong: total=%d nodes=%+v", no.Total, no.Nodes)
	}

	s, err := eng.StatsForMembershipAt(ctx, "mainnet", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.Dialable != 1 {
		t.Errorf("stats dialable = %d, want 1", s.Dialable)
	}
}

func TestEngineServesEmptyBeforeFirstSnapshot(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(st, []string{"mainnet"}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	if res, err := eng.Nodes(ctx, Filter{Network: "mainnet"}); err != nil || res.Total != 0 {
		t.Errorf("Nodes before snapshot: err=%v total=%d, want empty/no-error", err, res.Total)
	}
	if s, err := eng.StatsForMembershipAt(ctx, "mainnet", "", time.Now()); err != nil || s.Total != 0 {
		t.Errorf("Stats before snapshot: err=%v total=%d", err, s.Total)
	}
	if _, _, err := eng.MapPointsForMembershipAt(ctx, "mainnet", "", time.Now()); err != nil {
		t.Errorf("MapPoints before snapshot: err=%v", err)
	}
}

func TestEngineRejectsUnimplementedFilterValues(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(st, []string{"mainnet"}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	cases := []struct {
		name string
		f    Filter
	}{
		{"layer", Filter{Layer: "bogus"}},
		{"protocol", Filter{Protocol: "bogus"}},
		{"ipstack", Filter{IPStack: "bogus"}},
		{"hosting", Filter{Hosting: "bogus"}},
		{"dialable", Filter{Dialable: "bogus"}},
		{"fork status", Filter{ForkStatus: "bogus"}},
		{"membership", Filter{Membership: "bogus"}},
		{"cgc_min", Filter{CGCMin: "bogus"}},
		{"cgc_min negative", Filter{CGCMin: "-1"}},
		{"cgc_max", Filter{CGCMax: "99999999999"}},
	}
	// A new enum in filterEnums without a case here would ship unguarded.
	if len(cases) != len(filterEnums)+3 {
		t.Fatalf("filterEnums has %d entries, test covers %d non-cgc cases", len(filterEnums), len(cases)-3)
	}
	for _, tc := range cases {
		tc.f.Network = "mainnet"
		if _, err := eng.Nodes(ctx, tc.f); err == nil {
			t.Errorf("%s: unimplemented value accepted, want rejection not unfiltered rows", tc.name)
		}
	}
	// Every entry point that reaches where() must reject, not just Nodes: an unfiltered
	// aggregate silently overstates the published counts.
	at := time.Now()
	if _, _, err := eng.MapPointsForMembershipAt(ctx, "mainnet", "bogus", at); err == nil {
		t.Error("MapPoints accepted an unimplemented membership")
	}
	if _, err := eng.StatsForMembershipAt(ctx, "mainnet", "bogus", at); err == nil {
		t.Error("Stats accepted an unimplemented membership")
	}
	if _, _, err := eng.VersionsForMembershipAt(ctx, "mainnet", "Geth", "bogus", at); err == nil {
		t.Error("Versions accepted an unimplemented membership")
	}
}

// Pins a known, deliberately unfixed divergence: netconf tolerates "0x" and upper case
// while the SQL only lowercases. Safe because no writer emits those forms
// (TestRowForkHashIsCanonicalHex in internal/nodeset), but it means non-canonical hashes
// are outside the domain TestForkCurrencyMatchesSQLAndGo can assert equivalence over.
func TestForkCurrencyNormalizationDivergence(t *testing.T) {
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(st, []string{"mainnet"}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	at := time.Now().UTC()
	mainnet, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	current := mainnet.CurrentForkIDAt(at)
	canonical := hex.EncodeToString(current.Hash[:])

	for _, form := range []string{"0x" + canonical, strings.ToUpper(canonical), " " + canonical} {
		if !netconf.RowForkCurrentAt("el", "mainnet", form, current.Next, at) {
			t.Errorf("netconf rejected %q; it normalizes 0x/case/space", form)
		}
	}
	insertForkCorpus(t, eng, []forkCorpusRow{
		{id: fmt.Sprintf("%064d", 1), network: "mainnet", layer: "el", hash: "0x" + canonical, next: "0"},
		{id: fmt.Sprintf("%064d", 2), network: "mainnet", layer: "el", hash: strings.ToUpper(canonical), next: "0"},
	})
	sqlCurrent := forkStatusIDs(t, eng, "current", at)
	if sqlCurrent[fmt.Sprintf("%064d", 1)] {
		t.Error("SQL accepted a 0x-prefixed hash; it only lowercases, so this would mean the divergence closed")
	}
	if !sqlCurrent[fmt.Sprintf("%064d", 2)] {
		t.Error("SQL rejected an uppercase hash; lower(fork_hash) should accept it")
	}
}

// A broken fork configuration must fail the views that depend on it without taking down
// the audit view or requests scoped to a healthy network. An unresolvable network name
// stands in for the breakage: it is one of the two error paths the condition propagates.
func TestForkConditionFailureIsScoped(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(st, []string{"mainnet", "not-a-network"}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	if _, err := eng.Nodes(ctx, Filter{ForkStatus: "current", Limit: 10}); err == nil {
		t.Error("unscoped fork=current accepted a network whose fork config cannot be resolved")
	}
	if _, err := eng.Nodes(ctx, Filter{ForkStatus: "all", Limit: 10}); err != nil {
		t.Errorf("fork=all failed on broken fork config (%v); the audit view must survive it", err)
	}
	for _, status := range []string{"current", "stale", "all"} {
		if _, err := eng.Nodes(ctx, Filter{Network: "mainnet", ForkStatus: status, Limit: 10}); err != nil {
			t.Errorf("fork=%s scoped to healthy mainnet failed: %v", status, err)
		}
	}
	if _, err := eng.StatsForMembershipAt(ctx, "mainnet", "", time.Now()); err != nil {
		t.Errorf("stats scoped to healthy mainnet failed: %v", err)
	}
	if _, _, err := eng.MapPointsForMembershipAt(ctx, "mainnet", "", time.Now()); err != nil {
		t.Errorf("map points scoped to healthy mainnet failed: %v", err)
	}
	// An unserved network matched nothing before; it must not start erroring.
	if res, err := eng.Nodes(ctx, Filter{Network: "sepolia", ForkStatus: "current", Limit: 10}); err != nil || res.Total != 0 {
		t.Errorf("unserved network = %+v, %v; want an empty result and no error", res, err)
	}
}

type forkCorpusRow struct {
	id      string
	network string
	layer   any
	hash    any
	next    any
	label   string
}

// goCurrent is the Go verdict for this row, reading NULL as the zero value the SQL
// side coalesces it to.
func (r forkCorpusRow) goCurrent(at time.Time) bool {
	layer, _ := r.layer.(string)
	hash, _ := r.hash.(string)
	var next uint64
	if s, ok := r.next.(string); ok {
		next, _ = strconv.ParseUint(s, 10, 64)
	}
	return netconf.RowForkCurrentAt(layer, r.network, hash, next, at)
}

// forkCorpus spans the boundaries of the EIP-2124 persisted-row rule. Hashes are
// restricted to the canonical form every writer emits (8 lowercase hex digits) plus
// values both sides reject identically: netconf tolerates "0x" and upper case while
// the SQL only lowercases, so non-canonical forms are outside the equivalence domain
// and are pinned by TestForkCurrencyNormalizationDivergence instead.
func forkCorpus(t *testing.T, at time.Time, boundaries []time.Time) []forkCorpusRow {
	t.Helper()
	var out []forkCorpusRow
	add := func(network string, layer, hash, next any, label string) {
		out = append(out, forkCorpusRow{
			id:      fmt.Sprintf("%064d", len(out)),
			network: network, layer: layer, hash: hash, next: next, label: label,
		})
	}
	u64 := func(v uint64) any { return strconv.FormatUint(v, 10) }

	names := netconf.Names()
	for i, name := range names {
		nw, err := netconf.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		current := nw.CurrentForkIDAt(at)
		currentHash := hex.EncodeToString(current.Hash[:])
		unix := at.Unix()
		if unix < 0 {
			unix = 0
		}
		// Boundaries come from the accepted ranges themselves so a change to them cannot
		// leave this corpus testing the wrong edges.
		nexts := []uint64{0, 1, uint64(unix), math.MaxUint64}
		if unix > 0 {
			nexts = append(nexts, uint64(unix)-1)
		}
		for _, r := range netconf.CanonicalCurrentNextRanges(at) {
			if r[0] > 0 {
				nexts = append(nexts, r[0]-1)
			}
			nexts = append(nexts, r[0], r[1])
			if r[1] < math.MaxUint64 {
				nexts = append(nexts, r[1]+1)
			}
		}
		for _, next := range nexts {
			add(name, "el", currentHash, u64(next), "current hash, next="+strconv.FormatUint(next, 10))
		}
		// Earlier eras with their own exact canonical Next: EIP-2124 accepts these for
		// peering, but they are not current, so they belong here as negative cases.
		for _, b := range boundaries {
			id := nw.CurrentForkIDAt(b.Add(-time.Second))
			if id.Hash == current.Hash {
				continue
			}
			h := hex.EncodeToString(id.Hash[:])
			add(name, "el", h, u64(id.Next), "earlier era, exact next")
			if id.Next > 0 {
				add(name, "el", h, u64(id.Next-1), "earlier era, next-1")
			}
			if id.Next < math.MaxUint64 {
				add(name, "el", h, u64(id.Next+1), "earlier era, next+1")
			}
		}

		foreign, err := netconf.Get(names[(i+1)%len(names)])
		if err != nil {
			t.Fatal(err)
		}
		foreignID := foreign.CurrentForkIDAt(at)
		add(name, "el", hex.EncodeToString(foreignID.Hash[:]), u64(foreignID.Next), "foreign-network hash")

		add(name, "el", "deadbeef", u64(0), "unknown hash")
		add(name, "el", "", u64(0), "empty hash")
		add(name, "el", nil, u64(0), "NULL hash")
		add(name, "el", currentHash, nil, "NULL next")

		state, err := netconf.CLForkStateAt(name, at)
		if err != nil {
			t.Fatal(err)
		}
		clDigest := hex.EncodeToString(state.Digest[:])
		// CL ignores fork_next entirely, and MaxUint64 is its common value: CLForkState
		// defaults NextForkEpoch to MaxUint64 when no fork is scheduled.
		for _, next := range []any{u64(0), u64(1), u64(math.MaxUint64), nil} {
			add(name, "cl", clDigest, next, "current CL digest")
			add(name, "cl", "ffffffff", next, "stale CL digest")
		}

		add(name, "unknown", currentHash, u64(0), "unknown layer")
		add(name, nil, currentHash, u64(0), "NULL layer")
	}
	return out
}

func insertForkCorpus(t *testing.T, eng *Engine, rows []forkCorpusRow) {
	t.Helper()
	if _, err := eng.db.Exec("DELETE FROM nodes"); err != nil {
		t.Fatal(err)
	}
	// fork_next binds as a string cast in SQL because database/sql rejects a uint64
	// with the high bit set, which MaxUint64 rows need.
	tuples := make([]string, 0, len(rows))
	args := make([]any, 0, len(rows)*5)
	for _, r := range rows {
		tuples = append(tuples, "(?, ?, ?, ?, CAST(? AS UBIGINT))")
		args = append(args, r.id, r.network, r.layer, r.hash, r.next)
	}
	q := "INSERT INTO nodes (id, network, layer, fork_hash, fork_next) VALUES " + strings.Join(tuples, ", ")
	if _, err := eng.db.Exec(q, args...); err != nil {
		t.Fatalf("insert corpus of %d rows: %v", len(rows), err)
	}
	// The projection in columns does not coalesce these, so scanNodes cannot read a row
	// that leaves them NULL.
	if _, err := eng.db.Exec(`UPDATE nodes SET enode='', enr='', seq=0, ip='', ip6='',
		tcp=0, udp=0, tcp6=0, udp6=0, quic=0, quic6=0, has_v4=false, has_v5=false, score=0,
		first_seen=0, last_seen=0, last_check=0, client='', client_version='', os='', lang='',
		capabilities='', country='', city='', lat=0, lon=0, asn=0, org='', hosting=false,
		fp_status='', dialable=false`); err != nil {
		t.Fatalf("fill scannable defaults: %v", err)
	}
}

// forkBoundaries walks every scheduled EL and CL transition after from. There is not
// always an upcoming one - all three networks are currently past their last scheduled
// fork - so boundary sampling has to use real transitions wherever they fall.
func forkBoundaries(t *testing.T, from time.Time) []time.Time {
	t.Helper()
	var out []time.Time
	at := from
	for range 64 {
		_, next, err := netconf.ForkEraTokenAt(at)
		if err != nil {
			t.Fatal(err)
		}
		if next.IsZero() {
			break
		}
		out = append(out, next)
		at = next
	}
	return out
}

func forkStatusIDs(t *testing.T, eng *Engine, status string, at time.Time) map[string]bool {
	t.Helper()
	clause, args, err := Filter{ForkStatus: status, ForkAt: at}.where(netconf.Names())
	if err != nil {
		t.Fatalf("%s condition: %v", status, err)
	}
	sqlRows, err := eng.db.Query("SELECT id FROM nodes"+clause, args...)
	if err != nil {
		t.Fatalf("%s query: %v", status, err)
	}
	defer sqlRows.Close()
	got := map[string]bool{}
	for sqlRows.Next() {
		var id string
		if err := sqlRows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got[id] = true
	}
	if err := sqlRows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// The SQL pushdown picks which rows a fork=current response returns while
// RowForkCurrentAt sets each returned row's fork_compatible: disagreement means a
// response contradicts itself, and nothing else would catch it.
func TestForkCurrencyMatchesSQLAndGo(t *testing.T) {
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(st, netconf.Names(), t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	now := time.Now().UTC()
	boundaries := forkBoundaries(t, now.AddDate(-3, 0, 0))
	if len(boundaries) == 0 {
		t.Fatal("no scheduled fork transitions in the last three years; boundary sampling would be vacuous")
	}
	ats := map[string]time.Time{
		"now":              now,
		"past era":         now.Add(-400 * 24 * time.Hour),
		"pre-epoch (neg.)": time.Unix(-1000, 0).UTC(),
	}
	for _, b := range boundaries {
		ats["before "+b.Format(time.RFC3339)] = b.Add(-time.Second)
		ats["at "+b.Format(time.RFC3339)] = b
		ats["after "+b.Format(time.RFC3339)] = b.Add(time.Second)
	}
	for name, at := range ats {
		t.Run(name, func(t *testing.T) {
			rows := forkCorpus(t, at, boundaries)
			insertForkCorpus(t, eng, rows)

			sqlCurrent := forkStatusIDs(t, eng, "current", at)
			for _, r := range rows {
				if want := r.goCurrent(at); sqlCurrent[r.id] != want {
					t.Errorf("%s (%s network=%s layer=%v hash=%v next=%v): SQL current=%v, RowForkCurrentAt=%v",
						r.id, r.label, r.network, r.layer, r.hash, r.next, sqlCurrent[r.id], want)
				}
			}

			// fork=current and fork=stale must partition the table: NOT <cond> is only
			// the complement while <cond> can never evaluate to NULL.
			sqlStale := forkStatusIDs(t, eng, "stale", at)
			if got := len(sqlCurrent) + len(sqlStale); got != len(rows) {
				t.Errorf("current(%d) + stale(%d) = %d, want %d; rows fell out of both views",
					len(sqlCurrent), len(sqlStale), got, len(rows))
			}

			// The NULL rows exist for the partition check above; Node models layer and
			// fork_hash as non-nullable strings and fork_next as a plain uint64, so they
			// are not scannable by construction.
			scanned, err := eng.db.Query("SELECT " + columns +
				" FROM nodes WHERE layer IS NOT NULL AND fork_hash IS NOT NULL AND fork_next IS NOT NULL")
			if err != nil {
				t.Fatal(err)
			}
			defer scanned.Close()
			nodes, err := scanNodes(scanned, at)
			if err != nil {
				t.Fatal(err)
			}
			for _, n := range nodes {
				if n.ForkCompatible != sqlCurrent[n.ID] {
					t.Errorf("%s: fork_compatible=%v but SQL current=%v", n.ID, n.ForkCompatible, sqlCurrent[n.ID])
				}
			}
		})
	}
}

func TestClientVersionsNormalizeSemverPrefix(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(st, []string{"mainnet"}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	fpAt := time.Now().Unix()
	_, err = eng.db.Exec(`INSERT INTO nodes (network, layer, client, client_version, fp_status, fp_at) VALUES
		('mainnet', 'cl', 'Lighthouse', '8.2.0', 'ok', ?),
		('mainnet', 'cl', 'Lighthouse', 'v8.2.0', 'ok', ?),
		('mainnet', 'cl', 'Lighthouse', 'V8.2.0-120c3c6', 'ok', ?),
		('mainnet', 'cl', 'Lighthouse', 'v8.2.0-beta.1', 'ok', ?),
		('mainnet', 'cl', 'Lighthouse', 'version8', 'ok', ?),
		('mainnet', 'cl', 'Lighthouse', '', 'ok', ?),
		('mainnet', 'cl', 'Lighthouse', '  ', 'ok', ?),
		('mainnet', 'cl', 'Lighthouse', 'Unknown', 'ok', ?),
		('mainnet', 'cl', 'Lighthouse', 'vUNKNOWN+g1848a74', 'ok', ?),
		('mainnet', 'cl', 'Lighthouse', 'lighthouse', 'ok', ?),
		('mainnet', 'cl', 'Lighthouse', 'v8.2.0', 'stale', ?),
		('mainnet', 'cl', 'Lighthouse', '""', 'ok', ?),
		('mainnet', 'cl', 'Lighthouse', NULL, 'ok', ?),
		('mainnet', 'cl', 'Lighthouse', 'v9.9.9', 'failed', ?)`,
		fpAt, fpAt, fpAt, fpAt, fpAt, fpAt, fpAt, fpAt, fpAt, fpAt, fpAt, fpAt, fpAt, fpAt)
	if err != nil {
		t.Fatal(err)
	}
	clState, err := netconf.CLForkStateAt("mainnet", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	currentCL := hex.EncodeToString(clState.Digest[:])
	if _, err := eng.db.Exec("UPDATE nodes SET fork_hash = ?", currentCL); err != nil {
		t.Fatal(err)
	}

	want := map[string]int{"8.2.0": 5, "version8": 1, "Unknown": 7}
	versions, _, err := eng.VersionsForMembershipAt(ctx, "mainnet", "Lighthouse", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(versions, want) {
		t.Fatalf("Versions = %v, want %v", versions, want)
	}

	// The version breakdown is served from its own cache entry, so it has to reapply the membership
	// filter itself; without it the chart would count a population the aggregate excluded.
	if _, err := eng.db.Exec("UPDATE nodes SET membership_source = 'enr'"); err != nil {
		t.Fatal(err)
	}
	verified, _, err := eng.VersionsForMembershipAt(ctx, "mainnet", "Lighthouse", "verified", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(verified) != 0 {
		t.Fatalf("membership=verified versions = %v, want none for an all-ENR-claimed population", verified)
	}
	claimed, _, err := eng.VersionsForMembershipAt(ctx, "mainnet", "Lighthouse", "claimed", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(claimed, want) {
		t.Fatalf("membership=claimed versions = %v, want %v", claimed, want)
	}
}

func TestStatsClientSharesOnlyIncludeSuccessfulFingerprints(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(st, []string{"mainnet"}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	mainnet, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	current := mainnet.CurrentForkID().Hash
	currentHash := hex.EncodeToString(current[:])
	fpAt := time.Now().Unix()
	oldFpAt := time.Now().Add(-8 * 24 * time.Hour).Unix()
	clState, err := netconf.CLForkStateAt("mainnet", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	currentCL := hex.EncodeToString(clState.Digest[:])
	_, err = eng.db.Exec(`INSERT INTO nodes (network, layer, fork_hash, client, fp_status, fp_at, fp_direction) VALUES
		('mainnet', 'el', ?, 'Geth', 'ok', ?, 'inbound'),
		('mainnet', 'el', ?, 'Reth', 'stale', ?, 'outbound'),
		('mainnet', 'el', ?, 'Geth', 'failed', 0, ''),
		('mainnet', 'el', ?, 'Nethermind', 'pending', 0, ''),
		('mainnet', 'el', ?, 'Besu', 'ok', ?, 'outbound'),
		('mainnet', 'cl', ?, 'Lighthouse', 'ok', ?, 'outbound'),
		('mainnet', 'cl', ?, 'Lighthouse', 'failed', 0, ''),
		('mainnet', 'cl', ?, '', 'ok', ?, '')`,
		currentHash, fpAt, currentHash, fpAt, currentHash, currentHash, currentHash, oldFpAt,
		currentCL, fpAt, currentCL, currentCL, fpAt)
	if err != nil {
		t.Fatal(err)
	}

	stats, err := eng.StatsForMembershipAt(ctx, "mainnet", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"Geth": 1, "Reth": 1, "Lighthouse": 1}; !maps.Equal(stats.ByClient, want) {
		t.Fatalf("ByClient = %v, want %v", stats.ByClient, want)
	}
	if want := map[string]int{"Geth": 1, "Reth": 1}; !maps.Equal(stats.ByClientEL, want) {
		t.Fatalf("ByClientEL = %v, want %v", stats.ByClientEL, want)
	}
	if want := map[string]int{"Lighthouse": 1}; !maps.Equal(stats.ByClientCL, want) {
		t.Fatalf("ByClientCL = %v, want %v", stats.ByClientCL, want)
	}
	if stats.ELIdentifiedStale != 1 {
		t.Fatalf("ELIdentifiedStale = %d, want 1 (aged-out Besu)", stats.ELIdentifiedStale)
	}
	if want := map[string]int{"inbound": 1, "outbound": 1}; !maps.Equal(stats.ByDirectionEL, want) {
		t.Fatalf("ByDirectionEL = %v, want %v", stats.ByDirectionEL, want)
	}
}

func TestDialableColumnMatchesRowPredicate(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mainnet, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	elFork := mainnet.CurrentForkID().Hash
	clState, err := netconf.CLForkStateAt("mainnet", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	el, cl := hex.EncodeToString(elFork[:]), hex.EncodeToString(clState.Digest[:])
	rows := []nodeset.Row{
		{ID: "0000000000000000000000000000000000000000000000000000000000000001", Network: "mainnet", Layer: "el", ForkHash: el, IP: "1.1.1.1", TCP: 30303},
		{ID: "0000000000000000000000000000000000000000000000000000000000000002", Network: "mainnet", Layer: "el", ForkHash: el, IP: "1.1.1.2", UDP: 30303},
		{ID: "0000000000000000000000000000000000000000000000000000000000000003", Network: "mainnet", Layer: "cl", ForkHash: cl, IP6: "2606:4700::1", TCP: 9000},
		{ID: "0000000000000000000000000000000000000000000000000000000000000004", Network: "mainnet", Layer: "cl", ForkHash: cl, IP6: "2606:4700::2", QUIC6: 9001},
		{ID: "0000000000000000000000000000000000000000000000000000000000000005", Network: "mainnet", Layer: "cl", ForkHash: cl, IP: "1.1.1.3", QUIC: 9001},
		{ID: "0000000000000000000000000000000000000000000000000000000000000006", Network: "mainnet", Layer: "cl", ForkHash: cl, IP6: "2606:4700::3", UDP6: 9000},
	}
	data, err := nodeset.ParquetFromRows(rows)
	if err != nil {
		t.Fatal(err)
	}
	layout := snapshot.Layout{}
	gen := time.Now()
	key := layout.GenerationKey("mainnet", gen)
	if err := st.Put(ctx, key, data, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	m := testManifest(gen, "test-crawler", map[string]snapshot.NetworkSnapshot{
		"mainnet": {GenerationKey: key, NodeCount: len(rows), Bytes: len(data), SHA256: hex.EncodeToString(sum[:])},
	})
	if err := snapshot.Write(ctx, st, layout, m); err != nil {
		t.Fatal(err)
	}
	eng, err := New(st, []string{"mainnet"}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if err := eng.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	res, err := eng.Nodes(ctx, Filter{Network: "mainnet", ForkStatus: "all", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Nodes) != len(rows) {
		t.Fatalf("nodes = %d, want %d", len(res.Nodes), len(rows))
	}
	byID := map[string]nodeset.Row{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	for _, n := range res.Nodes {
		if want := byID[n.ID].Dialable(); n.Dialable != want {
			t.Errorf("id %s: SQL dialable = %v, Row.Dialable = %v", n.ID, n.Dialable, want)
		}
	}

	// The ipv6_dialable stat reuses the dialable column rather than repeating the v6 predicate, so
	// it has to be pinned against Row.DialableV6 the same way.
	stats, err := eng.StatsForMembershipAt(ctx, "mainnet", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var wantV6, wantExplicit int
	for _, r := range rows {
		if r.DialableV6() {
			wantV6++
		}
		if r.IP6 != "" && (r.TCP6 != 0 || r.QUIC6 != 0) {
			wantExplicit++
		}
	}
	if stats.IPv6Dialable != wantV6 {
		t.Errorf("IPv6Dialable = %d, want %d", stats.IPv6Dialable, wantV6)
	}
	if stats.IPv6ExplicitPort != wantExplicit {
		t.Errorf("IPv6ExplicitPort = %d, want %d", stats.IPv6ExplicitPort, wantExplicit)
	}
	if wantV6 == 0 || wantExplicit == 0 || wantV6 == wantExplicit {
		t.Fatalf("fixture cannot tell the two counts apart: v6=%d explicit=%d", wantV6, wantExplicit)
	}
}

func TestEngineEndToEnd(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	set := nodeset.NewWithLimit(0)
	set.Observe(mainnetNode(t), "v5", time.Now())
	data, err := set.ParquetForNetwork("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	layout := snapshot.Layout{}
	gen := time.Now()
	key := layout.GenerationKey("mainnet", gen)
	if err := st.Put(ctx, key, data, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	m := testManifest(gen, "test-crawler", map[string]snapshot.NetworkSnapshot{
		"mainnet": {GenerationKey: key, NodeCount: 1, Bytes: len(data), SHA256: hex.EncodeToString(sum[:])},
	})
	if err := snapshot.Write(ctx, st, layout, m); err != nil {
		t.Fatal(err)
	}

	eng, err := New(st, []string{"mainnet"}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if err := eng.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	res, err := eng.Nodes(ctx, Filter{Network: "mainnet"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || len(res.Nodes) != 1 {
		t.Fatalf("nodes: total=%d count=%d", res.Total, len(res.Nodes))
	}
	if res.Nodes[0].IP != "9.9.9.9" {
		t.Errorf("ip = %q, want 9.9.9.9", res.Nodes[0].IP)
	}

	s, err := eng.StatsForMembershipAt(ctx, "mainnet", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.Total != 1 || s.Execution != 1 {
		t.Errorf("stats: total=%d execution=%d", s.Total, s.Execution)
	}

	n, err := eng.NodeByKey(ctx, "9.9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	if n == nil {
		t.Fatal("NodeByKey by IP returned nil")
	}

	n, err = eng.NodeByKey(ctx, res.Nodes[0].ID[:16])
	if err != nil {
		t.Fatal(err)
	}
	if n == nil || n.ID != res.Nodes[0].ID {
		t.Fatalf("NodeByKey by ID prefix = %#v, want %s", n, res.Nodes[0].ID)
	}

	for _, key := range []string{strings.ToUpper(res.Nodes[0].ID), strings.ToUpper(res.Nodes[0].ID[:16])} {
		n, err = eng.NodeByKey(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if n == nil || n.ID != res.Nodes[0].ID {
			t.Fatalf("NodeByKey(%q) = %#v, want uppercase key to match lowercase id", key, n)
		}
	}
}

func TestCollapseUnrecognizedClients(t *testing.T) {
	m := map[string]int{"Geth": 10, "Reth": 5, "OP-Geth": 3, "github.com": 1, "hermes": 42, "rust-libp2p": 72}
	collapseUnrecognizedClients(m)
	if m["Geth"] != 10 || m["Reth"] != 5 {
		t.Fatalf("recognized clients altered: %v", m)
	}
	if m["Other"] != 3+1+42+72 {
		t.Fatalf("Other = %d, want %d", m["Other"], 3+1+42+72)
	}
	for _, junk := range []string{"OP-Geth", "github.com", "hermes", "rust-libp2p"} {
		if _, ok := m[junk]; ok {
			t.Errorf("%q should have collapsed into Other", junk)
		}
	}
}
