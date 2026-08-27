package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/store"
)

const (
	SchemaVersion               = 2
	OldestReadableSchemaVersion = 1
)
const MethodologyVersion = "2026-08-v3"

// Superseded methodologies stay readable so a bump is an online deployment rather than a
// cutover that strands the committed manifest. Writes are pinned to MethodologyVersion.
var readableMethodologyVersions = map[string]bool{
	"2026-07-v1":       true,
	"2026-07-v2":       true,
	MethodologyVersion: true,
}

func validateReadableMethodologyVersion(version string) error {
	if !readableMethodologyVersions[version] {
		return fmt.Errorf("unsupported methodology version %q", version)
	}
	return nil
}

// MaxManifestBytes bounds a manifest read.
const MaxManifestBytes = 4 << 20

const (
	maxCrawlerIDBytes = 256
	maxNetworks       = 64
)

const genTimeFormat = "2006-01-02T15:04:05.000000000Z"

var ErrNoManifest = errors.New("no snapshot manifest")
var ErrManifestConflict = errors.New("snapshot manifest changed since restore")
var ErrUnsupportedSchemaVersion = errors.New("unsupported snapshot schema version")

type Layout struct {
	Prefix string
}

func (l Layout) prefix() string {
	if l.Prefix == "" {
		return "snapshots"
	}
	return l.Prefix
}

func (l Layout) ManifestKey() string { return l.prefix() + "/manifest.json" }

func (l Layout) NetworkPrefix(network string) string {
	return fmt.Sprintf("%s/%s/", l.prefix(), network)
}

func validateReadableSchemaVersion(version int) error {
	if version < OldestReadableSchemaVersion || version > SchemaVersion {
		return fmt.Errorf("%w %d (supported %d..%d)", ErrUnsupportedSchemaVersion, version, OldestReadableSchemaVersion, SchemaVersion)
	}
	return nil
}

func (l Layout) GenerationKey(network string, generatedAt time.Time) string {
	return l.NetworkPrefix(network) + generatedAt.UTC().Format(genTimeFormat) + ".parquet"
}

type NetworkSnapshot struct {
	GenerationKey string `json:"generation_key"`
	NodeCount     int    `json:"node_count"`
	// CurrentNodeCount is the subset on the current fork. The publish guard needs it because a
	// classification regression can hold NodeCount steady while every node the map shows disappears.
	CurrentNodeCount int    `json:"current_node_count"`
	SHA256           string `json:"sha256"`
	Bytes            int    `json:"bytes"`
}

type RunMetadata struct {
	RunID                string    `json:"run_id"`
	SourceRevision       string    `json:"source_revision"`
	SourceURL            string    `json:"source_url"`
	ImageDigest          string    `json:"image_digest"`
	ConfigSHA256         string    `json:"config_sha256"`
	CrawlerStartedAt     time.Time `json:"crawler_started_at"`
	MethodologyStartedAt time.Time `json:"methodology_started_at"`
	MethodologyVersion   string    `json:"methodology_version"`
	MethodologyID        string    `json:"methodology_id"`
}

type Manifest struct {
	SchemaVersion int                        `json:"schema_version"`
	GeneratedAt   time.Time                  `json:"generated_at"`
	CrawlerID     string                     `json:"crawler_id"`
	Run           RunMetadata                `json:"run"`
	Networks      map[string]NetworkSnapshot `json:"networks"`
	// Accepted and ignored so a manifest written before the guards were simplified still parses -
	// decoding is strict about unknown fields. Delete once no schema-2 manifest can still be current.
	LegacyPopulationHistory json.RawMessage `json:"population_history,omitempty"`
}

func unmarshalStrict(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func Read(ctx context.Context, st store.Store, l Layout) (*Manifest, error) {
	data, err := st.Get(ctx, l.ManifestKey())
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNoManifest
	}
	if err != nil {
		return nil, err
	}
	return decodeManifest(data, l)
}

