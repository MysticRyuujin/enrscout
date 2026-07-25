package nodeset

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/parquet-go/parquet-go"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
)

func TestObserveAuthenticatedELStoresVerifiedEnodeWithoutENR(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("enode://%s@8.8.8.8:30303", hex.EncodeToString(crypto.FromECDSAPub(&key.PublicKey)[1:]))
	n, err := enode.ParseV4(url)
	if err != nil {
		t.Fatal(err)
	}
	s := NewWithLimit(0)
	if observed := s.ObserveAuthenticatedEL(n, "v4", "mainnet", "07c9462e", 0, time.Now()); !observed.Applied {
		t.Fatalf("legacy observation = %+v", observed)
	}
	s.SetFingerprint(n.ID(), "Nethermind", "v1.39.0", "linux/x86_64", "dotnet10", "eth/71", "outbound")
	row := s.rows("mainnet")[0]
	if row.ENR != "" || row.Enode != url || row.Layer != "el" || row.Client != "Nethermind" || !row.HasV4 {
		t.Fatalf("legacy row = %+v", row)
	}
}

func TestHasExecutionTCPIsOnlyATransportHint(t *testing.T) {
	if !HasExecutionTCP(nodeWithIP(t, 0x91, enr.IPv4{8, 8, 8, 8}, true)) {
		t.Fatal("TCP-capable record was not recognized")
	}
	if HasExecutionTCP(nodeWithIP(t, 0x92, enr.IPv4{8, 8, 4, 4}, false)) {
		t.Fatal("discovery-only record was treated as RLPx-capable")
	}
}

func TestSnapshotRequiresRepeatedInboundOnlyAuthentication(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	n := enode.NewV4(&key.PublicKey, []byte{8, 8, 8, 8}, 30303, 0)
	now := time.Unix(1700000000, 0)
	s := NewWithLimit(0)
	if observed := s.ObserveAuthenticatedEL(n, "inbound", "mainnet", "07c9462e", 0, now); !observed.Applied {
		t.Fatalf("first inbound observation = %+v", observed)
	}
	s.SetFingerprint(n.ID(), "Geth", "v1.17.4", "linux/x86_64", "go1.24", "eth/71", "outbound")
	if got := len(s.SnapshotNetworks([]string{"mainnet"})["mainnet"]); got != 0 {
		t.Fatalf("snapshot contains %d one-shot inbound identities, want 0", got)
	}

	if observed := s.ObserveAuthenticatedEL(n, "inbound", "mainnet", "07c9462e", 0, now.Add(time.Minute)); !observed.Accepted {
		t.Fatalf("second inbound observation = %+v", observed)
	}
	if got := len(s.SnapshotNetworks([]string{"mainnet"})["mainnet"]); got != 1 {
		t.Fatalf("snapshot contains %d repeatedly authenticated identities, want 1", got)
	}
}

func TestSetExecutionStatusRefreshesVerifiedFork(t *testing.T) {
	n := elNode(t, 7)
	s := NewWithLimit(0)
	if !s.Observe(n, "v5", time.Now()) {
		t.Fatal("EL node was not observed")
	}
	if got := s.NetworkOf(n.ID()); got != "mainnet" {
		t.Fatalf("NetworkOf = %q, want mainnet", got)
	}
	fork := forkid.ID{Hash: [4]byte{1, 2, 3, 4}, Next: 99}
	if !s.SetExecutionStatus(n.ID(), "mainnet", fork) {
		t.Fatal("matching execution Status was not applied")
	}
	row := s.rows("mainnet")[0]
	if row.ForkHash != "01020304" || row.ForkNext != 99 {
		t.Fatalf("execution Status fields = %s/%d", row.ForkHash, row.ForkNext)
	}
	if !s.SetExecutionStatus(n.ID(), "sepolia", fork) || s.NetworkOf(n.ID()) != "sepolia" {
		t.Fatal("authenticated live Status did not replace stale network metadata")
	}
}

func TestMembershipSourceAndDirectionProvenance(t *testing.T) {
	s := NewWithLimit(0)
	n := elNode(t, 44)
	now := time.Unix(1700000000, 0)
	s.Observe(n, "v5", now)
	if got := s.rows("mainnet")[0].MembershipSource; got != "enr" {
		t.Fatalf("ENR-classified source = %q, want enr", got)
	}
	if !s.SetExecutionStatus(n.ID(), "mainnet", forkid.ID{Hash: [4]byte{1, 2, 3, 4}}) {
		t.Fatal("execution status rejected")
	}
	s.SetFingerprint(n.ID(), "Geth", "v1", "linux", "go", "eth/71", "inbound")
	row := s.rows("mainnet")[0]
	if row.MembershipSource != "status" || row.FPDirection != "inbound" {
		t.Fatalf("provenance = %q/%q, want status/inbound", row.MembershipSource, row.FPDirection)
	}
	s.Observe(n, "v5", now.Add(time.Second))
	if got := s.rows("mainnet")[0].MembershipSource; got != "status" {
		t.Fatalf("later ENR observation downgraded source to %q", got)
	}
	sepolia, err := netconf.Get("sepolia")
	if err != nil {
		t.Fatal(err)
	}
	var changed enr.Record
	changed.SetSeq(n.Seq() + 1)
	changed.Set(enr.IPv4{1, 2, 3, 44})
	changed.Set(enr.TCP(30303))
	changed.Set(netconf.EthEntry{ForkID: sepolia.CurrentForkID()})
	changedNetwork := enode.SignNull(&changed, n.ID())
	s.Observe(changedNetwork, "v5", now.Add(2*time.Second))
	row = s.rows("sepolia")[0]
	if row.MembershipSource != "enr" {
		t.Fatalf("network-changing ENR retained source %q, want enr", row.MembershipSource)
	}

	restored := NewWithLimit(0)
	restored.Ingest(s.SnapshotNetworks([]string{"sepolia"})["sepolia"])
	row = restored.rows("sepolia")[0]
	if row.MembershipSource != "enr" || row.FPDirection != "inbound" {
		t.Fatalf("restored provenance = %q/%q", row.MembershipSource, row.FPDirection)
	}
}

