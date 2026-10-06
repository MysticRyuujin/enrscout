package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
)

const sepoliaAmsterdam = 1791294816

type sepoliaRecord struct {
	next     uint64 // ENR eth Next; the row's fork_next mirrors it unless rowNext is set
	rowNext  *uint64
	score    int32
	client   string
	v6       bool
	ownPort6 bool
}

func sepoliaELRow(t *testing.T, i int, rec sepoliaRecord, at time.Time) nodeset.Row {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	nw, err := netconf.Get("sepolia")
	if err != nil {
		t.Fatal(err)
	}
	id := forkid.ID{Hash: nw.CurrentForkIDAt(at).Hash, Next: rec.next}
	ip := net.IPv4(1, 2, byte(i/250), byte(i%250+1))
	var r enr.Record
	r.Set(enr.IPv4(ip.To4()))
	r.Set(enr.TCP(30303))
	r.Set(netconf.EthEntry{ForkID: id})
	r.Set(enr.WithEntry("snap", []uint{}))
	row := nodeset.Row{
		IP: ip.String(), TCP: 30303, Score: rec.score, HasV5: true, LastSeen: at.Unix(),
		Layer: "el", Network: "sepolia", ForkHash: hex.EncodeToString(id.Hash[:]), ForkNext: rec.next,
		Client: rec.client, FPStatus: "ok",
	}
	if rec.v6 {
		ip6 := net.ParseIP(fmt.Sprintf("2001:db8::%x", i+1))
		r.Set(enr.IPv6(ip6))
		row.IP6 = ip6.String()
		if rec.ownPort6 {
			r.Set(enr.TCP6(30303))
			row.TCP6 = 30303
		}
	}
	if rec.rowNext != nil {
		row.ForkNext = *rec.rowNext
	}
	if err := enode.SignV4(&r, key); err != nil {
		t.Fatal(err)
	}
	n, err := enode.New(enode.ValidSchemes, &r)
	if err != nil {
		t.Fatal(err)
	}
	b, err := rlp.EncodeToBytes(&r)
	if err != nil {
		t.Fatal(err)
	}
	row.ID, row.ENR = n.ID().String(), "enr:"+base64.RawURLEncoding.EncodeToString(b)
	return row
}

var beforeAmsterdam = time.Unix(sepoliaAmsterdam-3600, 0)

func readySelectOpts(limit int) selectOpts {
	return selectOpts{minScore: 1, protocol: "any", layer: "el", limit: limit, balance: balanceProportional}
}

// Ready records are ranked below in score on purpose: readiness has to win over score.
func TestReadyRecordsFillEachClientQuotaFirst(t *testing.T) {
	var rows []nodeset.Row
	for i, client := range []string{"Geth", "Besu"} {
		for j := range 10 {
			rec := sepoliaRecord{next: sepoliaAmsterdam, score: 5, client: client}
			if j >= 6 {
				rec = sepoliaRecord{next: 0, score: 50, client: client}
			}
			rows = append(rows, sepoliaELRow(t, i*10+j, rec, beforeAmsterdam))
		}
	}
	picked := pick(rankCandidates(rows, readySelectOpts(10), beforeAmsterdam), readySelectOpts(10))
	perClient := map[string]int{}
	for _, c := range picked {
		perClient[c.client]++
		if c.rank != rankReady {
			t.Errorf("picked %s record ranked %d while ready records of that client were left out", c.client, c.rank)
		}
	}
	if perClient["Geth"] != 5 || perClient["Besu"] != 5 {
		t.Fatalf("per-client counts = %v, want the proportional 5/5 unchanged by readiness", perClient)
	}
}

func TestReadinessRanksMismatchLastAndReadsTheRecord(t *testing.T) {
	fork := uint64(sepoliaAmsterdam)
	rows := []nodeset.Row{
		sepoliaELRow(t, 0, sepoliaRecord{next: sepoliaAmsterdam + 600, score: 90, client: "Geth"}, beforeAmsterdam),
		sepoliaELRow(t, 1, sepoliaRecord{next: 0, rowNext: &fork, score: 80, client: "Geth"}, beforeAmsterdam),
		sepoliaELRow(t, 2, sepoliaRecord{next: 0, score: 70, client: "Geth"}, beforeAmsterdam),
		sepoliaELRow(t, 3, sepoliaRecord{next: sepoliaAmsterdam, score: 10, client: "Geth"}, beforeAmsterdam),
	}
	cands := rankCandidates(rows, readySelectOpts(0), beforeAmsterdam)
	var got []string
	for _, c := range cands {
		got = append(got, fmt.Sprintf("%s:%d", c.row.IP, c.rank))
	}
	// The row claiming the fork time over Status still advertises Next = 0 in the record peers read.
	want := []string{"1.2.0.4:0", "1.2.0.2:1", "1.2.0.3:1", "1.2.0.1:2"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rank order = %v, want %v", got, want)
	}
}

