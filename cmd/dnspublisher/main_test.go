package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/dnsdisc"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/google/uuid"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
	"github.com/MysticRyuujin/enrscout/internal/snapshot"
	"github.com/MysticRyuujin/enrscout/internal/store"
)

const testKeyHex = "b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291"

func v4Row(t *testing.T, last byte, tcp int, score int32, now time.Time) nodeset.Row {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(enr.IPv4{1, 2, 3, last})
	if tcp > 0 {
		r.Set(enr.TCP(uint16(tcp)))
	}
	r.Set(netconf.EthEntry{ForkID: nw.CurrentForkIDAt(now)})
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
		IP: net.IPv4(1, 2, 3, last).String(), TCP: int32(tcp), Score: score,
		HasV5: true, LastSeen: now.Unix(),
	}
}

func TestSelectNodesDialableOnly(t *testing.T) {
	now := time.Unix(1700000000, 0)
	rows := []nodeset.Row{
		currentMainnetEL(t, v4Row(t, 1, 30303, 5, now), now),
		currentMainnetEL(t, v4Row(t, 2, 0, 5, now), now),
		currentMainnetEL(t, v4Row(t, 3, 30303, 0, now), now),
	}
	got := selectNodes(rows, selectOpts{minScore: 1, protocol: "any"}, now)
	if len(got) != 1 {
		t.Fatalf("selected %d nodes, want 1 (dialable + scored)", len(got))
	}
}

func TestSelectNodesRejectsStaleExecutionFork(t *testing.T) {
	now := time.Unix(1700000000, 0)
	current := v4Row(t, 1, 30303, 5, now)
	current.Layer = "el"
	current.Network = "mainnet"
	nw, _ := netconf.Get("mainnet")
	fork := nw.CurrentForkIDAt(now).Hash
	current.ForkHash = hex.EncodeToString(fork[:])
	stale := v4Row(t, 2, 30303, 5, now)
	stale.Layer = "el"
	stale.Network = "mainnet"
	stale.ForkHash = "9f3d2254"

	// Mainnet Frontier with its canonical Next: an earlier era that EIP-2124 accepts for
	// peering, but a published tree should carry working peers, not syncing ones.
	earlier := v4Row(t, 3, 30303, 5, now)
	earlier.Layer = "el"
	earlier.Network = "mainnet"
	earlier.ForkHash, earlier.ForkNext = "fc64ec04", 1150000

	got := selectNodes([]nodeset.Row{stale, earlier, current}, selectOpts{minScore: 1, protocol: "any"}, now)
	if len(got) != 1 || got[0].ID().String() != current.ID {
		t.Fatalf("selected %v, want only current-fork execution node %s", got, current.ID)
	}
}

func TestSelectNodesRejectsIncompatibleExecutionForkNext(t *testing.T) {
	now := time.Now().UTC()
	nw, _ := netconf.Get("mainnet")
	currentID := nw.CurrentForkIDAt(now)

	compatible := v4Row(t, 1, 30303, 5, now)
	compatible.Layer = "el"
	compatible.Network = "mainnet"
	compatible.ForkHash = hex.EncodeToString(currentID.Hash[:])
	compatible.ForkNext = currentID.Next

	incompatible := v4Row(t, 2, 30303, 5, now)
	incompatible.Layer = "el"
	incompatible.Network = "mainnet"
	incompatible.ForkHash = compatible.ForkHash
	incompatible.ForkNext = 1

	got := selectNodes([]nodeset.Row{incompatible, compatible}, selectOpts{minScore: 1, protocol: "any"}, now)
	if len(got) != 1 || got[0].ID().String() != compatible.ID {
		t.Fatalf("selected %v, want only EIP-2124-compatible node %s", got, compatible.ID)
	}
}

