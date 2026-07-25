package enrich

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	ethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/golang/snappy"
	"github.com/libp2p/go-libp2p"
	mplex "github.com/libp2p/go-libp2p-mplex"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/host"
	libp2pnetwork "github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/muxer/yamux"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	ma "github.com/multiformats/go-multiaddr"

	"github.com/MysticRyuujin/enrscout/internal/clientname"
	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/netpolicy"
)

const agentVersionKey = "AgentVersion"

const (
	statusProtocolV1 protocol.ID = "/eth2/beacon_chain/req/status/1/ssz_snappy"
	statusProtocolV2 protocol.ID = "/eth2/beacon_chain/req/status/2/ssz_snappy"
)

const (
	statusV1SSZLen = 84
	statusV2SSZLen = 92
)

type CLFingerprinter struct {
	host         host.Host
	timeout      time.Duration
	sub          event.Subscription
	allowPrivate bool
	inboundSem   chan struct{}

	// inboundLoop is the event-subscription reader; inbound are the per-peer handlers it spawns.
	// They are separate so Close can retire the only producer of handler Adds before waiting.
	inboundLoop sync.WaitGroup
	inbound     sync.WaitGroup
	closeOnce   sync.Once
}

type InboundCLFingerprint struct {
	NodeID      enode.ID
	Node        *enode.Node
	Fingerprint Fingerprint
	Err         error
}

// key gives the prober its advertised beacon identity (Nimbus accepts it); listenAddrs make it dialable by clients that refuse outbound probes (Caplin).

func NewCLFingerprinterWithLimits(timeout time.Duration, key *ecdsa.PrivateKey, allowPrivate bool, inboundLimit int, listenAddrs ...string) (*CLFingerprinter, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("fingerprint timeout must be positive, got %s", timeout)
	}
	if inboundLimit < 1 {
		return nil, fmt.Errorf("inbound identification limit must be positive, got %d", inboundLimit)
	}
	opts := []libp2p.Option{
		libp2p.UserAgent("enrscout"),
		libp2p.DisableRelay(),
		libp2p.Security(noise.ID, noise.New),
		libp2p.Muxer(yamux.ID, yamux.DefaultTransport),
		// Ethereum consensus clients must support mplex (kept despite general libp2p deprecation).
		libp2p.Muxer(mplex.ID, mplex.DefaultTransport),
	}
	if len(listenAddrs) > 0 {
		opts = append(opts, libp2p.ListenAddrStrings(listenAddrs...))
	} else {
		opts = append(opts, libp2p.NoListenAddrs)
	}
	// Never fall back to go-libp2p's default ed25519 identity: Nimbus (libp2p_pki_schemes=secp256k1) rejects non-secp256k1 peers at the security handshake.
	if key == nil {
		var err error
		key, err = ethcrypto.GenerateKey()
		if err != nil {
			return nil, err
		}
	}
	priv, err := libp2pcrypto.UnmarshalSecp256k1PrivateKey(ethcrypto.FromECDSA(key))
	if err != nil {
		return nil, err
	}
	opts = append(opts, libp2p.Identity(priv))
	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, err
	}
	return &CLFingerprinter{host: h, timeout: timeout, allowPrivate: allowPrivate, inboundSem: make(chan struct{}, inboundLimit)}, nil
}

func (f *CLFingerprinter) PeerID() string { return f.host.ID().String() }

// ShareInboundBudget makes per-network CL advertisers share one process-wide inbound identification bound, like EL identities share their RLPx read budget; call before WatchInbound.
func (f *CLFingerprinter) ShareInboundBudget(with *CLFingerprinter) {
	if with != nil {
		f.inboundSem = with.inboundSem
	}
}

