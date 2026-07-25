package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/store"
)

func TestIsGenerationKey(t *testing.T) {
	l := Layout{}
	good := l.GenerationKey("mainnet", time.Unix(1700000000, 0))
	if !l.IsGenerationKey("mainnet", good) {
		t.Errorf("%q should be a generation key", good)
	}
	if l.IsGenerationKey("mainnet", "snapshots/mainnet/latest.parquet") {
		t.Error("latest.parquet must not be treated as a generation")
	}
	if l.IsGenerationKey("mainnet", "snapshots/hoodi/2026-01-01T00:00:00Z.parquet") {
		t.Error("wrong-network key must be rejected")
	}
}

func TestGenerationKeysDoNotCollideWithinSecond(t *testing.T) {
	l := Layout{}
	base := time.Unix(1700000000, 100)
	a := l.GenerationKey("mainnet", base)
	b := l.GenerationKey("mainnet", base.Add(time.Nanosecond))
	if a == b {
		t.Fatalf("subsecond generations collided: %q", a)
	}
	if _, ok := l.GenerationTime("mainnet", a); !ok {
		t.Fatalf("new generation key did not parse: %q", a)
	}
	secondResolution := l.NetworkPrefix("mainnet") + base.UTC().Format("2006-01-02T15:04:05Z") + ".parquet"
	if _, ok := l.GenerationTime("mainnet", secondResolution); ok {
		t.Fatalf("second-resolution generation key was accepted: %q", secondResolution)
	}
}

func validManifest(now time.Time) *Manifest {
	l := Layout{}
	return &Manifest{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   now,
		CrawlerID:     "crawler-1",
		Run: RunMetadata{
			RunID: "run-1", SourceRevision: "revision-1", SourceURL: "https://example.com/source",
			ConfigSHA256: strings.Repeat("00", sha256.Size), CrawlerStartedAt: now.Add(-time.Minute),
			MethodologyStartedAt: now.Add(-time.Minute), MethodologyVersion: MethodologyVersion,
			MethodologyID: "method-1",
		},
		Networks: map[string]NetworkSnapshot{
			"mainnet": {
				GenerationKey: l.GenerationKey("mainnet", now),
				NodeCount:     1,
				SHA256:        strings.Repeat("00", sha256.Size),
				Bytes:         1,
			},
		},
	}
}

// The guards read the newest sample as the candidate for the generation being committed, so a
// manifest whose newest sample describes something else must never be written - while an
// already-committed manifest stays readable, since tightening the reader strands both binaries.

func TestManifestValidation(t *testing.T) {
	now := time.Now().UTC()
	if err := validManifest(now).Validate(Layout{}); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	cases := map[string]func(*Manifest){
		"empty crawler": func(m *Manifest) { m.CrawlerID = "" },
		"missing run":   func(m *Manifest) { m.Run = RunMetadata{} },
		"bad network":   func(m *Manifest) { m.Networks["../escape"] = m.Networks["mainnet"] },
		"bad key": func(m *Manifest) {
			n := m.Networks["mainnet"]
			n.GenerationKey = "snapshots/mainnet/latest.parquet"
			m.Networks["mainnet"] = n
		},
		"bad hash": func(m *Manifest) { n := m.Networks["mainnet"]; n.SHA256 = "xyz"; m.Networks["mainnet"] = n },
		"bad size": func(m *Manifest) { n := m.Networks["mainnet"]; n.Bytes = 0; m.Networks["mainnet"] = n },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := validManifest(now)
			mutate(m)
			if err := m.Validate(Layout{}); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}

func TestReadRejectsUnknownManifestField(t *testing.T) {
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := Layout{}
	raw, err := json.Marshal(validManifest(time.Now().UTC()))
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), `"schema_version":`, `"population_version":3,"schema_version":`, 1))
	if err := st.Put(context.Background(), l.ManifestKey(), raw, "application/json"); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(context.Background(), st, l); err == nil {
		t.Fatal("manifest with removed population_version field was accepted")
	}
}

func TestReadAcceptsManifestFromFastWriterClock(t *testing.T) {
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := Layout{}
	m := validManifest(time.Now().Add(10 * time.Minute))
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(context.Background(), l.ManifestKey(), raw, "application/json"); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(context.Background(), st, l); err != nil {
		t.Fatalf("future-skewed but structurally valid manifest rejected: %v", err)
	}
}

