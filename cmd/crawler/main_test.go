package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/p2p/netutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/MysticRyuujin/enrscout/internal/distinct"
	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
	"github.com/MysticRyuujin/enrscout/internal/snapshot"
	"github.com/MysticRyuujin/enrscout/internal/store"
)

func TestRecoverPeerCallbackIncrementsMetric(t *testing.T) {
	counter := mCallbackPanics.WithLabelValues("mainnet", layerEL)
	metric := &dto.Metric{}
	if err := counter.Write(metric); err != nil {
		t.Fatal(err)
	}
	before := metric.GetCounter().GetValue()
	returned := false
	func() {
		defer recoverPeerCallback("mainnet", layerEL)
		panic("crafted callback panic")
	}()
	returned = true
	if !returned {
		t.Fatal("callback panic escaped recovery boundary")
	}
	metric.Reset()
	if err := counter.Write(metric); err != nil {
		t.Fatal(err)
	}
	if got := metric.GetCounter().GetValue(); got != before+1 {
		t.Fatalf("panic counter = %v, want %v", got, before+1)
	}
}

func TestProcessLockRejectsConcurrentCrawlerAndAllowsRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".crawler.lock")
	first, err := acquireProcessLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := acquireProcessLock(path); err == nil {
		releaseProcessLock(second)
		t.Fatal("concurrent process lock was accepted")
	}
	releaseProcessLock(first)
	restarted, err := acquireProcessLock(path)
	if err != nil {
		t.Fatalf("restart could not reuse unlocked file: %v", err)
	}
	releaseProcessLock(restarted)
}

func TestProcessLockRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".crawler.lock")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if lock, err := acquireProcessLock(path); err == nil {
		releaseProcessLock(lock)
		t.Fatal("symlink process lock was accepted")
	}
}

