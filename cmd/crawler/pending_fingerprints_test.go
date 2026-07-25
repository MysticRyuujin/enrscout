package main

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"

	"github.com/MysticRyuujin/enrscout/internal/enrich"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
)

func TestPendingFingerprints(t *testing.T) {
	now := time.Unix(1000, 0)
	p := newPendingFingerprints(time.Minute, 2)
	var id enode.ID
	id[0] = 1
	p.Put(id, layerEL, enrich.Fingerprint{Client: "Nethermind"}, now)
	if _, ok := p.Take(id, layerCL, now); ok {
		t.Fatal("fingerprint applied to wrong layer")
	}
	got, ok := p.Take(id, layerEL, now)
	if !ok || got.Client != "Nethermind" {
		t.Fatalf("Take = %+v, %v", got, ok)
	}
	if _, ok := p.Take(id, layerEL, now); ok {
		t.Fatal("fingerprint was not consumed")
	}
}

func TestPendingFingerprintsExpire(t *testing.T) {
	now := time.Unix(1000, 0)
	p := newPendingFingerprints(time.Minute, 2)
	var id enode.ID
	p.Put(id, layerEL, enrich.Fingerprint{Client: "Geth"}, now)
	if _, ok := p.Take(id, layerEL, now.Add(time.Minute)); ok {
		t.Fatal("expired fingerprint returned")
	}
}

func TestPendingFingerprintsDrainKnown(t *testing.T) {
	now := time.Unix(1000, 0)
	p := newPendingFingerprints(time.Minute, 2)
	var known, unknown enode.ID
	known[0], unknown[0] = 1, 2
	p.Put(known, layerCL, enrich.Fingerprint{Client: "Caplin"}, now)
	p.Put(unknown, layerEL, enrich.Fingerprint{Client: "Nimbus"}, now)
	got := p.DrainKnown(now, func(id enode.ID) string {
		if id == known {
			return layerCL
		}
		return ""
	})
	if len(got) != 1 || got[0].id != known || got[0].value.Client != "Caplin" {
		t.Fatalf("DrainKnown = %+v", got)
	}
	if _, ok := p.Take(unknown, layerEL, now); !ok {
		t.Fatal("unknown fingerprint was removed")
	}
}

func TestPendingLegacyNodesReconcilesEitherDirection(t *testing.T) {
	now := time.Now()
	p := newPendingLegacyNodes(time.Minute, 2)
	node := enode.SignNull(new(enr.Record), enode.ID{1})
	p.Put(node, "v4", now)

	got, ok := p.Take(node.ID(), now.Add(time.Second))
	if !ok || got.node != node || got.via != "v4" {
		t.Fatalf("Take = %+v, %t", got, ok)
	}
	if _, ok := p.Take(node.ID(), now.Add(2*time.Second)); ok {
		t.Fatal("candidate was not consumed")
	}

	p.Put(node, "v4", now)
	if _, ok := p.Take(node.ID(), now.Add(2*time.Minute)); ok {
		t.Fatal("expired candidate was returned")
	}
}

func TestLegacyInboundCandidateFallsBackToTrackedUnclassifiedRecord(t *testing.T) {
	now := time.Now()
	set := nodeset.NewWithLimit(0)
	node := forklessELNode(t, 30303)
	if observed := set.ObserveResult(node, "v5", now); !observed.Applied {
		t.Fatalf("observe = %+v", observed)
	}
	if got := set.LayerOf(node.ID()); got != "" {
		t.Fatalf("unclassified layer = %q, want empty", got)
	}

	got, via := legacyInboundCandidate(set, newPendingLegacyNodes(time.Minute, 2), enrich.InboundFingerprint{NodeID: node.ID()}, now)
	if got == nil || got.ID() != node.ID() || via != "inbound" {
		t.Fatalf("candidate = %v via %q, want tracked node via inbound", got, via)
	}
}

