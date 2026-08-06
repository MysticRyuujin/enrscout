package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"

	"github.com/MysticRyuujin/enrscout/internal/nodeset"
)

// fakeZone is a Cloudflare stand-in that records the order batches arrive in, which is what the
// publish ordering guarantees depend on.
type fakeZone struct {
	records  map[string]cfRecord
	nextID   int
	batches  []cfBatch
	failOn   func(cfBatch) error
	listFail bool
}

func newFakeZone(existing ...cfRecord) *fakeZone {
	z := &fakeZone{records: map[string]cfRecord{}}
	for _, r := range existing {
		z.nextID++
		r.ID = fmt.Sprintf("id%d", z.nextID)
		r.Name = strings.ToLower(r.Name)
		z.records[r.Name] = r
	}
	return z
}

func (z *fakeZone) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/dns_records/batch") {
			var batch cfBatch
			if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if z.failOn != nil {
				if err := z.failOn(batch); err != nil {
					_ = json.NewEncoder(w).Encode(cfEnvelope{
						Errors: []cfAPIError{{Code: 1004, Message: err.Error()}},
					})
					return
				}
			}
			z.batches = append(z.batches, batch)
			for _, d := range batch.Deletes {
				for name, rec := range z.records {
					if rec.ID == d.ID {
						delete(z.records, name)
					}
				}
			}
			for _, p := range append(batch.Puts, batch.Posts...) {
				if p.ID == "" {
					z.nextID++
					p.ID = fmt.Sprintf("id%d", z.nextID)
				}
				// A real zone stores names lowercased, which is what exposes case-sensitive diffing.
				p.Name = strings.ToLower(p.Name)
				z.records[p.Name] = p
			}
			_ = json.NewEncoder(w).Encode(cfEnvelope{Success: true, Result: json.RawMessage(`{}`)})
			return
		}
		if z.listFail {
			_ = json.NewEncoder(w).Encode(cfEnvelope{Errors: []cfAPIError{{Code: 9109, Message: "unauthorized"}}})
			return
		}
		out := make([]cfRecord, 0, len(z.records))
		for _, rec := range z.records {
			out = append(out, rec)
		}
		body, _ := json.Marshal(out)
		env := cfEnvelope{Success: true, Result: body}
		env.Info.Page, env.Info.TotalPages = 1, 1
		_ = json.NewEncoder(w).Encode(env)
	})
}

func fakeCloudflare(t *testing.T, z *fakeZone) *cloudflareDNS {
	t.Helper()
	srv := httptest.NewServer(z.handler())
	t.Cleanup(srv.Close)
	c := newCloudflareDNS("zone1", "token")
	c.baseURL = srv.URL
	c.settle = 0
	return c
}

const cfDomain = "all.hoodi.example.org"

func TestSyncWritesEntriesBeforeTheRoot(t *testing.T) {
	z := newFakeZone()
	c := fakeCloudflare(t, z)
	want := map[string]string{
		cfDomain:          "enrtree-root:v1 seq=1",
		"AAA." + cfDomain: "enrtree-branch:BBB",
		"BBB." + cfDomain: "enr:-abc",
	}
	if _, err := c.Sync(context.Background(), cfDomain, want, nil); err != nil {
		t.Fatal(err)
	}
	rootBatch := -1
	for i, b := range z.batches {
		for _, r := range append(b.Puts, b.Posts...) {
			if r.Name == cfDomain {
				rootBatch = i
			} else if rootBatch != -1 {
				t.Fatalf("entry %s written in batch %d, after the root in batch %d", r.Name, i, rootBatch)
			}
		}
	}
	if rootBatch == -1 {
		t.Fatal("root was never written")
	}
	if rootBatch == 0 && len(z.batches) > 1 {
		t.Fatal("root shared the first batch with entries rather than following them")
	}
}

func TestSyncRetainsThePreviousGeneration(t *testing.T) {
	stale := cfRecord{Type: "TXT", Name: "OLD." + cfDomain, Content: "enr:-old", TTL: entryTTL}
	ancient := cfRecord{Type: "TXT", Name: "ANCIENT." + cfDomain, Content: "enr:-ancient", TTL: entryTTL}
	z := newFakeZone(stale, ancient)
	c := fakeCloudflare(t, z)

	want := map[string]string{cfDomain: "enrtree-root:v1 seq=2", "NEW." + cfDomain: "enr:-new"}
	retain := map[string]string{"OLD." + cfDomain: "enr:-old"}
	if _, err := c.Sync(context.Background(), cfDomain, want, retain); err != nil {
		t.Fatal(err)
	}
	if _, ok := z.records[dnsKey("OLD."+cfDomain)]; !ok {
		t.Error("retained record was deleted; a client on the previous root cannot finish its walk")
	}
	if _, ok := z.records[dnsKey("ANCIENT."+cfDomain)]; ok {
		t.Error("two-generations-old record was never garbage collected")
	}
	if _, ok := z.records[dnsKey("NEW."+cfDomain)]; !ok {
		t.Error("new entry was not written")
	}
}

