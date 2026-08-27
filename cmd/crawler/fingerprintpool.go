package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"

	"github.com/MysticRyuujin/enrscout/internal/enrich"
	"github.com/MysticRyuujin/enrscout/internal/netconf"
)

// fingerprintPool owns the outbound probe queues and their workers. Sends are guarded rather than
// ordered: resolve workers, the seed loop and the retry loop all enqueue, and making shutdown
// safety depend on waiting for every one of them meant a sender added later would panic on
// send-to-closed-channel. Holding mu across the send makes that impossible whoever calls.
type fingerprintPool struct {
	mu     sync.RWMutex
	closed bool
	once   sync.Once

	fpCh    chan elFingerprintTask
	clCh    chan *enode.Node
	workers sync.WaitGroup
}

func newFingerprintPool(depth int) *fingerprintPool {
	return &fingerprintPool{
		fpCh: make(chan elFingerprintTask, depth),
		clCh: make(chan *enode.Node, depth),
	}
}

func (p *fingerprintPool) enqueueEL(task elFingerprintTask) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return false
	}
	select {
	case p.fpCh <- task:
		return true
	default:
		return false
	}
}

func (p *fingerprintPool) enqueueCL(n *enode.Node) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return false
	}
	select {
	case p.clCh <- n:
		return true
	default:
		return false
	}
}

func (p *fingerprintPool) elPressure() bool {
	return len(p.fpCh) >= cap(p.fpCh)/2
}

// The whole close-and-drain is inside the Once: gating only the flag would let a second caller
// return while the first is still draining.
func (p *fingerprintPool) Close() {
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		close(p.fpCh)
		close(p.clCh)
		p.mu.Unlock()
		p.workers.Wait()
	})
}

func (p *fingerprintPool) startEL(ctx context.Context, cr *crawler, workers int, legacyStatus *netconf.Network) {
	for range workers {
		p.workers.Add(1)
		go func() {
			defer p.workers.Done()
			for task := range p.fpCh {
				mFingerprintQueueDepth.WithLabelValues("el").Set(float64(len(p.fpCh)))
				// Shutdown drains the queue through a canceled context; those probes never
				// dialed, so recording them as failures would persist spurious attempts.
				if ctx.Err() != nil {
					if task.claimed || !task.legacy {
						cr.set.UnclaimFingerprint(task.n.ID())
					}
					continue
				}
				start := time.Now()
				if task.legacy {
					r, err := cr.fp.ProbeStatus(ctx, task.n, legacyStatus)
					observeFingerprintAttempt("el", "outbound", time.Since(start), err)
					if task.claimed {
						if err != nil && ctx.Err() != nil {
							cr.set.UnclaimFingerprint(task.n.ID())
							continue
						}
						cr.finishCandidateFingerprint(task.n, r, err)
						continue
					}
					if err != nil {
						if ctx.Err() != nil {
							continue
						}
						reason := observeFingerprintFailure("el", "outbound", "", "initial", err)
						mLegacyFailures.WithLabelValues(reason).Inc()
						slog.Debug("legacy EL classification failed", "node", task.n.ID(), "via", task.via, "reason", reason, "err", err)
						continue
					}
					cr.pendingLegacy.Take(task.n.ID(), time.Now())
					cr.applyLegacyFingerprint(task.n, task.via, "outbound", r)
					continue
				}
				networkName := cr.set.NetworkOf(task.n.ID())
				nw, networkErr := netconf.Get(networkName)
				var r enrich.Fingerprint
				var err error
				if networkErr == nil {
					r, err = cr.fp.ProbeStatus(ctx, task.n, nw)
				} else {
					r, err = cr.fp.Probe(ctx, task.n)
				}
				observeFingerprintAttempt("el", "outbound", time.Since(start), err)
				if err != nil && ctx.Err() != nil {
					cr.set.UnclaimFingerprint(task.n.ID())
					continue
				}
				cr.finishFingerprint("el", task.n, r, err)
			}
		}()
	}
}

func (p *fingerprintPool) startCL(ctx context.Context, cr *crawler, workers int) {
	for range workers {
		p.workers.Add(1)
		go func() {
			defer p.workers.Done()
			for n := range p.clCh {
				mFingerprintQueueDepth.WithLabelValues("cl").Set(float64(len(p.clCh)))
				if ctx.Err() != nil {
					cr.set.UnclaimFingerprint(n.ID())
					continue
				}
				start := time.Now()
				var localFork []byte
				if fork, forkErr := netconf.CurrentCLForkENR(cr.set.NetworkOf(n.ID())); forkErr == nil {
					localFork = fork
				}
				r, err := cr.clfp.ProbeStatus(ctx, n, localFork)
				observeFingerprintAttempt("cl", "outbound", time.Since(start), err)
				if err == nil {
					mCLStatus.WithLabelValues("outbound", clStatusOutcome(r)).Inc()
				}
				if err != nil && ctx.Err() != nil {
					cr.set.UnclaimFingerprint(n.ID())
					continue
				}
				cr.finishFingerprint("cl", n, r, err)
			}
		}()
	}
}