func TestReadAcceptsOldestReadableSchemaVersion(t *testing.T) {
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := validManifest(time.Now().UTC())
	m.SchemaVersion = OldestReadableSchemaVersion
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	l := Layout{}
	if err := st.Put(context.Background(), l.ManifestKey(), raw, "application/json"); err != nil {
		t.Fatal(err)
	}
	got, err := Read(context.Background(), st, l)
	if err != nil {
		t.Fatalf("read previous schema: %v", err)
	}
	if got.SchemaVersion != OldestReadableSchemaVersion {
		t.Fatalf("schema version = %d, want %d", got.SchemaVersion, OldestReadableSchemaVersion)
	}
}

// A methodology bump must be an online deployment: the committed manifest still reads, but
// nothing may keep writing points into the series the bump ended.
func TestSupersededMethodologyReadsButNeverWrites(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := Layout{}
	now := time.Now().UTC()

	superseded := ""
	for version := range readableMethodologyVersions {
		if version != MethodologyVersion {
			superseded = version
		}
	}
	if superseded == "" {
		t.Skip("no superseded methodology version to exercise")
	}

	m := validManifest(now)
	m.Run.MethodologyVersion = superseded
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, l.ManifestKey(), raw, "application/json"); err != nil {
		t.Fatal(err)
	}
	got, err := Read(ctx, st, l)
	if err != nil {
		t.Fatalf("read superseded methodology %q: %v", superseded, err)
	}
	if got.Run.MethodologyVersion != superseded {
		t.Fatalf("methodology version = %q, want %q", got.Run.MethodologyVersion, superseded)
	}

	if err := Write(ctx, st, l, m); err == nil {
		t.Error("Write accepted a superseded methodology version")
	}
	if err := WriteConditional(ctx, st, l, got, m); err == nil {
		t.Error("WriteConditional accepted a superseded methodology version")
	}

	unknown := validManifest(now)
	unknown.Run.MethodologyVersion = "not-a-methodology"
	if err := unknown.Validate(l); err == nil {
		t.Error("an unknown methodology version was accepted on read")
	}
}

func TestVerifyGeneration(t *testing.T) {
	l := Layout{}
	key := l.GenerationKey("mainnet", time.Unix(1700000000, 0))
	data := []byte("parquet-bytes")
	sum := sha256.Sum256(data)
	ns := NetworkSnapshot{GenerationKey: key, Bytes: len(data), SHA256: hex.EncodeToString(sum[:])}

	if err := l.VerifyGeneration("mainnet", ns, data); err != nil {
		t.Errorf("valid generation failed: %v", err)
	}
	bad := ns
	bad.SHA256 = "deadbeef"
	if err := l.VerifyGeneration("mainnet", bad, data); err == nil {
		t.Error("sha256 mismatch should fail")
	}
	bad = ns
	bad.Bytes = 999
	if err := l.VerifyGeneration("mainnet", bad, data); err == nil {
		t.Error("size mismatch should fail")
	}
	bad = ns
	bad.GenerationKey = "snapshots/mainnet/latest.parquet"
	if err := l.VerifyGeneration("mainnet", bad, data); err == nil {
		t.Error("non-generation key should fail")
	}
}

func TestReadRejectsUnsupportedSchema(t *testing.T) {
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := Layout{}
	m := &Manifest{SchemaVersion: SchemaVersion + 999, GeneratedAt: time.Unix(1, 0), Networks: map[string]NetworkSnapshot{}}
	if err := Write(context.Background(), st, l, m); err == nil {
		t.Error("unsupported schema version should be rejected before writing")
	}
}

func TestWriteConditionalRejectsCompetingCrawler(t *testing.T) {
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	l := Layout{}
	first := validManifest(time.Unix(100, 0).UTC())
	if err := WriteConditional(ctx, st, l, nil, first); err != nil {
		t.Fatal(err)
	}

	next := validManifest(time.Unix(101, 0).UTC())
	if err := WriteConditional(ctx, st, l, first, next); err != nil {
		t.Fatalf("same crawler advance failed: %v", err)
	}

	competitor := validManifest(time.Unix(102, 0).UTC())
	competitor.CrawlerID = "crawler-2"
	competitor.Run.RunID = "run-2"
	if err := WriteConditional(ctx, st, l, first, competitor); !errors.Is(err, ErrManifestConflict) {
		t.Fatalf("stale competing write error = %v, want ErrManifestConflict", err)
	}
	got, err := Read(ctx, st, l)
	if err != nil {
		t.Fatal(err)
	}
	if !got.GeneratedAt.Equal(next.GeneratedAt) || got.CrawlerID != next.CrawlerID {
		t.Fatalf("manifest changed after rejected write: %+v", got)
	}
}

