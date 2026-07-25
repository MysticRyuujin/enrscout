package enrich

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/p2p/rlpx"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/golang/snappy"

	"github.com/MysticRyuujin/enrscout/internal/clientname"
	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/netpolicy"
)

const (
	handshakeMsg      = 0x00
	discMsg           = 0x01
	baseProtoVersion  = 5
	fingerprintName   = "enrscout"
	maxHandshakeBytes = 2048
	ethStatusMsg      = 0x10
	maxStatusBytes    = 2048
	minEthVersion     = 66
	maxEthVersion     = 72
	// eth/69 replaced the TD Status with the block-range format; eth/70-72 change only non-Status messages, so the range format still applies.
	ethRangeStatusVersion = 69
)

func ethCaps() []p2pCap {
	caps := make([]p2pCap, 0, maxEthVersion-minEthVersion+2)
	for v := maxEthVersion; v >= minEthVersion; v-- {
		caps = append(caps, p2pCap{"eth", uint(v)})
	}
	return append(caps, p2pCap{"snap", 1})
}

type p2pCap struct {
	Name    string
	Version uint
}

type protoHandshake struct {
	Version    uint64
	Name       string
	Caps       []p2pCap
	ListenPort uint64
	ID         []byte
	Rest       []rlp.RawValue `rlp:"tail"`
}

type Fingerprint struct {
	Client   string
	Version  string
	OS       string
	Lang     string
	Caps     string
	Network  string
	ForkHash string
	ForkID   forkid.ID
}

// InboundFingerprint is emitted for every accepted passive RLPx connection.
// NodeID is available once the encrypted handshake authenticates the peer; it
// remains zero for failures that happen before authentication.
type InboundFingerprint struct {
	NodeID      enode.ID
	Node        *enode.Node
	Fingerprint Fingerprint
	Err         error
	Duration    time.Duration
}

type Fingerprinter struct {
	priv         *ecdsa.PrivateKey
	id           []byte
	timeout      time.Duration
	sem          chan struct{}
	allowPrivate bool
}

// maxInflight bounds concurrent rlpx reads: the rlpx buffer allocates the full peer-controlled frame (~16 MiB) before we can reject it.

// NewFingerprinterWithPolicy permits private dial targets only for an
// explicitly isolated devnet.
func NewFingerprinterWithPolicy(timeout time.Duration, maxInflight int, key *ecdsa.PrivateKey, allowPrivate bool) (*Fingerprinter, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("fingerprint timeout must be positive, got %s", timeout)
	}
	if maxInflight < 1 {
		return nil, fmt.Errorf("max in-flight RLPx reads must be at least 1, got %d", maxInflight)
	}
	if key == nil {
		var err error
		if key, err = crypto.GenerateKey(); err != nil {
			return nil, err
		}
	}
	return &Fingerprinter{
		priv:         key,
		id:           crypto.FromECDSAPub(&key.PublicKey)[1:],
		timeout:      timeout,
		sem:          make(chan struct{}, maxInflight),
		allowPrivate: allowPrivate,
	}, nil
}

// WithIdentity creates another passive RLPx endpoint with its own authenticated
// identity while retaining the process-wide frame-read budget and policy.
func (f *Fingerprinter) WithIdentity(key *ecdsa.PrivateKey) (*Fingerprinter, error) {
	if key == nil {
		return nil, fmt.Errorf("RLPx identity key is required")
	}
	return &Fingerprinter{
		priv:         key,
		id:           crypto.FromECDSAPub(&key.PublicKey)[1:],
		timeout:      f.timeout,
		sem:          f.sem,
		allowPrivate: f.allowPrivate,
	}, nil
}

func (f *Fingerprinter) Probe(ctx context.Context, n *enode.Node) (Fingerprint, error) {
	return f.probe(ctx, n, nil)
}

// ProbeStatus additionally performs the eth Status exchange so the caller can
// verify network membership and retain the peer's live fork ID.
func (f *Fingerprinter) ProbeStatus(ctx context.Context, n *enode.Node, local *netconf.Network) (Fingerprint, error) {
	if local == nil {
		return Fingerprint{}, atProbeStage("eth_status", errors.New("local network is required"))
	}
	return f.probe(ctx, n, local)
}