func TestIPv6ReservationKeepsExplicitPortsFirst(t *testing.T) {
	rows := []nodeset.Row{
		sepoliaELRow(t, 0, sepoliaRecord{next: sepoliaAmsterdam, score: 90, client: "Geth", v6: true}, beforeAmsterdam),
		sepoliaELRow(t, 1, sepoliaRecord{next: 0, score: 10, client: "Geth", v6: true, ownPort6: true}, beforeAmsterdam),
	}
	picked := pick(rankCandidates(rows, readySelectOpts(1), beforeAmsterdam), readySelectOpts(1))
	if len(picked) != 1 || picked[0].row.IP != "1.2.0.2" {
		t.Fatalf("picked %v, want the explicit tcp6 record: dialability by discv5-crate consumers comes first", picked)
	}

	rows = []nodeset.Row{
		sepoliaELRow(t, 0, sepoliaRecord{next: 0, score: 90, client: "Geth", v6: true, ownPort6: true}, beforeAmsterdam),
		sepoliaELRow(t, 1, sepoliaRecord{next: sepoliaAmsterdam, score: 10, client: "Geth", v6: true, ownPort6: true}, beforeAmsterdam),
	}
	picked = pick(rankCandidates(rows, readySelectOpts(1), beforeAmsterdam), readySelectOpts(1))
	if len(picked) != 1 || picked[0].rank != rankReady {
		t.Fatalf("picked %v, want the ready record inside the explicit-port tier", picked)
	}
}

func TestCLRecordSchedulingTheTargetRanksReady(t *testing.T) {
	state, err := netconf.CLForkStateAt("sepolia", beforeAmsterdam)
	if err != nil {
		t.Fatal(err)
	}
	clRecord := func(i int, epoch uint64) nodeset.Row {
		key, err := crypto.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		entry := make(netconf.Eth2Entry, 16)
		copy(entry[:4], state.Digest[:])
		copy(entry[4:8], state.NextForkVersion[:])
		binary.LittleEndian.PutUint64(entry[8:], epoch)
		var r enr.Record
		r.Set(enr.IPv4{1, 2, 3, byte(i)})
		r.Set(enr.TCP(9000))
		r.Set(entry)
		if err := enode.SignV4(&r, key); err != nil {
			t.Fatal(err)
		}
		n, err := enode.New(enode.ValidSchemes, &r)
		if err != nil {
			t.Fatal(err)
		}
		b, err := rlp.EncodeToBytes(&r)
		if err != nil {
			t.Fatal(err)
		}
		return nodeset.Row{
			ID: n.ID().String(), ENR: "enr:" + base64.RawURLEncoding.EncodeToString(b),
			IP: net.IPv4(1, 2, 3, byte(i)).String(), TCP: 9000, Score: 5, HasV5: true, LastSeen: beforeAmsterdam.Unix(),
			Layer: "cl", Network: "sepolia", ForkHash: hex.EncodeToString(state.Digest[:]),
		}
	}
	opt := selectOpts{minScore: 1, protocol: "any", layer: "cl"}
	cands := rankCandidates([]nodeset.Row{clRecord(1, math.MaxUint64), clRecord(2, state.NextForkEpoch)}, opt, beforeAmsterdam)
	if len(cands) != 2 || cands[0].row.IP != "1.2.3.2" || cands[0].rank != rankReady || cands[1].rank != rankNeutral {
		t.Fatalf("CL ranks = %+v, want the record scheduling the target first and ready", cands)
	}
}