func TestInitializeDiscoveryMetricsCreatesZeroWalkerSeries(t *testing.T) {
	specs, err := identitySpecs([]string{"mainnet"}, 30303, 2, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	initializeDiscoveryMetrics(specs, []string{"udp4", "udp6"})
	for _, labels := range [][]string{
		{"mainnet-el", "v4", "udp4"},
		{"mainnet-el", "v5", "udp6"},
		{"mainnet-cl", "v5", "udp4"},
	} {
		metric := &dto.Metric{}
		if err := mDiscoveryWalkerSightings.WithLabelValues(labels...).Write(metric); err != nil {
			t.Fatal(err)
		}
		if got := metric.GetCounter().GetValue(); got != 0 {
			t.Fatalf("walker series %v = %v, want zero", labels, got)
		}
	}
}

func TestAttemptWindowExpiresAndEvictsInConstantWork(t *testing.T) {
	now := time.Unix(1700000000, 0)
	w := newExpiringMap[struct{}](time.Minute, 2)
	var ids [3]enode.ID
	for i := range ids {
		ids[i][0] = byte(i + 1)
	}
	if !w.Allow(ids[0], now) || w.Allow(ids[0], now) || !w.Allow(ids[1], now) || !w.Allow(ids[2], now) {
		t.Fatal("attempt admission/duplicate behavior is wrong")
	}
	if len(w.entries) != 2 {
		t.Fatalf("entries = %d, want bounded size 2", len(w.entries))
	}
	if !w.Allow(ids[0], now) {
		t.Fatal("oldest entry was not evicted at capacity")
	}
	if !w.Allow(ids[1], now.Add(time.Minute)) {
		t.Fatal("expired entry was not admitted again")
	}
}

// The legacy enqueue path claims the window before its budget and queue checks, then releases it
// when either rejects, so a deferred probe does not burn the node's next attempt.
func TestAttemptWindowReleaseRestoresTheClaim(t *testing.T) {
	now := time.Unix(1700000000, 0)
	w := newExpiringMap[struct{}](time.Hour, 8)
	var id enode.ID
	id[0] = 1
	if !w.Allow(id, now) {
		t.Fatal("first claim rejected")
	}
	if w.Allow(id, now) {
		t.Fatal("second claim granted while the first was outstanding")
	}
	w.Take(id, now)
	if !w.Allow(id, now) {
		t.Fatal("released claim did not free the window")
	}
}

func TestTargetDialBudgetSharesIPv4AndIPv6PrefixLimits(t *testing.T) {
	now := time.Unix(1700000000, 0)
	b := newTargetDialBudget(1, 1, 8, time.Hour)
	if !b.Allow(net.ParseIP("203.0.113.1"), now) || b.Allow(net.ParseIP("203.0.113.1"), now) {
		t.Fatal("IPv4 identities did not share one target budget")
	}
	if !b.Allow(net.ParseIP("2001:db8:1:2::1"), now) || b.Allow(net.ParseIP("2001:db8:1:2::ffff"), now) {
		t.Fatal("IPv6 addresses in one /64 did not share one target budget")
	}
	if !b.Allow(net.ParseIP("2001:db8:1:3::1"), now) {
		t.Fatal("unrelated IPv6 /64 was starved")
	}
}

func TestLoadOrCreateNodeKeyPersistsIdentity(t *testing.T) {
	path := t.TempDir() + "/nodekey"
	first, err := loadOrCreateNodeKey(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreateNodeKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(crypto.FromECDSA(first), crypto.FromECDSA(second)) {
		t.Fatal("node identity changed after reloading the persistent key")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("nodekey permissions = %04o, want 0600", got)
	}
}

func TestLoadOrCreateNodeKeyRejectsLoosePermissions(t *testing.T) {
	path := t.TempDir() + "/nodekey"
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := crypto.SaveECDSA(path, key); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateNodeKey(path); err == nil {
		t.Fatal("nodekey with group/world permissions was accepted")
	}
}

// Mainnet Frontier with its canonical Next: an earlier era that EIP-2124 accepts for
// peering but a current-fork view must not. Immutable history, so it cannot go stale.
const (
	earlierEraForkHash = "fc64ec04"
	earlierEraForkNext = 1150000
)

func TestMeasurementPointUsesSameForkAndFingerprintRules(t *testing.T) {
	at := time.Now().UTC()
	mainnet, _ := netconf.Get("mainnet")
	el := mainnet.CurrentForkIDAt(at)
	cl, err := netconf.CLForkStateAt("mainnet", at)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string][]nodeset.Row{"mainnet": {
		{Layer: "el", ForkHash: hex.EncodeToString(el.Hash[:]), ForkNext: el.Next, MembershipSource: "status", FPStatus: "ok", FPAt: at.Unix(), FPDirection: "inbound"},
		{Layer: "el", ForkHash: "deadbeef"},
		{Layer: "el", ForkHash: earlierEraForkHash, ForkNext: earlierEraForkNext},
		{Layer: "cl", ForkHash: hex.EncodeToString(cl.Digest[:]), MembershipSource: "enr"},
		{Layer: "cl", ForkHash: "ffffffff"},
	}}
	point := measurementPointAt(at, &snapshot.RunMetadata{}, rows, distinct.New("test", distinct.DefaultPrecision))
	got := point.Networks["mainnet"]
	if got.Current != 2 || got.ExecutionCurrent != 1 || got.ConsensusCurrent != 1 || got.ExecutionStale != 2 || got.ConsensusStale != 1 ||
		got.MembershipVerified != 1 || got.MembershipClaimed != 1 || got.FingerprintIdentified != 1 || got.FingerprintDirection["inbound"] != 1 {
		t.Fatalf("measurement aggregate = %+v", got)
	}
}

func TestFingerprintAttemptBucket(t *testing.T) {
	for attempts, want := range map[int32]string{-1: "0", 0: "0", 1: "1", 5: "5", 6: "6+", 99: "6+"} {
		if got := fingerprintAttemptBucket(attempts); got != want {
			t.Errorf("fingerprintAttemptBucket(%d) = %q, want %q", attempts, got, want)
		}
	}
}

func TestAllowedByNetrestrictChecksResolvedAddress(t *testing.T) {
	restrict, err := netutil.ParseNetlist("172.16.0.0/12")
	if err != nil {
		t.Fatal(err)
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	private := enode.NewV4(&key.PublicKey, net.ParseIP("172.16.0.26"), 30303, 30303)
	public := enode.NewV4(&key.PublicKey, net.ParseIP("47.186.76.213"), 30303, 30303)
	if !allowedByNetrestrict(private, restrict) {
		t.Fatal("private enclave address should be allowed")
	}
	if allowedByNetrestrict(public, restrict) {
		t.Fatal("resolved WAN address should be rejected")
	}
	if !allowedByNetrestrict(public, nil) {
		t.Fatal("nil restriction should allow any address")
	}
	var dualRecord enr.Record
	dualRecord.Set(enr.IPv4{172, 16, 0, 26})
	dualRecord.Set(enr.IPv6{0xfd, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1})
	if err := enode.SignV4(&dualRecord, key); err != nil {
		t.Fatal(err)
	}
	dual, err := enode.New(enode.ValidSchemes, &dualRecord)
	if err != nil {
		t.Fatal(err)
	}
	if allowedByNetrestrict(dual, restrict) {
		t.Fatal("dual-stack record with an out-of-range secondary address was allowed")
	}
}

func TestIsCrawlerRecordRejectsStaleDevnetIdentityByIP(t *testing.T) {
	currentKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	oldKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	ip := net.ParseIP("172.16.0.6")
	current := enode.NewV4(&currentKey.PublicKey, ip, 30303, 30303)
	stale := enode.NewV4(&oldKey.PublicKey, ip, 30303, 30303)
	self := map[enode.ID]bool{current.ID(): true}

	if !isCrawlerRecord(current, self, false, nil) {
		t.Fatal("current identity was not rejected")
	}
	if !isCrawlerRecord(stale, self, true, ip) {
		t.Fatal("stale devnet identity on crawler IP was not rejected")
	}
	if isCrawlerRecord(stale, self, false, ip) {
		t.Fatal("production mode rejected a co-located identity by IP")
	}
}

func mainnetNode(t *testing.T) *enode.Node {
	t.Helper()
	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(enr.IPv4{5, 6, 7, 8})
	r.Set(enr.TCP(30303))
	r.Set(netconf.EthEntry{ForkID: nw.CurrentForkID()})
	var id enode.ID
	id[0] = 99
	return enode.SignNull(&r, id)
}

func TestRestorePreviousSchema(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layout := snapshot.Layout{}

	set := nodeset.NewWithLimit(0)
	set.Observe(mainnetNode(t), "v5", time.Unix(1700000000, 0))
	data, err := set.ParquetForNetwork("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	gen := time.Unix(1700000000, 0)
	key := layout.GenerationKey("mainnet", gen)
	if err := st.Put(ctx, key, data, ""); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	m := &snapshot.Manifest{
		SchemaVersion: snapshot.OldestReadableSchemaVersion,
		GeneratedAt:   gen,
		CrawlerID:     "test-crawler",
		Run: snapshot.RunMetadata{
			RunID: "test-run", SourceRevision: "test-revision", SourceURL: "https://example.com/source",
			ConfigSHA256: hex.EncodeToString(make([]byte, sha256.Size)), CrawlerStartedAt: gen.Add(-time.Minute),
			MethodologyStartedAt: gen.Add(-time.Minute), MethodologyVersion: snapshot.MethodologyVersion,
			MethodologyID: "test-method",
		},
		Networks: map[string]snapshot.NetworkSnapshot{
			"mainnet": {GenerationKey: key, NodeCount: 1, Bytes: len(data), SHA256: hex.EncodeToString(sum[:])},
		},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, layout.ManifestKey(), raw, "application/json"); err != nil {
		t.Fatal(err)
	}

	restored := nodeset.NewWithLimit(0)
	m, err = restore(ctx, st, layout, restored, nil, []string{"mainnet"})
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("restore returned nil manifest")
	}
	if restored.Len() != 1 {
		t.Errorf("restored %d nodes, want 1", restored.Len())
	}
	if got := restored.CountForNetwork("mainnet"); got != 1 {
		t.Errorf("mainnet count = %d, want 1", got)
	}
}

func TestRestoreSkipsUnconfiguredNetworks(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layout := snapshot.Layout{}
	gen := time.Unix(1700000000, 0)

	set := nodeset.NewWithLimit(0)
	set.Observe(mainnetNode(t), "v5", gen)
	mainnetData, err := set.ParquetForNetwork("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := nodeset.RowsFromParquet(mainnetData)
	if err != nil {
		t.Fatal(err)
	}
	rows[0].Network = "sepolia"
	rows[0].ID = rows[0].ID[:len(rows[0].ID)-1] + "f"
	sepoliaData, err := nodeset.ParquetFromRows(rows)
	if err != nil {
		t.Fatal(err)
	}

	networks := map[string]snapshot.NetworkSnapshot{}
	for network, data := range map[string][]byte{"mainnet": mainnetData, "sepolia": sepoliaData} {
		key := layout.GenerationKey(network, gen)
		if err := st.Put(ctx, key, data, ""); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		networks[network] = snapshot.NetworkSnapshot{GenerationKey: key, NodeCount: 1, Bytes: len(data), SHA256: hex.EncodeToString(sum[:])}
	}
	m := &snapshot.Manifest{
		SchemaVersion: snapshot.SchemaVersion,
		GeneratedAt:   gen,
		CrawlerID:     "test-crawler",
		Run: snapshot.RunMetadata{
			RunID: "test-run", SourceRevision: "test-revision", SourceURL: "https://example.com/source",
			ConfigSHA256: hex.EncodeToString(make([]byte, sha256.Size)), CrawlerStartedAt: gen.Add(-time.Minute),
			MethodologyStartedAt: gen.Add(-time.Minute), MethodologyVersion: snapshot.MethodologyVersion,
			MethodologyID: "test-method",
		},
		Networks: networks,
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, layout.ManifestKey(), raw, "application/json"); err != nil {
		t.Fatal(err)
	}

	restored := nodeset.NewWithLimit(0)
	m, err = restore(ctx, st, layout, restored, nil, []string{"mainnet"})
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.CountForNetwork("mainnet"); got != 1 {
		t.Errorf("mainnet count = %d, want 1", got)
	}
	if got := restored.CountForNetwork("sepolia"); got != 0 {
		t.Errorf("sepolia count = %d, want 0 for an unconfigured network", got)
	}
	if len(m.Networks) != 2 {
		t.Errorf("returned manifest has %d networks, want the full stored set of 2", len(m.Networks))
	}
}

func TestRestoreNoManifestStartsEmpty(t *testing.T) {
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	set := nodeset.NewWithLimit(0)
	m, err := restore(context.Background(), st, snapshot.Layout{}, set, nil, []string{"mainnet"})
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Errorf("restore with no manifest should return nil, got %+v", m)
	}
	if set.Len() != 0 {
		t.Errorf("set should be empty, got %d", set.Len())
	}
}

func TestRestoreManifestErrorIsFatal(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layout := snapshot.Layout{}
	if err := st.Put(ctx, layout.ManifestKey(), []byte("not json"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := restore(ctx, st, layout, nodeset.NewWithLimit(0), nil, []string{"mainnet"}); err == nil {
		t.Fatal("restore should fail on an unreadable manifest, not start empty")
	}
}

func TestValidateSnapshotRows(t *testing.T) {
	good := []nodeset.Row{{ID: "a", Network: "mainnet"}}
	if err := snapshot.ValidateRows("mainnet", 1, good); err != nil {
		t.Fatalf("valid rows rejected: %v", err)
	}
	for _, rows := range [][]nodeset.Row{
		good,
		{{ID: "a", Network: "hoodi"}},
		{{ID: "a", Network: "mainnet"}, {ID: "a", Network: "mainnet"}},
	} {
		expected := len(rows)
		if len(rows) == 1 && rows[0].Network == "mainnet" {
			expected = 2
		}
		if err := snapshot.ValidateRows("mainnet", expected, rows); err == nil {
			t.Errorf("invalid rows accepted: %+v", rows)
		}
	}
}

func TestCleanupOrphanGenerationsRemovesUploadedKeys(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layout := snapshot.Layout{}
	keys := []string{
		layout.GenerationKey("mainnet", time.Unix(1700000000, 0)),
		layout.GenerationKey("hoodi", time.Unix(1700000000, 0)),
	}
	for _, key := range keys {
		if err := st.Put(ctx, key, []byte("parquet"), ""); err != nil {
			t.Fatal(err)
		}
	}
	cleanupOrphanGenerations(ctx, st, layout, keys)
	for _, key := range keys {
		if _, err := st.Get(ctx, key); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("key %s still present: %v", key, err)
		}
	}
}

func TestCleanupOrphanGenerationsProtectsCommittedGeneration(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layout := snapshot.Layout{}
	gen := time.Unix(1700000000, 0).UTC()
	committed := layout.GenerationKey("mainnet", gen)
	orphan := layout.GenerationKey("mainnet", gen.Add(time.Second))
	data := []byte("parquet")
	for _, key := range []string{committed, orphan} {
		if err := st.Put(ctx, key, data, ""); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256(data)
	m := &snapshot.Manifest{
		SchemaVersion: snapshot.SchemaVersion, GeneratedAt: gen, CrawlerID: "crawler",
		Run: snapshot.RunMetadata{
			RunID: "run", SourceRevision: "revision", SourceURL: "https://example.com/source",
			ConfigSHA256: hex.EncodeToString(make([]byte, sha256.Size)), CrawlerStartedAt: gen.Add(-time.Hour),
			MethodologyStartedAt: gen.Add(-time.Hour), MethodologyVersion: snapshot.MethodologyVersion, MethodologyID: "method",
		},
		Networks: map[string]snapshot.NetworkSnapshot{"mainnet": {
			GenerationKey: committed, NodeCount: 1, SHA256: hex.EncodeToString(sum[:]), Bytes: len(data),
		}},
	}
	if err := snapshot.Write(ctx, st, layout, m); err != nil {
		t.Fatal(err)
	}
	cleanupOrphanGenerations(ctx, st, layout, []string{committed, orphan})
	if _, err := st.Get(ctx, committed); err != nil {
		t.Fatalf("committed generation was deleted: %v", err)
	}
	if _, err := st.Get(ctx, orphan); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("orphan generation remains: %v", err)
	}
}

func TestPruneGenerations(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layout := snapshot.Layout{}
	base := time.Unix(1700000000, 0)
	for i := 0; i < 5; i++ {
		key := layout.GenerationKey("mainnet", base.Add(time.Duration(i)*time.Second))
		if err := st.Put(ctx, key, []byte("x"), ""); err != nil {
			t.Fatal(err)
		}
	}

	pruneGenerationsForNetworks(ctx, st, layout, 2, nil, netconf.Names())

	keys, err := st.List(ctx, layout.NetworkPrefix("mainnet"))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("kept %d generations, want 2", len(keys))
	}
	newest := layout.GenerationKey("mainnet", base.Add(4*time.Second))
	oldest := layout.GenerationKey("mainnet", base)
	var keptNewest, keptOldest bool
	for _, k := range keys {
		if k == newest {
			keptNewest = true
		}
		if k == oldest {
			keptOldest = true
		}
	}
	if !keptNewest || keptOldest {
		t.Errorf("retention kept wrong generations: %v", keys)
	}
}

func TestPruneGenerationsProtectsManifestGenerationAfterClockRollback(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layout := snapshot.Layout{}
	base := time.Unix(1700000000, 0)
	currentKey := layout.GenerationKey("mainnet", base)
	for i := 0; i < 4; i++ {
		key := layout.GenerationKey("mainnet", base.Add(time.Duration(i)*time.Hour))
		if err := st.Put(ctx, key, []byte("x"), ""); err != nil {
			t.Fatal(err)
		}
	}
	current := &snapshot.Manifest{Networks: map[string]snapshot.NetworkSnapshot{
		"mainnet": {GenerationKey: currentKey},
	}}
	pruneGenerationsForNetworks(ctx, st, layout, 2, current, netconf.Names())
	keys, err := st.List(ctx, layout.NetworkPrefix("mainnet"))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || !slices.Contains(keys, currentKey) {
		t.Fatalf("retained generations = %v; current %q was not protected", keys, currentKey)
	}
}

func TestPruneAggregatesSweepsEveryMethodology(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	key := func(methodology string, at time.Time) string {
		return fmt.Sprintf("snapshots/aggregates/%s/%s.json", methodology, at.Format(aggregateTimeFormat))
	}
	fresh := key("2026-07-v1-current0", now.Add(-time.Hour))
	staleCurrent := key("2026-07-v1-current0", now.Add(-40*24*time.Hour))
	stalePrior := key("2026-07-v1-retired0", now.Add(-40*24*time.Hour))
	unparseable := "snapshots/aggregates/2026-07-v1-current0/not-a-timestamp.json"
	for _, k := range []string{fresh, staleCurrent, stalePrior, unparseable} {
		if err := st.Put(ctx, k, []byte("{}"), "application/json"); err != nil {
			t.Fatal(err)
		}
	}

	pruneAggregates(ctx, st, "snapshots", 30*24*time.Hour, now)

	for _, k := range []string{fresh, unparseable} {
		if _, err := st.Get(ctx, k); err != nil {
			t.Fatalf("%s was removed: %v", k, err)
		}
	}
	for _, k := range []string{staleCurrent, stalePrior} {
		if _, err := st.Get(ctx, k); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s survived pruning: %v", k, err)
		}
	}
}

func guardManifest(total, current int) *snapshot.Manifest {
	return &snapshot.Manifest{Networks: map[string]snapshot.NetworkSnapshot{
		"mainnet": {NodeCount: total, CurrentNodeCount: current},
	}}
}

func TestShrankTooMuch(t *testing.T) {
	prev := guardManifest(20000, 18000)
	tests := []struct {
		name        string
		total, curr int
		want        string
	}{
		{"steady", 20000, 18000, ""},
		{"growth is never blocked", 200000, 180000, ""},
		{"small dip", 12000, 11000, ""},
		{"gutted set", 200, 180, "collapse_total"},
		{"fork regression holds the row count", 20000, 100, "collapse_current"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shrankTooMuch(prev, guardManifest(test.total, test.curr), 50); got != test.want {
				t.Fatalf("shrankTooMuch = %q, want %q", got, test.want)
			}
		})
	}
	if got := shrankTooMuch(nil, guardManifest(1, 1), 50); got != "" {
		t.Fatalf("first publish = %q, want no reason", got)
	}
	// A network dropped from --advertiser-networks is a scope change, not a collapse; blocking on it
	// would wedge publishing forever because the reference only advances on success.
	dropped := &snapshot.Manifest{Networks: map[string]snapshot.NetworkSnapshot{"hoodi": {NodeCount: 5, CurrentNodeCount: 5}}}
	if got := shrankTooMuch(prev, dropped, 50); got != "" {
		t.Fatalf("dropped network = %q, want no reason", got)
	}
}