func TestSetConsensusStatusRequiresCLLayer(t *testing.T) {
	s := NewWithLimit(0)
	el := elNode(t, 45)
	s.Observe(el, "v5", time.Now())
	if s.SetConsensusStatus(el.ID(), "mainnet", "6a95a1a1") {
		t.Fatal("consensus status applied to an execution node")
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cl := enode.NewV4(&key.PublicKey, []byte{8, 8, 4, 4}, 9000, 0)
	s.ObserveAuthenticatedCL(cl, "mainnet", "aabbccdd", time.Now())
	if !s.SetConsensusStatus(cl.ID(), "mainnet", "6a95a1a1") {
		t.Fatal("consensus status rejected for a CL node")
	}
	row := s.rows("mainnet")
	for _, r := range row {
		if r.Layer == "cl" && (r.ForkHash != "6a95a1a1" || r.MembershipSource != "status") {
			t.Fatalf("CL row = fork %q source %q", r.ForkHash, r.MembershipSource)
		}
	}
}

func TestObserveAuthenticatedCLForcesConsensusLayerWithoutENR(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	n := enode.NewV4(&key.PublicKey, []byte{8, 8, 4, 4}, 9000, 0)
	s := NewWithLimit(0)
	if observed := s.ObserveAuthenticatedCL(n, "mainnet", "6a95a1a1", time.Now()); !observed.Applied {
		t.Fatalf("CL observation = %+v", observed)
	}
	s.SetFingerprint(n.ID(), "Lighthouse", "v7.0.1", "linux/x86_64", "", "", "outbound")
	row := s.rows("mainnet")[0]
	if row.ENR != "" || row.Layer != "cl" || row.Client != "Lighthouse" || row.TCP != 9000 {
		t.Fatalf("authenticated CL row = %+v", row)
	}
}

func elNode(t *testing.T, tag byte) *enode.Node {
	t.Helper()
	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(enr.IPv4{1, 2, 3, tag})
	r.Set(enr.TCP(30303))
	r.Set(netconf.EthEntry{ForkID: nw.CurrentForkID()})
	var id enode.ID
	id[0] = tag
	return enode.SignNull(&r, id)
}

func elNodeWithIndex(t *testing.T, index int) *enode.Node {
	t.Helper()
	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(enr.IPv4{1, 2, 3, 4})
	r.Set(enr.TCP(30303))
	r.Set(netconf.EthEntry{ForkID: nw.CurrentForkID()})
	var id enode.ID
	binary.LittleEndian.PutUint64(id[:8], uint64(index+1))
	return enode.SignNull(&r, id)
}

func TestObserveScoreIncrementsAndCaps(t *testing.T) {
	s := NewWithLimit(0)
	n := elNode(t, 1)
	now := time.Now()
	for i := 0; i < 20; i++ {
		s.Observe(n, "v5", now)
	}
	if s.Len() != 1 {
		t.Fatalf("expected 1 node, got %d", s.Len())
	}
	rows := s.rows("")
	if rows[0].Score != scoreCap {
		t.Errorf("score = %d, want cap %d", rows[0].Score, scoreCap)
	}
	if rows[0].Network != "mainnet" || rows[0].Layer != "el" {
		t.Errorf("classification: network=%q layer=%q", rows[0].Network, rows[0].Layer)
	}
}

func TestSetCapacityRejectsNewIdentityButKeepsExistingUpdates(t *testing.T) {
	s := NewWithLimit(1)
	n := elNode(t, 1)
	if !s.Observe(n, "v5", time.Now()) {
		t.Fatal("first node rejected")
	}
	if s.Observe(elNode(t, 2), "v5", time.Now()) {
		t.Fatal("node beyond capacity accepted")
	}
	if !s.Observe(n, "v4", time.Now()) {
		t.Fatal("existing node update rejected at capacity")
	}
}

func TestCapacityEvictsLowerClassForBetterCandidate(t *testing.T) {
	s := NewWithLimit(2)
	now := time.Unix(1700000000, 0)
	if !s.ObserveFallbackResult(elNode(t, 10), "v5", now).Accepted || !s.ObserveFallbackResult(elNode(t, 11), "v5", now).Accepted {
		t.Fatal("fallback leads rejected below capacity")
	}
	observed := s.ObserveResult(elNode(t, 12), "v5", now)
	if !observed.Accepted || observed.Evicted != 2 || observed.EvictedClass != "fallback" {
		t.Fatalf("classified candidate at capacity = %+v", observed)
	}
	counts := s.ClassCounts()
	if counts[1] != 1 || counts[3] != 0 || s.Len() != 1 {
		t.Fatalf("class counts = %v len=%d", counts, s.Len())
	}
}

func TestCapacityBatchEvictsOnlyLowerClass(t *testing.T) {
	const maxNodes = 1000
	s := NewWithLimit(maxNodes)
	now := time.Unix(1700000000, 0)
	for i := 0; i < maxNodes-64; i++ {
		if !s.Observe(elNodeWithIndex(t, i), "v5", now) {
			t.Fatalf("classified node %d rejected below capacity", i)
		}
	}
	for i := maxNodes - 64; i < maxNodes; i++ {
		if !s.ObserveFallbackResult(elNodeWithIndex(t, i), "v5", now).Accepted {
			t.Fatalf("fallback node %d rejected below capacity", i)
		}
	}
	observed := s.ObserveResult(elNodeWithIndex(t, maxNodes), "v5", now)
	if !observed.Accepted || observed.Evicted != 64 || observed.EvictedClass != "fallback" {
		t.Fatalf("batch admission = %+v, want 64 fallback evictions", observed)
	}
	counts := s.ClassCounts()
	if counts[1] != maxNodes-63 || counts[3] != 0 {
		t.Fatalf("batch eviction touched equal class: counts=%v", counts)
	}
}

func TestCapacityNeverEvictsVerifiedOrEqualClass(t *testing.T) {
	s := NewWithLimit(2)
	now := time.Unix(1700000000, 0)
	a, b := elNode(t, 20), elNode(t, 21)
	s.Observe(a, "v5", now)
	s.Observe(b, "v5", now)
	s.SetFingerprint(a.ID(), "Geth", "v1", "linux", "go", "eth/71", "outbound")
	s.SetFingerprint(b.ID(), "Geth", "v1", "linux", "go", "eth/71", "outbound")
	observed := s.ObserveResult(elNode(t, 22), "v5", now)
	if observed.Accepted || observed.Reject != "capacity" {
		t.Fatalf("classified candidate displaced verified nodes: %+v", observed)
	}
	same := NewWithLimit(1)
	same.Observe(elNode(t, 23), "v5", now)
	if observed := same.ObserveResult(elNode(t, 24), "v5", now); observed.Accepted {
		t.Fatalf("equal-class candidate displaced a classified node: %+v", observed)
	}
}

func TestIngestEvictsFallbackForRestoredRows(t *testing.T) {
	s := NewWithLimit(1)
	now := time.Unix(1700000000, 0)
	s.ObserveFallbackResult(elNode(t, 30), "v5", now)

	src := NewWithLimit(0)
	src.Observe(elNode(t, 31), "v5", now)
	src.SetFingerprint(elNode(t, 31).ID(), "Geth", "v1", "linux", "go", "eth/71", "outbound")
	rows := src.SnapshotNetworks([]string{"mainnet"})["mainnet"]

	dropped, evicted := s.Ingest(rows)
	if dropped != 0 || evicted != 1 || s.Len() != 1 {
		t.Fatalf("ingest = dropped %d evicted %d len %d", dropped, evicted, s.Len())
	}
	if got := s.rows("mainnet")[0].Client; got != "Geth" {
		t.Fatalf("restored row lost to fallback lead: client=%q", got)
	}
}

func TestObserveFallbackDecaysExistingNode(t *testing.T) {
	s := NewWithLimit(0)
	n := elNode(t, 91)
	now := time.Now()
	for range scoreCap {
		s.Observe(n, "v5", now)
	}
	if !s.ObserveFallbackResult(n, "v5", now.Add(time.Second)).Accepted {
		t.Fatal("valid cached record rejected")
	}
	if got := s.rows("")[0].Score; got != scoreCap/2-penaltyStep {
		t.Fatalf("score after fallback = %d, want %d", got, scoreCap/2-penaltyStep)
	}
	s.ObserveFallbackResult(n, "v5", now.Add(2*time.Second))
	if s.Len() != 0 {
		t.Fatalf("repeated failed resolution retained node, have %d", s.Len())
	}

	fresh := elNode(t, 92)
	if !s.ObserveFallbackResult(fresh, "v5", now).Accepted || s.Len() != 1 {
		t.Fatal("first signed cached observation should remain as a discovery lead")
	}
}

func TestObserveFallbackRetainsFingerprintedNode(t *testing.T) {
	s := NewWithLimit(0)
	n := elNode(t, 93)
	now := time.Now()
	s.Observe(n, "v5", now)
	s.SetFingerprint(n.ID(), "Geth", "v1", "linux", "go", "eth/71", "outbound")

	s.ObserveFallbackResult(n, "v5", now.Add(time.Second))
	if s.Len() != 1 {
		t.Fatal("transient resolution failure removed a fingerprinted node")
	}
	rows := s.rows("")
	if rows[0].Score != dropBelow || rows[0].FPStatus != "ok" {
		t.Fatalf("retained row = score %d status %q, want %d/ok", rows[0].Score, rows[0].FPStatus, dropBelow)
	}
}

func TestObserveFallbackRetainsFingerprintBackoff(t *testing.T) {
	s := NewWithLimit(0)
	n := elNode(t, 99)
	now := time.Unix(1700000000, 0)
	for range scoreCap {
		s.Observe(n, "v5", now)
	}
	if !s.ClaimFingerprintAt(n.ID(), now) {
		t.Fatal("initial fingerprint claim failed")
	}
	retry := s.FingerprintFailed(n.ID(), now)
	s.ObserveFallbackResult(n, "v5", now.Add(time.Second))
	s.ObserveFallbackResult(n, "v5", now.Add(2*time.Second))
	if s.Len() != 1 {
		t.Fatal("resolution failures erased fingerprint retry state")
	}
	if s.ClaimFingerprintAt(n.ID(), retry.RetryAt.Add(-time.Nanosecond)) {
		t.Fatal("resolution failure bypassed fingerprint backoff")
	}
}

func TestSnapshotExcludesUnverifiedFallbackUntilResolvedOrAuthenticated(t *testing.T) {
	s := NewWithLimit(0)
	n := elNode(t, 100)
	now := time.Unix(1700000000, 0)
	if !s.ObserveFallbackResult(n, "v5", now).Accepted {
		t.Fatal("signed fallback lead rejected")
	}
	if s.Len() != 1 {
		t.Fatalf("in-memory leads = %d, want 1", s.Len())
	}
	if got := len(s.SnapshotNetworks([]string{"mainnet"})["mainnet"]); got != 0 {
		t.Fatalf("snapshot contains %d unverified fallback leads", got)
	}
	data, err := s.ParquetForNetwork("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := RowsFromParquet(data); err != nil || len(rows) != 0 {
		t.Fatalf("fallback parquet rows = %d, err = %v", len(rows), err)
	}

	if !s.Observe(n, "v5", now.Add(time.Second)) {
		t.Fatal("directly resolved observation rejected")
	}
	if got := len(s.SnapshotNetworks([]string{"mainnet"})["mainnet"]); got != 1 {
		t.Fatalf("snapshot contains %d resolved nodes, want 1", got)
	}

	authenticated := elNode(t, 101)
	if !s.ObserveFallbackResult(authenticated, "v5", now).Accepted {
		t.Fatal("authenticated fallback lead rejected")
	}
	s.SetFingerprint(authenticated.ID(), "Nethermind", "v1.39.1", "linux/x86_64", "dotnet10", "eth/71", "outbound")
	if got := len(s.SnapshotNetworks([]string{"mainnet"})["mainnet"]); got != 2 {
		t.Fatalf("snapshot contains %d verified nodes, want 2", got)
	}
}

func TestObservePreservesForkMetadataFromLessInformativeRecord(t *testing.T) {
	s := NewWithLimit(0)
	n := elNode(t, 55)
	now := time.Now()
	if !s.Observe(n, "v5", now) {
		t.Fatal("classified node should be recorded")
	}
	want := s.rows("")[0]

	var r enr.Record
	r.Set(enr.IPv4{1, 2, 3, 55})
	r.Set(enr.TCP(30303))
	var id enode.ID
	id[0] = 55
	if !s.Observe(enode.SignNull(&r, id), "v4", now.Add(time.Second)) {
		t.Fatal("later unclassified observation should be recorded")
	}

	got := s.rows("")[0]
	if got.Network != want.Network || got.Layer != want.Layer || got.ForkHash != want.ForkHash || got.ForkNext != want.ForkNext {
		t.Fatalf("classification was downgraded: got network=%q layer=%q fork=%q/%d, want %q %q %q/%d",
			got.Network, got.Layer, got.ForkHash, got.ForkNext,
			want.Network, want.Layer, want.ForkHash, want.ForkNext)
	}
}

func TestObserveDoesNotReplaceNewerENRWithStaleRecord(t *testing.T) {
	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	makeNode := func(seq uint64, last byte) *enode.Node {
		var r enr.Record
		r.SetSeq(seq)
		r.Set(enr.IPv4{9, 9, 9, last})
		r.Set(enr.TCP(30303))
		r.Set(netconf.EthEntry{ForkID: nw.CurrentForkID()})
		var id enode.ID
		id[0] = 88
		return enode.SignNull(&r, id)
	}
	s := NewWithLimit(0)
	if !s.Observe(makeNode(2, 2), "v5", time.Now()) {
		t.Fatal("newer record rejected")
	}
	result := s.ObserveResult(makeNode(1, 1), "v4", time.Now().Add(time.Second))
	if !result.Accepted {
		t.Fatal("stale record should still count as an observation")
	}
	if result.Applied || result.Changed {
		t.Fatalf("stale result = %+v, must not drive endpoint side effects", result)
	}
	got := s.rows("")[0]
	if got.Seq != 2 || got.IP != "9.9.9.2" {
		t.Fatalf("stale record rolled endpoint back: seq=%d ip=%s", got.Seq, got.IP)
	}
	if !got.HasV4 || !got.HasV5 {
		t.Fatalf("observation protocols were not merged: v4=%v v5=%v", got.HasV4, got.HasV5)
	}
}

func nodeWithIP(t *testing.T, tag byte, ip enr.IPv4, withTCP bool) *enode.Node {
	t.Helper()
	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(ip)
	if withTCP {
		r.Set(enr.TCP(30303))
	}
	r.Set(netconf.EthEntry{ForkID: nw.CurrentForkID()})
	var id enode.ID
	id[0] = tag
	return enode.SignNull(&r, id)
}

func TestObserveRejectsInvalidAddr(t *testing.T) {
	s := NewWithLimit(0)
	now := time.Now()

	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	var noIPRec enr.Record
	noIPRec.Set(netconf.EthEntry{ForkID: nw.CurrentForkID()})
	var noIPID enode.ID
	noIPID[0] = 50
	if s.Observe(enode.SignNull(&noIPRec, noIPID), "v5", now) {
		t.Error("node with no IP should be rejected")
	}
	if s.Observe(nodeWithIP(t, 51, enr.IPv4{192, 168, 1, 1}, true), "v4", now) {
		t.Error("private IP should be rejected")
	}
	if s.Observe(nodeWithIP(t, 52, enr.IPv4{127, 0, 0, 1}, true), "v4", now) {
		t.Error("loopback IP should be rejected")
	}
	mixed := nodeWithIP(t, 54, enr.IPv4{1, 2, 3, 4}, true)
	mixedRecord := *mixed.Record()
	mixedRecord.Set(enr.IPv6{0xfd, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1})
	if s.Observe(enode.SignNull(&mixedRecord, mixed.ID()), "v5", now) {
		t.Error("mixed public/private record should be rejected in its entirety")
	}
	if s.Len() != 0 {
		t.Fatalf("no invalid records should be stored, have %d", s.Len())
	}

	if !s.Observe(nodeWithIP(t, 53, enr.IPv4{1, 2, 3, 4}, true), "v5", now) {
		t.Error("valid public node should be recorded")
	}
	if s.Len() != 1 {
		t.Errorf("expected 1 valid node, got %d", s.Len())
	}
}

func TestPenalizeDropsFlakyNode(t *testing.T) {
	s := NewWithLimit(0)
	n := elNode(t, 2)
	now := time.Now()
	s.Observe(n, "v5", now)
	s.Penalize(n.ID(), now)
	if s.Len() != 0 {
		t.Errorf("freshly-seen node that failed should be dropped, have %d", s.Len())
	}
}

func TestPenalizeRetainsFingerprintedNode(t *testing.T) {
	s := NewWithLimit(0)
	n := elNode(t, 94)
	now := time.Now()
	s.Observe(n, "v4", now)
	s.SetFingerprint(n.ID(), "Geth", "v1", "linux", "go", "eth/71", "outbound")
	s.Penalize(n.ID(), now.Add(time.Second))
	if s.Len() != 1 {
		t.Fatal("transient resolution failure removed a fingerprinted node")
	}
	rows := s.rows("")
	if rows[0].Score != dropBelow || rows[0].FPStatus != "ok" {
		t.Fatalf("retained row = score %d status %q, want %d/ok", rows[0].Score, rows[0].FPStatus, dropBelow)
	}
}

func TestPenalizeRetainsFingerprintBackoff(t *testing.T) {
	s := NewWithLimit(0)
	n := elNode(t, 100)
	now := time.Unix(1700000000, 0)
	s.Observe(n, "v5", now)
	if !s.ClaimFingerprintAt(n.ID(), now) {
		t.Fatal("initial fingerprint claim failed")
	}
	s.FingerprintFailed(n.ID(), now)
	s.Penalize(n.ID(), now.Add(time.Second))
	if s.Len() != 1 {
		t.Fatal("resolution penalty erased fingerprint retry state")
	}
}

func TestPenalizeKeepsForcedSeed(t *testing.T) {
	s := NewWithLimit(0)
	n := elNode(t, 54)
	now := time.Now()
	if !s.ObserveSeedResult(n, "devnet", now).Accepted {
		t.Fatal("forced seed should be recorded")
	}
	for range 10 {
		s.Penalize(n.ID(), now)
	}
	if s.Len() != 1 {
		t.Fatalf("forced seed was evicted, have %d nodes", s.Len())
	}
}

func TestObserveDevnetForcesUnknownTCPRecordWithoutPinning(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(enr.IPv4{1, 2, 3, 101})
	r.Set(enr.TCP(30303))
	if err := enode.SignV4(&r, key); err != nil {
		t.Fatal(err)
	}
	n, err := enode.New(enode.ValidSchemes, &r)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0)
	s := NewWithLimit(0)
	if !s.ObserveDevnetResult(n, "v4", now).Accepted {
		t.Fatal("isolated devnet record rejected")
	}
	row := s.rows("")[0]
	if row.Network != "devnet" || row.Layer != "el" || !row.HasV4 || row.FPStatus != "pending" {
		t.Fatalf("forced devnet row = network %q layer %q v4=%v fp=%q", row.Network, row.Layer, row.HasV4, row.FPStatus)
	}
	if removed := s.PruneStaleWithVerified(now.Add(time.Second), now.Add(time.Second)); removed != 1 {
		t.Fatalf("ordinary devnet discovery was pinned; removed=%d", removed)
	}
}

