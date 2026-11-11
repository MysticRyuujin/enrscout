package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/params"

	"github.com/MysticRyuujin/enrscout/internal/discovery"
	"github.com/MysticRyuujin/enrscout/internal/netconf"
)

const (
	bootnodeProbeInterval = 5 * time.Minute
	bootnodeDownAfter     = 3
	bootnodeProbeWorkers  = 8
)

// gethBootnodes adds the lists go-ethereum ships, which still carry the legacy EF enodes that
// netconf's NodeOps records replace: clients dial them, so their health matters.
var gethBootnodes = map[string][]string{
	"mainnet": params.MainnetBootnodes,
	"sepolia": params.SepoliaBootnodes,
	"hoodi":   params.HoodiBootnodes,
}

type bootnodeTarget struct {
	network, layer string
	node           *enode.Node
	protocols      []string
	findnode       bool
	disc           *discovery.Crawler
}

func (t bootnodeTarget) label() string {
	return fmt.Sprintf("%s@%s", t.node.ID().TerminalString(), t.node.IPAddr())
}

type bootnodeMonitor struct {
	targets  []bootnodeTarget
	mu       sync.Mutex
	failures map[string]int
}

func newBootnodeMonitor(networks []string, identities []*runtimeIdentity) *bootnodeMonitor {
	m := &bootnodeMonitor{failures: map[string]int{}}
	for _, network := range networks {
		el, cl := monitorIdentities(network, identities)
		seen := map[enode.ID]bool{}
		add := func(n *enode.Node, layer string, disc *discovery.Crawler) {
			if disc == nil || seen[n.ID()] {
				return
			}
			seen[n.ID()] = true
			t := bootnodeTarget{network: network, layer: layer, node: n, disc: disc}
			switch {
			case len(n.Record().Signature()) == 0:
				t.protocols = []string{"v4"}
			case layer == layerEL:
				t.protocols, t.findnode = []string{"v4", "v5"}, true
			default:
				t.protocols, t.findnode = []string{"v5"}, true
			}
			m.targets = append(m.targets, t)
		}
		if nodes, err := netconf.Bootnodes(network); err == nil {
			for _, n := range nodes {
				add(n, layerEL, el)
			}
		}
		for _, raw := range gethBootnodes[network] {
			if n, err := enode.Parse(enode.ValidSchemes, raw); err == nil {
				add(n, layerEL, el)
			}
		}
		if nodes, err := netconf.CLBootnodes(network); err == nil {
			for _, n := range nodes {
				add(n, layerCL, cl)
			}
		}
	}
	return m
}

// monitorIdentities prefers walkers because they always exist for a network that advertises.
func monitorIdentities(network string, identities []*runtimeIdentity) (el, cl *discovery.Crawler) {
	for _, id := range identities {
		if id.spec.Network != network || id.discovery == nil {
			continue
		}
		switch {
		case id.spec.Layer == layerEL && el == nil && id.spec.Walker:
			el = id.discovery
		case id.spec.Layer == layerCL && cl == nil:
			cl = id.discovery
		}
	}
	return el, cl
}

// probe checks ctx before each request: geth's calls take no context, and Close waits for the
// running tick, so a round over dead targets must not hold up the final publish.
func (m *bootnodeMonitor) probe(ctx context.Context) {
	work := make(chan bootnodeTarget)
	var wg sync.WaitGroup
	for range bootnodeProbeWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range work {
				m.probeTarget(ctx, t)
			}
		}()
	}
	for _, t := range m.targets {
		if ctx.Err() != nil {
			break
		}
		work <- t
	}
	close(work)
	wg.Wait()
}

func (m *bootnodeMonitor) probeTarget(ctx context.Context, t bootnodeTarget) {
	label := t.label()
	for _, family := range t.disc.ProbeFamilies(t.node) {
		for _, proto := range t.protocols {
			if ctx.Err() != nil {
				return
			}
			start := time.Now()
			err := t.disc.Ping(t.node, proto, family)
			m.record(t, label, proto, family, time.Since(start), err)
		}
		if !t.findnode || ctx.Err() != nil {
			continue
		}
		nodes, err := t.disc.FindnodeV5(t.node, family, []uint{256})
		if err != nil {
			nodes = nil
		}
		mBootnodeFindnodeNodes.WithLabelValues(t.network, t.layer, family, label).Set(float64(len(nodes)))
	}
}

// record reports up only after a success and down only after bootnodeDownAfter failures in a
// row, so one lost UDP packet does not flap the gauge.
func (m *bootnodeMonitor) record(t bootnodeTarget, label, proto, family string, rtt time.Duration, err error) {
	key := t.network + "/" + label + "/" + proto + "/" + family
	m.mu.Lock()
	if err == nil {
		m.failures[key] = 0
	} else {
		m.failures[key]++
	}
	failures := m.failures[key]
	m.mu.Unlock()
	labels := []string{t.network, t.layer, proto, family, label}
	switch {
	case err == nil:
		mBootnodeUp.WithLabelValues(labels...).Set(1)
		mBootnodeRTT.WithLabelValues(labels...).Set(rtt.Seconds())
	case failures == bootnodeDownAfter:
		mBootnodeUp.WithLabelValues(labels...).Set(0)
		slog.Warn("bootnode down", "network", t.network, "layer", t.layer, "protocol", proto, "family", family, "node", label, "err", err)
	}
}