func TestBelowFloor(t *testing.T) {
	networks := []string{"mainnet", "hoodi"}
	if got := belowFloor(guardManifest(10, 10), networks, 1); got != "current_below_floor" {
		t.Fatalf("missing network = %q, want current_below_floor", got)
	}
	full := &snapshot.Manifest{Networks: map[string]snapshot.NetworkSnapshot{
		"mainnet": {NodeCount: 10, CurrentNodeCount: 10},
		"hoodi":   {NodeCount: 10, CurrentNodeCount: 0},
	}}
	if got := belowFloor(full, networks, 1); got != "current_below_floor" {
		t.Fatalf("zero current = %q, want current_below_floor", got)
	}
	if got := belowFloor(full, networks, 0); got != "" {
		t.Fatalf("disabled floor = %q, want no reason", got)
	}
}

func TestCurrentForkCount(t *testing.T) {
	now := time.Now()
	nw, _ := netconf.Get("mainnet")
	id := nw.CurrentForkIDAt(now)
	rows := []nodeset.Row{
		{Layer: "el", ForkHash: hex.EncodeToString(id.Hash[:]), ForkNext: id.Next},
		{Layer: "el", ForkHash: "deadbeef"},
		{Layer: "el", ForkHash: earlierEraForkHash, ForkNext: earlierEraForkNext},
		{Layer: "cl", ForkHash: "deadbeef"},
	}
	if got := currentForkCount("mainnet", rows, now); got != 1 {
		t.Fatalf("currentForkCount = %d, want 1", got)
	}
}