func decodeManifest(data []byte, l Layout) (*Manifest, error) {
	if len(data) > MaxManifestBytes {
		return nil, fmt.Errorf("manifest too large: %d bytes", len(data))
	}
	var m Manifest
	if err := unmarshalStrict(data, &m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := m.Validate(l); err != nil {
		return nil, fmt.Errorf("invalid manifest: %w", err)
	}
	return &m, nil
}

func (m *Manifest) Validate(l Layout) error {
	if err := validateReadableSchemaVersion(m.SchemaVersion); err != nil {
		return err
	}
	if m.GeneratedAt.IsZero() {
		return errors.New("generated_at is required")
	}
	if m.CrawlerID == "" || len(m.CrawlerID) > maxCrawlerIDBytes {
		return fmt.Errorf("crawler_id must contain 1..%d bytes", maxCrawlerIDBytes)
	}
	{
		if m.Run.RunID == "" || len(m.Run.RunID) > 128 {
			return errors.New("run.run_id must contain 1..128 bytes")
		}
		if m.Run.SourceRevision == "" || len(m.Run.SourceRevision) > 256 {
			return errors.New("run.source_revision must contain 1..256 bytes")
		}
		if m.Run.SourceURL == "" || len(m.Run.SourceURL) > 2048 {
			return errors.New("run.source_url must contain 1..2048 bytes")
		}
		if parsed, err := url.Parse(m.Run.SourceURL); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return errors.New("run.source_url must be an absolute HTTP(S) URL")
		}
		if len(m.Run.ConfigSHA256) != sha256.Size*2 {
			return errors.New("run.config_sha256 must be a SHA-256 hex digest")
		}
		if _, err := hex.DecodeString(m.Run.ConfigSHA256); err != nil {
			return fmt.Errorf("run.config_sha256: %w", err)
		}
		if m.Run.CrawlerStartedAt.IsZero() {
			return errors.New("run.crawler_started_at is required")
		}
		if m.Run.MethodologyStartedAt.IsZero() {
			return errors.New("run.methodology_started_at is required")
		}
		if m.Run.MethodologyStartedAt.After(m.GeneratedAt) {
			return errors.New("run.methodology_started_at cannot be after generated_at")
		}
		if err := validateReadableMethodologyVersion(m.Run.MethodologyVersion); err != nil {
			return err
		}
		if !ValidComponent(m.Run.MethodologyID) {
			return errors.New("run.methodology_id must be a safe non-empty component")
		}
	}
	if len(m.Networks) == 0 || len(m.Networks) > maxNetworks {
		return fmt.Errorf("manifest must contain 1..%d networks", maxNetworks)
	}
	for network, ns := range m.Networks {
		if !ValidComponent(network) {
			return fmt.Errorf("invalid network name %q", network)
		}
		if ns.GenerationKey != l.GenerationKey(network, m.GeneratedAt) {
			return fmt.Errorf("generation key %q does not match manifest generation", ns.GenerationKey)
		}
		if ns.NodeCount < 0 {
			return fmt.Errorf("network %q has negative node count", network)
		}
		if ns.CurrentNodeCount < 0 || ns.CurrentNodeCount > ns.NodeCount {
			return fmt.Errorf("network %q current node count %d must be in range 0..%d", network, ns.CurrentNodeCount, ns.NodeCount)
		}
		if ns.Bytes <= 0 || ns.Bytes > store.MaxObjectBytes {
			return fmt.Errorf("network %q bytes must be in range 1..%d", network, store.MaxObjectBytes)
		}
		if len(ns.SHA256) != sha256.Size*2 {
			return fmt.Errorf("network %q has invalid sha256 length", network)
		}
		if _, err := hex.DecodeString(ns.SHA256); err != nil {
			return fmt.Errorf("network %q has invalid sha256: %w", network, err)
		}
	}
	return nil
}