func TestSyncLeavesUnchangedRecordsAlone(t *testing.T) {
	root := cfRecord{Type: "TXT", Name: cfDomain, Content: "enrtree-root:v1 seq=1", TTL: rootTTL}
	entry := cfRecord{Type: "TXT", Name: "AAA." + cfDomain, Content: "enr:-abc", TTL: entryTTL}
	z := newFakeZone(root, entry)
	c := fakeCloudflare(t, z)

	want := map[string]string{cfDomain: root.Content, "AAA." + cfDomain: entry.Content}
	changed, err := c.Sync(context.Background(), cfDomain, want, nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Errorf("changed %d records when nothing differed", changed)
	}
	if len(z.batches) != 0 {
		t.Errorf("issued %d batches when nothing differed", len(z.batches))
	}
}

// A zone may return content longer than one character-string in its quoted presentation form.
// Without normalization every long ENR would look changed on every cycle.
func TestSyncNormalizesSplitContent(t *testing.T) {
	long := "enr:-" + strings.Repeat("x", 300)
	split := fmt.Sprintf("%q %q", long[:255], long[255:])
	z := newFakeZone(cfRecord{Type: "TXT", Name: "AAA." + cfDomain, Content: split, TTL: entryTTL})
	c := fakeCloudflare(t, z)

	changed, err := c.Sync(context.Background(), cfDomain, map[string]string{"AAA." + cfDomain: long}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Errorf("a split long record was seen as changed: %d", changed)
	}
}

// Cloudflare quotes TXT content on the caller's behalf and returns it quoted, so an unchanged
// record must not be seen as changed once it has made a round trip through the zone.
func TestSyncIgnoresQuotingAddedByTheZone(t *testing.T) {
	root := "enrtree-root:v1 e=VYEJEBXEIQO6V6O4GAM4E3PLUY l=FDXN3SN67NA5DKA4J2GOK7BVQI seq=1 sig=abc"
	z := newFakeZone(
		cfRecord{Type: "TXT", Name: cfDomain, Content: fmt.Sprintf("%q", root), TTL: rootTTL},
		cfRecord{Type: "TXT", Name: "AAA." + cfDomain, Content: `"enr:-abc"`, TTL: entryTTL},
	)
	c := fakeCloudflare(t, z)

	want := map[string]string{cfDomain: root, "AAA." + cfDomain: "enr:-abc"}
	changed, err := c.Sync(context.Background(), cfDomain, want, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Errorf("changed %d records that differ only by the zone's own quoting", changed)
	}
}

func TestSyncFailsWithoutWritingWhenListingFails(t *testing.T) {
	z := &fakeZone{records: map[string]cfRecord{}, listFail: true}
	c := fakeCloudflare(t, z)
	if _, err := c.Sync(context.Background(), cfDomain, map[string]string{cfDomain: "root"}, nil); err == nil {
		t.Fatal("sync succeeded despite an unreadable zone")
	}
	if len(z.batches) != 0 {
		t.Error("wrote records despite failing to list the zone first")
	}
}

func TestSyncReportsRootFailure(t *testing.T) {
	z := newFakeZone()
	z.failOn = func(b cfBatch) error {
		for _, r := range append(b.Puts, b.Posts...) {
			if r.Name == cfDomain {
				return errors.New("root rejected")
			}
		}
		return nil
	}
	c := fakeCloudflare(t, z)
	want := map[string]string{cfDomain: "enrtree-root:v1 seq=1", "AAA." + cfDomain: "enr:-abc"}
	if _, err := c.Sync(context.Background(), cfDomain, want, nil); err == nil {
		t.Fatal("sync succeeded despite the root write failing")
	}
}

type stubPublisher struct {
	err     error
	domains []string
}

func (s *stubPublisher) Sync(_ context.Context, domain string, _, _ map[string]string) (int, error) {
	s.domains = append(s.domains, domain)
	return 0, s.err
}

