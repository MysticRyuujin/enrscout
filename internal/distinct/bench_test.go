package distinct

import (
	"encoding/binary"
	"fmt"
	"testing"
	"time"
)

// fullState approximates production: 23 series, each with a full 169-hour window.
func fullState() *State {
	s := New("bench", DefaultPrecision)
	start := time.Unix(1700000000, 0)
	var id [8]byte
	for h := range RetainedHours {
		at := start.Add(time.Duration(h) * time.Hour)
		for k := range 23 {
			binary.BigEndian.PutUint64(id[:], uint64(h*23+k))
			s.Observe(id[:], at, fmt.Sprintf("walker/w%d/v5/udp4", k))
		}
	}
	return s
}

func BenchmarkMarshalFullState(b *testing.B) {
	s := fullState()
	for b.Loop() {
		if _, err := s.Marshal(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkObserveThreeKeys(b *testing.B) {
	s := New("bench", DefaultPrecision)
	at := time.Unix(1700000000, 0)
	var id [8]byte
	for i := 0; b.Loop(); i++ {
		binary.BigEndian.PutUint64(id[:], uint64(i))
		s.Observe(id[:], at, "v5/udp4", "walker/el-mainnet/v5/udp4", "all/all")
	}
}

// BenchmarkObserveDuringMarshal reports the worst Observe latency while Marshal runs in a loop,
// which is the stall a discovery reader sees during a publish.
func BenchmarkObserveDuringMarshal(b *testing.B) {
	s := fullState()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = s.Marshal()
			}
		}
	}()
	at := time.Unix(1700000000, 0).Add(RetainedHours * time.Hour)
	var id [8]byte
	var worst time.Duration
	for i := 0; b.Loop(); i++ {
		binary.BigEndian.PutUint64(id[:], uint64(i))
		start := time.Now()
		s.Observe(id[:], at, "v5/udp4", "walker/w0/v5/udp4", "all/all")
		worst = max(worst, time.Since(start))
	}
	close(stop)
	<-done
	b.ReportMetric(float64(worst.Microseconds()), "worst-µs")
}