func TestForcePublishConsumedOnlyByCommittedPublish(t *testing.T) {
	p := &publisher{
		cfg:      publishConfig{forcePublish: true, maxCollapsePct: 50},
		networks: []string{"mainnet"},
		prev:     guardManifest(20000, 18000),
	}
	shrunk := guardManifest(100, 90)

	admitted, forced := p.admit(shrunk)
	if !admitted || !forced {
		t.Fatalf("forced shrink admit = %v/%v, want true/true", admitted, forced)
	}
	admitted, forced = p.admit(shrunk)
	if !admitted || !forced {
		t.Fatalf("forced shrink admit after a failed commit = %v/%v, want true/true", admitted, forced)
	}

	admitted, forced = p.admit(guardManifest(20000, 18000))
	if !admitted || forced {
		t.Fatalf("benign admit = %v/%v, want true without consuming force", admitted, forced)
	}

	p.forceUsed = true
	if admitted, _ = p.admit(shrunk); admitted {
		t.Fatal("force override survived a committed forced publish")
	}

	pending := &publisher{
		cfg:          publishConfig{forcePublish: true, maxCollapsePct: 50},
		networks:     []string{"mainnet"},
		prev:         guardManifest(20000, 18000),
		forcePending: true,
	}
	if admitted, _ := pending.admit(shrunk); admitted {
		t.Fatal("an unsettled ambiguous forced commit admitted another forced shrink")
	}
	if admitted, forced := pending.admit(guardManifest(20000, 18000)); !admitted || forced {
		t.Fatalf("benign admit with a pending force = %v/%v, want admitted without forcing", admitted, forced)
	}
}
