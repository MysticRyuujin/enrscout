package distinct

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestRollingDistinctAndRestore(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	s := New("method-v1", DefaultPrecision)
	for i := 0; i < 20_000; i++ {
		id := []byte(fmt.Sprintf("node-%d", i))
		s.Observe("v5/udp4", id, now)
		s.Observe("v5/udp4", id, now) // re-observation must not inflate cardinality
	}
	est := s.Estimates(now)[0]
	if delta := int64(est.Distinct) - 20_000; delta < -700 || delta > 700 {
		t.Fatalf("estimate = %d, outside deterministic error bound", est.Distinct)
	}
	raw, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
		t.Fatal("persisted distinct state is not gzip-compressed")
	}
	restored, err := Restore(raw, "method-v1", DefaultPrecision)
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.Estimates(now)[0].Distinct; got != est.Distinct {
		t.Fatalf("restored estimate = %d, want %d", got, est.Distinct)
	}
}

func TestRestoreRejectsUncompressedJSON(t *testing.T) {
	s := New("method-v1", DefaultPrecision)
	s.Observe("all/all", []byte("node"), time.Now())
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(raw, "method-v1", DefaultPrecision); err == nil {
		t.Fatal("uncompressed distinct state was accepted")
	}
}

func TestBucketExpiryAndMethodologyMismatch(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC().Truncate(time.Hour)
	s := New("old", DefaultPrecision)
	s.Observe("v4/udp4", []byte("expired"), now.Add(-WindowHours*time.Hour))
	s.Observe("v4/udp4", []byte("current"), now)
	if got := s.Estimates(now)[0].Distinct; got != 1 {
		t.Fatalf("rolling estimate = %d, want 1", got)
	}
	raw, _ := s.Marshal()
	if _, err := Restore(raw, "new", DefaultPrecision); err == nil {
		t.Fatal("mismatched methodology state was accepted")
	}
}

func TestRestoreRejectsUnorderedBuckets(t *testing.T) {
	s := New("m", DefaultPrecision)
	at := time.Unix(1_800_000_000, 0).UTC()
	s.Observe("k", []byte{1}, at)
	s.Observe("k", []byte{2}, at.Add(time.Hour))
	data, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := Restore(data, "m", DefaultPrecision)
	if err != nil {
		t.Fatalf("ordered state rejected: %v", err)
	}
	buckets := restored.Series["k"].Buckets
	buckets[0], buckets[1] = buckets[1], buckets[0]
	swapped, err := restored.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(swapped, "m", DefaultPrecision); err == nil {
		t.Fatal("unordered buckets accepted; sort.Search lookups in Observe and Estimates would misbehave")
	}
}

func TestRestoreRejectsImpossibleRegisterValues(t *testing.T) {
	s := New("m", DefaultPrecision)
	at := time.Unix(1_800_000_000, 0).UTC()
	s.Observe("k", []byte{1}, at)
	restored, err := Restore(mustMarshal(t, s), "m", DefaultPrecision)
	if err != nil {
		t.Fatal(err)
	}
	restored.Series["k"].Buckets[0].Registers[0] = 255
	if _, err := Restore(mustMarshal(t, restored), "m", DefaultPrecision); err == nil {
		t.Fatal("register above the maximum rank accepted; it would permanently inflate merged estimates")
	}
}

func mustMarshal(t *testing.T, s *State) []byte {
	t.Helper()
	raw, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