func (f *Fingerprinter) probe(ctx context.Context, n *enode.Node, local *netconf.Network) (Fingerprint, error) {
	var fp Fingerprint
	endpoints := rlpxEndpointsWithPolicy(n, f.allowPrivate)
	if len(endpoints) == 0 {
		return fp, atProbeStage("endpoint", fmt.Errorf("node %s has no TCP endpoint", n.ID()))
	}
	pub := n.Pubkey()
	if pub == nil {
		return fp, atProbeStage("identity", fmt.Errorf("node %s has no public key", n.ID()))
	}

	d := net.Dialer{Timeout: f.timeout}
	var raw net.Conn
	var err error
	for _, endpoint := range endpoints {
		raw, err = d.DialContext(ctx, "tcp", endpoint)
		if err == nil {
			break
		}
	}
	if raw == nil {
		return fp, atProbeStage("dial", err)
	}
	defer raw.Close()

	conn := rlpx.NewConn(raw, pub)
	_ = raw.SetDeadline(time.Now().Add(f.timeout))
	if _, err := conn.Handshake(f.priv); err != nil {
		return fp, atProbeStage("rlpx_handshake", err)
	}

	if err := f.writeHello(conn); err != nil {
		return fp, err
	}

	if f.sem != nil {
		select {
		case f.sem <- struct{}{}:
			defer func() { <-f.sem }()
		case <-ctx.Done():
			return fp, atProbeStage("read_capacity", ctx.Err())
		}
		// Queuing for a slot burned read budget; re-arm so waiting doesn't fail healthy peers.
		_ = raw.SetDeadline(time.Now().Add(f.timeout))
	}
	fp, their, err := readHelloDetails(conn)
	if err != nil {
		return fp, err
	}
	if local == nil {
		return fp, nil
	}
	conn.SetSnappy(their.Version >= baseProtoVersion)
	_ = raw.SetDeadline(time.Now().Add(f.timeout))
	if err := exchangeEthStatus(conn, their.Caps, local, &fp); err != nil {
		return fp, err
	}
	return fp, nil
}

// InboundListener is a running passive RLPx endpoint. Close stops accepting and waits for the
// handlers already in flight, so a caller that must observe every callback before snapshotting -
// the crawler's final publish - can rely on Close returning after the last one.
type InboundListener struct {
	ln       net.Listener
	accepted sync.WaitGroup
	loop     sync.WaitGroup
	once     sync.Once
}

func (l *InboundListener) Addr() net.Addr { return l.ln.Addr() }

// Close is safe to call concurrently and more than once. Closing the listener ends the accept
// loop, which is the only producer of accepted handlers, so waiting on it before the handler group
// avoids the Add-racing-Wait hazard.
func (l *InboundListener) Close() error {
	var err error
	l.once.Do(func() {
		err = l.ln.Close()
		l.loop.Wait()
		l.accepted.Wait()
	})
	return err
}

// ListenInbound passively fingerprints authenticated peers that dial the
// crawler's advertised RLPx endpoint. The shared semaphore bounds all inbound
// handshakes and outbound frame reads; excess inbound connections are closed
// immediately instead of creating unbounded goroutines or frame buffers.
func (f *Fingerprinter) ListenInbound(ctx context.Context, addr string, local *netconf.Network, onResult func(InboundFingerprint)) (*InboundListener, error) {
	if onResult == nil {
		return nil, fmt.Errorf("inbound fingerprint callback is required")
	}
	if local == nil {
		return nil, fmt.Errorf("inbound fingerprint network is required")
	}
	network := "tcp"
	if host, _, splitErr := net.SplitHostPort(addr); splitErr == nil {
		if ip := net.ParseIP(host); ip != nil {
			if ip.To4() != nil {
				network = "tcp4"
			} else {
				network = "tcp6"
			}
		}
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		return nil, err
	}
	listener := &InboundListener{ln: ln}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	listener.loop.Add(1)
	go func() {
		defer listener.loop.Done()
		for {
			raw, err := ln.Accept()
			if err != nil {
				if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
					return
				}
				// A transient error (e.g. EMFILE) must not end inbound fingerprinting for the advertiser's lifetime.
				slog.Warn("inbound RLPx accept failed; retrying", "addr", addr, "err", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
				continue
			}
			start := time.Now()
			select {
			case f.sem <- struct{}{}:
				listener.accepted.Add(1)
				go func() {
					defer listener.accepted.Done()
					defer func() { <-f.sem }()
					safeInboundCallback(onResult, f.handleInbound(raw, start, local))
				}()
			default:
				_ = raw.Close()
				safeInboundCallback(onResult, InboundFingerprint{
					Err:      atProbeStage("read_capacity", fmt.Errorf("inbound RLPx capacity exhausted")),
					Duration: time.Since(start),
				})
			}
		}
	}()
	return listener, nil
}

