package distinct

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sort"
	"sync"
	"time"
)

const (
	DefaultPrecision = uint8(12)
	RetainedHours    = 169
	WindowHours      = 168
	maxStateBytes    = 64 << 20
)

type Bucket struct {
	Hour      int64  `json:"hour"`
	Registers []byte `json:"registers"`
	Sightings uint64 `json:"sightings"`
}

type Series struct {
	Buckets []Bucket `json:"buckets"`
	Seen    uint64   `json:"seen"`
}

type State struct {
	Version     int                `json:"version"`
	Methodology string             `json:"methodology"`
	Precision   uint8              `json:"precision"`
	Series      map[string]*Series `json:"series"`
	mu          sync.Mutex
}

type Estimate struct {
	Key         string
	Distinct    uint64
	Sightings   uint64
	WindowStart time.Time
	WindowEnd   time.Time
	Error       float64
}

func New(methodology string, precision uint8) *State {
	if precision < 4 || precision > 18 {
		precision = DefaultPrecision
	}
	return &State{Version: 1, Methodology: methodology, Precision: precision, Series: make(map[string]*Series)}
}

func Restore(data []byte, methodology string, precision uint8) (*State, error) {
	if len(data) == 0 {
		return nil, errors.New("distinct state is empty")
	}
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return nil, errors.New("distinct state is not gzip-compressed")
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	decoded, readErr := io.ReadAll(io.LimitReader(zr, maxStateBytes+1))
	closeErr := zr.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(decoded) > maxStateBytes {
		return nil, errors.New("distinct state exceeds decompressed size limit")
	}
	var state State
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("distinct state contains multiple JSON values")
		}
		return nil, err
	}
	if state.Version != 1 || state.Methodology != methodology || state.Precision != precision {
		return nil, errors.New("distinct state version, methodology, or precision mismatch")
	}
	if state.Series == nil {
		state.Series = make(map[string]*Series)
	}
	want := 1 << state.Precision
	for key, series := range state.Series {
		if key == "" || series == nil || len(series.Buckets) > RetainedHours {
			return nil, errors.New("invalid distinct state")
		}
		// 64 - precision + 1 is the largest rank slot can produce; a corrupted larger
		// register would permanently inflate every merged estimate.
		maxRank := byte(64 - state.Precision + 1)
		for i, bucket := range series.Buckets {
			if len(bucket.Registers) != want {
				return nil, errors.New("invalid distinct register count")
			}
			for _, register := range bucket.Registers {
				if register > maxRank {
					return nil, errors.New("invalid distinct register value")
				}
			}
			if i > 0 && bucket.Hour <= series.Buckets[i-1].Hour {
				return nil, errors.New("distinct state buckets are not ordered by hour")
			}
		}
	}
	return &state, nil
}

// Marshal encodes a copy so the multi-megabyte JSON and gzip work runs outside the lock that every
// discovery reader's Observe needs.
func (s *State) Marshal() ([]byte, error) {
	s.mu.Lock()
	snapshot := &State{Version: s.Version, Methodology: s.Methodology, Precision: s.Precision, Series: make(map[string]*Series, len(s.Series))}
	for key, series := range s.Series {
		buckets := make([]Bucket, len(series.Buckets))
		for i, bucket := range series.Buckets {
			buckets[i] = Bucket{Hour: bucket.Hour, Registers: bytes.Clone(bucket.Registers), Sightings: bucket.Sightings}
		}
		snapshot.Series[key] = &Series{Buckets: buckets, Seen: series.Seen}
	}
	s.mu.Unlock()
	var out bytes.Buffer
	zw, err := gzip.NewWriterLevel(&out, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if err := json.NewEncoder(zw).Encode(snapshot); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func hourNumber(at time.Time) int64 { return at.UTC().Unix() / int64(time.Hour/time.Second) }

// Observe records one sighting of identity under every key, hashing it once.
func (s *State) Observe(identity []byte, at time.Time, keys ...string) {
	if len(identity) == 0 {
		return
	}
	index, rank := s.slot(identity)
	hour := hourNumber(at)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range keys {
		if key == "" {
			continue
		}
		series := s.Series[key]
		if series == nil {
			series = &Series{}
			s.Series[key] = series
		}
		s.pruneLocked(series, hour)
		idx := sort.Search(len(series.Buckets), func(i int) bool { return series.Buckets[i].Hour >= hour })
		if idx == len(series.Buckets) || series.Buckets[idx].Hour != hour {
			series.Buckets = append(series.Buckets, Bucket{})
			copy(series.Buckets[idx+1:], series.Buckets[idx:])
			series.Buckets[idx] = Bucket{Hour: hour, Registers: make([]byte, 1<<s.Precision)}
		}
		if registers := series.Buckets[idx].Registers; rank > registers[index] {
			registers[index] = rank
		}
		series.Buckets[idx].Sightings++
		series.Seen++
	}
}

func (s *State) pruneLocked(series *Series, currentHour int64) {
	cutoff := currentHour - (RetainedHours - 1)
	i := sort.Search(len(series.Buckets), func(i int) bool { return series.Buckets[i].Hour >= cutoff })
	if i > 0 {
		series.Buckets = append([]Bucket(nil), series.Buckets[i:]...)
	}
}

// slot reads only the immutable Precision, so it is safe to call without the lock.
func (s *State) slot(identity []byte) (uint64, uint8) {
	precision := s.Precision
	sum := sha256.Sum256(identity)
	h := binary.BigEndian.Uint64(sum[:8])
	index := h >> (64 - precision)
	remaining := h << precision
	rank := uint8(1)
	if remaining == 0 {
		rank = 64 - precision + 1
	} else {
		for remaining&(uint64(1)<<63) == 0 {
			rank++
			remaining <<= 1
		}
	}
	return index, rank
}

func (s *State) Estimates(at time.Time) []Estimate {
	s.mu.Lock()
	defer s.mu.Unlock()
	currentHour := hourNumber(at)
	windowStartHour := currentHour - (WindowHours - 1)
	keys := make([]string, 0, len(s.Series))
	for key := range s.Series {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]Estimate, 0, len(keys))
	for _, key := range keys {
		series := s.Series[key]
		s.pruneLocked(series, currentHour)
		registers := make([]byte, 1<<s.Precision)
		var sightings uint64
		for _, bucket := range series.Buckets {
			if bucket.Hour < windowStartHour || bucket.Hour > currentHour {
				continue
			}
			for i, value := range bucket.Registers {
				if value > registers[i] {
					registers[i] = value
				}
			}
			sightings += bucket.Sightings
		}
		out = append(out, Estimate{
			Key: key, Distinct: estimate(registers), Sightings: sightings,
			WindowStart: time.Unix(windowStartHour*3600, 0).UTC(),
			WindowEnd:   time.Unix((currentHour+1)*3600, 0).UTC(),
			Error:       1.04 / math.Sqrt(float64(len(registers))),
		})
	}
	return out
}

func estimate(registers []byte) uint64 {
	m := float64(len(registers))
	var alpha float64
	switch len(registers) {
	case 16:
		alpha = 0.673
	case 32:
		alpha = 0.697
	case 64:
		alpha = 0.709
	default:
		alpha = 0.7213 / (1 + 1.079/m)
	}
	var sum float64
	zeros := 0
	for _, register := range registers {
		sum += math.Ldexp(1, -int(register))
		if register == 0 {
			zeros++
		}
	}
	value := alpha * m * m / sum
	if value <= 2.5*m && zeros > 0 {
		value = m * math.Log(m/float64(zeros))
	}
	return uint64(math.Round(value))
}