// WatchInbound reports the client of any peer that dials in; Caplin refuses
// outbound probes but identifies on connections it initiates. Unknown peers
// receive a network only after a consensus Status exchange.
func (f *CLFingerprinter) WatchInbound(localFork []byte, onFP func(InboundCLFingerprint)) error {
	if len(localFork) < 4 {
		return errors.New("inbound consensus fork entry is required")
	}
	if f.sub != nil {
		// A second subscription would orphan the first reader goroutine and hang Close.
		return errors.New("inbound watcher already running")
	}
	sub, err := f.host.EventBus().Subscribe(new(event.EvtPeerIdentificationCompleted))
	if err != nil {
		return err
	}
	f.sub = sub
	f.inboundLoop.Add(1)
	go func() {
		defer f.inboundLoop.Done()
		for e := range sub.Out() {
			evt := e.(event.EvtPeerIdentificationCompleted)
			if evt.Conn == nil || evt.Conn.Stat().Direction != libp2pnetwork.DirInbound {
				continue
			}
			select {
			case f.inboundSem <- struct{}{}:
				f.inbound.Add(1)
				go func() {
					defer f.inbound.Done()
					defer func() {
						<-f.inboundSem
						if recovered := recover(); recovered != nil {
							slog.Error("panic handling inbound libp2p identification", "panic", recovered)
						}
					}()
					f.handleInboundIdentification(evt, localFork, onFP)
				}()
			default:
				safeInboundCLCallback(onFP, InboundCLFingerprint{Err: atProbeStage("read_capacity", errors.New("inbound libp2p identification capacity exhausted"))})
			}
		}
	}()
	return nil
}

func (f *CLFingerprinter) handleInboundIdentification(evt event.EvtPeerIdentificationCompleted, localFork []byte, onFP func(InboundCLFingerprint)) {
	pid := evt.Peer
	pk, err := pid.ExtractPublicKey()
	if err != nil {
		return
	}
	raw, err := pk.Raw()
	if err != nil {
		return
	}
	pub, err := ethcrypto.DecompressPubkey(raw)
	if err != nil {
		return
	}
	av := evt.AgentVersion
	if av == "" {
		if v, err := f.host.Peerstore().Get(pid, agentVersionKey); err == nil {
			av, _ = v.(string)
		}
	}
	if av == "" {
		return
	}
	var fp Fingerprint
	fp.Client, fp.Version, fp.OS, fp.Lang = parseCLAgent(av)
	node := inboundCLNode(pub, evt.Conn.RemoteMultiaddr(), evt.ListenAddrs, f.allowPrivate)
	cctx, cancel := context.WithTimeout(context.Background(), f.timeout)
	defer cancel()
	if digest, err := f.exchangeStatus(cctx, pid, localFork); err == nil {
		fp.Network = netconf.ClassifyCL(digest)
		fp.ForkHash = fmt.Sprintf("%x", digest)
	}
	safeInboundCLCallback(onFP, InboundCLFingerprint{NodeID: enode.PubkeyToIDV4(pub), Node: node, Fingerprint: fp})
}

func safeInboundCLCallback(callback func(InboundCLFingerprint), result InboundCLFingerprint) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("panic in inbound libp2p callback", "panic", recovered)
		}
	}()
	callback(result)
}

// Close retires the inbound path before tearing the host down: closing the subscription ends the
// reader loop, which is the only producer of handler goroutines, so both waits are then safe. The
// handlers write fingerprints into the caller's nodeset, so returning early would let one land
// after a final snapshot.
func (f *CLFingerprinter) Close() error {
	var err error
	f.closeOnce.Do(func() {
		if f.sub != nil {
			f.sub.Close()
		}
		f.inboundLoop.Wait()
		f.inbound.Wait()
		err = f.host.Close()
	})
	return err
}

func (f *CLFingerprinter) Probe(ctx context.Context, n *enode.Node) (Fingerprint, error) {
	return f.probe(ctx, n, nil)
}

// ProbeStatus prefers localFork for the consensus Status request (falling back to the target's own eth2 entry) and retains the peer's fork digest.
func (f *CLFingerprinter) ProbeStatus(ctx context.Context, n *enode.Node, localFork []byte) (Fingerprint, error) {
	return f.probe(ctx, n, localFork)
}

// Status must not delay or fail an identify-only success beyond this grace.
const clStatusGrace = 2 * time.Second

type statusOutcome struct {
	digest [4]byte
	err    error
}

