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
}

func TestReadinessHistoryKeyStaysOutOfGenerationPrefix(t *testing.T) {
	l := Layout{}
	key := l.ReadinessHistoryKey("sepolia", "Glamsterdam")
	if strings.HasPrefix(key, l.NetworkPrefix("sepolia")) || key != "snapshots/state/readiness/sepolia/glamsterdam.json" {
		t.Fatalf("key = %q", key)
	}
}
