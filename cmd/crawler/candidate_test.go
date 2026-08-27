package main

import (
	"errors"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/enode"

	"github.com/MysticRyuujin/enrscout/internal/enrich"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
)

func seqZeroNode(t *testing.T, tag byte) *enode.Node {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return enode.NewV4(&key.PublicKey, []byte{8, 8, 8, tag}, 30303, 30303)
}

func candidateCrawler(t *testing.T, maxCandidates int) *crawler {
	t.Helper()
	fp, err := enrich.NewFingerprinterWithPolicy(time.Second, 1, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	return &crawler{
		conf:          &config{maxLegacyCandidates: maxCandidates},
		set:           nodeset.NewWithLimit(0),
		fp:            fp,
		pending:       newPendingFingerprints(time.Minute, 10),
		pendingLegacy: newPendingLegacyNodes(time.Minute, 10),
	}
}

func TestRetainCandidateAdmitsAndKillSwitchFallsBack(t *testing.T) {
	c := candidateCrawler(t, 10)
	n := seqZeroNode(t, 1)
	now := time.Unix(1700000000, 0)
	if !c.retainCandidate(n, now) {
		t.Fatal("TCP-capable candidate was not retained")
	}
	if got := c.set.LayerOf(n.ID()); got != "el" {
		t.Fatalf("retained layer = %q, want el", got)
	}
	if !c.retainCandidate(n, now.Add(time.Second)) {
		t.Fatal("re-sighting of a retained candidate was not absorbed")
	}
	if got := c.set.CountELCandidates(); got != 1 {
		t.Fatalf("candidate count = %d, want 1", got)
	}

	off := candidateCrawler(t, 0)
	if off.retainCandidate(seqZeroNode(t, 2), now) {
		t.Fatal("kill switch did not fall back to the legacy path")
	}
	if off.set.Len() != 0 {
		t.Fatal("disabled retention still admitted a candidate")
	}
}

func TestFinishCandidateFingerprintFailureKeepsRetrying(t *testing.T) {
	c := candidateCrawler(t, 10)
	n := seqZeroNode(t, 3)
	now := time.Unix(1700000000, 0)
	if !c.retainCandidate(n, now) {
		t.Fatal("candidate was not retained")
	}
	if !c.set.ClaimFingerprintAt(n.ID(), now) {
		t.Fatal("claim failed")
	}
	c.finishCandidateFingerprint(n, enrich.Fingerprint{Client: "Nethermind", Version: "v1.39.3"}, errors.New("eth status exchange: EOF"))
	if got := c.set.NetworkOf(n.ID()); got != "" {
		t.Fatalf("network after failed Status = %q, want unclassified", got)
	}
	if got := c.set.CountELCandidates(); got != 1 {
		t.Fatalf("candidate count after failure = %d, want retained", got)
	}
	if c.set.ClaimFingerprintAt(n.ID(), time.Now()) {
		t.Fatal("failed candidate claimable before its backoff elapsed")
	}
	// finishCandidateFingerprint schedules from the wall clock; the first retry
	// lands within [90%, 110%] of the schedule's 1m opening rung.
	if !c.set.ClaimFingerprintAt(n.ID(), time.Now().Add(70*time.Second)) {
		t.Fatal("failed candidate did not become claimable on the retry schedule")
	}
}

func TestFinishCandidateFingerprintSuccessPromotes(t *testing.T) {
	c := candidateCrawler(t, 10)
	n := seqZeroNode(t, 4)
	now := time.Unix(1700000000, 0)
	if !c.retainCandidate(n, now) {
		t.Fatal("candidate was not retained")
	}
	if !c.set.ClaimFingerprintAt(n.ID(), now) {
		t.Fatal("claim failed")
	}
	r := enrich.Fingerprint{
		Client: "Nethermind", Version: "v1.39.3", Network: "mainnet",
		ForkID: forkid.ID{Hash: [4]byte{0x07, 0xc9, 0x46, 0x2e}},
	}
	c.finishCandidateFingerprint(n, r, nil)
	rows := c.set.SnapshotNetworks([]string{"mainnet"})["mainnet"]
	if len(rows) != 1 {
		t.Fatalf("snapshot rows = %d, want promoted candidate published", len(rows))
	}
	row := rows[0]
	if row.Network != "mainnet" || row.MembershipSource != "status" || row.Client != "Nethermind" || row.ENR != "" {
		t.Fatalf("promoted row = %+v", row)
	}
	if got := c.set.CountELCandidates(); got != 0 {
		t.Fatalf("candidate count after promotion = %d, want 0", got)
	}
}

func TestApplyInboundHelloOnlyOnCandidateDoesNotVerify(t *testing.T) {
	c := candidateCrawler(t, 10)
	n := seqZeroNode(t, 5)
	now := time.Unix(1700000000, 0)
	if !c.retainCandidate(n, now) {
		t.Fatal("candidate was not retained")
	}
	c.applyInbound(n.ID(), layerEL, enrich.Fingerprint{Client: "Nethermind", Version: "v1.39.3"})
	if got := c.set.NetworkOf(n.ID()); got != "" {
		t.Fatalf("network after hello-only inbound = %q, want unclassified", got)
	}
	if got := c.set.CountELCandidates(); got != 1 {
		t.Fatalf("candidate count = %d, want 1", got)
	}
	if !c.set.ClaimFingerprintAt(n.ID(), now) {
		t.Fatal("hello-only inbound stopped Status retries")
	}
}

func TestApplyInboundStatusPromotesCandidate(t *testing.T) {
	c := candidateCrawler(t, 10)
	n := seqZeroNode(t, 6)
	now := time.Unix(1700000000, 0)
	if !c.retainCandidate(n, now) {
		t.Fatal("candidate was not retained")
	}
	r := enrich.Fingerprint{
		Client: "Nethermind", Version: "v1.39.3", Network: "mainnet",
		ForkID: forkid.ID{Hash: [4]byte{0x07, 0xc9, 0x46, 0x2e}},
	}
	c.applyInbound(n.ID(), layerEL, r)
	if got := c.set.NetworkOf(n.ID()); got != "mainnet" {
		t.Fatalf("network after inbound Status = %q, want mainnet", got)
	}
	if got := len(c.set.SnapshotNetworks([]string{"mainnet"})["mainnet"]); got != 0 {
		t.Fatalf("snapshot rows after one inbound sighting = %d, want 0 until re-confirmed", got)
	}
}

func TestRetainCandidateDeclinesPromotedEndpointChange(t *testing.T) {
	c := candidateCrawler(t, 10)
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	n := enode.NewV4(&key.PublicKey, []byte{8, 8, 8, 7}, 30303, 30303)
	moved := enode.NewV4(&key.PublicKey, []byte{8, 8, 9, 7}, 30311, 30311)
	now := time.Unix(1700000000, 0)
	if !c.retainCandidate(n, now) {
		t.Fatal("candidate was not retained")
	}
	if !c.set.ClaimFingerprintAt(n.ID(), now) {
		t.Fatal("claim failed")
	}
	r := enrich.Fingerprint{
		Client: "Nethermind", Version: "v1.39.3", Network: "mainnet",
		ForkID: forkid.ID{Hash: [4]byte{0x07, 0xc9, 0x46, 0x2e}},
	}
	c.finishCandidateFingerprint(n, r, nil)

	if !c.retainCandidate(n, now.Add(time.Minute)) {
		t.Fatal("same-endpoint re-sight of a promoted row was not absorbed")
	}
	if c.retainCandidate(moved, now.Add(2*time.Minute)) {
		t.Fatal("moved-endpoint re-sight was absorbed instead of falling back to the legacy dial")
	}
}