func seedArtifact(t *testing.T, outDir, name string, out output) {
	t.Helper()
	if _, err := emitArtifact(out, outDir, name); err != nil {
		t.Fatal(err)
	}
}

func treeOutput(domain string, nodes int, seq uint64) output {
	return output{
		SchemaVersion: outputSchemaVersion, URL: "enrtree://X@" + domain, Domain: domain,
		Network: "hoodi", Capability: "all", Nodes: nodes, Seq: seq,
		Records: map[string]string{domain: fmt.Sprintf("enrtree-root:v1 seq=%d", seq)},
	}
}

// The collapse guard has to measure against what DNS actually serves. If a failed push advanced the
// baseline, the next cycle would compare against a tree nobody can resolve and let a real collapse
// through — while the sequence floor must still advance, or changed content could reuse a sequence.
func TestFailedPushAdvancesTheSequenceButNotTheBaseline(t *testing.T) {
	outDir := t.TempDir()
	cfg := multiConfig{outDir: outDir, publisher: &stubPublisher{err: errors.New("zone unreachable")}}

	seedArtifact(t, outDir, cfDomain+publishedSuffix, treeOutput(cfDomain, 1000, 5))
	built := treeOutput(cfDomain, 600, 6)
	seedArtifact(t, outDir, cfDomain, built)

	if err := publishNetwork(context.Background(), cfg, "hoodi", []output{built}); err == nil {
		t.Fatal("publishNetwork reported success despite the push failing")
	}
	nodes, seq, err := baselinesFor(cfg, cfDomain, "hoodi", "all")
	if err != nil {
		t.Fatal(err)
	}
	if nodes != 1000 {
		t.Errorf("collapse baseline moved to %d; a failed push must leave it at what DNS serves (1000)", nodes)
	}
	if seq != 6 {
		t.Errorf("sequence floor = %d, want 6: an attempted publish must not reuse a sequence", seq)
	}
}

func TestSuccessfulPushCommitsThePublishedBaseline(t *testing.T) {
	outDir := t.TempDir()
	cfg := multiConfig{outDir: outDir, publisher: &stubPublisher{}}

	seedArtifact(t, outDir, cfDomain+publishedSuffix, treeOutput(cfDomain, 1000, 5))
	built := treeOutput(cfDomain, 900, 6)
	seedArtifact(t, outDir, cfDomain, built)

	if err := publishNetwork(context.Background(), cfg, "hoodi", []output{built}); err != nil {
		t.Fatal(err)
	}
	nodes, _, err := baselinesFor(cfg, cfDomain, "hoodi", "all")
	if err != nil {
		t.Fatal(err)
	}
	if nodes != 900 {
		t.Errorf("collapse baseline = %d, want 900 after a successful publish", nodes)
	}
}

// Without a publisher the artifact write is the publication, so the baseline must keep coming from
// the built artifact exactly as it did before DNS support existed.
func TestArtifactOnlyModeKeepsItsOwnBaseline(t *testing.T) {
	outDir := t.TempDir()
	cfg := multiConfig{outDir: outDir}
	seedArtifact(t, outDir, cfDomain, treeOutput(cfDomain, 750, 4))

	nodes, seq, err := baselinesFor(cfg, cfDomain, "hoodi", "all")
	if err != nil {
		t.Fatal(err)
	}
	if nodes != 750 || seq != 4 {
		t.Errorf("artifact-only baseline = (%d, %d), want (750, 4)", nodes, seq)
	}
}

// ToTXT emits uppercase base32 labels and a zone lowercases them, so a case-sensitive diff sees
// every entry as both missing and stale and churns the whole tree on every cycle.
func TestSyncIsStableAcrossCaseFoldedNames(t *testing.T) {
	z := newFakeZone()
	c := fakeCloudflare(t, z)
	want := map[string]string{
		cfDomain:                                 "enrtree-root:v1 seq=1",
		"I7575NHE3IIZZT6HFE7BRX6NP4." + cfDomain: "enr:-abc",
		"JWXYDBPXYWG6FX3GMDIBFA6CJ4." + cfDomain: "enrtree-branch:",
	}
	if _, err := c.Sync(context.Background(), cfDomain, want, nil); err != nil {
		t.Fatal(err)
	}
	first := len(z.batches)
	if first == 0 {
		t.Fatal("first publish wrote nothing")
	}
	changed, err := c.Sync(context.Background(), cfDomain, want, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Errorf("republishing an identical tree changed %d records", changed)
	}
	if len(z.batches) != first {
		t.Errorf("republishing issued %d more batches; uppercase labels are not matching the stored names", len(z.batches)-first)
	}
	if len(z.records) != len(want) {
		t.Errorf("zone holds %d records for a %d record tree; entries were duplicated", len(z.records), len(want))
	}
}

