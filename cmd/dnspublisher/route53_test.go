package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"
)

type fakeSet struct {
	rtype  types.RRType
	values []string
	ttl    int64
}

// fakeRoute53 rejects the changes a real hosted zone rejects - a CREATE over an existing name, a
// DELETE whose values do not repeat what is stored, a duplicate name inside one batch, an oversized
// batch - so a test that passes here is not relying on a forgiving stand-in.
type fakeRoute53 struct {
	stored   map[string]fakeSet
	batches  [][]types.Change
	calls    []string
	pending  map[string]int
	polls    int
	stuck    bool
	pageSize int
	nextID   int
	listFail bool
	failOn   func([]types.Change) error
}

func newFakeRoute53() *fakeRoute53 {
	return &fakeRoute53{stored: map[string]fakeSet{}, pending: map[string]int{}}
}

func (z *fakeRoute53) seedTXT(name, content string, ttl int64) {
	z.stored[dnsKey(name)] = fakeSet{rtype: types.RRTypeTxt, values: []string{splitTXT(content)}, ttl: ttl}
}

// seedForeignTXT stores content the way another tool might have: split at 255 rather than 253, and
// across several resource records instead of one.
func (z *fakeRoute53) seedForeignTXT(name, content string, ttl int64) {
	var values []string
	for len(content) > 0 {
		n := min(len(content), 255)
		values = append(values, strconv.Quote(content[:n]))
		content = content[n:]
	}
	z.stored[dnsKey(name)] = fakeSet{rtype: types.RRTypeTxt, values: values, ttl: ttl}
}

func (z *fakeRoute53) ListResourceRecordSets(_ context.Context, in *route53.ListResourceRecordSetsInput, _ ...func(*route53.Options)) (*route53.ListResourceRecordSetsOutput, error) {
	if z.listFail {
		return nil, errors.New("access denied")
	}
	names := make([]string, 0, len(z.stored))
	for name := range z.stored {
		names = append(names, name)
	}
	sort.Strings(names)
	if in.StartRecordName != nil {
		for len(names) > 0 && names[0] < dnsKey(*in.StartRecordName) {
			names = names[1:]
		}
	}
	page, truncated := names, false
	if z.pageSize > 0 && len(names) > z.pageSize {
		page, truncated = names[:z.pageSize], true
	}
	out := &route53.ListResourceRecordSetsOutput{IsTruncated: truncated}
	for _, name := range page {
		set := z.stored[name]
		// A real zone answers with a trailing dot and lowercased labels.
		rrset := types.ResourceRecordSet{Name: aws.String(name + "."), Type: set.rtype, TTL: aws.Int64(set.ttl)}
		for _, value := range set.values {
			rrset.ResourceRecords = append(rrset.ResourceRecords, types.ResourceRecord{Value: aws.String(value)})
		}
		out.ResourceRecordSets = append(out.ResourceRecordSets, rrset)
	}
	if truncated {
		out.NextRecordName, out.NextRecordType = aws.String(names[z.pageSize]), types.RRTypeTxt
	}
	return out, nil
}

func (z *fakeRoute53) ChangeResourceRecordSets(_ context.Context, in *route53.ChangeResourceRecordSetsInput, _ ...func(*route53.Options)) (*route53.ChangeResourceRecordSetsOutput, error) {
	batch := in.ChangeBatch.Changes
	if z.failOn != nil {
		if err := z.failOn(batch); err != nil {
			return nil, err
		}
	}
	size, count := 0, 0
	seen := map[string]bool{}
	for _, change := range batch {
		n := changeCount(change)
		size += changeSize(change) * n
		count += n
		name := dnsKey(*change.ResourceRecordSet.Name)
		if seen[name] {
			return nil, fmt.Errorf("duplicate name %s in one change batch", name)
		}
		seen[name] = true
	}
	if count > route53ChangeCountLimit || size > route53ChangeSizeLimit {
		return nil, fmt.Errorf("batch of %d changes and %d bytes exceeds the limits", count, size)
	}
	for _, change := range batch {
		name := dnsKey(*change.ResourceRecordSet.Name)
		current, exists := z.stored[name]
		incoming := fakeSet{rtype: change.ResourceRecordSet.Type, ttl: *change.ResourceRecordSet.TTL}
		for _, rr := range change.ResourceRecordSet.ResourceRecords {
			incoming.values = append(incoming.values, *rr.Value)
		}
		switch change.Action {
		case types.ChangeActionCreate:
			if exists {
				return nil, fmt.Errorf("CREATE of already existing %s", name)
			}
			z.stored[name] = incoming
		case types.ChangeActionUpsert:
			z.stored[name] = incoming
		case types.ChangeActionDelete:
			if !exists {
				return nil, fmt.Errorf("DELETE of unknown %s", name)
			}
			if strings.Join(current.values, "") != strings.Join(incoming.values, "") || current.ttl != incoming.ttl {
				return nil, fmt.Errorf("DELETE of %s does not repeat the stored values and TTL", name)
			}
			delete(z.stored, name)
		}
	}
	z.batches = append(z.batches, batch)
	z.nextID++
	id := fmt.Sprintf("/change/C%d", z.nextID)
	z.calls = append(z.calls, "change "+id)
	z.pending[id] = z.polls
	return &route53.ChangeResourceRecordSetsOutput{
		ChangeInfo: &types.ChangeInfo{Id: aws.String(id), Status: types.ChangeStatusPending},
	}, nil
}

