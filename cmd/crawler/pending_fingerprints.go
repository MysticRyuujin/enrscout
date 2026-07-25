package main

import (
	"container/heap"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"

	"github.com/MysticRyuujin/enrscout/internal/enrich"
)

type pendingFingerprint struct {
	layer string
	value enrich.Fingerprint
}

type identifiedFingerprint struct {
	id    enode.ID
	layer string
	value enrich.Fingerprint
}

type pendingLegacyNode struct {
	node *enode.Node
	via  string
}

type pendingExpiry struct {
	id  enode.ID
	at  time.Time
	seq uint64
}

type pendingExpiryHeap []pendingExpiry

func (h pendingExpiryHeap) Len() int           { return len(h) }
func (h pendingExpiryHeap) Less(i, j int) bool { return h[i].at.Before(h[j].at) }
func (h pendingExpiryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *pendingExpiryHeap) Push(x any)        { *h = append(*h, x.(pendingExpiry)) }
func (h *pendingExpiryHeap) Pop() any {
	old := *h
	item := old[len(old)-1]
	*h = old[:len(old)-1]
	return item
}

type expiringEntry[V any] struct {
	value V
	at    time.Time
	seq   uint64
}

type expiringMap[V any] struct {
	mu      sync.Mutex
	entries map[enode.ID]expiringEntry[V]
	ttl     time.Duration
	max     int
	seq     uint64
	expiry  pendingExpiryHeap
}

func newExpiringMap[V any](ttl time.Duration, max int) *expiringMap[V] {
	return &expiringMap[V]{entries: make(map[enode.ID]expiringEntry[V]), ttl: ttl, max: max}
}

func (p *expiringMap[V]) Put(id enode.ID, value V, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune(now)
	p.putLocked(id, value, now)
}

// Allow reserves id for one ttl window and reports whether the reservation was granted, so callers
// can rate-limit attempts per node. The check and the insert are one critical section: split across
// two calls, concurrent workers both observe no reservation and both proceed.
func (p *expiringMap[V]) Allow(id enode.ID, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune(now)
	if _, reserved := p.entries[id]; reserved {
		return false
	}
	var zero V
	p.putLocked(id, zero, now)
	return true
}

func (p *expiringMap[V]) putLocked(id enode.ID, value V, now time.Time) {
	if _, exists := p.entries[id]; !exists && len(p.entries) >= p.max {
		p.evictOldest()
	}
	p.seq++
	p.entries[id] = expiringEntry[V]{value: value, at: now, seq: p.seq}
	heap.Push(&p.expiry, pendingExpiry{id: id, at: now, seq: p.seq})
	if len(p.expiry) > 2*p.max {
		p.compact()
	}
}

func (p *expiringMap[V]) Take(id enode.ID, now time.Time) (V, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune(now)
	entry, ok := p.entries[id]
	if ok {
		delete(p.entries, id)
	}
	return entry.value, ok
}

func (p *expiringMap[V]) TakeIf(id enode.ID, now time.Time, accept func(V) bool) (V, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune(now)
	entry, ok := p.entries[id]
	if !ok || !accept(entry.value) {
		var zero V
		return zero, false
	}
	delete(p.entries, id)
	return entry.value, true
}

func (p *expiringMap[V]) Drain(now time.Time, accept func(enode.ID, V) bool) []enode.ID {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune(now)
	var out []enode.ID
	for id, entry := range p.entries {
		if !accept(id, entry.value) {
			continue
		}
		out = append(out, id)
		delete(p.entries, id)
	}
	return out
}

func (p *expiringMap[V]) prune(now time.Time) {
	for p.expiry.Len() > 0 && !p.expiry[0].at.Add(p.ttl).After(now) {
		item := heap.Pop(&p.expiry).(pendingExpiry)
		if entry, ok := p.entries[item.id]; ok && entry.seq == item.seq {
			delete(p.entries, item.id)
		}
	}
}

func (p *expiringMap[V]) evictOldest() {
	for p.expiry.Len() > 0 {
		item := heap.Pop(&p.expiry).(pendingExpiry)
		if entry, ok := p.entries[item.id]; ok && entry.seq == item.seq {
			delete(p.entries, item.id)
			return
		}
	}
}

func (p *expiringMap[V]) compact() {
	p.expiry = p.expiry[:0]
	for id, entry := range p.entries {
		p.expiry = append(p.expiry, pendingExpiry{id: id, at: entry.at, seq: entry.seq})
	}
	heap.Init(&p.expiry)
}

type pendingFingerprints struct {
	*expiringMap[pendingFingerprint]
}

func newPendingFingerprints(ttl time.Duration, max int) *pendingFingerprints {
	return &pendingFingerprints{expiringMap: newExpiringMap[pendingFingerprint](ttl, max)}
}

func (p *pendingFingerprints) Put(id enode.ID, layer string, value enrich.Fingerprint, now time.Time) {
	if id == (enode.ID{}) {
		return
	}
	p.expiringMap.Put(id, pendingFingerprint{layer: layer, value: value}, now)
}

func (p *pendingFingerprints) Take(id enode.ID, layer string, now time.Time) (enrich.Fingerprint, bool) {
	entry, ok := p.TakeIf(id, now, func(entry pendingFingerprint) bool { return entry.layer == layer })
	return entry.value, ok
}

// DrainKnown reconciles passive identifications with nodes registered through paths outside the
// discovery workers, notably the isolated-devnet probe API.
func (p *pendingFingerprints) DrainKnown(now time.Time, layerOf func(enode.ID) string) []identifiedFingerprint {
	var out []identifiedFingerprint
	p.Drain(now, func(id enode.ID, entry pendingFingerprint) bool {
		if layerOf(id) != entry.layer {
			return false
		}
		out = append(out, identifiedFingerprint{id: id, layer: entry.layer, value: entry.value})
		return true
	})
	return out
}

type pendingLegacyNodes struct {
	*expiringMap[pendingLegacyNode]
}

func newPendingLegacyNodes(ttl time.Duration, max int) *pendingLegacyNodes {
	return &pendingLegacyNodes{expiringMap: newExpiringMap[pendingLegacyNode](ttl, max)}
}

func (p *pendingLegacyNodes) Put(node *enode.Node, via string, now time.Time) {
	if node == nil {
		return
	}
	p.expiringMap.Put(node.ID(), pendingLegacyNode{node: node, via: via}, now)
}
