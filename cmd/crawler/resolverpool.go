package main

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/netutil"
	"golang.org/x/time/rate"

	"github.com/MysticRyuujin/enrscout/internal/discovery"
	"github.com/MysticRyuujin/enrscout/internal/distinct"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
)

type boundSource struct {
	source discovery.Source
	owner  *discovery.Crawler
	walker string
}

type discovered struct {
	n     *enode.Node
	via   string
	owner *discovery.Crawler
}

// resolverPool owns the discovery random-walk readers, the channel between them and the resolve
// workers, and the workers themselves. Close stops the iterators and returns once both groups have
// exited, so the caller cannot accidentally publish while resolution is still mutating the nodeset.
type resolverPool struct {
	cr         *crawler
	identities []*runtimeIdentity
	sources    []boundSource
	self       map[enode.ID]bool
	restrict   *netutil.Netlist
	staticIP   net.IP
	limiter    *rate.Limiter
	distinct   *distinct.State

	nodes   chan discovered
	readers sync.WaitGroup
	workers sync.WaitGroup
	once    sync.Once
}

func newResolverPool(cr *crawler, identities []*runtimeIdentity, restrict *netutil.Netlist, staticIP net.IP, state *distinct.State) *resolverPool {
	p := &resolverPool{
		cr: cr, identities: identities, restrict: restrict, staticIP: staticIP, distinct: state,
		self:  make(map[enode.ID]bool, len(identities)),
		nodes: make(chan discovered, cr.conf.workers),
	}
	for _, identity := range identities {
		p.self[identity.discovery.LocalID()] = true
		for _, source := range identity.discovery.Sources() {
			if !walkSource(identity.spec, source.Proto) {
				continue
			}
			p.sources = append(p.sources, boundSource{source: source, owner: identity.discovery, walker: walkerName(identity.spec)})
		}
	}
	if cr.conf.resolveRate > 0 {
		p.limiter = rate.NewLimiter(rate.Limit(cr.conf.resolveRate), max(1, int(2*cr.conf.resolveRate)))
	}
	mWalkIterators.Set(float64(len(p.sources)))
	return p
}

func (p *resolverPool) Start(ctx context.Context) {
	for _, bound := range p.sources {
		p.readers.Add(1)
		go func(bound boundSource) {
			defer p.readers.Done()
			p.read(ctx, bound)
		}(bound)
	}
	go func() {
		p.readers.Wait()
		close(p.nodes)
	}()
	for range p.cr.conf.workers {
		p.workers.Add(1)
		go func() {
			defer p.workers.Done()
			for d := range p.nodes {
				p.resolve(d)
			}
		}()
	}
}

func (p *resolverPool) Close() {
	p.once.Do(func() {
		for _, bound := range p.sources {
			bound.source.Iter.Close()
		}
		p.readers.Wait()
		p.workers.Wait()
	})
}

func (p *resolverPool) read(ctx context.Context, bound boundSource) {
	src := bound.source
	protoKey := src.Proto + "/" + src.Family
	walkerKey := "walker/" + bound.walker + "/" + protoKey
	for src.Iter.Next() {
		node := src.Iter.Node()
		if node == nil {
			continue
		}
		mDiscoverySightings.WithLabelValues(src.Proto, src.Family).Inc()
		mDiscoveryWalkerSightings.WithLabelValues(bound.walker, src.Proto, src.Family).Inc()
		nodeID := node.ID()
		seen := time.Now()
		p.distinct.Observe(protoKey, nodeID[:], seen)
		p.distinct.Observe(walkerKey, nodeID[:], seen)
		p.distinct.Observe("all/all", nodeID[:], seen)
		if p.limiter != nil {
			waitStart := time.Now()
			if err := p.limiter.Wait(ctx); err != nil {
				return
			}
			mResolveThrottleWait.Add(time.Since(waitStart).Seconds())
		}
		select {
		case p.nodes <- discovered{n: node, via: src.Proto, owner: bound.owner}:
		case <-ctx.Done():
			return
		}
	}
}