func (z *fakeRoute53) GetChange(_ context.Context, in *route53.GetChangeInput, _ ...func(*route53.Options)) (*route53.GetChangeOutput, error) {
	id := *in.Id
	z.calls = append(z.calls, "sync "+id)
	status := types.ChangeStatusInsync
	if z.stuck || z.pending[id] > 0 {
		z.pending[id]--
		status = types.ChangeStatusPending
	}
	return &route53.GetChangeOutput{ChangeInfo: &types.ChangeInfo{Id: in.Id, Status: status}}, nil
}

func fakeRoute53DNS(t *testing.T, z *fakeRoute53) *route53DNS {
	t.Helper()
	return &route53DNS{api: z, zoneID: "Z1", poll: time.Millisecond, syncTimeout: 2 * time.Second}
}

const r53Domain = "all.hoodi.example.org"

func r53Tree(seq int) map[string]string {
	return map[string]string{
		r53Domain: fmt.Sprintf("enrtree-root:v1 e=AAA l=BBB seq=%d sig=xyz", seq),
		"I7575NHE3IIZZT6HFE7BRX6NP4." + r53Domain: "enrtree-branch:JWXYDBPXYWG6FX3GMDIBFA6CJ4",
		"JWXYDBPXYWG6FX3GMDIBFA6CJ4." + r53Domain: "enr:-abc",
	}
}

func TestRoute53WritesEntriesBeforeTheRoot(t *testing.T) {
	z := newFakeRoute53()
	c := fakeRoute53DNS(t, z)
	if _, err := c.Sync(context.Background(), r53Domain, r53Tree(1), nil); err != nil {
		t.Fatal(err)
	}
	rootBatch := -1
	for i, batch := range z.batches {
		for _, change := range batch {
			if dnsKey(*change.ResourceRecordSet.Name) == r53Domain {
				rootBatch = i
			} else if rootBatch != -1 {
				t.Fatalf("entry %s written in batch %d, after the root in batch %d", *change.ResourceRecordSet.Name, i, rootBatch)
			}
		}
	}
	if rootBatch < 1 {
		t.Fatalf("root written in batch %d; it must follow the entry batches", rootBatch)
	}
}

// Ordering the API calls does not order propagation. If the root is written while its entries are
// still PENDING, a resolver can follow the new root into a subtree it cannot resolve yet.
func TestRoute53ConfirmsEntriesInSyncBeforeTheRoot(t *testing.T) {
	z := newFakeRoute53()
	z.polls = 3
	c := fakeRoute53DNS(t, z)
	if _, err := c.Sync(context.Background(), r53Domain, r53Tree(1), nil); err != nil {
		t.Fatal(err)
	}
	entryChange, rootChange, lastSync := -1, -1, -1
	for i, call := range z.calls {
		switch {
		case call == "change /change/C1":
			entryChange = i
		case strings.HasPrefix(call, "change "):
			rootChange = i
		case call == "sync /change/C1":
			lastSync = i
		}
	}
	if entryChange == -1 || rootChange == -1 {
		t.Fatalf("expected an entry write and a root write, got %v", z.calls)
	}
	if lastSync == -1 {
		t.Fatalf("entry propagation was never awaited: %v", z.calls)
	}
	if lastSync > rootChange {
		t.Errorf("root written at call %d before the entries were in sync at %d: %v", rootChange, lastSync, z.calls)
	}
}

