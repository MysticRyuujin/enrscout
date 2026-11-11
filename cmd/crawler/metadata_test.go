package main

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"

	"github.com/MysticRyuujin/enrscout/internal/enrich"
	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
)

func mainnetCLNode(t *testing.T) *enode.Node {
	t.Helper()
	state, err := netconf.CLForkStateAt("mainnet", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(enr.IPv4{5, 6, 7, 9})
	r.Set(enr.TCP(9000))
	r.Set(netconf.Eth2Entry(state.ENRForkID()))
	if err := enode.SignV4(&r, key); err != nil {
		t.Fatal(err)
	}
	n, err := enode.New(enode.ValidSchemes, &r)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func storedMetadata(t *testing.T, set *nodeset.Set) nodeset.Row {
	t.Helper()
	rows := set.SnapshotNetworks([]string{"mainnet"})["mainnet"]
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	return rows[0]
}

var okMetadata = enrich.CLMetadata{Outcome: "ok", Version: 3, Attnets: 0b11, Syncnets: 0b1, CGC: 8, CGCKnown: true, At: time.Unix(1700000000, 0)}

func TestOutboundMetadataIsStoredWhenStatusFails(t *testing.T) {
	set := nodeset.NewWithLimit(0)
	n := mainnetCLNode(t)
	set.Observe(n, "v5", time.Now())
	if !set.ClaimFingerprint(n.ID()) {
		t.Fatal("fixture node could not be claimed")
	}
	c := &crawler{set: set}

	c.finishFingerprint(layerCL, n, enrich.Fingerprint{Client: "Lighthouse", Metadata: okMetadata}, nil)

	row := storedMetadata(t, set)
	if !row.MDCGCKnown || row.MDCGC != 8 || row.MDAttnets != 0b11 || row.MDSyncnets != 0b1 || row.MDAt != okMetadata.At.Unix() {
		t.Fatalf("stored MetaData = cgc %d known %v attnets %b syncnets %b at %d, want the probe's answer without a Status",
			row.MDCGC, row.MDCGCKnown, row.MDAttnets, row.MDSyncnets, row.MDAt)
	}
}

func TestInboundMetadataSurvivesThePendingCache(t *testing.T) {
	set := nodeset.NewWithLimit(0)
	n := mainnetCLNode(t)
	c := &crawler{set: set, pending: newPendingFingerprints(time.Minute, 10), pendingLegacy: newPendingLegacyNodes(time.Minute, 10)}

	c.applyInbound(n.ID(), layerCL, enrich.Fingerprint{Client: "Lighthouse", Metadata: okMetadata})
	set.Observe(n, "v5", time.Now())
	for _, cached := range c.pending.DrainKnown(time.Now(), set.LayerOf) {
		c.applyInbound(cached.id, cached.layer, cached.value)
	}

	if row := storedMetadata(t, set); !row.MDCGCKnown || row.MDCGC != 8 {
		t.Fatalf("stored MetaData = cgc %d known %v, want the cached inbound answer", row.MDCGC, row.MDCGCKnown)
	}
}