func TestObserveDevnetStillRejectsStaleENRMetadata(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	makeNode := func(seq uint64, last byte) *enode.Node {
		var r enr.Record
		r.SetSeq(seq)
		r.Set(enr.IPv4{1, 2, 3, last})
		r.Set(enr.TCP(30303))
		if err := enode.SignV4(&r, key); err != nil {
			t.Fatal(err)
		}
		n, err := enode.New(enode.ValidSchemes, &r)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	s := NewWithLimit(0)
	now := time.Unix(1700000000, 0)
	s.ObserveDevnetResult(makeNode(2, 102), "v5", now)
	s.ObserveDevnetResult(makeNode(1, 103), "v4", now.Add(time.Second))
	row := s.rows("")[0]
	if row.Seq != 2 || row.IP != "1.2.3.102" || !row.HasV4 || !row.HasV5 {
		t.Fatalf("stale devnet record rolled metadata back: %+v", row)
	}
}

func TestPruneStaleKeepsPinnedAndFreshNodes(t *testing.T) {
	s := NewWithLimit(0)
	now := time.Now()
	s.Observe(elNode(t, 95), "v5", now.Add(-2*time.Hour))
	s.Observe(elNode(t, 96), "v5", now)
	s.ObserveSeedResult(elNode(t, 97), "devnet", now.Add(-2*time.Hour))
	if removed := s.PruneStaleWithVerified(now.Add(-time.Hour), now.Add(-time.Hour)); removed != 1 {
		t.Fatalf("removed %d stale nodes, want 1", removed)
	}
	if s.Len() != 2 {
		t.Fatalf("retained %d nodes, want pinned + fresh", s.Len())
	}
}

func TestPruneStaleRetainsVerifiedNodeForLongerLifetime(t *testing.T) {
	s := NewWithLimit(0)
	now := time.Now()
	verified := elNode(t, 90)
	unverified := elNode(t, 91)
	s.Observe(verified, "v5", now.Add(-48*time.Hour))
	s.SetFingerprint(verified.ID(), "Nethermind", "v1.38.1", "linux/x86_64", "dotnet9", "eth/71", "outbound")
	s.mu.Lock()
	s.m[verified.ID()].LastResolved = now.Add(-48 * time.Hour)
	s.mu.Unlock()
	s.Observe(unverified, "v5", now.Add(-48*time.Hour))

	if removed := s.PruneStaleWithVerified(now.Add(-24*time.Hour), now.Add(-7*24*time.Hour)); removed != 1 {
		t.Fatalf("removed %d nodes, want only the unverified lead", removed)
	}
	if got := s.rows(""); len(got) != 1 || got[0].ID != verified.ID().String() {
		t.Fatalf("retained rows = %+v, want verified node", got)
	}
	if removed := s.PruneStaleWithVerified(now.Add(-24*time.Hour), now.Add(-24*time.Hour)); removed != 1 {
		t.Fatalf("removed %d verified nodes after extended lifetime, want 1", removed)
	}
}

func TestVerifiedPruneUsesLastResolvedNotFallbackSightings(t *testing.T) {
	s := NewWithLimit(0)
	now := time.Now()
	n := elNode(t, 92)
	s.Observe(n, "v5", now.Add(-8*24*time.Hour))
	s.SetFingerprint(n.ID(), "Geth", "v1", "linux", "go", "eth/71", "outbound")
	s.mu.Lock()
	s.m[n.ID()].LastResolved = now.Add(-8 * 24 * time.Hour)
	s.mu.Unlock()
	s.ObserveFallbackResult(n, "v5", now)
	if removed := s.PruneStaleWithVerified(now.Add(-24*time.Hour), now.Add(-7*24*time.Hour)); removed != 1 {
		t.Fatalf("removed %d nodes, want cached fallback sighting not to keep verified zombie", removed)
	}
}

func TestFingerprintRetriesWithBackoffWithoutGivingUp(t *testing.T) {
	s := NewWithLimit(0)
	n := elNode(t, 3)
	id := n.ID()
	now := time.Unix(1700000000, 0)
	s.Observe(n, "v5", now)

	for i := 0; i < fpFailedAfter; i++ {
		if !s.ClaimFingerprintAt(id, now) {
			t.Fatalf("claim attempt %d should succeed", i+1)
		}
		if s.ClaimFingerprintAt(id, now) {
			t.Fatalf("parallel claim attempt %d should fail", i+1)
		}
		retry := s.FingerprintFailed(id, now)
		if retry.Attempts != i+1 || !retry.RetryAt.After(now) {
			t.Fatalf("retry %d = %+v", i+1, retry)
		}
		if s.ClaimFingerprintAt(id, retry.RetryAt.Add(-time.Nanosecond)) {
			t.Fatalf("claim attempt %d ignored backoff", i+2)
		}
		now = retry.RetryAt
	}
	if got := s.rows("")[0].FPStatus; got != "failed" {
		t.Fatalf("status after repeated failures = %q, want failed", got)
	}
	if !s.ClaimFingerprintAt(id, now) {
		t.Fatal("due retry should remain claimable after failed status")
	}

	if failures := s.SetFingerprint(id, "Geth", "v1.17.4", "linux/x86_64", "go1.24", "eth/68", "outbound"); failures != fpFailedAfter {
		t.Fatalf("reported prior failures = %d, want %d", failures, fpFailedAfter)
	}
	s.UnclaimFingerprint(id)
	if s.ClaimFingerprint(id) {
		t.Error("no further claims once fingerprinted")
	}
}

func TestFingerprintRetryBackoffSurvivesSnapshotRestore(t *testing.T) {
	s := NewWithLimit(0)
	n := elNode(t, 30)
	now := time.Unix(1700000000, 0)
	s.Observe(n, "v5", now)
	for i := 0; i < 4; i++ {
		if !s.ClaimFingerprintAt(n.ID(), now) {
			t.Fatalf("claim %d failed", i+1)
		}
		retry := s.FingerprintFailed(n.ID(), now)
		now = retry.RetryAt
	}
	rows := s.rows("")
	restored := NewWithLimit(0)
	restored.Ingest(rows)
	if restored.ClaimFingerprintAt(n.ID(), now.Add(-time.Second)) {
		t.Fatal("restored node ignored persisted retry deadline")
	}
	if !restored.ClaimFingerprintAt(n.ID(), now) {
		t.Fatal("restored node was not claimable at persisted retry deadline")
	}
	if retry := restored.FingerprintFailed(n.ID(), now); retry.Attempts != 5 {
		t.Fatalf("restored attempt count = %d, want 5", retry.Attempts)
	}
}

func TestFingerprintCandidatesIncludeDueDormantRetry(t *testing.T) {
	s := NewWithLimit(0)
	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(enr.IPv4{1, 2, 3, 31})
	r.Set(enr.TCP(30303))
	r.Set(netconf.EthEntry{ForkID: nw.CurrentForkID()})
	if err := enode.SignV4(&r, key); err != nil {
		t.Fatal(err)
	}
	n, err := enode.New(enode.ValidSchemes, &r)
	if err != nil {
		t.Fatal(err)
	}
	id := n.ID()
	now := time.Unix(1700000000, 0)
	s.Observe(n, "v5", now)
	if !s.ClaimFingerprintAt(id, now) {
		t.Fatal("initial claim failed")
	}
	retry := s.FingerprintFailed(id, now)
	if got := s.FingerprintCandidates(retry.RetryAt.Add(-time.Nanosecond), 10); len(got) != 0 {
		t.Fatalf("candidate returned before backoff: %d", len(got))
	}
	got := s.FingerprintCandidates(retry.RetryAt, 10)
	if len(got) != 1 || got[0].ID() != id {
		t.Fatalf("due candidates = %v, want %s", got, id)
	}
}

func TestFingerprintCandidatesOrdersByPriorityWhenLimited(t *testing.T) {
	s := NewWithLimit(0)
	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0)
	mk := func(tag byte) *enode.Node {
		t.Helper()
		key, err := crypto.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		var r enr.Record
		r.Set(enr.IPv4{1, 2, 3, tag})
		r.Set(enr.TCP(30303))
		r.Set(netconf.EthEntry{ForkID: nw.CurrentForkID()})
		if err := enode.SignV4(&r, key); err != nil {
			t.Fatal(err)
		}
		n, err := enode.New(enode.ValidSchemes, &r)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	low := mk(0x41)
	high := mk(0x42)
	s.Observe(low, "v5", now)
	s.Observe(high, "v5", now)
	for range scoreCap {
		s.Observe(high, "v5", now.Add(time.Second))
	}
	// Both are due immediately (never fingerprinted). With limit=1 the higher-score
	// node must win even though map iteration order is nondeterministic.
	got := s.FingerprintCandidates(now.Add(time.Minute), 1)
	if len(got) != 1 || got[0].ID() != high.ID() {
		t.Fatalf("limited candidates = %v, want highest-score %s", got, high.ID())
	}
}

func TestFingerprintCandidatesPrioritizesOldestEffectiveDueTime(t *testing.T) {
	s := NewWithLimit(0)
	now := time.Unix(1700000000, 0)
	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(tag byte) *enode.Node {
		key, err := crypto.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		var r enr.Record
		r.Set(enr.IPv4{1, 2, 3, tag})
		r.Set(enr.TCP(30303))
		r.Set(netconf.EthEntry{ForkID: nw.CurrentForkID()})
		if err := enode.SignV4(&r, key); err != nil {
			t.Fatal(err)
		}
		n, err := enode.New(enode.ValidSchemes, &r)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	older := mk(0x43)
	newer := mk(0x44)
	s.Observe(older, "v5", now.Add(-2*time.Hour))
	s.Observe(newer, "v5", now.Add(-time.Hour))
	for range scoreCap {
		s.Observe(newer, "v5", now)
	}
	got := s.FingerprintCandidates(now, 1)
	if len(got) != 1 || got[0].ID() != older.ID() {
		t.Fatalf("limited candidates = %v, want oldest-due %s", got, older.ID())
	}
}

func TestLateFingerprintFailureDoesNotOverrideSuccess(t *testing.T) {
	s := NewWithLimit(0)
	n := elNode(t, 32)
	now := time.Unix(1700000000, 0)
	s.Observe(n, "v5", now)
	if !s.ClaimFingerprintAt(n.ID(), now) {
		t.Fatal("initial claim failed")
	}
	s.SetFingerprint(n.ID(), "Geth", "v1", "linux", "go", "eth/71", "outbound")
	if retry := s.FingerprintFailed(n.ID(), now); !retry.RetryAt.IsZero() {
		t.Fatalf("late failure scheduled over success: %+v", retry)
	}
	if got := s.rows("")[0].FPStatus; got != "ok" {
		t.Fatalf("status = %q, want ok", got)
	}
}

func TestSetGeoDiscardsResultForASupersededAddress(t *testing.T) {
	base := elNode(t, 93)
	id := base.ID()
	r1 := *base.Record()
	r1.SetSeq(1)
	n1 := enode.SignNull(&r1, id)
	r2 := r1
	r2.SetSeq(2)
	r2.Set(enr.IPv4{9, 9, 9, 93})
	n2 := enode.SignNull(&r2, id)

	s := NewWithLimit(0)
	now := time.Unix(1700000000, 0)
	s.Observe(n1, "v5", now)
	s.Observe(n2, "v5", now.Add(time.Second))

	s.SetGeo(id, n1.IP(), "US", "Fond du Lac", "WI", 43.77, -88.45, 1, "Old", false, true, true, 20)
	if got := s.rows("")[0]; got.Country != "" || got.ASN != 0 {
		t.Fatalf("geo for the superseded address was applied: country=%q asn=%d", got.Country, got.ASN)
	}

	s.SetGeo(id, n2.IP(), "DE", "Berlin", "BE", 52.52, 13.40, 2, "New", true, true, true, 10)
	if got := s.rows("")[0]; got.Country != "DE" || got.ASN != 2 {
		t.Fatalf("geo for the current address was discarded: country=%q asn=%d", got.Country, got.ASN)
	}
}

// A record that gains IPv4 changes which endpoint enode resolves, so an IPv6 lookup still in
// flight describes an address the record no longer presents - typically a tunnel broker in
// another city than the IPv4 address.
func TestSetGeoDiscardsResultForADeprioritisedFamily(t *testing.T) {
	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	var id enode.ID
	id[0] = 92
	var r1 enr.Record
	r1.SetSeq(1)
	r1.Set(enr.IPv6{0x20, 0x01, 0x4, 0x70, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x92})
	r1.Set(enr.TCP(30303))
	r1.Set(netconf.EthEntry{ForkID: nw.CurrentForkID()})
	n1 := enode.SignNull(&r1, id)
	r2 := r1
	r2.SetSeq(2)
	r2.Set(enr.IPv4{9, 9, 9, 92})
	n2 := enode.SignNull(&r2, id)

	s := NewWithLimit(0)
	now := time.Unix(1700000000, 0)
	s.Observe(n1, "v5", now)
	if got := n1.IP(); got.To4() != nil {
		t.Fatalf("first record resolved %s, want the IPv6 endpoint", got)
	}
	s.Observe(n2, "v5", now.Add(time.Second))
	if got := n2.IP(); got.To4() == nil {
		t.Fatalf("second record resolved %s, want the IPv4 endpoint", got)
	}

	s.SetGeo(id, n1.IP(), "US", "Fremont", "CA", 37.55, -121.99, 6939, "Tunnel", true, true, true, 100)
	if got := s.rows("")[0]; got.Country != "" || got.ASN != 0 {
		t.Fatalf("geo for the deprioritised family was applied: country=%q asn=%d", got.Country, got.ASN)
	}

	s.SetGeo(id, n2.IP(), "DE", "Berlin", "BE", 52.52, 13.40, 2, "New", true, true, true, 10)
	if got := s.rows("")[0]; got.Country != "DE" {
		t.Fatalf("geo for the preferred address was discarded: country=%q", got.Country)
	}
}

func TestUnclaimedFingerprintSurvivesRefreshInvalidation(t *testing.T) {
	base := elNode(t, 94)
	id := base.ID()
	r1 := *base.Record()
	r1.SetSeq(1)
	n1 := enode.SignNull(&r1, id)
	r2 := r1
	r2.SetSeq(2)
	r2.Set(enr.IPv4{1, 2, 4, 94})
	n2 := enode.SignNull(&r2, id)

	s := NewWithLimit(0)
	now := time.Unix(1700000000, 0)
	s.Observe(n1, "v5", now)
	if !s.ClaimFingerprintAt(id, now) {
		t.Fatal("initial claim failed")
	}
	s.Observe(n2, "v5", now.Add(time.Second))
	if failures := s.SetFingerprint(id, "Nethermind", "v2", "linux", "dotnet", "eth/71", "outbound"); failures != 0 {
		t.Fatalf("inbound failures = %d, want 0", failures)
	}
	if got := s.rows("")[0]; got.Client != "Nethermind" || got.FPStatus != "ok" {
		t.Fatalf("inbound fingerprint not applied: client=%q status=%q", got.Client, got.FPStatus)
	}
	s.SetClaimedFingerprint(id, "Geth", "v1", "linux", "go", "eth/68", "outbound")
	if got := s.rows("")[0]; got.Client != "Nethermind" {
		t.Fatalf("stale claimed result overwrote fresh fingerprint: %q", got.Client)
	}
	if n := s.m[id]; n.fpRefresh || n.fpInFlight {
		t.Fatalf("claim state not cleared: fpRefresh=%v fpInFlight=%v", n.fpRefresh, n.fpInFlight)
	}
}

func TestNewerRecordRefreshesFingerprint(t *testing.T) {
	base := elNode(t, 93)
	id := base.ID()
	r1 := *base.Record()
	r1.SetSeq(1)
	n1 := enode.SignNull(&r1, id)
	r2 := r1
	r2.SetSeq(2)
	r2.Set(enr.IPv4{1, 2, 4, 93})
	n2 := enode.SignNull(&r2, id)

	s := NewWithLimit(0)
	s.Observe(n1, "v5", time.Now())
	s.SetFingerprint(id, "Geth", "v1", "linux", "go", "eth/68", "outbound")
	if !s.Observe(n2, "v5", time.Now().Add(time.Second)) {
		t.Fatal("newer record rejected")
	}
	if row := s.rows("")[0]; row.Client != "Geth" || row.FPStatus != "stale" {
		t.Fatalf("verified fingerprint was not retained during endpoint refresh: %+v", row)
	}
	if !s.ClaimFingerprint(id) {
		t.Fatal("newer record did not become fingerprintable again")
	}
}

func TestNewerRecordDiscardsInFlightFingerprint(t *testing.T) {
	base := elNode(t, 94)
	id := base.ID()
	r1 := *base.Record()
	r1.SetSeq(1)
	n1 := enode.SignNull(&r1, id)
	r2 := r1
	r2.SetSeq(2)
	r2.Set(enr.IPv4{1, 2, 4, 94})
	n2 := enode.SignNull(&r2, id)

	s := NewWithLimit(0)
	s.Observe(n1, "v5", time.Now())
	if !s.ClaimFingerprint(id) {
		t.Fatal("initial fingerprint claim failed")
	}
	s.Observe(n2, "v5", time.Now().Add(time.Second))
	s.SetClaimedFingerprint(id, "Geth", "stale", "linux", "go", "eth/68", "outbound")
	if got := s.rows("")[0].Client; got != "" {
		t.Fatalf("stale in-flight fingerprint was retained as %q", got)
	}
	if !s.ClaimFingerprint(id) {
		t.Fatal("discarded in-flight fingerprint did not release a fresh claim")
	}
}

func TestFingerprintRefreshesAfterTTL(t *testing.T) {
	s := NewWithLimit(0)
	n := elNode(t, 98)
	now := time.Now()
	s.Observe(n, "v5", now)
	s.SetFingerprint(n.ID(), "Geth", "v1", "linux", "go", "eth/68", "outbound")
	s.mu.Lock()
	s.m[n.ID()].fpAt = now.Add(-fpRefreshAge)
	s.mu.Unlock()
	s.Observe(n, "v5", now)
	if row := s.rows("")[0]; row.Client != "Geth" || row.Version != "v1" || row.FPStatus != "stale" {
		t.Fatalf("expired fingerprint was discarded before revalidation: %+v", row)
	}
	if !s.ClaimFingerprint(n.ID()) {
		t.Fatal("expired fingerprint did not become claimable")
	}
	retry := s.FingerprintFailed(n.ID(), now)
	if !retry.Refresh || retry.Attempts != 1 {
		t.Fatalf("refresh failure = %+v, want retained refresh retry", retry)
	}
	if row := s.rows("")[0]; row.Client != "Geth" || row.Version != "v1" || row.FPStatus != "stale" {
		t.Fatalf("refresh failure erased verified fingerprint: %+v", row)
	}
	if !s.ClaimFingerprintAt(n.ID(), retry.RetryAt) {
		t.Fatal("refresh retry was not claimable after backoff")
	}
	s.SetFingerprint(n.ID(), "Nethermind", "v1.39", "linux/x86_64", "dotnet10", "eth/71", "outbound")
	if row := s.rows("")[0]; row.Client != "Nethermind" || row.Version != "v1.39" || row.FPStatus != "ok" {
		t.Fatalf("successful refresh did not atomically replace fingerprint: %+v", row)
	}
}

func TestStaleFingerprintRetrySurvivesSnapshotRestore(t *testing.T) {
	s := NewWithLimit(0)
	n := elNode(t, 99)
	now := time.Unix(1700000000, 0)
	s.Observe(n, "v5", now)
	s.SetFingerprint(n.ID(), "Reth", "v1.8.2", "linux/x86_64", "rust1.88", "eth/71", "outbound")
	s.mu.Lock()
	s.m[n.ID()].fpAt = now.Add(-fpRefreshAge)
	s.mu.Unlock()
	s.Observe(n, "v5", now)
	if !s.ClaimFingerprintAt(n.ID(), now) {
		t.Fatal("stale fingerprint was not claimable")
	}
	retry := s.FingerprintFailed(n.ID(), now)

	restored := NewWithLimit(0)
	restored.Ingest(s.rows(""))
	if row := restored.rows("")[0]; row.Client != "Reth" || row.FPStatus != "stale" {
		t.Fatalf("restored stale fingerprint = %+v", row)
	}
	restoredRetryAt := time.Unix(retry.RetryAt.Unix(), 0)
	if restored.ClaimFingerprintAt(n.ID(), restoredRetryAt.Add(-time.Nanosecond)) {
		t.Fatal("restored stale fingerprint ignored refresh backoff")
	}
	if !restored.ClaimFingerprintAt(n.ID(), restoredRetryAt) {
		t.Fatal("restored stale fingerprint was not refreshable when due")
	}
}

func TestFingerprintStatusDoesNotTrustClientENREntry(t *testing.T) {
	n := &Node{Layer: "cl", IP: "1.2.3.4", TCP: 9000, Client: "Grandine", fpAttempts: fpFailedAfter}
	if got := n.fpStatus(); got != "failed" {
		t.Fatalf("self-declared ENR client status = %q, want failed after probe attempts", got)
	}
	n.fpDone = true
	if got := n.fpStatus(); got != "ok" {
		t.Fatalf("completed fingerprint status = %q, want ok", got)
	}
}

func TestUnclaimFingerprintReleasesQueuedAttempt(t *testing.T) {
	s := NewWithLimit(0)
	n := elNode(t, 4)
	id := n.ID()
	s.Observe(n, "v5", time.Now())
	if !s.ClaimFingerprint(id) {
		t.Fatal("initial claim should succeed")
	}
	s.UnclaimFingerprint(id)
	if !s.ClaimFingerprint(id) {
		t.Fatal("claim should succeed after queued attempt is released")
	}
}

func clQUICNode(t *testing.T, tag byte) *enode.Node {
	t.Helper()
	var r enr.Record
	r.Set(enr.IPv4{1, 2, 3, tag})
	r.Set(enr.QUIC(9001))
	r.Set(netconf.AttnetsEntry{0})
	var id enode.ID
	id[0] = tag
	return enode.SignNull(&r, id)
}

func elQUICNode(t *testing.T, tag byte) *enode.Node {
	t.Helper()
	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(enr.IPv4{1, 2, 3, tag})
	r.Set(enr.QUIC(9001))
	r.Set(netconf.EthEntry{ForkID: nw.CurrentForkID()})
	var id enode.ID
	id[0] = tag
	return enode.SignNull(&r, id)
}

func TestClaimFingerprintCLQUICOnly(t *testing.T) {
	s := NewWithLimit(0)
	n := clQUICNode(t, 20)
	if !s.Observe(n, "v5", time.Now()) {
		t.Fatal("CL QUIC-only node should be recorded")
	}
	if got := s.LayerOf(n.ID()); got != "cl" {
		t.Fatalf("layer = %q, want cl", got)
	}
	if !s.ClaimFingerprint(n.ID()) {
		t.Error("CL node with only a QUIC port should be claimable")
	}
}

func TestClaimFingerprintELQUICOnlyRejected(t *testing.T) {
	s := NewWithLimit(0)
	n := elQUICNode(t, 21)
	if !s.Observe(n, "v5", time.Now()) {
		t.Fatal("EL QUIC-only node should be recorded")
	}
	if got := s.LayerOf(n.ID()); got != "el" {
		t.Fatalf("layer = %q, want el", got)
	}
	if s.ClaimFingerprint(n.ID()) {
		t.Error("EL node with only a QUIC port should not be claimable (RLPx is TCP-only)")
	}
}

func TestClaimFingerprintIPv6OnlyEL(t *testing.T) {
	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(enr.IPv6{0x26, 0x06, 0x47, 0x00, 0x47, 0x00, 0, 0, 0, 0, 0, 0, 0, 0, 0x11, 0x11})
	r.Set(enr.TCP6(30303))
	r.Set(netconf.EthEntry{ForkID: nw.CurrentForkID()})
	var id enode.ID
	id[0] = 89
	n := enode.SignNull(&r, id)
	s := NewWithLimit(0)
	if !s.Observe(n, "v5", time.Now()) {
		t.Fatal("IPv6-only EL node rejected")
	}
	if !s.ClaimFingerprint(id) {
		t.Fatal("IPv6-only EL node was not fingerprintable")
	}
}

func TestObserveCanonicalizesSelfDeclaredClient(t *testing.T) {
	n := elNode(t, 90)
	r := *n.Record()
	r.Set(clientEntry{"erigon", "v3"})
	n = enode.SignNull(&r, n.ID())
	s := NewWithLimit(0)
	if !s.Observe(n, "v5", time.Now()) {
		t.Fatal("node rejected")
	}
	if got := s.rows("")[0].Client; got != "Erigon" {
		t.Fatalf("client = %q, want Erigon", got)
	}
}

func TestParquetRoundTripRestoresNodes(t *testing.T) {
	s := NewWithLimit(0)
	now := time.Unix(1700000000, 0)
	n := elNode(t, 7)
	s.Observe(n, "v5", now)
	s.SetFingerprint(n.ID(), "Geth", "v1.17.4", "linux", "go1.24", "eth/68", "outbound")
	s.SetGeo(n.ID(), n.IP(), "US", "Fond du Lac", "WI", 43.77, -88.45, 0, "", false, false, true, 20)

	data, err := s.ParquetForNetwork("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := RowsFromParquet(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}

	restored := NewWithLimit(0)
	restored.Ingest(rows)
	got := restored.rows("mainnet")
	if len(got) != 1 {
		t.Fatalf("restored rows = %d, want 1", len(got))
	}
	if got[0].ID != rows[0].ID || got[0].Client != "Geth" || got[0].Subdivision != "WI" || got[0].Score != rows[0].Score || got[0].FirstSeen != now.Unix() {
		t.Errorf("round-trip mismatch: got %+v want %+v", got[0], rows[0])
	}
	if restored.ClaimFingerprint(n.ID()) {
		t.Error("restored fingerprinted node should not be re-claimed")
	}
}

func TestIngestDoesNotPromoteSelfDeclaredClientToFingerprint(t *testing.T) {
	var id enode.ID
	id[0] = 42
	s := NewWithLimit(0)
	s.Ingest([]Row{{
		ID: id.String(), Layer: "cl", Network: "mainnet", IP: "1.2.3.4", TCP: 9000,
		Client: "Grandine", Version: "2.0.5", FPStatus: "pending",
	}})
	if !s.ClaimFingerprint(id) {
		t.Fatal("restored ENR client metadata should still require a fingerprint probe")
	}
}

func TestRowsFromEmptyParquet(t *testing.T) {
	data, err := NewWithLimit(0).ParquetForNetwork("sepolia")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := RowsFromParquet(data)
	if err != nil {
		t.Fatalf("read empty parquet: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %d, want 0", len(rows))
	}
}

func TestRowsFromParquetDefaultsMissingAdditiveColumns(t *testing.T) {
	type legacyGeoRow struct {
		ID      string `parquet:"id"`
		Country string `parquet:"country"`
		City    string `parquet:"city"`
	}
	var buf bytes.Buffer
	if err := parquet.Write(&buf, []legacyGeoRow{{ID: "legacy", Country: "US", City: "Fond du Lac"}}); err != nil {
		t.Fatal(err)
	}
	rows, err := RowsFromParquet(buf.Bytes())
	if err != nil {
		t.Fatalf("read legacy parquet: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "legacy" || rows[0].City != "Fond du Lac" || rows[0].Subdivision != "" {
		t.Fatalf("legacy rows = %+v", rows)
	}
}

func TestParquetPerNetworkFilters(t *testing.T) {
	s := NewWithLimit(0)
	s.Observe(elNode(t, 4), "v5", time.Now())
	if got := s.CountForNetwork("mainnet"); got != 1 {
		t.Errorf("mainnet count = %d, want 1", got)
	}
	if got := s.CountForNetwork("sepolia"); got != 0 {
		t.Errorf("sepolia count = %d, want 0", got)
	}
	data, err := s.ParquetForNetwork("mainnet")
	if err != nil {
		t.Fatalf("parquet: %v", err)
	}
	if len(data) == 0 {
		t.Error("mainnet parquet is empty")
	}
}

// internal/query compares fork_hash with a bare lower(), so the equivalence between the
// SQL predicate and netconf's laxer normalizer holds only while every producer emits this
// canonical form. See TestForkCurrencyNormalizationDivergence.
func TestRowForkHashIsCanonicalHex(t *testing.T) {
	canonical := regexp.MustCompile(`^[0-9a-f]{8}$`)
	upper := [4]byte{0xAB, 0xCD, 0xEF, 0x01}

	var el enr.Record
	el.Set(enr.IPv4{5, 6, 7, 8})
	el.Set(netconf.EthEntry{ForkID: forkid.ID{Hash: upper, Next: 7}})
	var elID enode.ID
	elID[0] = 1
	if got := extract(enode.SignNull(&el, elID)).forkHash; !canonical.MatchString(got) {
		t.Errorf("EL fork hash = %q, want 8 lowercase hex digits with no 0x prefix", got)
	}

	var cl enr.Record
	cl.Set(enr.IPv4{5, 6, 7, 9})
	eth2 := make([]byte, 16)
	copy(eth2[:4], upper[:])
	cl.Set(netconf.Eth2Entry(eth2))
	var clID enode.ID
	clID[0] = 2
	if got := extract(enode.SignNull(&cl, clID)).forkHash; !canonical.MatchString(got) {
		t.Errorf("CL fork hash = %q, want 8 lowercase hex digits with no 0x prefix", got)
	}
}

func TestExtractCLForkNextFromSpecENRForkID(t *testing.T) {
	var r enr.Record
	r.Set(enr.IPv4{5, 6, 7, 9})
	eth2 := make([]byte, 16)
	copy(eth2[:4], []byte{0xd2, 0xf1, 0x99, 0x7f})
	binary.LittleEndian.PutUint64(eth2[8:16], 2048)
	r.Set(netconf.Eth2Entry(eth2))
	var id enode.ID
	id[0] = 42
	e := extract(enode.SignNull(&r, id))
	if e.layer != "cl" {
		t.Fatalf("layer = %q, want cl", e.layer)
	}
	if e.forkNext != 2048 {
		t.Errorf("forkNext = %d, want 2048 (next_fork_epoch at bytes [8:16] of the 16-byte ENRForkID)", e.forkNext)
	}
}

func TestCurrentNodeReturnsTrackedRecord(t *testing.T) {
	s := NewWithLimit(0)
	if s.CurrentNode(elNode(t, 99).ID()) != nil {
		t.Fatal("absent id should return nil")
	}
	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(enr.IPv4{1, 2, 3, 7})
	r.Set(enr.TCP(30303))
	r.Set(netconf.EthEntry{ForkID: nw.CurrentForkID()})
	if err := enode.SignV4(&r, key); err != nil {
		t.Fatal(err)
	}
	n, err := enode.New(enode.ValidSchemes, &r)
	if err != nil {
		t.Fatal(err)
	}
	s.Observe(n, "v5", time.Unix(1700000000, 0))
	got := s.CurrentNode(n.ID())
	if got == nil || got.ID() != n.ID() {
		t.Fatalf("CurrentNode = %v, want id %v", got, n.ID())
	}
}

// A devnet seed with no fork entry and no TCP port keeps its forced network but never gains a
// layer. Publishing it would break the manifest's total = EL + CL invariant and block every
// publish, so it must stay in the set for retry and out of the snapshot.
func TestLayerlessForcedRecordIsRetainedButNotPublished(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(enr.IPv4{1, 2, 3, 111})
	r.Set(enr.UDP(30303))
	if err := enode.SignV4(&r, key); err != nil {
		t.Fatal(err)
	}
	n, err := enode.New(enode.ValidSchemes, &r)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0)
	s := NewWithLimit(0)
	if !s.ObserveSeedResult(n, "devnet", now).Accepted {
		t.Fatal("devnet seed rejected")
	}
	if got := s.LayerOf(n.ID()); got != "" {
		t.Fatalf("layer = %q, want empty for a record with no fork entry and no TCP port", got)
	}
	if s.Len() != 1 {
		t.Fatalf("set holds %d nodes, want the seed retained for retry", s.Len())
	}
	if got := len(s.SnapshotNetworks([]string{"devnet"})["devnet"]); got != 0 {
		t.Fatalf("published %d layer-less rows, want 0", got)
	}
	data, err := s.ParquetForNetwork("devnet")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := RowsFromParquet(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("parquet holds %d layer-less rows, want 0", len(rows))
	}
}

func elNodeSeq(t *testing.T, tag byte, seq uint64, port uint16) *enode.Node {
	t.Helper()
	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.SetSeq(seq)
	r.Set(enr.IPv4{1, 2, 3, tag})
	r.Set(enr.TCP(port))
	r.Set(netconf.EthEntry{ForkID: nw.CurrentForkID()})
	var id enode.ID
	id[0] = tag
	return enode.SignNull(&r, id)
}

func TestObserveAuthenticatedELRecordsStatusForkForSignedForklessRecord(t *testing.T) {
	var r enr.Record
	r.SetSeq(1)
	r.Set(enr.IPv4{8, 8, 8, 8})
	r.Set(enr.TCP(30303))
	var id enode.ID
	id[0] = 0x51
	n := enode.SignNull(&r, id)
	s := NewWithLimit(0)
	if observed := s.ObserveAuthenticatedEL(n, "inbound", "mainnet", "07c9462e", 0, time.Now()); !observed.Applied {
		t.Fatalf("observation = %+v", observed)
	}
	row := s.rows("mainnet")[0]
	if row.ForkHash != "07c9462e" || row.ForkSource != "status" {
		t.Fatalf("fork = %q/%q, want authenticated Status fork recorded despite Seq > 0", row.ForkHash, row.ForkSource)
	}
}

func TestObserveAuthenticatedELStaleRecordDoesNotStampMembership(t *testing.T) {
	s := NewWithLimit(0)
	now := time.Unix(1700000000, 0)
	if !s.Observe(elNodeSeq(t, 0x56, 2, 30303), "v5", now) {
		t.Fatal("newer record was not observed")
	}
	stale := elNodeSeq(t, 0x56, 1, 30303)
	observed := s.ObserveAuthenticatedEL(stale, "inbound", "mainnet", "07c9462e", 0, now.Add(time.Second))
	if !observed.Accepted || observed.Applied {
		t.Fatalf("stale observation = %+v, want accepted but not applied", observed)
	}
	row := s.rows("mainnet")[0]
	if row.MembershipSource == "status" || row.MembershipVerifiedAt != 0 {
		t.Fatalf("stale authenticated observation stamped membership: %q at %v", row.MembershipSource, row.MembershipVerifiedAt)
	}
}

func TestSetClaimedFingerprintReportsDiscardAfterRecordChange(t *testing.T) {
	s := NewWithLimit(0)
	now := time.Unix(1700000000, 0)
	unchanged := elNodeSeq(t, 0x54, 1, 30303)
	s.Observe(unchanged, "v5", now)
	if !s.ClaimFingerprintAt(unchanged.ID(), now) {
		t.Fatal("claim failed")
	}
	if _, applied := s.SetClaimedFingerprintAt(unchanged.ID(), "Geth", "v1", "linux", "go", "eth/68", "outbound", now.Add(time.Second)); !applied {
		t.Fatal("unchanged-record claimed completion was discarded")
	}

	changed := elNodeSeq(t, 0x55, 1, 30303)
	s.Observe(changed, "v5", now)
	if !s.ClaimFingerprintAt(changed.ID(), now) {
		t.Fatal("claim failed")
	}
	s.Observe(elNodeSeq(t, 0x55, 2, 30304), "v5", now.Add(time.Second))
	if _, applied := s.SetClaimedFingerprintAt(changed.ID(), "Geth", "v1", "linux", "go", "eth/68", "outbound", now.Add(2*time.Second)); applied {
		t.Fatal("post-change claimed completion was applied")
	}
}

func TestUnclaimFingerprintAfterInboundCompletionLeavesNodeClaimable(t *testing.T) {
	s := NewWithLimit(0)
	now := time.Unix(1700000000, 0)
	n := elNodeSeq(t, 0x53, 1, 30303)
	s.Observe(n, "v5", now)
	if !s.ClaimFingerprintAt(n.ID(), now) {
		t.Fatal("initial claim failed")
	}
	s.Observe(elNodeSeq(t, 0x53, 2, 30304), "v5", now.Add(time.Second))
	s.SetFingerprintAt(n.ID(), "Geth", "v1", "linux", "go", "eth/68", "inbound", now.Add(2*time.Second))
	s.UnclaimFingerprint(n.ID())
	s.Observe(elNodeSeq(t, 0x53, 3, 30305), "v5", now.Add(3*time.Second))
	if !s.ClaimFingerprintAt(n.ID(), now.Add(4*time.Second)) {
		t.Fatal("node stayed unclaimable after the claim owner unclaimed it")
	}
}
