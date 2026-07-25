package discovery

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

type countingResolver struct {
	node  *enode.Node
	err   error
	calls int
}

func (r *countingResolver) RequestENR(*enode.Node) (*enode.Node, error) {
	r.calls++
	return r.node, r.err
}

func TestValidateConfig(t *testing.T) {
	valid := Config{CLOnly: true, PortV4: 30303, PortV5: 30303}
	if err := validateConfig(valid); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"missing network", Config{PortV4: 30303, PortV5: 30303}, "network is required"},
		{"invalid v4 port", Config{CLOnly: true, PortV4: 0, PortV5: 30303}, "discovery ports"},
		{"invalid v5 port", Config{CLOnly: true, PortV4: 30303, PortV5: 65536}, "discovery ports"},
		{"different ports", Config{CLOnly: true, PortV4: 30303, PortV5: 30304}, "must share"},
		{"invalid TCP", Config{CLOnly: true, PortV4: 30303, PortV5: 30303, TCP: 65536}, "TCP port"},
		{"invalid QUIC", Config{CLOnly: true, PortV4: 30303, PortV5: 30303, QUIC: 65536}, "QUIC port"},
		{"invalid family", Config{CLOnly: true, PortV4: 30303, PortV5: 30303, Families: []string{"tcp4"}}, "unsupported"},
		{"duplicate family", Config{CLOnly: true, PortV4: 30303, PortV5: 30303, Families: []string{"udp4", "udp4"}}, "duplicate"},
		{"unspecified static IP", Config{CLOnly: true, PortV4: 30303, PortV5: 30303, StaticIPs: []net.IP{net.IPv4zero}}, "invalid static IP"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateConfig(tt.cfg); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validateConfig() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestResolveProtocolUsesOnlyRequestedProtocol(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	node := enode.NewV4(&key.PublicKey, net.ParseIP("127.0.0.1"), 30303, 30303)
	v4 := &countingResolver{node: node}
	v5 := &countingResolver{err: errors.New("unexpected")}
	crawler := &Crawler{endpoints: []endpoint{
		{proto: "v5", family: "udp4", res: v5},
		{proto: "v4", family: "udp4", res: v4},
	}}

	got, via, err := crawler.ResolveProtocol(node, "v4")
	if err != nil || got != node || via != "v4" {
		t.Fatalf("ResolveProtocol = %v, %q, %v", got, via, err)
	}
	if v4.calls != 1 || v5.calls != 0 {
		t.Fatalf("resolver calls v4=%d v5=%d", v4.calls, v5.calls)
	}
}

// A client that knows only the crawler must still learn peers from it: the crawler runs
// full discv4/discv5 endpoints, so its routing table answers FINDNODE like any bootnode.
func TestCrawlerAnswersFindnodeLikeABootnode(t *testing.T) {
	loopback := net.IP{127, 0, 0, 1}

	newV5 := func(boot []*enode.Node) (*discover.UDPv5, *enode.Node) {
		t.Helper()
		key, err := crypto.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		db, err := enode.OpenDB("")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(db.Close)
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback})
		if err != nil {
			t.Fatal(err)
		}
		ln := enode.NewLocalNode(db, key)
		ln.SetStaticIP(loopback)
		ln.SetFallbackIP(loopback)
		ln.SetFallbackUDP(conn.LocalAddr().(*net.UDPAddr).Port)
		sock, err := discover.ListenV5(conn, ln, discover.Config{PrivateKey: key, Bootnodes: boot})
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		t.Cleanup(sock.Close)
		return sock, ln.Node()
	}

	peerSock, peer := newV5(nil)

	probe, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback})
	if err != nil {
		t.Fatal(err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()

	crawler, err := New(Config{
		CLOnly:    true,
		Key:       nil,
		PortV4:    port,
		PortV5:    port,
		Families:  []string{"udp4"},
		StaticIPs: []net.IP{loopback},
		Bootnodes: []*enode.Node{peer},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer crawler.Close()

	// The client is told about the crawler and nothing else.
	client, _ := newV5([]*enode.Node{crawler.LocalNode()})

	deadline := time.Now().Add(30 * time.Second)
	for {
		for _, n := range client.Lookup(peer.ID()) {
			if n.ID() == peer.ID() {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("client never learned peer %s through the crawler (peer table has %d nodes)",
				peer.ID().TerminalString(), len(peerSock.AllNodes()))
		}
		time.Sleep(500 * time.Millisecond)
	}
}
