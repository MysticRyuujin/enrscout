package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type countingCloser struct{ closed *atomic.Int64 }

func (c countingCloser) Close() error { c.closed.Add(1); return nil }

// A construction failure must release everything built before it. Those resources used to be held
// by run()-scoped defers; the runtime is now responsible for them.
func TestIdentityRuntimeRollsBackPartialConstruction(t *testing.T) {
	var closed atomic.Int64
	built := 0
	rt := &identityRuntime{}
	rt.build = func(spec identitySpec) (*runtimeIdentity, error) {
		if built == 2 {
			return nil, errors.New("boom")
		}
		built++
		rt.track(countingCloser{&closed})
		rt.track(countingCloser{&closed})
		return &runtimeIdentity{spec: spec}, nil
	}

	specs := []identitySpec{{Network: "a"}, {Network: "b"}, {Network: "c"}}
	if err := rt.start(specs); err == nil {
		t.Fatal("start returned nil on a failing build")
	}
	if got := closed.Load(); got != 4 {
		t.Fatalf("closed %d resources, want the 4 built before the failure", got)
	}
}

func TestIdentityRuntimeCloseIsIdempotent(t *testing.T) {
	var closed atomic.Int64
	rt := &identityRuntime{}
	rt.track(countingCloser{&closed})
	rt.Close()
	rt.Close()
	if got := closed.Load(); got != 1 {
		t.Fatalf("closed %d times, want exactly 1", got)
	}
}

// Close must cancel its own context before waiting. If it waited on run()'s signal context the
// refresh loop would still be live and Close would hang -- and defers being LIFO, a deferred Close
// runs before the deferred signal stop().
func TestIdentityRuntimeCloseStopsRefreshWithoutTheParentContext(t *testing.T) {
	parent, stop := context.WithCancel(context.Background())
	defer stop()
	ctx, cancel := context.WithCancel(parent)
	rt := &identityRuntime{cancel: cancel}
	rt.startRefresh(ctx)

	done := make(chan struct{})
	go func() {
		rt.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung: the refresh loop outlived it while the parent context was still live")
	}
}
