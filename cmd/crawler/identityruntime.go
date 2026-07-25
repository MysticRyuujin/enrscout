package main

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/discovery"
	"github.com/MysticRyuujin/enrscout/internal/enrich"
	"github.com/MysticRyuujin/enrscout/internal/netconf"
)

// identityRuntime owns every advertiser resource: discovery sockets, EL inbound listeners, CL
// libp2p hosts, and the ENR refresh loop.
//
// It carries its own cancel rather than relying on run()'s context. Close must work from a startup
// error path, where the signal context is still live and a refresh loop waiting only on that
// context would never exit -- and since defers are LIFO, a deferred Close runs before the deferred
// signal stop().
type identityRuntime struct {
	identities []*runtimeIdentity
	closers    []io.Closer
	cancel     context.CancelFunc
	refresh    sync.WaitGroup
	once       sync.Once

	// build is swapped in tests to fail at a chosen index.
	build func(spec identitySpec) (*runtimeIdentity, error)
}

func (rt *identityRuntime) track(c io.Closer) { rt.closers = append(rt.closers, c) }

// start rolls back on the first failure: without it, resources built by earlier iterations would
// leak, which is what the per-resource defers in run() used to prevent.
func (rt *identityRuntime) start(specs []identitySpec) error {
	for _, spec := range specs {
		identity, err := rt.build(spec)
		if err != nil {
			rt.Close()
			return err
		}
		rt.identities = append(rt.identities, identity)
	}
	return nil
}

// Close cancels the refresh loop and waits for it before releasing resources, newest first.
func (rt *identityRuntime) Close() {
	rt.once.Do(func() {
		if rt.cancel != nil {
			rt.cancel()
		}
		rt.refresh.Wait()
		for i := len(rt.closers) - 1; i >= 0; i-- {
			_ = rt.closers[i].Close()
		}
	})
}

func (rt *identityRuntime) startRefresh(ctx context.Context) {
	networks := make([]string, 0, len(rt.identities))
	seen := make(map[string]struct{}, len(rt.identities))
	for _, identity := range rt.identities {
		if _, ok := seen[identity.spec.Network]; !ok {
			seen[identity.spec.Network] = struct{}{}
			networks = append(networks, identity.spec.Network)
		}
	}
	refreshAt := func(at time.Time) {
		for _, identity := range rt.identities {
			if identity.spec.Layer == layerEL {
				nw, _ := netconf.Get(identity.spec.Network)
				nw.RefreshClassifyWindowAt(at)
				identity.discovery.SetELForkID(nw.CurrentForkIDAt(at))
				continue
			}
			entry, err := netconf.CurrentCLForkENRAt(identity.spec.Network, at)
			if err != nil {
				slog.Warn("refresh consensus advertiser ENR", "network", identity.spec.Network, "err", err)
				continue
			}
			nfd, err := netconf.CurrentCLNFDAt(identity.spec.Network, at)
			if err != nil {
				slog.Warn("refresh consensus advertiser nfd", "network", identity.spec.Network, "err", err)
				continue
			}
			identity.discovery.SetCLForkState(entry, nfd)
		}
	}
	nextDelay := func(at time.Time) time.Duration {
		delay := time.Hour
		_, transition, err := netconf.ForkEraTokenAt(at, networks...)
		if err == nil && !transition.IsZero() {
			if until := time.Until(transition); until < delay {
				delay = until
			}
		}
		if delay < 10*time.Millisecond {
			return 10 * time.Millisecond
		}
		return delay
	}
	rt.refresh.Add(1)
	go func() {
		defer rt.refresh.Done()
		t := time.NewTimer(nextDelay(time.Now()))
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				now := time.Now()
				refreshAt(now)
				t.Reset(nextDelay(now))
			}
		}
	}()
}

type discoveryCloser struct{ c *discovery.Crawler }

func (d discoveryCloser) Close() error { d.c.Close(); return nil }

type clCloser struct{ c *enrich.CLFingerprinter }

func (d clCloser) Close() error { d.c.Close(); return nil }