func safeInboundCallback(callback func(InboundFingerprint), result InboundFingerprint) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("panic in inbound RLPx callback", "panic", recovered)
		}
	}()
	callback(result)
}

func (f *Fingerprinter) handleInbound(raw net.Conn, start time.Time, local *netconf.Network) (result InboundFingerprint) {
	defer raw.Close()
	defer func() { result.Duration = time.Since(start) }()
	_ = raw.SetDeadline(time.Now().Add(f.timeout))
	conn := rlpx.NewConn(raw, nil)
	pub, err := conn.Handshake(f.priv)
	if err != nil {
		result.Err = atProbeStage("rlpx_handshake", err)
		return
	}
	result.NodeID = enode.PubkeyToIDV4(pub)
	if err := f.writeHello(conn); err != nil {
		result.Err = err
		return
	}
	var their *protoHandshake
	result.Fingerprint, their, result.Err = readHelloDetails(conn)
	if result.Err != nil {
		return
	}
	result.Node = inboundRLPxNode(pub, raw.RemoteAddr(), their.ListenPort, f.allowPrivate)
	conn.SetSnappy(their.Version >= baseProtoVersion)
	_ = raw.SetDeadline(time.Now().Add(f.timeout))
	result.Err = exchangeEthStatus(conn, their.Caps, local, &result.Fingerprint)
	return
}

func inboundRLPxNode(pub *ecdsa.PublicKey, remote net.Addr, listenPort uint64, allowPrivate bool) *enode.Node {
	if pub == nil || listenPort == 0 || listenPort > 65535 {
		return nil
	}
	tcpAddr, ok := remote.(*net.TCPAddr)
	if !ok || !netpolicy.Usable(tcpAddr.IP, allowPrivate) {
		return nil
	}
	return enode.NewV4(pub, tcpAddr.IP, int(listenPort), 0)
}

func (f *Fingerprinter) writeHello(conn *rlpx.Conn) error {
	hello := &protoHandshake{Version: baseProtoVersion, Name: fingerprintName, ID: f.id, Caps: ethCaps()}
	payload, err := rlp.EncodeToBytes(hello)
	if err != nil {
		return atProbeStage("hello_encode", err)
	}
	if _, err := conn.Write(handshakeMsg, payload); err != nil {
		return atProbeStage("hello_write", err)
	}
	return nil
}

func readHelloDetails(conn *rlpx.Conn) (Fingerprint, *protoHandshake, error) {
	var fp Fingerprint
	code, data, _, err := conn.Read()
	if err != nil {
		return fp, nil, atProbeStage("hello_read", err)
	}
	if code == discMsg {
		reason := decodeDisconnect(data)
		return fp, nil, atProbeStage("peer_disconnect", &peerDisconnectError{Reason: reason})
	}
	if code != handshakeMsg {
		return fp, nil, atProbeStage("hello_protocol", fmt.Errorf("unexpected first message code %d", code))
	}
	if len(data) > maxHandshakeBytes {
		return fp, nil, atProbeStage("hello_protocol", fmt.Errorf("oversized handshake (%d bytes)", len(data)))
	}
	var their protoHandshake
	if err := rlp.DecodeBytes(data, &their); err != nil {
		return fp, nil, atProbeStage("hello_decode", err)
	}

	fp.Client, fp.Version, fp.OS, fp.Lang = parseName(their.Name)
	fp.Client = clientname.ExecutionVersion(fp.Client, fp.Version)
	fp.Caps = formatCaps(their.Caps)
	return fp, &their, nil
}

type ethStatusLegacy struct {
	ProtocolVersion uint32
	NetworkID       uint64
	TD              *big.Int
	Head            common.Hash
	Genesis         common.Hash
	ForkID          forkid.ID
}

type ethStatusRange struct {
	ProtocolVersion uint32
	NetworkID       uint64
	Genesis         common.Hash
	ForkID          forkid.ID
	EarliestBlock   uint64
	LatestBlock     uint64
	LatestBlockHash common.Hash
}