func (f *CLFingerprinter) probe(ctx context.Context, n *enode.Node, localFork []byte) (Fingerprint, error) {
	var fp Fingerprint
	pub := n.Pubkey()
	if pub == nil {
		return fp, atProbeStage("identity", fmt.Errorf("node %s has no public key", n.ID()))
	}
	addrs := libp2pAddrsWithPolicy(n, f.allowPrivate)
	if len(addrs) == 0 {
		return fp, atProbeStage("endpoint", fmt.Errorf("node %s has no libp2p endpoint", n.ID()))
	}
	lpk, err := libp2pcrypto.UnmarshalSecp256k1PublicKey(ethcrypto.CompressPubkey(pub))
	if err != nil {
		return fp, atProbeStage("identity", err)
	}
	pid, err := peer.IDFromPublicKey(lpk)
	if err != nil {
		return fp, atProbeStage("identity", err)
	}

	cctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	if err := f.host.Connect(cctx, peer.AddrInfo{ID: pid, Addrs: addrs}); err != nil {
		return fp, atProbeStage("libp2p_connect", err)
	}
	defer f.host.Network().ClosePeer(pid)

	if len(localFork) < 4 {
		var e2 netconf.Eth2Entry
		if n.Load(&e2) == nil && len(e2) >= 4 {
			localFork = e2
		}
	}
	statusCh := make(chan statusOutcome, 1)
	if len(localFork) >= 4 {
		go func() {
			digest, err := f.exchangeStatus(cctx, pid, localFork)
			statusCh <- statusOutcome{digest: digest, err: err}
		}()
	} else {
		statusCh <- statusOutcome{err: errors.New("no consensus fork entry for status request")}
	}

	for {
		if v, err := f.host.Peerstore().Get(pid, agentVersionKey); err == nil {
			if s, ok := v.(string); ok && s != "" {
				fp.Client, fp.Version, fp.OS, fp.Lang = parseCLAgent(s)
				f.awaitStatus(cctx, statusCh, &fp)
				return fp, nil
			}
		}
		select {
		case <-cctx.Done():
			return fp, atProbeStage("identify", errors.New("identify did not complete"))
		case <-time.After(120 * time.Millisecond):
		}
	}
}

func (f *CLFingerprinter) awaitStatus(ctx context.Context, statusCh <-chan statusOutcome, fp *Fingerprint) {
	select {
	case r := <-statusCh:
		if r.err == nil {
			fp.Network = netconf.ClassifyCL(r.digest)
			fp.ForkHash = fmt.Sprintf("%x", r.digest)
		}
	case <-time.After(clStatusGrace):
	case <-ctx.Done():
	}
}

func parseCLAgent(name string) (client, version, os, lang string) {
	client, version, os, lang = parseName(name)
	parts := strings.Split(name, "/")
	rawClient := strings.ToLower(client)
	client = clientname.Consensus(client)
	if len(parts) > 2 && clientname.ConsensusAgentHasNestedVersion(rawClient, parts[1]) {
		switch rawClient {
		case "erigon", "caplin":
			version = strings.TrimSpace(parts[2])
			os = ""
			if len(parts) > 3 {
				os = NormalizeOS(parts[3])
			}
			if len(parts) > 4 {
				lang = strings.TrimSpace(strings.Join(parts[4:], "/"))
			}
		case "teku":
			// Teku reports teku/teku/<version>/<os>/<jvm>, unlike the common
			// client/version/os/runtime convention.
			version = strings.TrimSpace(parts[2])
			os = ""
			if len(parts) > 3 {
				os = NormalizeOS(parts[3])
			}
			if len(parts) > 4 {
				lang = normalizeJVM(strings.Join(parts[4:], "/"))
			}
		}
	}
	return client, version, os, lang
}

// "-eclipseadoptium-openjdk64bitservervm-java-26" → "java26", matching the EL
// runtime convention (go1.24.6, dotnet9.0.8).
func normalizeJVM(s string) string {
	lower := strings.ToLower(strings.TrimSpace(s))
	i := strings.LastIndex(lower, "java")
	if i < 0 {
		return strings.TrimSpace(s)
	}
	ver := strings.Trim(lower[i+len("java"):], "- ")
	return "java" + ver
}