// ValidComponent reports whether s is safe as one snapshot path component.
func ValidComponent(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func (l Layout) IsGenerationKey(network, key string) bool {
	_, ok := l.GenerationTime(network, key)
	return ok
}

func (l Layout) GenerationTime(network, key string) (time.Time, bool) {
	prefix := l.NetworkPrefix(network)
	if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, ".parquet") {
		return time.Time{}, false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(key, prefix), ".parquet")
	if generatedAt, err := time.Parse(genTimeFormat, name); err == nil {
		return generatedAt, true
	}
	return time.Time{}, false
}

func (l Layout) VerifyGeneration(network string, ns NetworkSnapshot, data []byte) error {
	if !l.IsGenerationKey(network, ns.GenerationKey) {
		return fmt.Errorf("generation key %q is not a valid %s generation", ns.GenerationKey, network)
	}
	if len(data) != ns.Bytes {
		return fmt.Errorf("generation %q size mismatch: got %d want %d", ns.GenerationKey, len(data), ns.Bytes)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != ns.SHA256 {
		return fmt.Errorf("generation %q sha256 mismatch", ns.GenerationKey)
	}
	return nil
}

// Indented despite costing roughly double the bytes: operators read committed manifests directly.
func encode(m *Manifest) ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
}

// Validate accepts superseded schema and methodology versions so committed manifests stay
// readable across a bump; a write must still pin the head, or it would keep appending to a
// series the bump exists to end.
func validateWritable(m *Manifest, l Layout) error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("write snapshot schema version %d: current version is %d", m.SchemaVersion, SchemaVersion)
	}
	if m.Run.MethodologyVersion != MethodologyVersion {
		return fmt.Errorf("write methodology version %q: current version is %q", m.Run.MethodologyVersion, MethodologyVersion)
	}
	if err := m.Validate(l); err != nil {
		return fmt.Errorf("validate manifest: %w", err)
	}
	return nil
}

// Write is the commit point: every referenced generation object must already be stored.
func Write(ctx context.Context, st store.Store, l Layout, m *Manifest) error {
	if m == nil {
		return errors.New("manifest is nil")
	}
	if err := validateWritable(m, l); err != nil {
		return err
	}
	data, err := encode(m)
	if err != nil {
		return err
	}
	return st.Put(ctx, l.ManifestKey(), data, "application/json")
}

// WriteConditional advances the manifest only if previous is still committed.
// It prevents last-writer-win corruption but does not provide writer election:
// callers must run exactly one writer per prefix and stop on ErrManifestConflict.
func WriteConditional(ctx context.Context, st store.Store, l Layout, previous, next *Manifest) error {
	if next == nil {
		return errors.New("manifest is nil")
	}
	if err := validateWritable(next, l); err != nil {
		return err
	}
	conditional, ok := st.(store.ConditionalStore)
	if !ok {
		return errors.New("store does not support conditional manifest writes")
	}

	expectedVersion := ""
	data, version, err := conditional.GetVersion(ctx, l.ManifestKey())
	if errors.Is(err, store.ErrNotFound) {
		if previous != nil {
			return ErrManifestConflict
		}
	} else if err != nil {
		return err
	} else {
		current, err := decodeManifest(data, l)
		if err != nil {
			return err
		}
		// A stored manifest from the caller's own run is an earlier write whose success
		// the caller never saw (ambiguous PutIfVersion), not a competing writer; adopting
		// it requires next to keep manifest generations monotonic.
		ownWrite := current.Run.RunID == next.Run.RunID && next.GeneratedAt.After(current.GeneratedAt)
		if !ownWrite && (previous == nil || current.CrawlerID != previous.CrawlerID || !current.GeneratedAt.Equal(previous.GeneratedAt)) {
			return fmt.Errorf("%w: current crawler=%q generation=%s", ErrManifestConflict, current.CrawlerID, current.GeneratedAt.Format(time.RFC3339Nano))
		}
		expectedVersion = version
	}

	encoded, err := encode(next)
	if err != nil {
		return err
	}
	if _, err := conditional.PutIfVersion(ctx, l.ManifestKey(), expectedVersion, encoded, "application/json"); err != nil {
		if errors.Is(err, store.ErrCASConflict) {
			return ErrManifestConflict
		}
		return err
	}
	return nil
}