func exchangeEthStatus(conn *rlpx.Conn, caps []p2pCap, local *netconf.Network, fp *Fingerprint) error {
	version := negotiatedEthVersion(caps)
	if version == 0 {
		return atProbeStage("eth_status", errors.New("peer has no mutually supported eth capability"))
	}
	genesis := local.GenesisHash()
	var status any
	if version >= ethRangeStatusVersion {
		status = &ethStatusRange{
			ProtocolVersion: uint32(version), NetworkID: local.NetworkID, Genesis: genesis,
			ForkID: local.CurrentForkID(), LatestBlockHash: genesis,
		}
	} else {
		status = &ethStatusLegacy{
			ProtocolVersion: uint32(version), NetworkID: local.NetworkID, TD: local.GenesisDifficulty(),
			Head: genesis, Genesis: genesis, ForkID: local.CurrentForkID(),
		}
	}
	payload, err := rlp.EncodeToBytes(status)
	if err != nil {
		return atProbeStage("eth_status_encode", err)
	}
	if _, err := conn.Write(ethStatusMsg, payload); err != nil {
		return atProbeStage("eth_status_write", err)
	}
	code, data, _, err := conn.Read()
	if err != nil {
		return atProbeStage("eth_status_read", err)
	}
	if code == discMsg {
		return atProbeStage("peer_disconnect", &peerDisconnectError{Reason: decodeDisconnect(data)})
	}
	if code != ethStatusMsg {
		return atProbeStage("eth_status_protocol", fmt.Errorf("unexpected status message code %d", code))
	}
	if len(data) > maxStatusBytes {
		return atProbeStage("eth_status_protocol", fmt.Errorf("oversized status (%d bytes)", len(data)))
	}
	if err := decodeEthStatus(data, version, fp); err != nil {
		return err
	}
	if fp.Network == "" {
		return atProbeStage("eth_status_classify", errors.New("unsupported execution network"))
	}
	return nil
}

type ethStatus interface {
	fields() (protocolVersion uint32, networkID uint64, genesis common.Hash, fork forkid.ID)
}

func (s *ethStatusRange) fields() (uint32, uint64, common.Hash, forkid.ID) {
	return s.ProtocolVersion, s.NetworkID, s.Genesis, s.ForkID
}

func (s *ethStatusLegacy) fields() (uint32, uint64, common.Hash, forkid.ID) {
	return s.ProtocolVersion, s.NetworkID, s.Genesis, s.ForkID
}

func decodeEthStatus(data []byte, version uint, fp *Fingerprint) error {
	var status ethStatus = &ethStatusLegacy{}
	if version >= ethRangeStatusVersion {
		status = &ethStatusRange{}
	}
	if err := rlp.DecodeBytes(data, status); err != nil {
		return atProbeStage("eth_status_decode", err)
	}
	protocolVersion, networkID, genesis, fork := status.fields()
	fp.Network = netconf.ClassifyStatus(networkID, genesis)
	fp.ForkID = fork
	if uint(protocolVersion) != version {
		return atProbeStage("eth_status_protocol", fmt.Errorf("status protocol version %d does not match negotiated eth/%d", protocolVersion, version))
	}
	return nil
}

func negotiatedEthVersion(caps []p2pCap) uint {
	var best uint
	for _, cap := range caps {
		if cap.Name == "eth" && cap.Version >= minEthVersion && cap.Version <= maxEthVersion && cap.Version > best {
			best = cap.Version
		}
	}
	return best
}

type peerDisconnectError struct {
	Reason p2p.DiscReason
}

func (e *peerDisconnectError) Error() string {
	return fmt.Sprintf("peer disconnected before hello: %s", e.Reason)
}

// decodeDisconnect accepts the list payload, the legacy scalar encoding, and
// snappy-compressed forms (ethrex compresses pre-Hello disconnects). Compressed
// must be tried first: it also parses as plain RLP, with the snappy length
// varint mimicking a bogus reason code.
func decodeDisconnect(data []byte) p2p.DiscReason {
	// Bound before decoding: snappy.Decode allocates the peer-supplied decoded length
	// (up to 4 GiB) before it can fail, and this path runs before any size check.
	if n, err := snappy.DecodedLen(data); err == nil && n <= maxHandshakeBytes {
		if dec, err := snappy.Decode(nil, data); err == nil {
			if reason, ok := parseDisconnect(dec); ok {
				return reason
			}
		}
	}
	if reason, ok := parseDisconnect(data); ok {
		return reason
	}
	return p2p.DiscInvalid
}

