package enrich

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	ethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/golang/snappy"
	"github.com/libp2p/go-libp2p"
	mplex "github.com/libp2p/go-libp2p-mplex"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/event"
	libp2pnetwork "github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	ma "github.com/multiformats/go-multiaddr"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
)

func TestSafeInboundCLCallbackRecoversPanic(t *testing.T) {
	returned := false
	safeInboundCLCallback(func(InboundCLFingerprint) { panic("crafted callback panic") }, InboundCLFingerprint{})
	returned = true
	if !returned {
		t.Fatal("callback panic escaped recovery boundary")
	}
}

func TestCLFingerprinterSupportsMplexOnlyPeer(t *testing.T) {
	var privateIP net.IP
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range addrs {
		ip, _, _ := net.ParseCIDR(addr.String())
		if ip != nil && ip.To4() != nil && ip.IsPrivate() && !ip.IsLoopback() {
			privateIP = ip
			break
		}
	}
	if privateIP == nil {
		t.Skip("no non-loopback private IPv4 address for isolated integration test")
	}
	key, err := ethcrypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	lpriv, err := libp2pcrypto.UnmarshalSecp256k1PrivateKey(ethcrypto.FromECDSA(key))
	if err != nil {
		t.Fatal(err)
	}
	remote, err := libp2p.New(
		libp2p.Identity(lpriv),
		libp2p.ListenAddrStrings("/ip4/0.0.0.0/tcp/0"),
		libp2p.Security(noise.ID, noise.New),
		libp2p.Muxer(mplex.ID, mplex.DefaultTransport),
		libp2p.UserAgent("teku/teku/v26.7.1/linux-x86_64/-eclipseadoptium-openjdk64bitservervm-java-26"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()

	portText, err := remote.Addrs()[0].ValueForProtocol(ma.P_TCP)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	db, err := enode.OpenDB("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	local := enode.NewLocalNode(db, key)
	local.SetStaticIP(privateIP)
	local.Set(enr.TCP(port))

	fingerprinter, err := NewCLFingerprinterWithLimits(5*time.Second, nil, true, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer fingerprinter.Close()
	fp, err := fingerprinter.Probe(context.Background(), local.Node())
	if err != nil {
		t.Fatal(err)
	}
	if fp.Client != "Teku" || fp.Version != "v26.7.1" || fp.OS != "linux/x86_64" || fp.Lang != "java26" {
		t.Fatalf("unexpected fingerprint: %+v", fp)
	}
}

func TestInboundCLNodeRequiresAdvertisedAddressMatchingSource(t *testing.T) {
	key, err := ethcrypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	remote := ma.StringCast("/ip4/10.0.0.1/tcp/49152")
	matching := ma.StringCast("/ip4/10.0.0.1/tcp/9000")
	mismatch := ma.StringCast("/ip4/10.0.0.2/tcp/9000")
	if got := inboundCLNode(&key.PublicKey, remote, []ma.Multiaddr{matching}, false); got != nil {
		t.Fatalf("private endpoint accepted without devnet policy: %v", got)
	}
	got := inboundCLNode(&key.PublicKey, remote, []ma.Multiaddr{mismatch, matching}, true)
	if got == nil || got.TCP() != 9000 || !got.IP().Equal(net.ParseIP("10.0.0.1")) || got.UDP() != 0 {
		t.Fatalf("inbound CL node = %v", got)
	}
	if got := inboundCLNode(&key.PublicKey, remote, []ma.Multiaddr{mismatch}, true); got != nil {
		t.Fatalf("mismatched advertised address accepted: %v", got)
	}
}

func TestCLStatusExchangeClassifiesForkDigest(t *testing.T) {
	testIP := privateTestIPv4(t)
	listen := "/ip4/" + testIP.String() + "/tcp/0"
	server, err := NewCLFingerprinterWithLimits(2*time.Second, nil, true, 32, listen)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := NewCLFingerprinterWithLimits(2*time.Second, nil, true, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	entry, err := netconf.CurrentCLForkENR("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	serveCLStatus(server, entry)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.host.Connect(ctx, peer.AddrInfo{ID: server.host.ID(), Addrs: server.host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	digest, err := client.exchangeStatus(ctx, server.host.ID(), entry)
	if err != nil {
		t.Fatal(err)
	}
	if network := netconf.ClassifyCL(digest); network != "mainnet" {
		t.Fatalf("digest %x classified as %q", digest, network)
	}
}

func TestCLInboundWatcherIgnoresOutboundIdentification(t *testing.T) {
	testIP := privateTestIPv4(t)
	listen := "/ip4/" + testIP.String() + "/tcp/0"
	remote, err := NewCLFingerprinterWithLimits(time.Second, nil, true, 32, listen)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	local, err := NewCLFingerprinterWithLimits(time.Second, nil, true, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()

	entry, err := netconf.CurrentCLForkENR("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	serveCLStatus(remote, entry)

	identified, err := local.host.EventBus().Subscribe(new(event.EvtPeerIdentificationCompleted))
	if err != nil {
		t.Fatal(err)
	}
	defer identified.Close()
	callbacks := make(chan InboundCLFingerprint, 1)
	if err := local.WatchInbound(entry, func(result InboundCLFingerprint) { callbacks <- result }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := local.host.Connect(ctx, peer.AddrInfo{ID: remote.host.ID(), Addrs: remote.host.Addrs()}); err != nil {
		t.Fatal(err)
	}

	select {
	case raw := <-identified.Out():
		evt := raw.(event.EvtPeerIdentificationCompleted)
		if evt.Conn == nil || evt.Conn.Stat().Direction != libp2pnetwork.DirOutbound {
			t.Fatalf("identify direction = %v, want outbound", evt.Conn)
		}
	case <-ctx.Done():
		t.Fatal("outbound identify did not complete")
	}
	select {
	case result := <-callbacks:
		t.Fatalf("outbound connection reached inbound callback: %+v", result)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestCLInboundWatcherAcceptsInboundIdentification(t *testing.T) {
	testIP := privateTestIPv4(t)
	listen := "/ip4/" + testIP.String() + "/tcp/0"
	local, err := NewCLFingerprinterWithLimits(time.Second, nil, true, 32, listen)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	remote, err := NewCLFingerprinterWithLimits(time.Second, nil, true, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()

	entry, err := netconf.CurrentCLForkENR("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	serveCLStatus(remote, entry)
	callbacks := make(chan InboundCLFingerprint, 1)
	if err := local.WatchInbound(entry, func(result InboundCLFingerprint) { callbacks <- result }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := remote.host.Connect(ctx, peer.AddrInfo{ID: local.host.ID(), Addrs: local.host.Addrs()}); err != nil {
		t.Fatal(err)
	}

	select {
	case result := <-callbacks:
		if result.NodeID == (enode.ID{}) || result.Fingerprint.Network != "mainnet" {
			t.Fatalf("inbound fingerprint = %+v", result)
		}
	case <-ctx.Done():
		t.Fatal("inbound identify did not reach callback")
	}
}

func serveCLStatus(server *CLFingerprinter, entry []byte) {
	server.host.SetStreamHandler(statusProtocolV2, func(s libp2pnetwork.Stream) {
		defer s.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(s, 1024))
		payload := make([]byte, statusV2SSZLen)
		copy(payload, entry[:4])
		var response bytes.Buffer
		response.WriteByte(0)
		var prefix [binary.MaxVarintLen64]byte
		response.Write(prefix[:binary.PutUvarint(prefix[:], statusV2SSZLen)])
		writer := snappy.NewBufferedWriter(&response)
		_, _ = writer.Write(payload)
		_ = writer.Close()
		_, _ = s.Write(response.Bytes())
	})
}

// Close must not return while an inbound identification handler is still running: the handler
// writes a fingerprint into the caller's nodeset, and the crawler closes before its final publish.
func TestCLFingerprinterCloseWaitsForInboundHandlers(t *testing.T) {
	listen := "/ip4/127.0.0.1/tcp/0"
	server, err := NewCLFingerprinterWithLimits(2*time.Second, nil, true, 32, listen)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var finished atomic.Bool
	if err := server.WatchInbound([]byte{1, 2, 3, 4}, func(InboundCLFingerprint) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		finished.Store(true)
	}); err != nil {
		server.Close()
		t.Fatal(err)
	}

	client, err := NewCLFingerprinterWithLimits(2*time.Second, nil, true, 32)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.host.Connect(ctx, peer.AddrInfo{ID: server.host.ID(), Addrs: server.host.Addrs()}); err != nil {
		close(release)
		server.Close()
		t.Fatalf("connect: %v", err)
	}

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(release)
		server.Close()
		t.Fatal("inbound handler never ran")
	}

	closed := make(chan struct{})
	go func() { _ = server.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned while an inbound handler was still in flight")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return after the handler finished")
	}
	if !finished.Load() {
		t.Fatal("Close returned before the handler completed")
	}
}
