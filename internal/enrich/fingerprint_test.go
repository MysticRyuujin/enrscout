package enrich

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/p2p/rlpx"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
)

func TestProbeFailureKindIsBounded(t *testing.T) {
	underlying := errors.New("peer-controlled detail")
	err := atProbeStage("hello_read", underlying)
	if got := ProbeFailureKind(err); got != "hello_read" {
		t.Fatalf("kind = %q, want hello_read", got)
	}
	if !errors.Is(err, underlying) {
		t.Fatal("probe stage did not preserve wrapped error")
	}
	if got := ProbeFailureKind(underlying); got != "unknown" {
		t.Fatalf("unclassified kind = %q, want unknown", got)
	}
}

func TestSafeInboundCallbackRecoversPanic(t *testing.T) {
	returned := false
	func() {
		safeInboundCallback(func(InboundFingerprint) { panic("crafted callback panic") }, InboundFingerprint{})
		returned = true
	}()
	if !returned {
		t.Fatal("callback panic escaped recovery boundary")
	}
}

func TestFingerprinterRequiresResourceBounds(t *testing.T) {
	if _, err := NewFingerprinterWithPolicy(0, 1, nil, false); err == nil {
		t.Fatal("zero timeout accepted")
	}
	if _, err := NewFingerprinterWithPolicy(time.Second, 0, nil, false); err == nil {
		t.Fatal("unbounded RLPx reads accepted")
	}
	if _, err := NewCLFingerprinterWithLimits(0, nil, false, 32); err == nil {
		t.Fatal("zero CL timeout accepted")
	}
}

func TestDisconnectReasonDecoding(t *testing.T) {
	for name, value := range map[string]any{
		"list":   []p2p.DiscReason{p2p.DiscTooManyPeers},
		"legacy": p2p.DiscTooManyPeers,
	} {
		t.Run(name, func(t *testing.T) {
			payload, err := rlp.EncodeToBytes(value)
			if err != nil {
				t.Fatal(err)
			}
			if got := decodeDisconnect(payload); got != p2p.DiscTooManyPeers {
				t.Fatalf("decodeDisconnect = %v, want %v", got, p2p.DiscTooManyPeers)
			}
		})
	}
	// 0204c104: ethrex's snappy-compressed pre-Hello too-many-peers rejection, captured on the wire.
	if got := decodeDisconnect([]byte{0x02, 0x04, 0xc1, 0x04}); got != p2p.DiscTooManyPeers {
		t.Fatalf("compressed payload = %v, want %v", got, p2p.DiscTooManyPeers)
	}
	if got := decodeDisconnect([]byte{0xff}); got != p2p.DiscInvalid {
		t.Fatalf("invalid payload = %v, want %v", got, p2p.DiscInvalid)
	}
	err := atProbeStage("peer_disconnect", &peerDisconnectError{Reason: p2p.DiscAlreadyConnected})
	if got := ProbeFailureKind(err); got != "peer_disconnect" {
		t.Fatalf("failure kind = %q, want peer_disconnect", got)
	}
	if got := ProbeDisconnectReason(err); got != "already_connected" {
		t.Fatalf("disconnect reason = %q, want already_connected", got)
	}
	if got := ProbeDisconnectReason(errors.New("other")); got != "" {
		t.Fatalf("non-disconnect reason = %q, want empty", got)
	}
}