func (f *CLFingerprinter) exchangeStatus(ctx context.Context, pid peer.ID, localFork []byte) ([4]byte, error) {
	var digest [4]byte
	if len(localFork) < 4 {
		return digest, errors.New("local consensus fork digest is required")
	}
	s, err := f.host.NewStream(ctx, pid, statusProtocolV2, statusProtocolV1)
	if err != nil {
		return digest, err
	}
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(f.timeout))

	statusLen := statusV1SSZLen
	if s.Protocol() == statusProtocolV2 {
		statusLen = statusV2SSZLen
	}
	payload := make([]byte, statusLen)
	copy(payload[0:4], localFork[:4])

	var req bytes.Buffer
	var lp [binary.MaxVarintLen64]byte
	req.Write(lp[:binary.PutUvarint(lp[:], uint64(statusLen))])
	sw := snappy.NewBufferedWriter(&req)
	if _, err := sw.Write(payload); err != nil {
		return digest, err
	}
	if err := sw.Close(); err != nil {
		return digest, err
	}
	if _, err := s.Write(req.Bytes()); err != nil {
		return digest, err
	}
	if err := s.CloseWrite(); err != nil {
		return digest, err
	}

	br := bufio.NewReader(io.LimitReader(s, 1024))
	code, err := br.ReadByte()
	if err != nil {
		return digest, err
	}
	if code != 0 {
		return digest, fmt.Errorf("consensus status response code %d", code)
	}
	responseLen, err := binary.ReadUvarint(br)
	if err != nil {
		return digest, err
	}
	if responseLen != uint64(statusLen) {
		return digest, fmt.Errorf("consensus status length %d, want %d", responseLen, statusLen)
	}
	response := make([]byte, statusLen)
	if _, err := io.ReadFull(snappy.NewReader(br), response); err != nil {
		return digest, err
	}
	copy(digest[:], response[:4])
	return digest, nil
}

func inboundCLNode(pub *ecdsa.PublicKey, remote ma.Multiaddr, listenAddrs []ma.Multiaddr, allowPrivate bool) *enode.Node {
	if pub == nil {
		return nil
	}
	remoteIP := multiaddrIP(remote)
	if !netpolicy.Usable(remoteIP, allowPrivate) {
		return nil
	}
	for _, addr := range listenAddrs {
		if ip := multiaddrIP(addr); ip == nil || !ip.Equal(remoteIP) {
			continue
		}
		rawPort, err := addr.ValueForProtocol(ma.P_TCP)
		if err != nil {
			continue
		}
		port, err := strconv.Atoi(rawPort)
		if err != nil || port < 1 || port > 65535 {
			continue
		}
		return enode.NewV4(pub, remoteIP, port, 0)
	}
	return nil
}

func multiaddrIP(addr ma.Multiaddr) net.IP {
	if addr == nil {
		return nil
	}
	for _, code := range []int{ma.P_IP4, ma.P_IP6} {
		raw, err := addr.ValueForProtocol(code)
		if err == nil {
			return net.ParseIP(raw)
		}
	}
	return nil
}

func libp2pAddrsWithPolicy(n *enode.Node, allowPrivate bool) []ma.Multiaddr {
	var ip4 enr.IPv4
	var ip6 enr.IPv6
	var tcp enr.TCP
	var tcp6 enr.TCP6
	var quic enr.QUIC
	var quic6 enr.QUIC6
	hasIP4 := n.Load(&ip4) == nil
	hasIP6 := n.Load(&ip6) == nil
	n.Load(&tcp)
	n.Load(&quic)
	if n.Load(&tcp6) != nil {
		tcp6 = enr.TCP6(tcp)
	}
	if n.Load(&quic6) != nil {
		quic6 = enr.QUIC6(quic)
	}

	var addrs []ma.Multiaddr
	add := func(format string, args ...any) {
		if m, err := ma.NewMultiaddr(fmt.Sprintf(format, args...)); err == nil {
			addrs = append(addrs, m)
		}
	}
	if hasIP4 && netpolicy.Usable(net.IP(ip4[:]), allowPrivate) {
		ip := net.IP(ip4[:]).String()
		if tcp > 0 {
			add("/ip4/%s/tcp/%d", ip, tcp)
		}
		if quic > 0 {
			add("/ip4/%s/udp/%d/quic-v1", ip, quic)
		}
	}
	if hasIP6 && netpolicy.Usable(net.IP(ip6[:]), allowPrivate) {
		ip := net.IP(ip6[:]).String()
		if tcp6 > 0 {
			add("/ip6/%s/tcp/%d", ip, tcp6)
		}
		if quic6 > 0 {
			add("/ip6/%s/udp/%d/quic-v1", ip, quic6)
		}
	}
	return addrs
}