// Failing before the root is written leaves entries the still-current root does not reference. The
// opposite order would publish a root pointing into a subtree nobody can resolve.
func TestRoute53AbortsBeforeTheRootWhenPropagationStalls(t *testing.T) {
	z := newFakeRoute53()
	z.stuck = true
	c := fakeRoute53DNS(t, z)
	c.syncTimeout = 10 * time.Millisecond
	if _, err := c.Sync(context.Background(), r53Domain, r53Tree(1), nil); err == nil {
		t.Fatal("sync succeeded despite entries never reaching INSYNC")
	}
	if _, ok := z.stored[r53Domain]; ok {
		t.Error("root was written while its entries were still pending")
	}
	if len(z.stored) == 0 {
		t.Error("no entries were written at all; the test is not exercising the stall")
	}
}

func TestRoute53RetainsThePreviousGeneration(t *testing.T) {
	z := newFakeRoute53()
	z.seedTXT("OLD."+r53Domain, "enr:-old", entryTTL)
	z.seedTXT("ANCIENT."+r53Domain, "enr:-ancient", entryTTL)
	c := fakeRoute53DNS(t, z)

	want := map[string]string{r53Domain: "enrtree-root:v1 seq=2", "NEW." + r53Domain: "enr:-new"}
	retain := map[string]string{"OLD." + r53Domain: "enr:-old"}
	if _, err := c.Sync(context.Background(), r53Domain, want, retain); err != nil {
		t.Fatal(err)
	}
	if _, ok := z.stored[dnsKey("OLD."+r53Domain)]; !ok {
		t.Error("retained record was deleted; a client on the previous root cannot finish its walk")
	}
	if _, ok := z.stored[dnsKey("ANCIENT."+r53Domain)]; ok {
		t.Error("two-generations-old record was never garbage collected")
	}
	if _, ok := z.stored[dnsKey("NEW."+r53Domain)]; !ok {
		t.Error("new entry was not written")
	}
}