func readinessTreeConfig(t *testing.T, outDir string, publisher recordPublisher) multiConfig {
	t.Helper()
	key, err := crypto.HexToECDSA(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	return multiConfig{
		baseDomain: "nodes.example.org", key: key, outDir: outDir, publisher: publisher,
		sel:          selectOpts{minScore: 1, protocol: "any", layer: "el", balance: balanceProportional},
		minTreeNodes: 1, maxDropPct: 50, publishEvery: 6 * time.Hour,
	}
}

func seedSepoliaBaseline(t *testing.T, outDir, suffix string, nodes int, seq uint64) {
	t.Helper()
	for _, capability := range []string{"all", "snap"} {
		domain := capability + ".sepolia.nodes.example.org"
		seedArtifact(t, outDir, domain+suffix, output{
			SchemaVersion: outputSchemaVersion, URL: "enrtree://X@" + domain, Domain: domain,
			Network: "sepolia", Capability: capability, Nodes: nodes, Seq: seq,
			Records: map[string]string{domain: fmt.Sprintf("enrtree-root:v1 seq=%d", seq)},
		})
	}
}

func readinessRows(t *testing.T, ready, notReady int, at time.Time) []nodeset.Row {
	var rows []nodeset.Row
	for i := range ready + notReady {
		next := uint64(sepoliaAmsterdam)
		if i >= ready {
			next = 0
		}
		rows = append(rows, sepoliaELRow(t, i, sepoliaRecord{next: next, score: 5, client: "Geth"}, at))
	}
	return rows
}

func allTree(t *testing.T, trees []builtTree) builtTree {
	t.Helper()
	for _, tree := range trees {
		if tree.Capability == "all" {
			return tree
		}
	}
	t.Fatal("no all tree")
	return builtTree{}
}

func TestReadyOnlyTreeNearActivation(t *testing.T) {
	cases := []struct {
		name          string
		at            time.Time
		previous      int
		wantNodes     int
		wantReadyOnly bool
	}{
		{"inside the window", beforeAmsterdam, 0, 7, true},
		{"outside the window", time.Unix(sepoliaAmsterdam, 0).Add(-24 * time.Hour), 0, 10, false},
		{"ready-only would collapse", beforeAmsterdam, 20, 10, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outDir := t.TempDir()
			cfg := readinessTreeConfig(t, outDir, nil)
			if tc.previous > 0 {
				seedSepoliaBaseline(t, outDir, "", tc.previous, uint64(tc.at.Add(-6*time.Hour).Unix()))
			}
			trees, skip, err := buildNetworkTrees(readinessRows(t, 7, 3, tc.at), "sepolia", tc.at, tc.at, cfg, map[string]uint64{})
			if err != nil || skip.reason != "" {
				t.Fatalf("skipped as %q: %v", skip.reason, err)
			}
			got := allTree(t, trees)
			if got.Nodes != tc.wantNodes || got.readyOnly != tc.wantReadyOnly {
				t.Fatalf("all tree = %d nodes, ready-only %v; want %d, %v", got.Nodes, got.readyOnly, tc.wantNodes, tc.wantReadyOnly)
			}
		})
	}
}

// The published artifact dates the baseline, not the build floor: a failed exempt push advances the
// build sequence past activation, and reading that instead would close the exemption and wedge the
// domain again.
func TestCollapseExemptionAfterActivation(t *testing.T) {
	activation := time.Unix(sepoliaAmsterdam, 0)
	for _, tc := range []struct {
		name     string
		at       time.Time
		builtSeq uint64
		wantSkip string
	}{
		{"first cycle keeps the pre-fork tree", activation.Add(time.Hour), 0, "collapse"},
		{"a cycle later the drop is accepted", activation.Add(7 * time.Hour), 0, ""},
		{"a failed exempt push does not close the exemption", activation.Add(13 * time.Hour), uint64(activation.Add(7 * time.Hour).Unix()), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outDir := t.TempDir()
			cfg := readinessTreeConfig(t, outDir, &stubPublisher{})
			publishedSeq := uint64(activation.Add(-2 * time.Hour).Unix())
			seedSepoliaBaseline(t, outDir, publishedSuffix, 100, publishedSeq)
			seedSepoliaBaseline(t, outDir, "", 100, max(publishedSeq, tc.builtSeq))
			rows := readinessRows(t, 0, 10, tc.at)
			trees, skip, err := buildNetworkTrees(rows, "sepolia", tc.at, tc.at, cfg, map[string]uint64{})
			if err != nil {
				t.Fatal(err)
			}
			if skip.reason != tc.wantSkip {
				t.Fatalf("skip = %q, want %q", skip.reason, tc.wantSkip)
			}
			if tc.wantSkip == "" && allTree(t, trees).Nodes != 10 {
				t.Fatalf("trees = %+v, want the 10-node post-fork tree", trees)
			}
		})
	}

	outDir := t.TempDir()
	cfg := readinessTreeConfig(t, outDir, &stubPublisher{})
	at := activation.Add(10 * 24 * time.Hour)
	seedSepoliaBaseline(t, outDir, publishedSuffix, 100, uint64(at.Add(-6*time.Hour).Unix()))
	seedSepoliaBaseline(t, outDir, "", 100, uint64(at.Add(-6*time.Hour).Unix()))
	if _, skip, err := buildNetworkTrees(readinessRows(t, 0, 10, at), "sepolia", at, at, cfg, map[string]uint64{}); err != nil || skip.reason != "collapse" {
		t.Fatalf("skip = %q (%v), want collapse with no activation since the last publish", skip.reason, err)
	}
}

