package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/netutil"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/MysticRyuujin/enrscout/internal/discovery"
)

func TestBootnodeUpNeedsThreeMissedRounds(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	target := bootnodeTarget{network: "test-up", layer: layerEL, node: enode.NewV4(&key.PublicKey, net.IP{192, 0, 2, 1}, 0, 30303)}
	label := target.label()
	m := &bootnodeMonitor{failures: map[string]int{}}
	up := func() float64 {
		return testutil.ToFloat64(mBootnodeUp.WithLabelValues("test-up", layerEL, "v4", "udp4", label))
	}
	miss := errors.New("timeout")

	series := testutil.CollectAndCount(mBootnodeUp)
	for range bootnodeDownAfter - 1 {
		m.record(target, label, "v4", "udp4", 0, miss)
	}
	if got := testutil.CollectAndCount(mBootnodeUp); got != series {
		t.Fatalf("series = %d after %d misses with no answer yet, want %d: a scrape would read down too early", got, bootnodeDownAfter-1, series)
	}
	m.record(target, label, "v4", "udp4", 0, miss)
	if up() != 0 {
		t.Fatalf("up = %v after %d misses with no answer yet, want 0", up(), bootnodeDownAfter)
	}

	m.record(target, label, "v4", "udp4", time.Millisecond, nil)
	if up() != 1 {
		t.Fatalf("up = %v after an answer, want 1", up())
	}
	for i := 1; i < bootnodeDownAfter; i++ {
		m.record(target, label, "v4", "udp4", 0, miss)
		if up() != 1 {
			t.Fatalf("up = %v after %d missed rounds, want still 1", up(), i)
		}
	}
	m.record(target, label, "v4", "udp4", 0, miss)
	if up() != 0 {
		t.Fatalf("up = %v after %d missed rounds, want 0", up(), bootnodeDownAfter)
	}
	m.record(target, label, "v4", "udp4", time.Millisecond, nil)
	if up() != 1 {
		t.Fatalf("up = %v after a new answer, want 1", up())
	}
}

func TestBootnodeProbeStopsPromptlyOnCancel(t *testing.T) {
	loopback := net.IP{127, 0, 0, 1}
	probe, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback})
	if err != nil {
		t.Fatal(err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()
	disc, err := discovery.New(discovery.Config{
		PortV4: port, PortV5: port, Families: []string{"udp4"},
		StaticIPs: []net.IP{loopback}, CLOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer disc.Close()

	m := &bootnodeMonitor{failures: map[string]int{}}
	for i := range 64 {
		key, err := crypto.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		dead := enode.NewV4(&key.PublicKey, loopback, 0, 40000+i)
		m.targets = append(m.targets, bootnodeTarget{network: "test-cancel", layer: layerEL, node: dead,
			protocols: []string{"v4", "v5"}, findnode: true, disc: disc})
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.probe(ctx)
		close(done)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("probe still running 5s after cancel; 64 dead targets at ~3s each would take ~24s")
	}
}

func TestBootnodeTargetsCoverEveryListWithItsProtocols(t *testing.T) {
	disc := &discovery.Crawler{}
	identities := []*runtimeIdentity{
		{spec: identitySpec{Network: "mainnet", Layer: layerEL, Walker: true}, discovery: disc},
		{spec: identitySpec{Network: "mainnet", Layer: layerCL}, discovery: disc},
	}
	m := newBootnodeMonitor([]string{"mainnet"}, identities, nil)

	byKind := map[string]int{}
	seen := map[enode.ID]bool{}
	for _, tg := range m.targets {
		if seen[tg.node.ID()] {
			t.Fatalf("target %s listed twice", tg.label())
		}
		seen[tg.node.ID()] = true
		switch {
		case tg.layer == layerEL && len(tg.node.Record().Signature()) == 0:
			if len(tg.protocols) != 1 || tg.protocols[0] != "v4" || tg.findnode {
				t.Fatalf("enode target %s probes %v findnode=%v, want v4 only", tg.label(), tg.protocols, tg.findnode)
			}
			byKind["enode"]++
		case tg.layer == layerEL:
			if len(tg.protocols) != 2 || !tg.findnode {
				t.Fatalf("EL ENR target %s probes %v findnode=%v, want v4, v5 and findnode", tg.label(), tg.protocols, tg.findnode)
			}
			byKind["el"]++
		default:
			if len(tg.protocols) != 1 || tg.protocols[0] != "v5" || !tg.findnode {
				t.Fatalf("CL target %s probes %v findnode=%v, want v5 and findnode", tg.label(), tg.protocols, tg.findnode)
			}
			byKind["cl"]++
		}
	}
	if byKind["enode"] == 0 || byKind["el"] == 0 || byKind["cl"] == 0 {
		t.Fatalf("targets by kind = %v, want legacy enodes, EL ENRs and CL ENRs", byKind)
	}
}

func TestBootnodeTargetsHonourNetrestrict(t *testing.T) {
	disc := &discovery.Crawler{}
	identities := []*runtimeIdentity{
		{spec: identitySpec{Network: "mainnet", Layer: layerEL, Walker: true}, discovery: disc},
		{spec: identitySpec{Network: "mainnet", Layer: layerCL}, discovery: disc},
	}
	restrict, err := netutil.ParseNetlist("192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if m := newBootnodeMonitor([]string{"mainnet"}, identities, restrict); len(m.targets) != 0 {
		t.Fatalf("%d targets outside --netrestrict, want none", len(m.targets))
	}
}