func (p *resolverPool) resolveCandidate(d discovered) (*enode.Node, string, error) {
	if d.n.Seq() == 0 && d.via == "v4" {
		return d.owner.ResolveProtocol(d.n, "v4")
	}
	if rn, via, err := d.owner.Resolve(d.n); err == nil {
		return rn, via, nil
	}
	// One alternate identity only: further identities dial the same endpoint from the same host and fail identically, multiplying UDP timeouts.
	for _, identity := range p.identities {
		if identity.discovery == d.owner {
			continue
		}
		if rn, via, err := identity.discovery.Resolve(d.n); err == nil {
			return rn, via, nil
		}
		break
	}
	return nil, "", errors.New("no advertiser identity resolved node")
}

func (p *resolverPool) resolve(d discovered) {
	cr, set, conf := p.cr, p.cr.set, p.cr.conf
	n := d.n
	if isCrawlerRecord(n, p.self, conf.devnetOnly, p.staticIP) {
		mIgnoredSelf.Inc()
		return
	}
	// Out-of-netrestrict nodes still arrive via peer responses; resolving
	// them just burns worker time on guaranteed timeouts.
	if !allowedByNetrestrict(n, p.restrict) {
		return
	}
	if !cr.attempted.Allow(n.ID(), time.Now()) {
		return
	}
	mDiscovered.Inc()
	rn, via, err := p.resolveCandidate(d)
	fallback := false
	if err != nil {
		// Clients that answer neither ENRRequest nor discv5 are still real:
		// their DHT-served record is signed, so record it rather than drop it.
		if n.Seq() > 0 {
			mResolveFallbacks.Inc()
			rn, via = n, d.via
			fallback = true
		} else if conf.devnetForce {
			rn, via = n, d.via
			fallback = true
		} else if cached, ok := cr.pending.Take(n.ID(), layerEL, time.Now()); ok && cached.Network != "" {
			cr.applyLegacyFingerprint(n, d.via, "inbound", cached)
			return
		} else if disposition := cr.enqueueLegacyFingerprint(n, d.via); disposition == legacyFingerprintUnavailable {
			mResolveFailures.Inc()
			set.Penalize(n.ID(), time.Now())
			return
		} else {
			// Queued, rate-limited, and capacity-deferred probes are all
			// live work. They must not decay an already verified legacy node.
			return
		}
	} else {
		mResolved.Inc()
	}
	// ENR resolution can replace a private candidate address with a newer
	// WAN-vouchered address. Recheck the resolved record before it can
	// overwrite an in-range devnet observation.
	if !allowedByNetrestrict(rn, p.restrict) {
		return
	}
	observedAt := time.Now()
	var observed nodeset.Observation
	switch {
	case conf.devnetForce && fallback:
		observed = set.ObserveDevnetFallbackResult(rn, via, observedAt)
	case conf.devnetForce:
		observed = set.ObserveDevnetResult(rn, via, observedAt)
	case fallback:
		observed = set.ObserveFallbackResult(rn, via, observedAt)
	default:
		observed = set.ObserveResult(rn, via, observedAt)
	}
	if !observed.Accepted {
		mInvalidRecords.WithLabelValues(rejectReason(observed)).Inc()
		return
	}
	if observed.New {
		mNodesetAdmissions.Inc()
	}
	recordEvictions(observed)
	if observed.Changed && cr.geo != nil {
		addr := rn.IP()
		g := cr.geo.Lookup(addr)
		set.SetGeo(rn.ID(), addr, g.Country, g.City, g.Subdivision, g.Lat, g.Lon, g.ASN, g.Org, g.Hosting, g.HostingKnown, g.Geolocated, g.AccuracyRadiusKM)
	}
	layer := set.LayerOf(rn.ID())
	if cached, ok := cr.pending.Take(rn.ID(), layer, observedAt); ok {
		cr.applyInbound(rn.ID(), layer, cached)
	}
	// A valid, freshly resolved ENR can still omit the eth fork-ID
	// entry (Nimbus EL is one example). TCP alone is not enough to
	// classify it, but it is enough to attempt the existing bounded
	// RLPx Status path; only authenticated Status can publish it.
	if layer == "" && nodeset.HasExecutionTCP(rn) {
		cr.enqueueLegacyFingerprint(rn, via)
		return
	}
	if observed.Applied {
		cr.enqueueFingerprint(rn, observedAt)
	}
}