func TestWriteConditionalAdoptsOwnAmbiguousCommit(t *testing.T) {
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	l := Layout{}
	prev := validManifest(time.Unix(100, 0).UTC())
	if err := WriteConditional(ctx, st, l, nil, prev); err != nil {
		t.Fatal(err)
	}
	committed := validManifest(time.Unix(101, 0).UTC())
	if err := WriteConditional(ctx, st, l, prev, committed); err != nil {
		t.Fatal(err)
	}

	next := validManifest(time.Unix(102, 0).UTC())
	if err := WriteConditional(ctx, st, l, prev, next); err != nil {
		t.Fatalf("same-run advance over an unseen own commit failed: %v", err)
	}
	got, err := Read(ctx, st, l)
	if err != nil {
		t.Fatal(err)
	}
	if !got.GeneratedAt.Equal(next.GeneratedAt) {
		t.Fatalf("manifest generation = %s, want %s", got.GeneratedAt, next.GeneratedAt)
	}

	stale := validManifest(time.Unix(102, 0).UTC())
	if err := WriteConditional(ctx, st, l, prev, stale); !errors.Is(err, ErrManifestConflict) {
		t.Fatalf("non-monotonic same-run write error = %v, want ErrManifestConflict", err)
	}
}

func TestWriteConditionalAdoptsOwnFirstCommitWithoutPrevious(t *testing.T) {
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	l := Layout{}
	committed := validManifest(time.Unix(100, 0).UTC())
	if err := WriteConditional(ctx, st, l, nil, committed); err != nil {
		t.Fatal(err)
	}
	next := validManifest(time.Unix(101, 0).UTC())
	if err := WriteConditional(ctx, st, l, nil, next); err != nil {
		t.Fatalf("first-publish retry over an unseen own commit failed: %v", err)
	}

	other := validManifest(time.Unix(102, 0).UTC())
	other.Run.RunID = "run-2"
	if err := WriteConditional(ctx, st, l, nil, other); !errors.Is(err, ErrManifestConflict) {
		t.Fatalf("fresh-writer clobber error = %v, want ErrManifestConflict", err)
	}
}

func TestWriteConditionalUpgradesPreviousSchema(t *testing.T) {
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	l := Layout{}
	previous := validManifest(time.Unix(100, 0).UTC())
	previous.SchemaVersion = OldestReadableSchemaVersion
	raw, err := json.Marshal(previous)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, l.ManifestKey(), raw, "application/json"); err != nil {
		t.Fatal(err)
	}
	next := validManifest(time.Unix(101, 0).UTC())
	if err := WriteConditional(ctx, st, l, previous, next); err != nil {
		t.Fatalf("upgrade schema: %v", err)
	}
	got, err := Read(ctx, st, l)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", got.SchemaVersion, SchemaVersion)
	}
}

// Manifests written before the population guards were simplified carry a population_history the
// decoder no longer models, and decoding is strict about unknown fields. They must still read, or a
// deployment cannot roll forward without the crawler losing its restored node set.
func TestReadAcceptsAManifestWithLegacyPopulationHistory(t *testing.T) {
	now := time.Now().UTC()
	l := Layout{}
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, err := encode(validManifest(now))
	if err != nil {
		t.Fatal(err)
	}
	legacy := `"population_history":[{"generated_at":"2026-07-01T00:00:00Z","networks":{"mainnet":{"total":1,"execution":1,"current":1,"execution_current":1}}}],"schema_version":`
	data = []byte(strings.Replace(string(data), `"schema_version":`, legacy, 1))
	if err := st.Put(context.Background(), l.ManifestKey(), data, "application/json"); err != nil {
		t.Fatal(err)
	}
	got, err := Read(context.Background(), st, l)
	if err != nil {
		t.Fatalf("a manifest with legacy population_history is unreadable, so a rollout would lose the restored set: %v", err)
	}
	if got.Networks["mainnet"].NodeCount != 1 {
		t.Fatalf("network count = %d, want 1", got.Networks["mainnet"].NodeCount)
	}
}
