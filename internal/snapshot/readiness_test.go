package snapshot

import (
	"strings"
	"testing"
	"time"
)

func TestReadinessHistoryAppendSpacesAndPrunes(t *testing.T) {
	h := &ReadinessHistory{Version: ReadinessHistoryVersion}
	start := time.Unix(1_790_000_000, 0)
	if !h.Append(ReadinessPoint{At: start.Unix()}) {
		t.Fatal("first point rejected")
	}
	if h.Append(ReadinessPoint{At: start.Add(ReadinessInterval - time.Second).Unix()}) {
		t.Fatal("point inside the interval accepted")
	}
	if !h.Append(ReadinessPoint{At: start.Add(ReadinessInterval).Unix()}) {
		t.Fatal("point one interval later rejected")
	}
	if !h.Append(ReadinessPoint{At: start.Add(ReadinessRetention + ReadinessInterval).Unix()}) {
		t.Fatal("late point rejected")
	}
	if len(h.Points) != 2 || h.Points[0].At != start.Add(ReadinessInterval).Unix() {
		t.Fatalf("points after retention = %+v, want the first pruned", h.Points)
	}
}

func TestReadinessHistoryDecodeRejectsUnknownVersionAndFields(t *testing.T) {
	h := &ReadinessHistory{Version: ReadinessHistoryVersion, Network: "sepolia", Fork: "Glamsterdam", Points: []ReadinessPoint{{At: 1}}}
	data, err := h.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecodeReadinessHistory(data); err != nil || got.Fork != "Glamsterdam" || len(got.Points) != 1 {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	if _, err := DecodeReadinessHistory([]byte(`{"version":2,"points":[]}`)); err == nil {
		t.Fatal("future version accepted")
	}
	if _, err := DecodeReadinessHistory([]byte(`{"version":1,"points":[],"extra":1}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
	legacy := []byte(`{"version":1,"network":"sepolia","fork":"Glamsterdam","points":[{"at":1,"el":{"ready":1},"clients_el":{"Geth":[1,1]}}]}`)
	got, err := DecodeReadinessHistory(legacy)
	if err != nil {
		t.Fatalf("pre-release per-client fields rejected: %v", err)
	}
	if data, err := got.Encode(); err != nil || strings.Contains(string(data), "clients_el") {
		t.Fatalf("legacy fields re-encoded: %s, %v", data, err)
	}
}

func TestReadinessHistoryKeyStaysOutOfGenerationPrefix(t *testing.T) {
	l := Layout{}
	key, err := l.ReadinessHistoryKey("sepolia", "Glamsterdam")
	if err != nil || strings.HasPrefix(key, l.NetworkPrefix("sepolia")) || key != "snapshots/state/readiness/sepolia/glamsterdam.json" {
		t.Fatalf("key = %q, %v", key, err)
	}
	if slashed, err := (Layout{Prefix: "snapshots/"}).ReadinessHistoryKey("sepolia", "Glamsterdam"); err != nil || slashed != key {
		t.Fatalf("trailing-slash prefix key = %q, %v, want %q", slashed, err, key)
	}
	if _, err := l.ReadinessHistoryKey("sepolia", "../manifest"); err == nil {
		t.Fatal("path segment accepted")
	}
}