func TestLegacyInboundCandidatePriority(t *testing.T) {
	now := time.Now()
	set := nodeset.NewWithLimit(0)
	tracked := forklessELNode(t, 30303)
	if !set.Observe(tracked, "v5", now) {
		t.Fatal("tracked node was rejected")
	}
	inbound := tracked
	pending := newPendingLegacyNodes(time.Minute, 2)
	pending.Put(tracked, "v4", now)

	got, via := legacyInboundCandidate(set, pending, enrich.InboundFingerprint{NodeID: tracked.ID(), Node: inbound}, now)
	if got != tracked || via != "v4" {
		t.Fatalf("pending candidate = %p via %q, want %p via v4", got, via, tracked)
	}
	got, via = legacyInboundCandidate(set, pending, enrich.InboundFingerprint{NodeID: tracked.ID(), Node: inbound}, now)
	if got != inbound || via != "inbound" {
		t.Fatalf("inbound candidate = %p via %q, want %p via inbound", got, via, inbound)
	}
	got, via = legacyInboundCandidate(nil, pending, enrich.InboundFingerprint{NodeID: enode.ID{9}}, now)
	if got != nil || via != "" {
		t.Fatalf("missing candidate = %v via %q", got, via)
	}
}

func TestConsensusInboundCandidateFallsBackToTrackedRecord(t *testing.T) {
	now := time.Now()
	set := nodeset.NewWithLimit(0)
	tracked := unclassifiedQUICNode(t, 9000)
	if observed := set.ObserveResult(tracked, "v5", now); !observed.Applied {
		t.Fatalf("observe = %+v", observed)
	}
	if got := set.LayerOf(tracked.ID()); got != "" {
		t.Fatalf("unclassified layer = %q, want empty", got)
	}

	got := consensusInboundCandidate(set, enrich.InboundCLFingerprint{NodeID: tracked.ID()})
	if got == nil || got.ID() != tracked.ID() {
		t.Fatalf("candidate = %v, want tracked node", got)
	}
	if observed := set.ObserveAuthenticatedCL(got, "mainnet", "6a95a1a9", now); !observed.Accepted {
		t.Fatalf("authenticated observe = %+v", observed)
	}
	if layer, network := set.LayerOf(tracked.ID()), set.NetworkOf(tracked.ID()); layer != layerCL || network != "mainnet" {
		t.Fatalf("promoted node = %q/%q, want cl/mainnet", layer, network)
	}
	inbound := forklessELNode(t, 9001)
	got = consensusInboundCandidate(set, enrich.InboundCLFingerprint{NodeID: tracked.ID(), Node: inbound})
	if got != inbound {
		t.Fatalf("candidate = %p, want inbound node %p", got, inbound)
	}
	if got := consensusInboundCandidate(nil, enrich.InboundCLFingerprint{NodeID: enode.ID{9}}); got != nil {
		t.Fatalf("missing candidate = %v", got)
	}
}

func unclassifiedQUICNode(t *testing.T, port int) *enode.Node {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(enr.IPv4{1, 2, 3, 4})
	r.Set(enr.QUIC(port))
	if err := enode.SignV4(&r, key); err != nil {
		t.Fatal(err)
	}
	node, err := enode.New(enode.ValidSchemes, &r)
	if err != nil {
		t.Fatal(err)
	}
	return node
}

func forklessELNode(t *testing.T, port int) *enode.Node {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(enr.IPv4{1, 2, 3, 4})
	r.Set(enr.TCP(port))
	if err := enode.SignV4(&r, key); err != nil {
		t.Fatal(err)
	}
	node, err := enode.New(enode.ValidSchemes, &r)
	if err != nil {
		t.Fatal(err)
	}
	return node
}

func TestApplyInboundHelloOnlyKeepsMembershipUnverified(t *testing.T) {
	now := time.Unix(1700000000, 0)
	set := nodeset.NewWithLimit(0)
	n := mainnetNode(t)
	set.Observe(n, "v5", now)
	c := &crawler{set: set, pending: newPendingFingerprints(time.Minute, 10), pendingLegacy: newPendingLegacyNodes(time.Minute, 10)}

	c.applyInbound(n.ID(), layerEL, enrich.Fingerprint{Client: "Geth", Version: "v1.17.4"})
	data, err := set.ParquetForNetwork("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := nodeset.RowsFromParquet(data)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Client != "Geth" {
		t.Fatalf("client = %q, want hello-derived identity applied", rows[0].Client)
	}
	if rows[0].MembershipSource == "status" || rows[0].MembershipVerifiedAt != 0 {
		t.Fatalf("membership = %q at %d, want unverified without a completed Status", rows[0].MembershipSource, rows[0].MembershipVerifiedAt)
	}

	var unknown enode.ID
	unknown[0] = 0x77
	c.applyInbound(unknown, layerEL, enrich.Fingerprint{Client: "Nethermind"})
	if _, ok := c.pending.Take(unknown, layerEL, time.Now()); !ok {
		t.Fatal("hello-only fingerprint for an unknown node was not cached")
	}
}
