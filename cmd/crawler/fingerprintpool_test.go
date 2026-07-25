package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/enrich"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
)

func TestFingerprintPoolRejectsAfterClose(t *testing.T) {
	p := newFingerprintPool(4)
	if !p.enqueueEL(elFingerprintTask{}) || !p.enqueueCL(nil) {
		t.Fatal("open pool rejected work")
	}
	p.Close()
	if p.enqueueEL(elFingerprintTask{}) {
		t.Error("EL enqueue accepted after Close")
	}
	if p.enqueueCL(nil) {
		t.Error("CL enqueue accepted after Close")
	}
}

func TestFingerprintPoolReportsFullQueue(t *testing.T) {
	p := newFingerprintPool(1)
	if !p.enqueueEL(elFingerprintTask{}) {
		t.Fatal("first enqueue rejected")
	}
	if p.enqueueEL(elFingerprintTask{}) {
		t.Error("full queue accepted a second task")
	}
	if !p.elPressure() {
		t.Error("elPressure false with a full queue")
	}
}

// Close must not return until the workers have drained, or the final publish can race work that is
// still mutating the nodeset.
func TestFingerprintPoolCloseDrainsWorkers(t *testing.T) {
	p := newFingerprintPool(8)
	var handled atomic.Int64
	p.workers.Add(1)
	go func() {
		defer p.workers.Done()
		for range p.fpCh {
			time.Sleep(time.Millisecond)
			handled.Add(1)
		}
	}()
	for range 8 {
		if !p.enqueueEL(elFingerprintTask{}) {
			t.Fatal("enqueue rejected while open")
		}
	}
	p.Close()
	if got := handled.Load(); got != 8 {
		t.Fatalf("Close returned with %d of 8 tasks handled", got)
	}
}

// A second Close must also observe drained workers, which a bare closed flag would not give it.
func TestFingerprintPoolConcurrentCloseAllWaitForDrain(t *testing.T) {
	p := newFingerprintPool(4)
	var handled atomic.Int64
	p.workers.Add(1)
	go func() {
		defer p.workers.Done()
		for range p.fpCh {
			time.Sleep(2 * time.Millisecond)
			handled.Add(1)
		}
	}()
	for range 4 {
		p.enqueueEL(elFingerprintTask{})
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Close()
			if got := handled.Load(); got != 4 {
				t.Errorf("Close returned with %d of 4 tasks handled", got)
			}
		}()
	}
	wg.Wait()
}

// The hazard this design exists to remove: senders that outlive the close must not panic.
func TestFingerprintPoolEnqueueRacingCloseNeverPanics(t *testing.T) {
	p := newFingerprintPool(4)
	p.workers.Add(1)
	go func() {
		defer p.workers.Done()
		for range p.fpCh { //nolint:revive // draining
		}
	}()
	var senders sync.WaitGroup
	stop := make(chan struct{})
	for range 8 {
		senders.Add(1)
		go func() {
			defer senders.Done()
			for {
				select {
				case <-stop:
					return
				default:
					p.enqueueEL(elFingerprintTask{})
					p.enqueueCL(nil)
				}
			}
		}()
	}
	time.Sleep(5 * time.Millisecond)
	p.Close()
	time.Sleep(5 * time.Millisecond)
	close(stop)
	senders.Wait()
}

// A rejected enqueue must release what the caller reserved, or the node is stranded: still claimed,
// so the retry loop never offers it again.
func TestCrawlerEnqueueReleasesClaimsWhenPoolIsClosed(t *testing.T) {
	n := mainnetNode(t)
	set := nodeset.NewWithLimit(0)
	now := time.Now()
	if !set.ObserveResult(n, "v4", now).Accepted {
		t.Fatal("test node rejected by the nodeset")
	}
	fp, err := enrich.NewFingerprinterWithPolicy(time.Second, 1, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	cr := &crawler{
		set: set, fp: fp,
		pendingLegacy:   newPendingLegacyNodes(time.Minute, 16),
		targetBudget:    newTargetDialBudget(0, 0, 16, time.Hour),
		legacyAttempted: newExpiringMap[struct{}](time.Hour, 16),
		pool:            newFingerprintPool(4),
	}
	cr.pool.Close()

	if cr.enqueueFingerprint(n, now) {
		t.Fatal("closed pool accepted an enqueue")
	}
	if !set.ClaimFingerprintAt(n.ID(), now) {
		t.Error("rejected enqueue left the fingerprint claim held")
	}
	set.UnclaimFingerprint(n.ID())

	if got := cr.enqueueLegacyFingerprint(n, "v4"); got != legacyFingerprintDeferred {
		t.Fatalf("legacy enqueue = %v, want deferred", got)
	}
	if !cr.legacyAttempted.Allow(n.ID(), now) {
		t.Error("rejected legacy enqueue left the attempt window claimed")
	}
}