func TestPassiveInboundRLPxFingerprint(t *testing.T) {
	const testTimeout = 5 * time.Second
	server, err := NewFingerprinterWithPolicy(testTimeout, 2, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan InboundFingerprint, 1)
	mainnet, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	testIP := privateTestIPv4(t)
	ln, err := server.ListenInbound(ctx, net.JoinHostPort(testIP.String(), "0"), mainnet, func(result InboundFingerprint) {
		results <- result
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if tcpAddr, ok := ln.Addr().(*net.TCPAddr); !ok || tcpAddr.IP.To4() == nil {
		t.Fatalf("listener address = %v, want IPv4", ln.Addr())
	}

	clientKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.DialTimeout("tcp", ln.Addr().String(), testTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(testTimeout))
	conn := rlpx.NewConn(raw, &server.priv.PublicKey)
	if _, err := conn.Handshake(clientKey); err != nil {
		t.Fatal(err)
	}
	payload, err := rlp.EncodeToBytes(&protoHandshake{
		Version:    baseProtoVersion,
		Name:       "Nethermind/v1.34.0/linux-x64/dotnet9.0.8",
		Caps:       []p2pCap{{"eth", 69}},
		ListenPort: 30303,
		ID:         crypto.FromECDSAPub(&clientKey.PublicKey)[1:],
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(handshakeMsg, payload); err != nil {
		t.Fatal(err)
	}
	if code, _, _, err := conn.Read(); err != nil || code != handshakeMsg {
		t.Fatalf("server hello: code=%d err=%v", code, err)
	}
	conn.SetSnappy(true)
	if code, _, _, err := conn.Read(); err != nil || code != ethStatusMsg {
		t.Fatalf("server status: code=%d err=%v", code, err)
	}
	status, err := rlp.EncodeToBytes(&ethStatusRange{
		ProtocolVersion: 69,
		NetworkID:       mainnet.NetworkID,
		Genesis:         mainnet.GenesisHash(),
		ForkID:          mainnet.CurrentForkID(),
		LatestBlockHash: mainnet.GenesisHash(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(ethStatusMsg, status); err != nil {
		t.Fatal(err)
	}

	select {
	case result := <-results:
		if result.Err != nil {
			t.Fatal(result.Err)
		}
		if want := enode.PubkeyToIDV4(&clientKey.PublicKey); result.NodeID != want {
			t.Fatalf("node ID = %v, want %v", result.NodeID, want)
		}
		if result.Node == nil || result.Node.ID() != result.NodeID || !result.Node.IP().Equal(testIP) || result.Node.TCP() != 30303 || result.Node.UDP() != 0 {
			t.Fatalf("inbound node = %v", result.Node)
		}
		if result.Fingerprint.Client != "Nethermind" || result.Fingerprint.Version != "v1.34.0" || result.Fingerprint.OS != "linux/x86_64" {
			t.Fatalf("fingerprint = %+v", result.Fingerprint)
		}
		if result.Fingerprint.Network != "mainnet" {
			t.Fatalf("network = %q, want mainnet", result.Fingerprint.Network)
		}
		if result.Duration <= 0 {
			t.Fatalf("duration = %v, want positive", result.Duration)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for passive fingerprint")
	}
}

func TestInboundRLPxNodeRequiresUsableEndpoint(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	private := &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 40000}
	if got := inboundRLPxNode(&key.PublicKey, private, 30303, false); got != nil {
		t.Fatalf("private endpoint accepted without devnet policy: %v", got)
	}
	if got := inboundRLPxNode(&key.PublicKey, private, 30303, true); got == nil || got.TCP() != 30303 || got.UDP() != 0 {
		t.Fatalf("devnet endpoint = %v", got)
	}
	if got := inboundRLPxNode(&key.PublicKey, private, 0, true); got != nil {
		t.Fatalf("zero listen port accepted: %v", got)
	}
	if got := inboundRLPxNode(&key.PublicKey, private, 65536, true); got != nil {
		t.Fatalf("out-of-range listen port accepted: %v", got)
	}
}

func TestProbeStatusRoundTrip(t *testing.T) {
	mainnet, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	testIP := privateTestIPv4(t)
	ln, err := net.Listen("tcp4", net.JoinHostPort(testIP.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serverDone := make(chan error, 1)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(2 * time.Second))
		conn := rlpx.NewConn(raw, nil)
		if _, err := conn.Handshake(serverKey); err != nil {
			serverDone <- fmt.Errorf("handshake: %w", err)
			return
		}
		code, _, _, err := conn.Read()
		if err != nil || code != handshakeMsg {
			serverDone <- fmt.Errorf("client hello: code=%d err=%v", code, err)
			return
		}
		hello, err := rlp.EncodeToBytes(&protoHandshake{
			Version: baseProtoVersion,
			Name:    "Nethermind/v1.39.1/linux-x64/dotnet9.0.8",
			Caps:    []p2pCap{{"eth", 71}},
			ID:      crypto.FromECDSAPub(&serverKey.PublicKey)[1:],
		})
		if err != nil {
			serverDone <- err
			return
		}
		if _, err := conn.Write(handshakeMsg, hello); err != nil {
			serverDone <- fmt.Errorf("server hello: %w", err)
			return
		}
		conn.SetSnappy(true)
		code, data, _, err := conn.Read()
		if err != nil || code != ethStatusMsg {
			serverDone <- fmt.Errorf("client status: code=%d err=%v", code, err)
			return
		}
		var clientStatus ethStatusRange
		if err := rlp.DecodeBytes(data, &clientStatus); err != nil {
			serverDone <- fmt.Errorf("decode client status: %w", err)
			return
		}
		if clientStatus.ProtocolVersion != 71 || clientStatus.NetworkID != mainnet.NetworkID || clientStatus.Genesis != mainnet.GenesisHash() {
			serverDone <- fmt.Errorf("unexpected client status: %+v", clientStatus)
			return
		}
		status, err := rlp.EncodeToBytes(&ethStatusRange{
			ProtocolVersion: 71,
			NetworkID:       mainnet.NetworkID,
			Genesis:         mainnet.GenesisHash(),
			ForkID:          mainnet.CurrentForkID(),
			LatestBlockHash: mainnet.GenesisHash(),
		})
		if err != nil {
			serverDone <- err
			return
		}
		if _, err := conn.Write(ethStatusMsg, status); err != nil {
			serverDone <- fmt.Errorf("server status: %w", err)
			return
		}
		serverDone <- nil
	}()

	serverAddr := ln.Addr().(*net.TCPAddr)
	n := enode.NewV4(&serverKey.PublicKey, serverAddr.IP, serverAddr.Port, serverAddr.Port)
	client, err := NewFingerprinterWithPolicy(2*time.Second, 1, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := client.ProbeStatus(context.Background(), n, mainnet)
	if err != nil {
		t.Fatal(err)
	}
	if fp.Client != "Nethermind" || fp.Version != "v1.39.1" || fp.Network != "mainnet" || fp.ForkID != mainnet.CurrentForkID() {
		t.Fatalf("fingerprint = %+v", fp)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

// eth/66-68 negotiate the TD-carrying legacy Status rather than the block-range form;
// only decode was covered before, so a regression in the version split at
// exchangeEthStatus would break every legacy peer with the suite green.
func TestProbeStatusRoundTripLegacyEth66(t *testing.T) {
	mainnet, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	testIP := privateTestIPv4(t)
	ln, err := net.Listen("tcp4", net.JoinHostPort(testIP.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serverDone := make(chan error, 1)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(2 * time.Second))
		conn := rlpx.NewConn(raw, nil)
		if _, err := conn.Handshake(serverKey); err != nil {
			serverDone <- fmt.Errorf("handshake: %w", err)
			return
		}
		code, _, _, err := conn.Read()
		if err != nil || code != handshakeMsg {
			serverDone <- fmt.Errorf("client hello: code=%d err=%v", code, err)
			return
		}
		hello, err := rlp.EncodeToBytes(&protoHandshake{
			Version: baseProtoVersion,
			Name:    "Geth/v1.13.0-stable/linux-amd64/go1.21.0",
			Caps:    []p2pCap{{"eth", 66}},
			ID:      crypto.FromECDSAPub(&serverKey.PublicKey)[1:],
		})
		if err != nil {
			serverDone <- err
			return
		}
		if _, err := conn.Write(handshakeMsg, hello); err != nil {
			serverDone <- fmt.Errorf("server hello: %w", err)
			return
		}
		conn.SetSnappy(true)
		code, data, _, err := conn.Read()
		if err != nil || code != ethStatusMsg {
			serverDone <- fmt.Errorf("client status: code=%d err=%v", code, err)
			return
		}
		var clientStatus ethStatusLegacy
		if err := rlp.DecodeBytes(data, &clientStatus); err != nil {
			serverDone <- fmt.Errorf("decode client legacy status: %w", err)
			return
		}
		if clientStatus.ProtocolVersion != 66 || clientStatus.NetworkID != mainnet.NetworkID ||
			clientStatus.Genesis != mainnet.GenesisHash() || clientStatus.TD == nil {
			serverDone <- fmt.Errorf("unexpected client legacy status: %+v", clientStatus)
			return
		}
		status, err := rlp.EncodeToBytes(&ethStatusLegacy{
			ProtocolVersion: 66,
			NetworkID:       mainnet.NetworkID,
			TD:              big.NewInt(1),
			Head:            mainnet.GenesisHash(),
			Genesis:         mainnet.GenesisHash(),
			ForkID:          mainnet.CurrentForkID(),
		})
		if err != nil {
			serverDone <- err
			return
		}
		if _, err := conn.Write(ethStatusMsg, status); err != nil {
			serverDone <- fmt.Errorf("server status: %w", err)
			return
		}
		serverDone <- nil
	}()

	serverAddr := ln.Addr().(*net.TCPAddr)
	n := enode.NewV4(&serverKey.PublicKey, serverAddr.IP, serverAddr.Port, serverAddr.Port)
	client, err := NewFingerprinterWithPolicy(2*time.Second, 1, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := client.ProbeStatus(context.Background(), n, mainnet)
	if err != nil {
		t.Fatal(err)
	}
	if fp.Client != "Geth" || fp.Network != "mainnet" || fp.ForkID != mainnet.CurrentForkID() {
		t.Fatalf("fingerprint = %+v", fp)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func privateTestIPv4(t *testing.T) net.IP {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err == nil && ip.To4() != nil && ip.IsPrivate() {
			return ip
		}
	}
	t.Skip("no private IPv4 interface available for policy-compliant RLPx test")
	return nil
}

func TestWithIdentitySharesReadBudget(t *testing.T) {
	firstKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewFingerprinterWithPolicy(time.Second, 1, firstKey, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := first.WithIdentity(secondKey)
	if err != nil {
		t.Fatal(err)
	}
	if first.sem != second.sem {
		t.Fatal("identity did not retain the shared read budget")
	}
	if first.priv == second.priv {
		t.Fatal("identity key was not replaced")
	}
}

func TestDecodeEthStatusClassifiesLegacyAndRange(t *testing.T) {
	mainnet, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	fork := mainnet.CurrentForkID()
	cases := []struct {
		version uint
		status  any
	}{
		{68, &ethStatusLegacy{ProtocolVersion: 68, NetworkID: mainnet.NetworkID, TD: new(big.Int), Head: mainnet.GenesisHash(), Genesis: mainnet.GenesisHash(), ForkID: fork}},
		{71, &ethStatusRange{ProtocolVersion: 71, NetworkID: mainnet.NetworkID, Genesis: mainnet.GenesisHash(), ForkID: fork, LatestBlockHash: mainnet.GenesisHash()}},
	}
	for _, tc := range cases {
		data, err := rlp.EncodeToBytes(tc.status)
		if err != nil {
			t.Fatal(err)
		}
		var fp Fingerprint
		if err := decodeEthStatus(data, tc.version, &fp); err != nil {
			t.Fatal(err)
		}
		if fp.Network != "mainnet" || fp.ForkID != fork {
			t.Fatalf("decoded status = %+v", fp)
		}
	}
}

func TestDecodeEthStatusRejectsNegotiatedVersionMismatch(t *testing.T) {
	mainnet, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	data, err := rlp.EncodeToBytes(&ethStatusRange{
		ProtocolVersion: 70,
		NetworkID:       mainnet.NetworkID,
		Genesis:         mainnet.GenesisHash(),
		ForkID:          mainnet.CurrentForkID(),
		LatestBlockHash: mainnet.GenesisHash(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeEthStatus(data, 71, new(Fingerprint)); ProbeFailureKind(err) != "eth_status_protocol" {
		t.Fatalf("version mismatch error = %v, kind = %q", err, ProbeFailureKind(err))
	}
}

func TestNegotiatedEthVersion(t *testing.T) {
	if got := negotiatedEthVersion([]p2pCap{{"eth", 68}, {"snap", 1}, {"eth", 72}, {"eth", 73}}); got != 72 {
		t.Fatalf("negotiated version = %d, want 72", got)
	}
}

func TestEthCapsMatchNegotiationRange(t *testing.T) {
	caps := ethCaps()
	if got := negotiatedEthVersion(caps); got != maxEthVersion {
		t.Fatalf("negotiatedEthVersion(ethCaps()) = %d, want maxEthVersion %d", got, maxEthVersion)
	}
	var minSeen, maxSeen uint
	var snap bool
	for _, c := range caps {
		switch c.Name {
		case "eth":
			if minSeen == 0 || c.Version < minSeen {
				minSeen = c.Version
			}
			if c.Version > maxSeen {
				maxSeen = c.Version
			}
		case "snap":
			snap = true
		}
	}
	if minSeen != minEthVersion || maxSeen != maxEthVersion || !snap {
		t.Fatalf("ethCaps() range = eth/%d..%d snap=%v, want eth/%d..%d snap=true", minSeen, maxSeen, snap, minEthVersion, maxEthVersion)
	}
}

func TestRLPxEndpointsUsesFamilySpecificTCP6(t *testing.T) {
	var r enr.Record
	r.Set(enr.IPv4{1, 2, 3, 4})
	r.Set(enr.IPv6{0x26, 0x06, 0x47, 0x00, 0x47, 0x00, 0, 0, 0, 0, 0, 0, 0, 0, 0x11, 0x11})
	r.Set(enr.TCP6(30303))
	var id enode.ID
	id[0] = 1
	got := rlpxEndpointsWithPolicy(enode.SignNull(&r, id), false)
	want := net.JoinHostPort("2606:4700:4700::1111", "30303")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("rlpxEndpointsWithPolicy = %v, want [%s]", got, want)
	}
}

func TestRLPxEndpointsRejectsPrivateAddress(t *testing.T) {
	var r enr.Record
	r.Set(enr.IPv4{10, 0, 0, 1})
	r.Set(enr.TCP(30303))
	var id enode.ID
	id[0] = 2
	if got := rlpxEndpointsWithPolicy(enode.SignNull(&r, id), false); len(got) != 0 {
		t.Fatalf("private endpoints = %v, want none", got)
	}
	if got := rlpxEndpointsWithPolicy(enode.SignNull(&r, id), true); len(got) != 1 {
		t.Fatalf("isolated-devnet endpoints = %v, want one", got)
	}
}

func TestParseName(t *testing.T) {
	cases := []struct{ in, client, version, os, lang string }{
		{"Geth/v1.16.7-stable-b9f3a3d9/linux-amd64/go1.24", "Geth", "v1.16.7-stable-b9f3a3d9", "linux/x86_64", "go1.24"},
		{"erigon/v3.3.9/linux/go1.23", "erigon", "v3.3.9", "linux", "go1.23"},
		{"reth/v0.2.0", "reth", "v0.2.0", "", ""},
		{"solo", "solo", "", "", ""},
		{"", "", "", "", ""},
	}
	for _, c := range cases {
		client, version, os, lang := parseName(c.in)
		if client != c.client || version != c.version || os != c.os || lang != c.lang {
			t.Errorf("parseName(%q) = (%q,%q,%q,%q), want (%q,%q,%q,%q)", c.in, client, version, os, lang, c.client, c.version, c.os, c.lang)
		}
	}
}

func TestParseCLAgent(t *testing.T) {
	cases := []struct{ in, client, version, os, lang string }{
		{"teku/teku/v26.7.1/linux-x86_64/-eclipseadoptium-openjdk64bitservervm-java-26", "Teku", "v26.7.1", "linux/x86_64", "java26"},
		{"Nimbus/v26.7.0-4110bc-stateofus", "Nimbus", "v26.7.0-4110bc-stateofus", "", ""},
		{"lighthouse/v7.0.1/linux-x86_64", "Lighthouse", "v7.0.1", "linux/x86_64", ""},
		{"caplin/v3.2.1/linux-amd64", "Caplin", "v3.2.1", "linux/x86_64", ""},
		{"caplin/caplin/v3.5.2/linux-amd64/go1.25", "Caplin", "v3.5.2", "linux/x86_64", "go1.25"},
		{"erigon/caplin/v3.3.9/linux-amd64/go1.25", "Caplin", "v3.3.9", "linux/x86_64", "go1.25"},
	}
	for _, c := range cases {
		client, version, os, lang := parseCLAgent(c.in)
		if client != c.client || version != c.version || os != c.os || lang != c.lang {
			t.Errorf("parseCLAgent(%q) = (%q,%q,%q,%q), want (%q,%q,%q,%q)", c.in, client, version, os, lang, c.client, c.version, c.os, c.lang)
		}
	}
}

func TestNormalizeOS(t *testing.T) {
	cases := map[string]string{
		"linux-amd64":              "linux/x86_64",
		"linux-x64":                "linux/x86_64",
		"linux-x86_64":             "linux/x86_64",
		"x86_64-unknown-linux-gnu": "linux/x86_64",
		"linux-aarch_64":           "linux/arm64",
		"windows-amd64":            "windows/x86_64",
		"":                         "",
		"a0071826c5daf7dc3a6e768":  "",
	}
	for in, want := range cases {
		if got := NormalizeOS(in); got != want {
			t.Errorf("NormalizeOS(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatCaps(t *testing.T) {
	got := formatCaps([]p2pCap{{"eth", 68}, {"snap", 1}})
	if got != "eth/68,snap/1" {
		t.Errorf("formatCaps = %q, want eth/68,snap/1", got)
	}
	if formatCaps(nil) != "" {
		t.Errorf("formatCaps(nil) should be empty")
	}
}

// Close must not return while a handler is still running, or the crawler's final snapshot can be
// taken before an inbound identification has been recorded.
func TestInboundListenerCloseWaitsForHandlers(t *testing.T) {
	const testTimeout = 5 * time.Second
	// A bare TCP dial never completes the RLPx handshake, so the handler only reaches the callback
	// once the probe timeout fires; keep it short so the test does not wait on it.
	server, err := NewFingerprinterWithPolicy(200*time.Millisecond, 4, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	mainnet, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool
	ln, err := server.ListenInbound(ctx, net.JoinHostPort(privateTestIPv4(t).String(), "0"), mainnet,
		func(InboundFingerprint) {
			close(entered)
			<-release
			finished.Store(true)
		})
	if err != nil {
		t.Fatal(err)
	}

	raw, err := net.DialTimeout("tcp", ln.Addr().String(), testTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// Any peer reaching the callback is enough; a failed handshake still reports a result.
	select {
	case <-entered:
	case <-time.After(testTimeout):
		t.Fatal("handler never ran")
	}

	closed := make(chan struct{})
	go func() { _ = ln.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned while a handler was still in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(testTimeout):
		t.Fatal("Close did not return after the handler finished")
	}
	if !finished.Load() {
		t.Fatal("Close returned before the handler completed")
	}
}
