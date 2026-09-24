package snapshot

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	ReadinessHistoryVersion = 1
	ReadinessInterval       = 15 * time.Minute
	ReadinessRetention      = 30 * 24 * time.Hour
	// maxReadinessBytes keeps a corrupt or runaway object from being loaded whole.
	maxReadinessBytes = 8 << 20
)

// ReadinessPoint is one sample of a network's fork readiness: rows per readiness state, per layer.
type ReadinessPoint struct {
	At int64          `json:"at"`
	EL map[string]int `json:"el,omitempty"`
	CL map[string]int `json:"cl,omitempty"`
	// Pre-release writers also recorded per-client pairs. They are accepted so those objects stay
	// decodable and are dropped on the next write.
	LegacyClientsEL json.RawMessage `json:"clients_el,omitempty"`
	LegacyClientsCL json.RawMessage `json:"clients_cl,omitempty"`
}

// ReadinessHistory is a rolling series for one tracked fork on one network. It is written by the
// crawler only after a manifest commit, so it inherits the one-writer-per-prefix rule, and it is
// deliberately outside the manifest, whose strict decoding would reject a new field.
type ReadinessHistory struct {
	Version int              `json:"version"`
	Network string           `json:"network"`
	Fork    string           `json:"fork"`
	ELTime  uint64           `json:"el_time,omitempty"`
	CLEpoch uint64           `json:"cl_epoch,omitempty"`
	Points  []ReadinessPoint `json:"points"`
}

var keySegment = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ReadinessHistoryKey keeps history out of NetworkPrefix, which generation pruning owns, under the same
// trimmed root as the crawler's other state objects. It checks both segments, so the writer and the
// reader hold the key to one shape.
func (l Layout) ReadinessHistoryKey(network, fork string) (string, error) {
	fork = strings.ToLower(fork)
	if !keySegment.MatchString(network) || !keySegment.MatchString(fork) {
		return "", fmt.Errorf("readiness history key segments %q/%q", network, fork)
	}
	return fmt.Sprintf("%s/state/readiness/%s/%s.json", strings.TrimSuffix(l.prefix(), "/"), network, fork), nil
}

func DecodeReadinessHistory(data []byte) (*ReadinessHistory, error) {
	if len(data) > maxReadinessBytes {
		return nil, fmt.Errorf("readiness history is %d bytes, over the %d-byte limit", len(data), maxReadinessBytes)
	}
	var h ReadinessHistory
	if err := UnmarshalStrict(data, &h); err != nil {
		return nil, err
	}
	if h.Version != ReadinessHistoryVersion {
		return nil, fmt.Errorf("unsupported readiness history version %d", h.Version)
	}
	for i := range h.Points {
		h.Points[i].LegacyClientsEL, h.Points[i].LegacyClientsCL = nil, nil
	}
	return &h, nil
}

func (h *ReadinessHistory) Append(p ReadinessPoint) bool {
	if n := len(h.Points); n > 0 && p.At-h.Points[n-1].At < int64(ReadinessInterval.Seconds()) {
		return false
	}
	h.Points = append(h.Points, p)
	cutoff := p.At - int64(ReadinessRetention.Seconds())
	keep := 0
	for keep < len(h.Points) && h.Points[keep].At < cutoff {
		keep++
	}
	h.Points = h.Points[keep:]
	return true
}

func (h *ReadinessHistory) Encode() ([]byte, error) {
	data, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	if len(data) > maxReadinessBytes {
		return nil, fmt.Errorf("readiness history is %d bytes, over the %d-byte limit", len(data), maxReadinessBytes)
	}
	return data, nil
}