func TestReachableFallbackAndFamilies(t *testing.T) {
	cases := []struct {
		name string
		row  nodeset.Row
		want bool
	}{
		{"v4 tcp", nodeset.Row{IP: "1.2.3.4", TCP: 30303}, true},
		{"v4 quic only", nodeset.Row{IP: "1.2.3.4", QUIC: 30303}, true},
		{"v4 no ports", nodeset.Row{IP: "1.2.3.4"}, false},
		{"tcp but no ip", nodeset.Row{TCP: 30303}, false},
		{"v6-only tcp6", nodeset.Row{IP6: "2001:db8::1", TCP6: 30303}, true},
		{"v6-only tcp fallback", nodeset.Row{IP6: "2001:db8::1", TCP: 30303}, true},
		{"v6-only quic fallback", nodeset.Row{IP6: "2001:db8::1", QUIC: 9000}, true},
		{"v6-only no ports", nodeset.Row{IP6: "2001:db8::1"}, false},
	}
	for _, c := range cases {
		if got := c.row.Dialable(); got != c.want {
			t.Errorf("%s: dialable = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSelectNodesProtocolAndLimit(t *testing.T) {
	now := time.Unix(1700000000, 0)
	rows := []nodeset.Row{
		currentMainnetEL(t, v4Row(t, 1, 30303, 5, now), now),
		currentMainnetEL(t, v4Row(t, 2, 30303, 5, now), now),
		currentMainnetEL(t, v4Row(t, 3, 30303, 5, now), now),
	}
	if got := selectNodes(rows, selectOpts{minScore: 1, protocol: "v4"}, now); len(got) != 0 {
		t.Errorf("protocol=v4 should exclude v5-only nodes, got %d", len(got))
	}
	if got := selectNodes(rows, selectOpts{minScore: 1, protocol: "v5", limit: 2}, now); len(got) != 2 {
		t.Errorf("limit=2 should cap selection, got %d", len(got))
	}
}

func v4RowSnap(t *testing.T, last byte, now time.Time) nodeset.Row {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(enr.IPv4{1, 2, 3, last})
	r.Set(enr.TCP(30303))
	r.Set(enr.WithEntry("snap", uint(1)))
	r.Set(netconf.EthEntry{ForkID: nw.CurrentForkIDAt(now)})
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
		IP: net.IPv4(1, 2, 3, last).String(), TCP: 30303, Score: 5,
		HasV5: true, LastSeen: now.Unix(),
	}
}

// clRow signs an ENR carrying the current mainnet eth2 digest in the 16-byte SSZ ENRForkID shape.
func clRow(t *testing.T, last byte, now time.Time) nodeset.Row {
	t.Helper()
	state, err := netconf.CLForkStateAt("mainnet", now)
	if err != nil {
		t.Fatal(err)
	}
	entry := make(netconf.Eth2Entry, 16)
	copy(entry, state.Digest[:])
	return clRowEntry(t, last, entry, now)
}

// clRowEntry takes the eth2 value verbatim so a caller can advertise a digest the columns disagree with.
func clRowEntry(t *testing.T, last byte, entry netconf.Eth2Entry, now time.Time) nodeset.Row {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	state, err := netconf.CLForkStateAt("mainnet", now)
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(enr.IPv4{1, 2, 3, last})
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
		IP: net.IPv4(1, 2, 3, last).String(), TCP: 9000, Score: 5,
		HasV5: true, LastSeen: now.Unix(),
		Layer: "cl", Network: "mainnet", ForkHash: hex.EncodeToString(state.Digest[:]),
	}
}

func currentMainnetEL(t *testing.T, row nodeset.Row, now time.Time) nodeset.Row {
	t.Helper()
	nw, _ := netconf.Get("mainnet")
	id := nw.CurrentForkIDAt(now)
	row.Layer, row.Network = "el", "mainnet"
	row.ForkHash, row.ForkNext = hex.EncodeToString(id.Hash[:]), id.Next
	return row
}

func TestSelectNodesLayerFilter(t *testing.T) {
	now := time.Unix(1700000000, 0)
	el := currentMainnetEL(t, v4Row(t, 1, 30303, 5, now), now)
	if got := selectNodes([]nodeset.Row{el}, selectOpts{minScore: 1, protocol: "any", layer: "el"}, now); len(got) != 1 {
		t.Fatalf("layer=el should keep the EL node, got %d", len(got))
	}
	if got := selectNodes([]nodeset.Row{el}, selectOpts{minScore: 1, protocol: "any", layer: "cl"}, now); len(got) != 0 {
		t.Fatalf("layer=cl should exclude the EL node, got %d", len(got))
	}
	if got := selectNodes([]nodeset.Row{el}, selectOpts{minScore: 1, protocol: "any", layer: "any"}, now); len(got) != 1 {
		t.Fatalf("layer=any should keep the EL node, got %d", len(got))
	}
}

func TestSelectNodesCapabilitySnap(t *testing.T) {
	now := time.Unix(1700000000, 0)
	plain := currentMainnetEL(t, v4Row(t, 1, 30303, 5, now), now)
	snap := currentMainnetEL(t, v4RowSnap(t, 2, now), now)
	rows := []nodeset.Row{plain, snap}

	if got := selectNodes(rows, selectOpts{minScore: 1, protocol: "any", layer: "el", capability: "all"}, now); len(got) != 2 {
		t.Fatalf("capability=all should keep both, got %d", len(got))
	}
	got := selectNodes(rows, selectOpts{minScore: 1, protocol: "any", layer: "el", capability: "snap"}, now)
	if len(got) != 1 || got[0].ID().String() != snap.ID {
		t.Fatalf("capability=snap should keep only the snap-advertising node, got %v", got)
	}
}

// ethEntryRow takes the "eth" value as raw bytes so a caller can build a malformed one.
func ethEntryRow(t *testing.T, last byte, value []byte, now time.Time) nodeset.Row {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(enr.IPv4{1, 2, 3, last})
	r.Set(enr.TCP(30303))
	r.Set(enr.WithEntry("eth", rlp.RawValue(value)))
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
	row := nodeset.Row{
		ID: n.ID().String(), ENR: "enr:" + base64.RawURLEncoding.EncodeToString(b),
		IP: net.IPv4(1, 2, 3, last).String(), TCP: 30303, Score: 5,
		HasV5: true, LastSeen: now.Unix(),
	}
	return currentMainnetEL(t, row, now)
}

func TestSelectNodesRejectsUndecodableEthEntry(t *testing.T) {
	now := time.Unix(1700000000, 0)
	nw, _ := netconf.Get("mainnet")
	canonical, err := rlp.EncodeToBytes(&netconf.EthEntry{ForkID: nw.CurrentForkIDAt(now)})
	if err != nil {
		t.Fatal(err)
	}
	// The entry value must be the fork-id list itself; this wraps it in an RLP string instead.
	doubled, err := rlp.EncodeToBytes(canonical)
	if err != nil {
		t.Fatal(err)
	}

	good := ethEntryRow(t, 1, canonical, now)
	bad := ethEntryRow(t, 2, doubled, now)

	got := selectNodes([]nodeset.Row{good, bad}, selectOpts{minScore: 1, protocol: "any", layer: "el"}, now)
	if len(got) != 1 || got[0].ID().String() != good.ID {
		t.Fatalf("only the canonically encoded eth entry should be published, got %v", got)
	}

	if enrEntryDecodes(mustParseENR(t, bad.ENR), new(netconf.EthEntry)) {
		t.Fatal("double-encoded eth entry should not count as decodable")
	}
	if !enrEntryDecodes(mustParseENR(t, good.ENR), new(netconf.EthEntry)) {
		t.Fatal("canonical eth entry should count as decodable")
	}
}

// A record observed in the wild: its "eth" value is the fork-id list wrapped in an RLP string, which
// go-ethereum tolerates by ignoring the entry while other clients reject the whole record.
func TestSelectNodesRejectsObservedDoubleEncodedRecord(t *testing.T) {
	const observed = "enr:-Ji4QMMo4cKZYU8xSLxvVOV_Q9zcHcXiH-6ojYRtbtC1MuvVOjSQ4_DW5649TSAl4qN4Z9XmOVgn4RxTJ63pzaruSV4Cg2V0aIjHxoQjqhNRgIJpZIJ2NIJpcISfw0KIiXNlY3AyNTZrMaEC9nDXsv-k4hHhfMQexeUKSMN21XzjpwrtIe0Nk27R3GODdGNwgnZfg3VkcIJ2Xw"
	n := mustParseENR(t, observed)
	if enrEntryDecodes(n, new(netconf.EthEntry)) {
		t.Fatal("the observed record's eth entry must not count as decodable")
	}
	if !enrHasEntry(n, "eth") {
		t.Fatal("the entry is present, so absence is not why it fails to decode")
	}
}

// A port above uint16 is not representable in the ENR-typed entry. go-ethereum signs and parses the
// record anyway and reports the problem only when that entry is loaded, so such a node stays dialable
// through another transport and would otherwise reach the tree.
func TestSelectNodesRejectsOutOfRangePort(t *testing.T) {
	now := time.Now()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(enr.IPv4{9, 9, 9, 9})
	r.Set(enr.WithEntry("tcp", uint32(70000)))
	r.Set(enr.QUIC(30303))
	portNW, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	r.Set(netconf.EthEntry{ForkID: portNW.CurrentForkIDAt(now)})
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
	row := currentMainnetEL(t, nodeset.Row{
		ID: n.ID().String(), ENR: "enr:" + base64.RawURLEncoding.EncodeToString(b),
		IP: net.IPv4(9, 9, 9, 9).String(), QUIC: 30303, Score: 5,
		HasV5: true, LastSeen: now.Unix(),
	}, now)
	if !row.Dialable() {
		t.Fatal("fixture must be dialable, otherwise the port check is not what excludes it")
	}

	if got := selectNodes([]nodeset.Row{row}, selectOpts{minScore: 1, protocol: "any", layer: "el", capability: "all"}, now); len(got) != 0 {
		t.Fatalf("published %d records carrying an out-of-range tcp entry, want 0", len(got))
	}
}

// A live staging record: classified EL by an authenticated Status handshake, so its row columns are
// current while the ENR it publishes carries no eth entry at all (keys id/ip/secp256k1/tcp/udp).
func TestSelectNodesRequiresENRForkEntry(t *testing.T) {
	now := time.Unix(1700000000, 0)
	const observed = "enr:-Iu4QMYOLXOWDG7Ue4PZJuzbGnPLWihjXBKsaVLvy68HyHulWyjqqLqzFlQT1mhXn-dMt8xexh_vh-PvW7zFYwDNHTkBgmlkgnY0gmlwhC4-vcmJc2VjcDI1NmsxoQOrYxy0NZSXwH_5AWxdHqp7YvhL-9aAnf0jfwlWUfJ_mIN0Y3CCdl-DdWRwgnZf"
	n := mustParseENR(t, observed)
	statusVerified := currentMainnetEL(t, nodeset.Row{
		ID: n.ID().String(), ENR: observed, IP: "46.62.189.201", TCP: 30303, Score: 5,
		HasV5: true, LastSeen: now.Unix(),
	}, now)
	keeper := currentMainnetEL(t, v4Row(t, 1, 30303, 5, now), now)

	got := selectNodes([]nodeset.Row{statusVerified, keeper}, selectOpts{minScore: 1, protocol: "any", layer: "el"}, now)
	if len(got) != 1 || got[0].ID().String() != keeper.ID {
		t.Fatalf("only the record advertising its fork should be published, got %v", got)
	}
}

func TestSelectNodesRejectsStaleENRForkEntry(t *testing.T) {
	now := time.Unix(1700000000, 0)
	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	staleEntry, err := rlp.EncodeToBytes(&netconf.EthEntry{ForkID: nw.CurrentForkIDAt(time.Unix(1438269973, 0))})
	if err != nil {
		t.Fatal(err)
	}
	// Columns come out current (ethEntryRow applies currentMainnetEL); only the ENR self-describes stale.
	stale := ethEntryRow(t, 4, staleEntry, now)
	keeper := currentMainnetEL(t, v4Row(t, 1, 30303, 5, now), now)

	got := selectNodes([]nodeset.Row{stale, keeper}, selectOpts{minScore: 1, protocol: "any", layer: "el"}, now)
	if len(got) != 1 || got[0].ID().String() != keeper.ID {
		t.Fatalf("a record self-describing a stale fork must not be published, got %v", got)
	}
}

func TestSelectNodesCLRequiresENREth2Digest(t *testing.T) {
	now := time.Unix(1700000000, 0)
	good := clRow(t, 1, now)
	absent := v4Row(t, 2, 30303, 5, now)
	absent.Layer, absent.Network, absent.ForkHash = "cl", "mainnet", good.ForkHash
	staleDigest := make(netconf.Eth2Entry, 16)
	copy(staleDigest, []byte{0xde, 0xad, 0xbe, 0xef})
	stale := clRowEntry(t, 3, staleDigest, now)

	got := selectNodes([]nodeset.Row{good, absent, stale}, selectOpts{minScore: 1, protocol: "any", layer: "cl"}, now)
	if len(got) != 1 || got[0].ID().String() != good.ID {
		t.Fatalf("only the record advertising the current digest should be published, got %v", got)
	}
}

func mustParseENR(t *testing.T, record string) *enode.Node {
	t.Helper()
	n, err := enode.Parse(enode.ValidSchemes, record)
	if err != nil {
		t.Fatalf("go-ethereum must still parse the record: %v", err)
	}
	return n
}

// v6Row sets ip6 in the ENR as well as the row column, so the record it publishes really is
// IPv6-reachable. A column without the matching ENR entry would let the reservation pass while the
// published tree carried no usable v6 endpoint.
func v6Row(t *testing.T, last byte, ownPort bool, now time.Time) nodeset.Row {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	ip6 := net.ParseIP(fmt.Sprintf("2001:db8::%d", last))
	if ip6 == nil {
		t.Fatalf("bad fixture address for %d", last)
	}
	var r enr.Record
	r.Set(enr.IPv4{1, 2, 3, last})
	r.Set(enr.TCP(30303))
	r.Set(enr.IPv6(ip6.To16()))
	if ownPort {
		r.Set(enr.TCP6(30304))
	}
	mainnet, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	r.Set(netconf.EthEntry{ForkID: mainnet.CurrentForkIDAt(now)})
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
	row := nodeset.Row{
		ID: n.ID().String(), ENR: "enr:" + base64.RawURLEncoding.EncodeToString(b),
		IP: net.IPv4(1, 2, 3, last).String(), TCP: 30303, Score: 5,
		IP6: ip6.String(), HasV5: true, LastSeen: now.Unix(),
	}
	if ownPort {
		row.TCP6 = 30304
	}
	return currentMainnetEL(t, row, now)
}

func TestSelectNodesReservesIPv6Slots(t *testing.T) {
	now := time.Unix(1700000000, 0)
	rows := []nodeset.Row{}
	for i := byte(1); i <= 40; i++ {
		rows = append(rows, currentMainnetEL(t, v4Row(t, i, 30303, 9, now), now))
	}
	// Lowest score, so rank alone would never reach them inside the limit.
	v6 := v6Row(t, 200, false, now)
	v6.Score = 1
	rows = append(rows, v6)

	got := selectNodes(rows, selectOpts{minScore: 1, protocol: "any", layer: "el", limit: 10, balance: balanceProportional}, now)
	if len(got) != 10 {
		t.Fatalf("selected %d nodes, want 10", len(got))
	}
	var v6Count int
	for _, n := range got {
		if n.ID().String() == v6.ID {
			v6Count++
		}
	}
	if v6Count != 1 {
		t.Fatalf("the IPv6-dialable node must hold a reserved slot, got %d", v6Count)
	}
}

// The reservation takes slots before balanceClients runs, so the two together must still fill the
// limit exactly and pick the same nodes in the same order every time. Tree layout depends on input
// order, so an unstable order rewrites every branch record.
func TestSelectNodesIPv6ReservationKeepsLimitAndOrder(t *testing.T) {
	now := time.Unix(1700000000, 0)
	var rows []nodeset.Row
	for i := byte(1); i <= 60; i++ {
		rows = append(rows, currentMainnetEL(t, v4Row(t, i, 30303, 9, now), now))
	}
	// Lowest score, so only the reservation can reach them.
	for i := byte(100); i <= 105; i++ {
		r := v6Row(t, i, i%2 == 0, now)
		r.Score = 2
		rows = append(rows, r)
	}

	for _, tc := range []struct{ limit, wantV6 int }{{1, 1}, {10, 1}, {25, 2}, {50, 5}, {66, 6}} {
		opt := selectOpts{minScore: 1, protocol: "any", layer: "el", limit: tc.limit, balance: balanceProportional}
		first := selectNodes(rows, opt, now)
		if len(first) != tc.limit {
			t.Fatalf("limit %d selected %d nodes", tc.limit, len(first))
		}
		var v6 int
		for _, n := range first {
			if enrHasEntry(n, "ip6") {
				v6++
			}
		}
		if v6 != tc.wantV6 {
			t.Fatalf("limit %d published %d IPv6 records, want %d", tc.limit, v6, tc.wantV6)
		}
		again := selectNodes(rows, opt, now)
		for i := range first {
			if first[i].ID() != again[i].ID() {
				t.Fatalf("limit %d: selection is not stable at index %d", tc.limit, i)
			}
		}
	}
}

// A reserved IPv6 node already represents its own client, so it must not also consume that client's
// floor slot. Otherwise three slots across three clients can publish one client twice and drop a third.
func TestSelectNodesIPv6ReservationKeepsClientFloors(t *testing.T) {
	now := time.Now()
	rows := balanceRows(t, map[string]int{"Geth": 2, "Besu": 1, "Reth": 1})
	v6 := v6Row(t, 200, false, now)
	v6.Client, v6.FPStatus, v6.ID = "Geth", "ok", "Geth-v6"
	v6.Score = 1
	rows = append(rows, v6)

	got := selectNodes(rows, selectOpts{minScore: 1, protocol: "any", layer: "el", limit: 3, balance: balanceProportional}, now)
	if len(got) != 3 {
		t.Fatalf("selected %d nodes, want 3", len(got))
	}
	mix := clientMix(got, rows)
	for _, client := range []string{"Geth", "Besu", "Reth"} {
		if mix[client] == 0 {
			t.Fatalf("client %s was dropped, mix=%v", client, mix)
		}
	}
}

func TestReserveIPv6PrefersExplicitTCP6(t *testing.T) {
	inherited := ipv6Slot{dialable: true}
	explicit := ipv6Slot{dialable: true, ownPort: true}
	plain := ipv6Slot{}

	// One slot available, and the explicit-tcp6 record is ranked last.
	got := reserveIPv6([]ipv6Slot{inherited, plain, plain, explicit}, 1)
	if countTrue(got) != 1 || !got[3] {
		t.Fatalf("explicit tcp6 should take the only slot, got %v", got)
	}
}

func TestReserveIPv6ProportionalShare(t *testing.T) {
	pool := make([]ipv6Slot, 1108)
	for i := range 26 {
		pool[i] = ipv6Slot{dialable: true}
	}
	// 26/1108 of 25 slots rounds to 0.6, so the floor of one slot is what keeps IPv6 present.
	if got := countTrue(reserveIPv6(pool, 25)); got != 1 {
		t.Fatalf("reserved %d slots at limit 25, want 1", got)
	}
	if got := countTrue(reserveIPv6(pool, 1000)); got != 23 {
		t.Fatalf("reserved %d slots at limit 1000, want 23", got)
	}
	if got := countTrue(reserveIPv6(pool, 3000)); got != 26 {
		t.Fatalf("reserved %d slots at limit 3000, want all 26 available", got)
	}
	if got := countTrue(reserveIPv6(make([]ipv6Slot, 50), 10)); got != 0 {
		t.Fatalf("no IPv6 candidates should reserve nothing, got %d", got)
	}
}

func TestParseNetworks(t *testing.T) {
	got, err := parseNetworks(" mainnet, hoodi ,sepolia")
	if err != nil || len(got) != 3 || got[0] != "mainnet" || got[2] != "sepolia" {
		t.Fatalf("parseNetworks = %v, %v", got, err)
	}
	if _, err := parseNetworks("  "); err == nil {
		t.Fatal("empty list should error")
	}
	if _, err := parseNetworks("bad/name"); err == nil {
		t.Fatal("unsafe name should error")
	}
}

func TestBuildTree(t *testing.T) {
	now := time.Unix(1700000000, 0)
	rows := []nodeset.Row{currentMainnetEL(t, v4Row(t, 1, 30303, 5, now), now)}
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	out, err := buildTree(rows, selectOpts{minScore: 1, protocol: "any", layer: "el", capability: "all"}, 42, "all.mainnet.example.org", "mainnet", key, now)
	if err != nil {
		t.Fatal(err)
	}
	if out.Nodes != 1 || out.Domain != "all.mainnet.example.org" || out.Seq != 42 || out.Capability != "all" {
		t.Fatalf("unexpected tree output: %+v", out)
	}
	if _, _, err := dnsdisc.ParseURL(out.URL); err != nil {
		t.Fatalf("signed tree url invalid: %v", err)
	}
}

func mainnetELNode(t *testing.T, last byte, withSnap bool) *enode.Node {
	t.Helper()
	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(enr.IPv4{9, 9, 9, last})
	r.Set(enr.TCP(30303))
	r.Set(netconf.EthEntry{ForkID: nw.CurrentForkID()})
	if withSnap {
		r.Set(enr.WithEntry("snap", uint(1)))
	}
	if err := enode.SignV4(&r, key); err != nil {
		t.Fatal(err)
	}
	n, err := enode.New(enode.ValidSchemes, &r)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func writeMainnetSnapshot(t *testing.T, ctx context.Context, st store.Store, set *nodeset.Set, gen time.Time) {
	t.Helper()
	layout := snapshot.Layout{}
	data, err := set.ParquetForNetwork("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	key := layout.GenerationKey("mainnet", gen)
	if err := st.Put(ctx, key, data, "application/vnd.apache.parquet"); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	count := set.Len()
	m := &snapshot.Manifest{
		SchemaVersion: snapshot.SchemaVersion, GeneratedAt: gen, CrawlerID: "dnspub-test",
		Run: snapshot.RunMetadata{
			RunID: "dnspub-run", SourceRevision: "rev", SourceURL: "https://example.com/s",
			ConfigSHA256: strings.Repeat("00", sha256.Size), CrawlerStartedAt: gen.Add(-time.Minute),
			MethodologyStartedAt: gen.Add(-time.Minute), MethodologyVersion: snapshot.MethodologyVersion,
			MethodologyID: "dnspub-method",
		},
		Networks: map[string]snapshot.NetworkSnapshot{
			"mainnet": {GenerationKey: key, NodeCount: count, Bytes: len(data), SHA256: hex.EncodeToString(sum[:])},
		},
	}
	if err := snapshot.Write(ctx, st, layout, m); err != nil {
		t.Fatal(err)
	}
}

func readTreeOut(t *testing.T, path string) output {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var o output
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return o
}

func TestRunMultiTreeWritesAllTrees(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	set := nodeset.NewWithLimit(0)
	now := time.Unix(1700000000, 0)
	set.Observe(mainnetELNode(t, 1, false), "v5", now)
	set.Observe(mainnetELNode(t, 2, true), "v5", now)
	gen := time.Unix(1700000100, 0)
	writeMainnetSnapshot(t, ctx, st, set, gen)

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	if err := runMultiTree(ctx, st, snapshot.Layout{}, multiConfig{
		networks: []string{"mainnet"}, baseDomain: "nodes.example.org", outDir: outDir, key: key,
		sel: selectOpts{minScore: 1, protocol: "any", layer: "el"},
	}); err != nil {
		t.Fatal(err)
	}

	all := readTreeOut(t, filepath.Join(outDir, "all.mainnet.nodes.example.org.json"))
	snap := readTreeOut(t, filepath.Join(outDir, "snap.mainnet.nodes.example.org.json"))
	if all.Nodes != 2 {
		t.Fatalf("all tree = %d nodes, want 2", all.Nodes)
	}
	if snap.Nodes != 1 {
		t.Fatalf("snap tree = %d nodes, want 1 (only the snap-advertising node)", snap.Nodes)
	}
	if all.Seq != uint64(gen.Unix()) {
		t.Fatalf("seq = %d, want snapshot generation %d", all.Seq, gen.Unix())
	}
	if all.Capability != "all" || snap.Capability != "snap" {
		t.Fatalf("capability labels: all=%q snap=%q", all.Capability, snap.Capability)
	}
	if _, _, err := dnsdisc.ParseURL(snap.URL); err != nil {
		t.Fatalf("snap tree url invalid: %v", err)
	}
}

func TestCollapsed(t *testing.T) {
	cases := []struct {
		current, previous, pct int
		want                   bool
	}{
		{1400, 2900, 50, true},  // >50% drop
		{1500, 2900, 50, false}, // ~48% drop, under threshold
		{2900, 2900, 50, false}, // no change
		{10, 2900, 50, true},    // near-total collapse
		{25, 51, 50, true},      // 50.98% drop; integer-floor math would miss this
		{1, 2900, 0, false},     // check disabled
		{1, 0, 50, false},       // no baseline
	}
	for _, c := range cases {
		if got := collapsed(c.current, c.previous, c.pct); got != c.want {
			t.Errorf("collapsed(%d, %d, %d) = %v, want %v", c.current, c.previous, c.pct, got, c.want)
		}
	}
}

func TestRunMultiTreeSkipsBelowFloor(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	set := nodeset.NewWithLimit(0)
	set.Observe(mainnetELNode(t, 1, false), "v5", time.Unix(1700000000, 0))
	writeMainnetSnapshot(t, ctx, st, set, time.Now())

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	if err := runMultiTree(ctx, st, snapshot.Layout{}, multiConfig{
		networks: []string{"mainnet"}, baseDomain: "nodes.example.org", outDir: outDir, key: key,
		sel:            selectOpts{minScore: 1, protocol: "any", layer: "el"},
		minTreeNodes:   50,
		maxSnapshotAge: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "all.mainnet.nodes.example.org.json")); !os.IsNotExist(err) {
		t.Fatal("a below-floor network must not publish a tree")
	}
}

// An empty tree signs and parses like any other, so nothing downstream would notice it replacing a
// working one. The snap subset is the realistic case: no row has to advertise the ENR entry.
func TestRunMultiTreePublishesNoTreeWhenTheSnapSubsetIsEmpty(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	set := nodeset.NewWithLimit(0)
	for tag := byte(1); tag <= 3; tag++ {
		set.Observe(mainnetELNode(t, tag, false), "v5", time.Unix(1700000000, 0))
	}
	writeMainnetSnapshot(t, ctx, st, set, time.Now())

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	if err := runMultiTree(ctx, st, snapshot.Layout{}, multiConfig{
		networks: []string{"mainnet"}, baseDomain: "nodes.example.org", outDir: outDir, key: key,
		sel:            selectOpts{minScore: 1, protocol: "any", layer: "el"},
		minTreeNodes:   1,
		maxSnapshotAge: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	for _, domain := range []string{"snap.mainnet.nodes.example.org", "all.mainnet.nodes.example.org"} {
		if _, err := os.Stat(filepath.Join(outDir, domain+".json")); !os.IsNotExist(err) {
			t.Fatalf("%s was published even though the snap subset selected no nodes", domain)
		}
	}
}

func TestRunMultiTreeSkipsCollapseVsArtifact(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	set := nodeset.NewWithLimit(0)
	set.Observe(mainnetELNode(t, 1, false), "v5", time.Unix(1700000000, 0))
	writeMainnetSnapshot(t, ctx, st, set, time.Now())

	outDir := t.TempDir()
	lastGood := output{
		SchemaVersion: outputSchemaVersion, URL: "enrtree://ABC@nodes.example.org",
		Domain: "all.mainnet.nodes.example.org", Network: "mainnet", Capability: "all",
		Nodes: 2900, Seq: 1, Records: map[string]string{},
	}
	buf, err := json.MarshalIndent(lastGood, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	allPath := filepath.Join(outDir, "all.mainnet.nodes.example.org.json")
	if err := os.WriteFile(allPath, buf, 0o644); err != nil {
		t.Fatal(err)
	}

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := runMultiTree(ctx, st, snapshot.Layout{}, multiConfig{
		networks: []string{"mainnet"}, baseDomain: "nodes.example.org", outDir: outDir, key: key,
		sel:            selectOpts{minScore: 1, protocol: "any", layer: "el"},
		minTreeNodes:   0,
		maxDropPct:     50,
		maxSnapshotAge: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	got := readTreeOut(t, allPath)
	if got.Nodes != 2900 {
		t.Fatalf("collapse guard should have kept the last-good artifact (2900), got %d", got.Nodes)
	}
}

func TestRunMultiTreeSkipsStaleSnapshot(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	set := nodeset.NewWithLimit(0)
	set.Observe(mainnetELNode(t, 1, false), "v5", time.Unix(1700000000, 0))
	set.Observe(mainnetELNode(t, 2, true), "v5", time.Unix(1700000000, 0))
	writeMainnetSnapshot(t, ctx, st, set, time.Now().Add(-2*time.Hour))

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	if err := runMultiTree(ctx, st, snapshot.Layout{}, multiConfig{
		networks: []string{"mainnet"}, baseDomain: "nodes.example.org", outDir: outDir, key: key,
		sel:            selectOpts{minScore: 1, protocol: "any", layer: "el"},
		maxSnapshotAge: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "all.mainnet.nodes.example.org.json")); !os.IsNotExist(err) {
		t.Fatal("a stale snapshot must not publish any tree")
	}
}

func TestSelectNodesPrefersScoreThenFreshness(t *testing.T) {
	now := time.Unix(1700000000, 0)
	oldHigh := currentMainnetEL(t, v4Row(t, 1, 30303, 10, now), now)
	oldHigh.LastSeen = now.Add(-time.Minute).Unix()
	newHigh := currentMainnetEL(t, v4Row(t, 2, 30303, 10, now), now)
	newHigh.LastSeen = now.Unix()
	low := currentMainnetEL(t, v4Row(t, 3, 30303, 1, now), now)

	got := selectNodes([]nodeset.Row{low, oldHigh, newHigh}, selectOpts{minScore: 1, protocol: "any", limit: 1}, now)
	if len(got) != 1 || got[0].ID().String() != newHigh.ID {
		t.Fatalf("selected %v, want freshest highest-score node %s", got, newHigh.ID)
	}
}

func TestSelectNodesAgeUsesPublicationTime(t *testing.T) {
	snapshotTime := time.Unix(1700000000, 0)
	row := currentMainnetEL(t, v4Row(t, 1, 30303, 5, snapshotTime), snapshotTime)
	row.LastSeen = snapshotTime.Add(-30 * time.Minute).Unix()
	if got := selectNodes([]nodeset.Row{row}, selectOpts{minScore: 1, maxAge: time.Hour, protocol: "any"}, snapshotTime.Add(24*time.Hour)); len(got) != 0 {
		t.Fatalf("selected %d nodes from a day-old snapshot", len(got))
	}
}

func TestSelectOptsValidate(t *testing.T) {
	valid := selectOpts{minScore: 1, maxAge: time.Hour, protocol: "any", layer: "el", balance: balanceProportional}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}
	mutate := func(f func(*selectOpts)) selectOpts {
		o := valid
		f(&o)
		return o
	}
	tests := []struct {
		name string
		opts selectOpts
	}{
		{"negative score", mutate(func(o *selectOpts) { o.minScore = -1 })},
		// int32 previously truncated this to zero before it could be rejected.
		{"score past int32", mutate(func(o *selectOpts) { o.minScore = 1 << 32 })},
		{"negative age", mutate(func(o *selectOpts) { o.maxAge = -time.Second })},
		{"invalid protocol", mutate(func(o *selectOpts) { o.protocol = "v6" })},
		{"invalid layer", mutate(func(o *selectOpts) { o.layer = "exec" })},
		{"negative limit", mutate(func(o *selectOpts) { o.limit = -1 })},
		{"unknown client balance", mutate(func(o *selectOpts) { o.balance = "weighted" })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.opts.Validate(); err == nil {
				t.Fatal("invalid options accepted")
			}
		})
	}
}

func TestOutputSequenceRequiresAKnownSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mainnet.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"seq":7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	prev, exists, err := readPrevious(path)
	if err != nil || !exists {
		t.Fatalf("readPrevious = %v, %v", exists, err)
	}
	if got, err := outputSequence(prev); err != nil || got != 7 {
		t.Fatalf("outputSequence = %d, %v; want 7, nil", got, err)
	}
	if err := os.WriteFile(path, []byte(`{"seq":9}`), 0o600); err != nil {
		t.Fatal(err)
	}
	prev, exists, err = readPrevious(path)
	if err != nil || !exists {
		t.Fatalf("readPrevious = %v, %v", exists, err)
	}
	if _, err := outputSequence(prev); err == nil {
		t.Fatal("schema-less artifact was accepted")
	}
}

func TestLoadKeyRejectsOverlyBroadPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dns.key")
	key, err := crypto.HexToECDSA(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	if err := crypto.SaveECDSA(path, key); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadKey(path, "", false); err == nil {
		t.Fatal("world-readable signing key accepted")
	}
	if _, err := loadKey(t.TempDir(), "", false); err == nil {
		t.Fatal("signing key directory accepted")
	}
}

func TestLoadKeyKeystoreMatchesHex(t *testing.T) {
	key, err := crypto.HexToECDSA(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	hexPath := filepath.Join(dir, "hex.key")
	if err := crypto.SaveECDSA(hexPath, key); err != nil {
		t.Fatal(err)
	}
	fromHex, err := loadKey(hexPath, "", false)
	if err != nil {
		t.Fatalf("load hex key: %v", err)
	}

	ksKey := &keystore.Key{Address: crypto.PubkeyToAddress(key.PublicKey), PrivateKey: key, Id: uuid.New()}
	ksJSON, err := keystore.EncryptKey(ksKey, "", keystore.LightScryptN, keystore.LightScryptP)
	if err != nil {
		t.Fatal(err)
	}
	ksPath := filepath.Join(dir, "keystore.json")
	if err := os.WriteFile(ksPath, ksJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	fromKeystore, err := loadKey(ksPath, "", false)
	if err != nil {
		t.Fatalf("load empty-password keystore (devp2p style): %v", err)
	}

	want := crypto.FromECDSA(key)
	if !bytes.Equal(crypto.FromECDSA(fromHex), want) || !bytes.Equal(crypto.FromECDSA(fromKeystore), want) {
		t.Fatal("loaded key differs from the original; enrtree pubkey would not match")
	}
	tree, err := dnsdisc.MakeTree(1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	url, err := tree.Sign(fromKeystore, "nodes.example.org")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := dnsdisc.ParseURL(url); err != nil {
		t.Fatalf("keystore-signed tree url invalid: %v", err)
	}
}

func TestReadKeyPassphrasePreservesSpaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(path, []byte("  spaced pass  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readKeyPassphrase(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "  spaced pass  " {
		t.Fatalf("passphrase = %q, want surrounding spaces preserved", got)
	}
}

func TestReadKeyPassphraseRejectsUnsafeFile(t *testing.T) {
	dir := t.TempDir()
	permissive := filepath.Join(dir, "permissive")
	if err := os.WriteFile(permissive, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readKeyPassphrase(permissive); err == nil {
		t.Fatal("group/world-readable passphrase file was accepted")
	}
	if _, err := readKeyPassphrase(dir); err == nil {
		t.Fatal("passphrase directory was accepted")
	}
	oversized := filepath.Join(dir, "oversized")
	if err := os.WriteFile(oversized, bytes.Repeat([]byte("x"), maxKeyPassphraseBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readKeyPassphrase(oversized); err == nil {
		t.Fatal("oversized passphrase file was accepted")
	}
}

func TestLoadKeyKeystoreWithSpacedPassphrase(t *testing.T) {
	key, err := crypto.HexToECDSA(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	pass := "  pass with spaces  "
	ksKey := &keystore.Key{Address: crypto.PubkeyToAddress(key.PublicKey), PrivateKey: key, Id: uuid.New()}
	ksJSON, err := keystore.EncryptKey(ksKey, pass, keystore.LightScryptN, keystore.LightScryptP)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ksPath := filepath.Join(dir, "keystore.json")
	if err := os.WriteFile(ksPath, ksJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	passPath := filepath.Join(dir, "pass")
	if err := os.WriteFile(passPath, []byte(pass+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gotPass, err := readKeyPassphrase(passPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := loadKey(ksPath, gotPass, false)
	if err != nil {
		t.Fatalf("decrypt keystore with spaced passphrase (TrimSpace would break this): %v", err)
	}
	if !bytes.Equal(crypto.FromECDSA(loaded), crypto.FromECDSA(key)) {
		t.Fatal("decrypted key differs from original")
	}
}

func TestSignedTreeDeterministicAndValid(t *testing.T) {
	now := time.Unix(1700000000, 0)
	rows := []nodeset.Row{
		currentMainnetEL(t, v4Row(t, 1, 30303, 5, now), now),
		currentMainnetEL(t, v4Row(t, 2, 30303, 5, now), now),
	}
	nodes := selectNodes(rows, selectOpts{minScore: 1, protocol: "any"}, now)
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}

	key, err := crypto.HexToECDSA(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	sign := func() string {
		tree, err := dnsdisc.MakeTree(1, nodes, nil)
		if err != nil {
			t.Fatal(err)
		}
		url, err := tree.Sign(key, "nodes.example.org")
		if err != nil {
			t.Fatal(err)
		}
		return url
	}
	url1, url2 := sign(), sign()
	if url1 != url2 {
		t.Errorf("tree signing not deterministic:\n%s\n%s", url1, url2)
	}
	if _, _, err := dnsdisc.ParseURL(url1); err != nil {
		t.Errorf("signed tree url does not parse: %v", err)
	}
}

// recordResolver serves a tree's own ToTXT output in place of a live zone.
type recordResolver struct {
	records map[string]string
	lookups int
}

func (r *recordResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	r.lookups++
	value, ok := r.records[strings.TrimSuffix(name, ".")]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	return []string{value}, nil
}

// ParseURL only checks the root signature, so nothing else here proves a client can walk the
// records back to the ENRs. That is the property publishing to DNS depends on.
func TestPublishedRecordsResolveBackToEveryNode(t *testing.T) {
	const domain = "all.mainnet.example.org"
	key, err := crypto.HexToECDSA(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	want := make(map[string]bool, 40)
	nodes := make([]*enode.Node, 0, 40)
	for i := range 40 {
		n := mainnetELNode(t, byte(i+1), false)
		nodes = append(nodes, n)
		want[n.String()] = true
	}
	tree, err := dnsdisc.MakeTree(1, nodes, nil)
	if err != nil {
		t.Fatal(err)
	}
	url, err := tree.Sign(key, domain)
	if err != nil {
		t.Fatal(err)
	}

	resolver := &recordResolver{records: tree.ToTXT(domain)}
	client := dnsdisc.NewClient(dnsdisc.Config{Resolver: resolver, RateLimit: 1_000_000})
	synced, err := client.SyncTree(url)
	if err != nil {
		t.Fatalf("a client cannot resolve the published records: %v", err)
	}
	if got := len(synced.Nodes()); got != len(nodes) {
		t.Fatalf("recovered %d nodes, want %d", got, len(nodes))
	}
	for _, n := range synced.Nodes() {
		if !want[n.String()] {
			t.Errorf("resolved a node that was never published: %s", n.String())
		}
		delete(want, n.String())
	}
	if len(want) != 0 {
		t.Errorf("%d published nodes were unreachable through the records", len(want))
	}
	if resolver.lookups < len(nodes) {
		t.Errorf("only %d lookups for %d nodes; the tree cannot have been walked", resolver.lookups, len(nodes))
	}
}

// An empty artifact written before the empty-tree guard must not wedge its domain: it carries no
// collapse baseline, but its sequence still has to be exceeded so resolvers see the replacement.
func TestZeroNodeArtifactYieldsNoBaselineButStillRaisesTheSequence(t *testing.T) {
	outDir := t.TempDir()
	legacy := output{
		SchemaVersion: outputSchemaVersion, Domain: "snap.mainnet.nodes.example.org", Network: "mainnet",
		Capability: "snap", Nodes: 0, Seq: 1_900_000_000, Records: map[string]string{"root": "enrtree-root:v1"},
	}
	if _, err := emitArtifact(legacy, outDir, legacy.Domain); err != nil {
		t.Fatal(err)
	}
	nodes, seq, err := baselineFor(outDir, legacy.Domain, "mainnet", "snap")
	if err != nil {
		t.Fatalf("legacy empty artifact wedged the domain: %v", err)
	}
	if nodes != 0 {
		t.Fatalf("collapse baseline = %d, want 0", nodes)
	}
	if seq != legacy.Seq {
		t.Fatalf("baseline seq = %d, want %d", seq, legacy.Seq)
	}
	generatedAt := time.Unix(1_700_000_000, 0).UTC()
	if got := treeSequence(generatedAt, seq); uint64(got) != legacy.Seq+1 {
		t.Fatalf("replacement seq = %d, want %d", got, legacy.Seq+1)
	}
}

// Without --out there is no artifact to read a sequence floor from, but selection still filters on
// wall-clock time, so successive cycles over one manifest must not reuse a sequence.
func TestRunMultiTreeRaisesTheSequenceWithoutADurableArtifact(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	set := nodeset.NewWithLimit(0)
	set.Observe(mainnetELNode(t, 1, true), "v5", time.Unix(1700000000, 0))
	writeMainnetSnapshot(t, ctx, st, set, time.Now())
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := multiConfig{
		networks: []string{"mainnet"}, baseDomain: "nodes.example.org", key: key,
		sel:            selectOpts{minScore: 1, protocol: "any", layer: "el"},
		minTreeNodes:   1,
		maxSnapshotAge: time.Hour,
	}
	m, err := snapshot.Read(ctx, st, snapshot.Layout{})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := snapshot.LoadNetworkRows(ctx, st, snapshot.Layout{}, m, "mainnet")
	if err != nil {
		t.Fatal(err)
	}
	generatedAt := time.Unix(1700000000, 0).UTC()
	issued := map[string]uint64{}
	var seqs []uint64
	for range 3 {
		trees, skip, err := buildNetworkTrees(rows, "mainnet", generatedAt, time.Now(), cfg, issued)
		if err != nil || skip.reason != "" {
			t.Fatalf("cycle skipped as %q: %v", skip.reason, err)
		}
		for _, out := range trees {
			if out.Domain == "all.mainnet.nodes.example.org" {
				seqs = append(seqs, out.Seq)
				issued[out.Domain] = out.Seq
			}
		}
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("stdout cycle %d reused sequence %d (previous %d)", i, seqs[i], seqs[i-1])
		}
	}
}

func TestTreeSequenceStaysStrictlyIncreasing(t *testing.T) {
	generatedAt := time.Unix(1_700_000_000, 0).UTC()
	if got := treeSequence(generatedAt, 0); got != 1_700_000_000 {
		t.Fatalf("first publish seq = %d, want the generation second", got)
	}
	// Two generations inside one second, or a nanosecond-only difference, must not reuse a sequence.
	if got := treeSequence(generatedAt.Add(time.Nanosecond), 1_700_000_000); got != 1_700_000_001 {
		t.Fatalf("same-second replacement seq = %d, want 1700000001", got)
	}
	if got := treeSequence(generatedAt.Add(-time.Hour), 1_700_000_000); got != 1_700_000_001 {
		t.Fatalf("clock step back seq = %d, want 1700000001", got)
	}
}

func TestValidateBaselineRejectsParseableButUnusableArtifacts(t *testing.T) {
	good := &output{
		SchemaVersion: outputSchemaVersion, Domain: "all.mainnet.example.org", Network: "mainnet",
		Capability: "all", Nodes: 12, Seq: 7, Records: map[string]string{"root": "enrtree-root:v1"},
	}
	if err := validateBaseline(good, "all.mainnet.example.org", "mainnet", "all"); err != nil {
		t.Fatalf("a valid baseline was rejected: %v", err)
	}
	cases := map[string]func(*output){
		"empty object":   func(o *output) { *o = output{} },
		"negative nodes": func(o *output) { o.Nodes = -1 },
		"no records":     func(o *output) { o.Records = nil },
		"no sequence":    func(o *output) { o.Seq = 0 },
		"wrong schema":   func(o *output) { o.SchemaVersion = 99 },
		"wrong domain":   func(o *output) { o.Domain = "all.sepolia.example.org" },
		"wrong network":  func(o *output) { o.Network = "sepolia" },
		"bad capability": func(o *output) { o.Capability = "les" },
		"absurd sequence would wrap treeSequence":             func(o *output) { o.Seq = ^uint64(0) },
		"absurd node count would overflow the collapse guard": func(o *output) { o.Nodes = 1 << 40 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := *good
			mutate(&candidate)
			if err := validateBaseline(&candidate, "all.mainnet.example.org", "mainnet", "all"); err == nil {
				t.Fatal("accepted as a collapse baseline; a collapsed tree could overwrite the last good one")
			}
		})
	}
}

func TestBuildNetworkTreesCLLayerOmitsSnapTree(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cl := clRow(t, 9, now)

	key, err := crypto.HexToECDSA(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	cfg := multiConfig{baseDomain: "nodes.example.org", key: key,
		sel: selectOpts{minScore: 1, protocol: "any", layer: "cl"}}
	trees, skip, err := buildNetworkTrees([]nodeset.Row{cl}, "mainnet", now, now, cfg, map[string]uint64{})
	if err != nil {
		t.Fatal(err)
	}
	if skip.reason != "" {
		t.Fatalf("skip = %q, want the CL all-tree published without a permanently empty snap gate", skip.reason)
	}
	if len(trees) != 1 || trees[0].Capability != "all" {
		t.Fatalf("trees = %+v, want exactly the all tree", trees)
	}
}

func TestRunMultiTreeSkipsUnloadableNetworkAndPublishesTheRest(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	set := nodeset.NewWithLimit(0)
	set.Observe(mainnetELNode(t, 1, false), "v5", time.Unix(1700000000, 0))
	set.Observe(mainnetELNode(t, 2, true), "v5", time.Unix(1700000000, 0))
	writeMainnetSnapshot(t, ctx, st, set, time.Now())

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	if err := runMultiTree(ctx, st, snapshot.Layout{}, multiConfig{
		networks: []string{"hoodi", "mainnet"}, baseDomain: "nodes.example.org", outDir: outDir, key: key,
		sel:            selectOpts{minScore: 1, protocol: "any", layer: "el"},
		maxSnapshotAge: time.Hour,
	}); err != nil {
		t.Fatalf("a network missing from the manifest must not abort the cycle: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "all.mainnet.nodes.example.org.json")); err != nil {
		t.Fatalf("mainnet tree was not published after skipping the unloadable network: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "all.hoodi.nodes.example.org.json")); !os.IsNotExist(err) {
		t.Fatal("a tree was published for the unloadable network")
	}
}