func parseDisconnect(data []byte) (p2p.DiscReason, bool) {
	s := rlp.NewStream(bytes.NewReader(data), 100)
	kind, _, err := s.Kind()
	if err != nil {
		return p2p.DiscInvalid, false
	}
	if kind == rlp.List {
		if _, err := s.List(); err != nil {
			return p2p.DiscInvalid, false
		}
	}
	var reason p2p.DiscReason
	if err := s.Decode(&reason); err != nil {
		return p2p.DiscInvalid, false
	}
	return reason, true
}

// ProbeDisconnectReason returns a bounded label for a decoded devp2p
// disconnect, or an empty string when the failure was not a disconnect.
func ProbeDisconnectReason(err error) string {
	var de *peerDisconnectError
	if !errors.As(err, &de) {
		return ""
	}
	switch de.Reason {
	case p2p.DiscRequested:
		return "requested"
	case p2p.DiscNetworkError:
		return "network_error"
	case p2p.DiscProtocolError:
		return "protocol_error"
	case p2p.DiscUselessPeer:
		return "useless_peer"
	case p2p.DiscTooManyPeers:
		return "too_many_peers"
	case p2p.DiscAlreadyConnected:
		return "already_connected"
	case p2p.DiscIncompatibleVersion:
		return "incompatible_version"
	case p2p.DiscInvalidIdentity:
		return "invalid_identity"
	case p2p.DiscQuitting:
		return "quitting"
	case p2p.DiscUnexpectedIdentity:
		return "unexpected_identity"
	case p2p.DiscSelf:
		return "self"
	case p2p.DiscReadTimeout:
		return "read_timeout"
	case p2p.DiscSubprotocolError:
		return "subprotocol_error"
	case p2p.DiscInvalid:
		return "invalid"
	default:
		return "unknown"
	}
}

func rlpxEndpointsWithPolicy(n *enode.Node, allowPrivate bool) []string {
	var ip4 enr.IPv4
	var ip6 enr.IPv6
	var tcp enr.TCP
	var tcp6 enr.TCP6
	hasIP4 := n.Load(&ip4) == nil
	hasIP6 := n.Load(&ip6) == nil
	hasTCP := n.Load(&tcp) == nil && tcp > 0
	hasTCP6 := n.Load(&tcp6) == nil && tcp6 > 0
	if !hasTCP6 && hasTCP {
		tcp6 = enr.TCP6(tcp)
		hasTCP6 = true
	}
	var out []string
	if hasIP4 && hasTCP && netpolicy.Usable(net.IP(ip4[:]), allowPrivate) {
		out = append(out, net.JoinHostPort(net.IP(ip4[:]).String(), strconv.Itoa(int(tcp))))
	}
	if hasIP6 && hasTCP6 && netpolicy.Usable(net.IP(ip6[:]), allowPrivate) {
		out = append(out, net.JoinHostPort(net.IP(ip6[:]).String(), strconv.Itoa(int(tcp6))))
	}
	return out
}

func parseName(name string) (client, version, os, lang string) {
	parts := strings.Split(name, "/")
	client = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		version = strings.TrimSpace(parts[1])
	}
	if len(parts) > 2 {
		os = NormalizeOS(parts[2])
	}
	if len(parts) > 3 {
		lang = strings.TrimSpace(parts[3])
	}
	return client, version, os, lang
}

// NormalizeOS folds the many os/arch spellings clients report into "os/arch".
func NormalizeOS(raw string) string {
	l := strings.ToLower(strings.TrimSpace(raw))
	if l == "" {
		return ""
	}
	var os string
	switch {
	case strings.Contains(l, "linux"):
		os = "linux"
	case strings.Contains(l, "windows"):
		os = "windows"
	case strings.Contains(l, "darwin"), strings.Contains(l, "macos"), strings.Contains(l, "apple"):
		os = "darwin"
	case strings.Contains(l, "freebsd"):
		os = "freebsd"
	}
	var arch string
	switch {
	case strings.Contains(l, "aarch"), strings.Contains(l, "arm64"), strings.Contains(l, "arm"):
		arch = "arm64"
	case strings.Contains(l, "amd64"), strings.Contains(l, "x86_64"), strings.Contains(l, "x64"):
		arch = "x86_64"
	}
	switch {
	case os == "" && arch == "":
		return ""
	case arch == "":
		return os
	case os == "":
		return arch
	default:
		return os + "/" + arch
	}
}

func formatCaps(caps []p2pCap) string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, fmt.Sprintf("%s/%d", c.Name, c.Version))
	}
	return strings.Join(out, ",")
}