// RegisterDevnet is process-global and singular, so this is the only test in the package that may
// call it. The devnet's CL BPO activates at 1700038400 and its EL BPO at 1700050000.
func TestCollapseExemptionCountsOnlyTheTreeLayer(t *testing.T) {
	if _, err := netconf.Get("devnet"); err != nil {
		registerLayerSplitDevnet(t)
	}
	clFork := [2]time.Time{time.Unix(1700030000, 0), time.Unix(1700040000, 0)}
	elFork := [2]time.Time{time.Unix(1700045000, 0), time.Unix(1700060000, 0)}
	for _, tc := range []struct {
		layer  string
		window [2]time.Time
		want   bool
	}{
		{"el", clFork, false}, {"cl", clFork, true}, {"any", clFork, true},
		{"el", elFork, true}, {"cl", elFork, false}, {"any", elFork, true},
	} {
		if got := forkActivatedBetween("devnet", tc.layer, tc.window[0], tc.window[1]); got != tc.want {
			t.Errorf("layer %s over %d..%d = %v, want %v", tc.layer, tc.window[0].Unix(), tc.window[1].Unix(), got, tc.want)
		}
	}
}

func registerLayerSplitDevnet(t *testing.T) {
	t.Helper()
	err := netconf.RegisterDevnet(netconf.DevnetConfig{
		ELGenesisJSON: []byte(`{
	"config": {"chainId": 3151908, "homesteadBlock": 0, "eip150Block": 0,
		"eip155Block": 0, "eip158Block": 0, "byzantiumBlock": 0, "constantinopleBlock": 0,
		"petersburgBlock": 0, "istanbulBlock": 0, "berlinBlock": 0, "londonBlock": 0,
		"mergeNetsplitBlock": 0, "terminalTotalDifficulty": 0,
		"shanghaiTime": 0, "cancunTime": 0, "pragueTime": 0, "osakaTime": 0, "bpo1Time": 1700050000},
	"difficulty": "0x1", "gasLimit": "0x1c9c380", "timestamp": "0x0", "alloc": {}
}`),
		NetworkID:             3151909,
		GenesisValidatorsRoot: "0x7033d675f49ab2e76ffba52871d1ff7b73914b3a5ac8e1c0986c19549d67c0d7",
		GenesisTime:           1700000000, SecondsPerSlot: 12, SlotsPerEpoch: 32,
		CLForks:       []netconf.CLForkConfig{{Epoch: 0, Version: "0x10000038"}, {Epoch: 0, Version: "0x60000038"}, {Epoch: 0, Version: "0x70000038"}},
		FuluForkEpoch: 0,
		BlobSchedule:  []netconf.BlobParams{{Epoch: 0, MaxBlobs: 9}, {Epoch: 0, MaxBlobs: 15}, {Epoch: 100, MaxBlobs: 21}},
		BootnodeRecords: []string{
			"enode://a979fb575495b8d6db44f750317d0f4622bf4c2aa3365d6af7c284339968eef29b69ad0dce72a4d8db5ebb4968de0e3bec910127f134779fbcb0cb6d3331163c@52.16.188.185:30303",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}
