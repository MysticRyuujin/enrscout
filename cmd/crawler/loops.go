package main

import (
	"context"
	"sync"
	"time"
)

// backgroundLoops owns the periodic tasks that mutate the nodeset -- devnet seed re-observation and
// fingerprint retry. Close cancels and waits: cancelling the context alone does not stop them
// synchronously, so a ready ticker could still fire into the final publish.
type backgroundLoops struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

func newBackgroundLoops(parent context.Context) *backgroundLoops {
	ctx, cancel := context.WithCancel(parent)
	return &backgroundLoops{ctx: ctx, cancel: cancel}
}

// every runs tick on interval until Close. It does not run tick immediately; callers that need a
// first pass before the crawl starts do it themselves.
func (l *backgroundLoops) every(interval time.Duration, tick func(time.Time)) {
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-l.ctx.Done():
				return
			case now := <-t.C:
				tick(now)
			}
		}
	}()
}

func (l *backgroundLoops) Close() {
	l.once.Do(func() {
		l.cancel()
		l.wg.Wait()
	})
}