// Pointing at a zone already serving a tree must not delete it before anything has been published.
func TestFirstPublishNeverPrunes(t *testing.T) {
	foreign := cfRecord{Type: "TXT", Name: "existing." + cfDomain, Content: "enr:-live", TTL: entryTTL}
	z := newFakeZone(foreign)
	c := fakeCloudflare(t, z)

	if _, err := c.Sync(context.Background(), cfDomain, map[string]string{cfDomain: "enrtree-root:v1 seq=1"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := z.records[dnsKey("existing."+cfDomain)]; !ok {
		t.Error("first publish deleted a record it had never published")
	}
}

func TestChangedCountsAppliedWritesOnly(t *testing.T) {
	z := newFakeZone()
	z.failOn = func(b cfBatch) error {
		for _, r := range append(b.Puts, b.Posts...) {
			if r.Name == cfDomain {
				return errors.New("root rejected")
			}
		}
		return nil
	}
	c := fakeCloudflare(t, z)
	want := map[string]string{cfDomain: "enrtree-root:v1 seq=1", "AAA." + cfDomain: "enr:-abc"}
	changed, err := c.Sync(context.Background(), cfDomain, want, nil)
	if err == nil {
		t.Fatal("expected the root write to fail")
	}
	if changed != 1 {
		t.Errorf("changed = %d, want 1: the entry landed even though the root did not", changed)
	}
}

func TestBatchesRespectTheOperationLimit(t *testing.T) {
	z := newFakeZone()
	c := fakeCloudflare(t, z)
	want := map[string]string{cfDomain: "enrtree-root:v1 seq=1"}
	for i := range cloudflareBatchMax*2 + 5 {
		want[fmt.Sprintf("E%d.%s", i, cfDomain)] = fmt.Sprintf("enr:-%d", i)
	}
	if _, err := c.Sync(context.Background(), cfDomain, want, nil); err != nil {
		t.Fatal(err)
	}
	for i, b := range z.batches {
		if b.len() > cloudflareBatchMax {
			t.Fatalf("batch %d carries %d operations, over the %d limit", i, b.len(), cloudflareBatchMax)
		}
	}
}

func balanceRows(t *testing.T, perClient map[string]int) []nodeset.Row {
	t.Helper()
	now := time.Now()
	var rows []nodeset.Row
	score := int32(10)
	names := make([]string, 0, len(perClient))
	for name := range perClient {
		names = append(names, name)
	}
	sort.Strings(names)
	last := 1
	for _, name := range names {
		for i := 0; i < perClient[name]; i++ {
			row := currentMainnetEL(t, v4Row(t, byte(last), 30303, score, now), now)
			row.Client = name
			row.FPStatus = "ok"
			row.ID = fmt.Sprintf("%s-%04d", name, i)
			rows = append(rows, row)
			last++
			if last > 250 {
				last = 1
			}
		}
	}
	return rows
}

// Keyed by bucket rather than raw label: an unrecognized name is selected through the unknown bucket
// and must be counted there.
func clientMix(nodes []*enode.Node, rows []nodeset.Row) map[string]int {
	byENR := map[string]string{}
	for _, r := range rows {
		byENR[r.ENR] = clientBucket(r)
	}
	mix := map[string]int{}
	for _, n := range nodes {
		mix[byENR[n.String()]]++
	}
	return mix
}

// Score correlates with client, so taking the highest scoring nodes outright drops whole clients from
// a list peers bootstrap against. Every client in the pool must survive the limit.
func TestProportionalBalanceKeepsEveryClient(t *testing.T) {
	pool := map[string]int{"Geth": 400, "Reth": 120, "Erigon": 80, "Besu": 50, "Nethermind": 30}
	rows := balanceRows(t, pool)
	opt := selectOpts{minScore: 1, protocol: "any", layer: "el", capability: "all", limit: 25, balance: balanceProportional}

	got := selectNodes(rows, opt, time.Now())
	if len(got) != 25 {
		t.Fatalf("selected %d nodes, want 25", len(got))
	}
	mix := clientMix(got, rows)
	for client := range pool {
		if mix[client] == 0 {
			t.Errorf("client %s was excluded entirely", client)
		}
	}
	if mix["Geth"] >= 20 {
		t.Errorf("Geth took %d of 25 slots; the pool share is 60%%", mix["Geth"])
	}
}

// The tree layout depends on input order and every changed record costs a DNS write, so an unchanged
// candidate set must select an identical set in an identical order.
func TestProportionalBalanceIsDeterministic(t *testing.T) {
	rows := balanceRows(t, map[string]int{"Geth": 40, "Reth": 20, "Besu": 11})
	opt := selectOpts{minScore: 1, protocol: "any", layer: "el", capability: "all", limit: 15, balance: balanceProportional}

	first := selectNodes(rows, opt, time.Now())
	for range 5 {
		again := selectNodes(rows, opt, time.Now())
		if len(again) != len(first) {
			t.Fatalf("selection size changed between runs: %d then %d", len(first), len(again))
		}
		for i := range first {
			if first[i].String() != again[i].String() {
				t.Fatalf("selection order changed at index %d", i)
			}
		}
	}
}

func TestBalanceNoneTakesHighestScoringOnly(t *testing.T) {
	rows := balanceRows(t, map[string]int{"Geth": 40, "Reth": 20})
	opt := selectOpts{minScore: 1, protocol: "any", layer: "el", capability: "all", limit: 10, balance: balanceNone}
	if got := len(selectNodes(rows, opt, time.Now())); got != 10 {
		t.Fatalf("selected %d nodes, want 10", got)
	}
}

// A limit larger than the pool must still fill from every client without over-allocating.
func TestProportionalBalanceHandlesLimitAbovePool(t *testing.T) {
	rows := balanceRows(t, map[string]int{"Geth": 5, "Reth": 3, "Besu": 1})
	opt := selectOpts{minScore: 1, protocol: "any", layer: "el", capability: "all", limit: 100, balance: balanceProportional}
	got := selectNodes(rows, opt, time.Now())
	if len(got) != 9 {
		t.Fatalf("selected %d nodes, want the whole 9 node pool", len(got))
	}
}

// Row.Client can be self-declared in an ENR, so invented names must not each reserve a slot.
func TestInventedClientNamesCannotCrowdOutRealClients(t *testing.T) {
	now := time.Now()
	var rows []nodeset.Row
	real := map[string]int{"Geth": 40, "Reth": 20, "Besu": 10}
	rows = append(rows, balanceRows(t, real)...)
	// One sybil per slot, each claiming a different unrecognized client, none fingerprinted.
	for i := range 40 {
		row := currentMainnetEL(t, v4Row(t, byte(i+60), 30303, 10, now), now)
		row.Client = fmt.Sprintf("TotallyRealClient%02d", i)
		row.FPStatus = "pending"
		row.ID = fmt.Sprintf("sybil-%04d", i)
		rows = append(rows, row)
	}

	opt := selectOpts{minScore: 1, protocol: "any", layer: "el", capability: "all", limit: 25, balance: balanceProportional}
	got := selectNodes(rows, opt, time.Now())
	mix := clientMix(got, rows)

	for name := range mix {
		if strings.HasPrefix(name, "TotallyRealClient") {
			t.Errorf("invented name %q became its own bucket and reserved a slot", name)
		}
	}
	for client := range real {
		if mix[client] == 0 {
			t.Errorf("real client %s was crowded out", client)
		}
	}
	// Collapsing into one bucket caps the sybils at that bucket's proportional share. Contributing
	// 40 of 110 candidates does earn ~36% of the tree; what it must not earn is 40 reserved slots.
	if unknown := mix[unknownClient]; unknown > 10 {
		t.Errorf("unknown bucket took %d of 25 slots, above its ~36%% pool share", unknown)
	}
}

// A recognized name without a verified fingerprint is still only a claim, so it must not reserve a slot.
func TestUnverifiedFingerprintDoesNotReserveASlot(t *testing.T) {
	claimed := currentMainnetEL(t, v4Row(t, 9, 30303, 10, time.Now()), time.Now())
	claimed.Client = "Reth"
	claimed.FPStatus = "pending"
	if got := clientBucket(claimed); got != unknownClient {
		t.Errorf("unverified Reth claim bucketed as %q, want %q", got, unknownClient)
	}
	claimed.FPStatus = "ok"
	if got := clientBucket(claimed); got != "Reth" {
		t.Errorf("verified Reth bucketed as %q, want Reth", got)
	}
}
