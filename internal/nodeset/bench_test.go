package nodeset

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"
)

// BenchmarkEvictFallbackBatch measures one capacity eviction over a 200k set whose fallback class
// holds 50k nodes, all under the write lock in production.
func BenchmarkEvictFallbackBatch(b *testing.B) {
	base := time.Unix(1700000000, 0)
	fill := func(s *Set) {
		for i := range 200_000 {
			var id enode.ID
			binary.BigEndian.PutUint64(id[:], uint64(i))
			score := 5
			if i%4 == 0 {
				score = dropBelow
			}
			s.m[id] = &Node{ID: id, Network: "mainnet", Score: score, LastResolved: base.Add(time.Duration(i) * time.Second)}
		}
	}
	s := NewWithLimit(200_000)
	fill(s)
	b.ResetTimer()
	for b.Loop() {
		b.StopTimer()
		if len(s.m) < 199_000 {
			s = NewWithLimit(200_000)
			fill(s)
		}
		b.StartTimer()
		if evicted, _ := s.evictForLocked(1); evicted != 200 {
			b.Fatalf("evicted %d, want 200", evicted)
		}
	}
}
