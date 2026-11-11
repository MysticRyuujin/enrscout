package main

import (
	"testing"

	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/MysticRyuujin/enrscout/internal/enrich"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
)

func TestCLMetadataWithoutENRCustodyIsNeverAMatch(t *testing.T) {
	set := nodeset.NewWithLimit(0)
	fp := enrich.Fingerprint{Client: "Lighthouse", Metadata: enrich.CLMetadata{Outcome: "ok", Version: 3, CGCKnown: true}}
	unknown := testutil.ToFloat64(mCLCGCCompare.WithLabelValues("Lighthouse", "enr_unknown"))
	match := testutil.ToFloat64(mCLCGCCompare.WithLabelValues("Lighthouse", "match"))

	observeCLMetadata(set, "inbound", enode.ID{1}, fp)

	if got := testutil.ToFloat64(mCLCGCCompare.WithLabelValues("Lighthouse", "enr_unknown")); got != unknown+1 {
		t.Fatalf("enr_unknown = %v, want %v", got, unknown+1)
	}
	if got := testutil.ToFloat64(mCLCGCCompare.WithLabelValues("Lighthouse", "match")); got != match {
		t.Fatalf("match = %v, want %v: a missing ENR cgc was compared as zero", got, match)
	}
}