// A DELETE must repeat the stored values and TTL exactly. Another tool's record may be split at other
// boundaries and across several values, so pruning works only if the stored values are sent verbatim
// rather than re-rendered from the content.
func TestRoute53PrunesRecordsSplitAcrossCharacterStrings(t *testing.T) {
	long := "enr:-" + strings.Repeat("x", 600)
	z := newFakeRoute53()
	z.seedForeignTXT("STALE."+r53Domain, long, 900)
	c := fakeRoute53DNS(t, z)

	want := map[string]string{r53Domain: "enrtree-root:v1 seq=2"}
	if _, err := c.Sync(context.Background(), r53Domain, want, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := z.stored[dnsKey("STALE."+r53Domain)]; ok {
		t.Error("stale multi-chunk record was not pruned")
	}
}

func TestRoute53LeavesUnchangedRecordsAlone(t *testing.T) {
	long := "enr:-" + strings.Repeat("x", 600)
	want := r53Tree(1)
	want["LONG."+r53Domain] = long

	z := newFakeRoute53()
	for name, content := range want {
		z.seedTXT(name, content, int64(ttlFor(dnsKey(name), r53Domain)))
	}
	c := fakeRoute53DNS(t, z)

	changed, err := c.Sync(context.Background(), r53Domain, want, nil)
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

// Chunk boundaries are presentation only: a resolver concatenates the character-strings. Comparing the
// rendered form would rewrite every long record a differently-splitting tool published.
func TestRoute53IgnoresForeignChunkBoundaries(t *testing.T) {
	long := "enr:-" + strings.Repeat("x", 600)
	z := newFakeRoute53()
	z.seedForeignTXT("LONG."+r53Domain, long, entryTTL)
	c := fakeRoute53DNS(t, z)

	changed, err := c.Sync(context.Background(), r53Domain, map[string]string{"LONG." + r53Domain: long}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Errorf("changed %d records that differ only in how the zone split them", changed)
	}
}

// ToTXT emits uppercase base32 labels and a zone lowercases them, so a case-sensitive diff sees every
// entry as both missing and stale and churns the whole tree on every cycle.
func TestRoute53IsStableAcrossCaseFoldedNames(t *testing.T) {
	z := newFakeRoute53()
	c := fakeRoute53DNS(t, z)
	want := r53Tree(1)
	if _, err := c.Sync(context.Background(), r53Domain, want, nil); err != nil {
		t.Fatal(err)
	}
	first := len(z.batches)
	changed, err := c.Sync(context.Background(), r53Domain, want, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Errorf("republishing an identical tree changed %d records", changed)
	}
	if len(z.batches) != first {
		t.Errorf("republishing issued %d more batches; uppercase labels are not matching the stored names", len(z.batches)-first)
	}
	if len(z.stored) != len(want) {
		t.Errorf("zone holds %d records for a %d record tree; entries were duplicated", len(z.stored), len(want))
	}
}

func TestRoute53CreatesNewNamesAndUpsertsExistingOnes(t *testing.T) {
	z := newFakeRoute53()
	z.seedTXT("JWXYDBPXYWG6FX3GMDIBFA6CJ4."+r53Domain, "enr:-stale", entryTTL)
	c := fakeRoute53DNS(t, z)
	if _, err := c.Sync(context.Background(), r53Domain, r53Tree(1), nil); err != nil {
		t.Fatal(err)
	}
	actions := map[string]types.ChangeAction{}
	for _, batch := range z.batches {
		for _, change := range batch {
			actions[dnsKey(*change.ResourceRecordSet.Name)] = change.Action
		}
	}
	if got := actions[dnsKey("JWXYDBPXYWG6FX3GMDIBFA6CJ4."+r53Domain)]; got != types.ChangeActionUpsert {
		t.Errorf("existing name used %s, want UPSERT", got)
	}
	if got := actions[dnsKey("I7575NHE3IIZZT6HFE7BRX6NP4."+r53Domain)]; got != types.ChangeActionCreate {
		t.Errorf("new name used %s, want CREATE", got)
	}
}

func TestRoute53IgnoresRecordsOutsideTheTree(t *testing.T) {
	z := newFakeRoute53()
	z.seedTXT("unrelated.example.org", "v=spf1 -all", 300)
	z.stored["www."+r53Domain] = fakeSet{rtype: types.RRTypeA, values: []string{"192.0.2.1"}, ttl: 300}
	c := fakeRoute53DNS(t, z)

	if _, err := c.Sync(context.Background(), r53Domain, r53Tree(1), map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := z.stored["unrelated.example.org"]; !ok {
		t.Error("pruned a TXT record belonging to another domain in the same zone")
	}
	if _, ok := z.stored["www."+r53Domain]; !ok {
		t.Error("pruned a non-TXT record under the tree domain")
	}
}

func TestRoute53PagesThroughTheZone(t *testing.T) {
	want := r53Tree(1)
	z := newFakeRoute53()
	z.pageSize = 1
	for name, content := range want {
		z.seedTXT(name, content, int64(ttlFor(dnsKey(name), r53Domain)))
	}
	c := fakeRoute53DNS(t, z)

	changed, err := c.Sync(context.Background(), r53Domain, want, nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Errorf("changed %d records; pages after the first were not read", changed)
	}
}

func TestRoute53FailsWithoutWritingWhenListingFails(t *testing.T) {
	z := newFakeRoute53()
	z.listFail = true
	c := fakeRoute53DNS(t, z)
	if _, err := c.Sync(context.Background(), r53Domain, r53Tree(1), nil); err == nil {
		t.Fatal("sync succeeded despite an unreadable zone")
	}
	if len(z.batches) != 0 {
		t.Error("wrote records despite failing to list the zone first")
	}
}

func TestRoute53ReportsRootFailure(t *testing.T) {
	z := newFakeRoute53()
	z.failOn = func(batch []types.Change) error {
		for _, change := range batch {
			if dnsKey(*change.ResourceRecordSet.Name) == r53Domain {
				return errors.New("root rejected")
			}
		}
		return nil
	}
	c := fakeRoute53DNS(t, z)
	if _, err := c.Sync(context.Background(), r53Domain, r53Tree(1), nil); err == nil {
		t.Fatal("sync succeeded despite the root write failing")
	}
}

// A prune failure leaves records the current root does not reference, so it must not fail a publish
// whose entries and root already landed.
func TestRoute53SurvivesAFailedPrune(t *testing.T) {
	z := newFakeRoute53()
	z.seedTXT("ANCIENT."+r53Domain, "enr:-ancient", entryTTL)
	z.failOn = func(batch []types.Change) error {
		if batch[0].Action == types.ChangeActionDelete {
			return errors.New("throttled")
		}
		return nil
	}
	c := fakeRoute53DNS(t, z)
	if _, err := c.Sync(context.Background(), r53Domain, r53Tree(1), map[string]string{}); err != nil {
		t.Fatalf("a failed prune failed the whole publish: %v", err)
	}
	if _, ok := z.stored[r53Domain]; !ok {
		t.Error("root was not published")
	}
}

func TestSplitChangesRespectsTheChangeCountLimit(t *testing.T) {
	var changes []types.Change
	for i := 0; i < route53ChangeCountLimit; i++ {
		changes = append(changes, txtChange(types.ChangeActionUpsert, fmt.Sprintf("e%d.%s", i, r53Domain), entryTTL, `"enr:-x"`))
	}
	batches := splitChanges(changes)
	if len(batches) != 2 {
		t.Fatalf("%d UPSERTs became %d batches; each counts twice against the %d limit", len(changes), len(batches), route53ChangeCountLimit)
	}
	for i, batch := range batches {
		count := 0
		for _, change := range batch {
			count += changeCount(change)
		}
		if count > route53ChangeCountLimit {
			t.Errorf("batch %d holds %d counted changes, over the %d limit", i, count, route53ChangeCountLimit)
		}
	}
}

func TestSplitChangesRespectsTheRDATASizeLimit(t *testing.T) {
	value := `"` + strings.Repeat("x", 1000) + `"`
	var changes []types.Change
	for i := 0; i < 100; i++ {
		changes = append(changes, txtChange(types.ChangeActionCreate, fmt.Sprintf("e%d.%s", i, r53Domain), entryTTL, value))
	}
	batches := splitChanges(changes)
	if len(batches) < 2 {
		t.Fatalf("%d bytes of RDATA fit in %d batch", len(value)*len(changes), len(batches))
	}
	for i, batch := range batches {
		size := 0
		for _, change := range batch {
			size += changeSize(change) * changeCount(change)
		}
		if size > route53ChangeSizeLimit {
			t.Errorf("batch %d holds %d bytes of RDATA, over the %d limit", i, size, route53ChangeSizeLimit)
		}
	}
}

// devp2p splits at 253 bytes per character-string. Matching it byte-for-byte is what keeps a zone it
// already published from being rewritten on the first enrscout cycle.
func TestSplitTXTMatchesDevp2pChunking(t *testing.T) {
	content := strings.Repeat("a", 253) + strings.Repeat("b", 47)
	got := splitTXT(content)
	want := `"` + strings.Repeat("a", 253) + `"` + `"` + strings.Repeat("b", 47) + `"`
	if got != want {
		t.Errorf("splitTXT produced %d bytes:\n%s\nwant\n%s", len(got), got, want)
	}
	if normalizeTXT(got) != content {
		t.Error("chunked content does not normalize back to the original")
	}
}

func TestParseRoute53Credentials(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    string
		keyID   string
		secret  string
		wantErr bool
	}{
		{
			name:   "env style",
			data:   "AWS_ACCESS_KEY_ID=AKIAEXAMPLE\nAWS_SECRET_ACCESS_KEY=s3cret\n",
			keyID:  "AKIAEXAMPLE",
			secret: "s3cret",
		},
		{
			name:   "shared credentials file",
			data:   "# managed by ansible\n[default]\naws_access_key_id = AKIAEXAMPLE\naws_secret_access_key = s3cret\n",
			keyID:  "AKIAEXAMPLE",
			secret: "s3cret",
		},
		{
			name:    "session token is not silently dropped",
			data:    "aws_access_key_id=A\naws_secret_access_key=B\naws_session_token=C\n",
			wantErr: true,
		},
		{
			name:    "two profiles",
			data:    "[a]\naws_access_key_id=A\n[b]\naws_secret_access_key=B\n",
			wantErr: true,
		},
		{
			name:    "missing secret",
			data:    "aws_access_key_id=A\n",
			wantErr: true,
		},
		{
			name:    "not key=value",
			data:    "AKIAEXAMPLE s3cret\n",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyID, secret, err := parseRoute53Credentials([]byte(tc.data))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsed %q without error", tc.data)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if keyID != tc.keyID || secret != tc.secret {
				t.Errorf("got (%q, %q), want (%q, %q)", keyID, secret, tc.keyID, tc.secret)
			}
		})
	}
}
