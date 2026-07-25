package discovery

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/p2p/netutil"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
)

var errUnresolved = errors.New("no resolver reached node")

type resolver interface {
	RequestENR(*enode.Node) (*enode.Node, error)
}

type endpoint struct {
	proto  string
	family string
	iter   enode.Iterator
	res    resolver
}

type Config struct {
	Network     *netconf.Network
	Bootnodes   []*enode.Node
	Families    []string
	PortV4      int
	PortV5      int
	NodeDBPath  string
	NetRestrict *netutil.Netlist

	// A CL identity (Eth2/Attnets/TCP, shared Key, CLOnly to drop eth so bootnodes file it as consensus) draws peer-hungry consensus clients to dial in.
	Key       *ecdsa.PrivateKey
	Eth2      []byte
	NFD       []byte
	Attnets   []byte
	Syncnets  []byte
	CGC       []byte
	TCP       int
	QUIC      int
	StaticIPs []net.IP
	CLOnly    bool
}

func DetectFamilies(allowPrivate bool) ([]string, error) {
	var v4, v6 bool
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || !ipnet.IP.IsGlobalUnicast() || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() || (!allowPrivate && ipnet.IP.IsPrivate()) {
			continue
		}
		if ipnet.IP.To4() != nil {
			v4 = true
		} else {
			v6 = true
		}
	}
	var fams []string
	if v4 {
		fams = append(fams, "udp4")
	}
	if v6 {
		fams = append(fams, "udp6")
	}
	if len(fams) == 0 {
		return nil, errors.New("auto IP-family detection found no globally usable address; select --ip-stack explicitly or use the isolated-devnet private-address policy")
	}
	return fams, nil
}

type Crawler struct {
	ln        *enode.LocalNode
	db        *enode.DB
	endpoints []endpoint
	conns     []*net.UDPConn
	closers   []func()
}

// sharedUDPConn gives discv5 the packets discv4 could not decode while both
// protocols advertise and use the same UDP endpoint. This is the same
// demultiplexing contract used by go-ethereum's p2p.Server.
type sharedUDPConn struct {
	*net.UDPConn
	unhandled <-chan discover.ReadPacket
}

func (s *sharedUDPConn) ReadFromUDPAddrPort(b []byte) (int, netip.AddrPort, error) {
	p, ok := <-s.unhandled
	if !ok {
		return 0, netip.AddrPort{}, net.ErrClosed
	}
	n := copy(b, p.Data)
	return n, p.Addr, nil
}

func (*sharedUDPConn) Close() error { return nil }

func New(cfg Config) (*Crawler, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	key := cfg.Key
	if key == nil {
		var err error
		if key, err = crypto.GenerateKey(); err != nil {
			return nil, err
		}
	}
	db, err := enode.OpenDB(cfg.NodeDBPath)
	if err != nil {
		return nil, err
	}
	ln := enode.NewLocalNode(db, key)
	// LocalNode keys endpoints by family, so pin each family separately: prediction
	// never converges for a family with no inbound traffic (e.g. v4-only bootstrap
	// leaves IPv6 absent from the ENR).
	for _, ip := range cfg.StaticIPs {
		ln.SetStaticIP(ip)
		ln.SetFallbackIP(ip)
	}
	// Without a fallback the ENR has no udp until endpoint prediction converges
	// (it may never on an isolated devnet), and strict discv5 peers reject such records.
	ln.SetFallbackUDP(cfg.PortV5)
	if !cfg.CLOnly {
		ln.Set(netconf.EthEntry{ForkID: cfg.Network.CurrentForkID()})
	}
	if len(cfg.Eth2) > 0 {
		ln.Set(netconf.Eth2Entry(cfg.Eth2))
	}
	if len(cfg.NFD) > 0 {
		ln.Set(netconf.NFDEntry(cfg.NFD))
	}
	if len(cfg.Attnets) > 0 {
		ln.Set(netconf.AttnetsEntry(cfg.Attnets))
	}
	if len(cfg.Syncnets) > 0 {
		ln.Set(netconf.SyncnetsEntry(cfg.Syncnets))
	}
	if len(cfg.CGC) > 0 {
		ln.Set(netconf.CGCEntry(cfg.CGC))
	}
	if cfg.TCP > 0 {
		ln.Set(enr.TCP(cfg.TCP))
	}
	// rust discv5 (Lighthouse, reth) ignores the spec's tcp -> tcp6 fallback (sigp/discv5#307).
	if cfg.TCP > 0 && hasStaticV6(cfg.StaticIPs) {
		ln.Set(enr.TCP6(cfg.TCP))
	}
	if cfg.QUIC > 0 {
		ln.Set(enr.QUIC(cfg.QUIC))
	}
	if cfg.QUIC > 0 && hasStaticV6(cfg.StaticIPs) {
		ln.Set(enr.QUIC6(cfg.QUIC))
	}

	dcfg := discover.Config{PrivateKey: key, Bootnodes: cfg.Bootnodes, NetRestrict: cfg.NetRestrict}
	c := &Crawler{ln: ln, db: db}

	families := cfg.Families
	if len(families) == 0 {
		families = []string{"udp4"}
	}

	for _, fam := range families {
		conn, err := listen(fam, cfg.PortV4)
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("listen %s discovery: %w", fam, err)
		}
		unhandled := make(chan discover.ReadPacket, 100)
		v4cfg := dcfg
		v4cfg.Unhandled = unhandled
		v4, err := discover.ListenV4(conn, ln, v4cfg)
		if err != nil {
			conn.Close()
			c.Close()
			return nil, fmt.Errorf("discv4 %s: %w", fam, err)
		}
		v5, err := discover.ListenV5(&sharedUDPConn{UDPConn: conn, unhandled: unhandled}, ln, dcfg)
		if err != nil {
			v4.Close()
			c.Close()
			return nil, fmt.Errorf("discv5 %s: %w", fam, err)
		}
		c.conns = append(c.conns, conn)
		c.closers = append(c.closers, v4.Close, v5.Close)
		c.endpoints = append(c.endpoints, endpoint{proto: "v4", family: fam, iter: v4.RandomNodes(), res: v4})
		c.endpoints = append(c.endpoints, endpoint{proto: "v5", family: fam, iter: v5.RandomNodes(), res: v5})
	}
	return c, nil
}

func hasStaticV6(ips []net.IP) bool {
	for _, ip := range ips {
		if ip.To4() == nil && ip.To16() != nil {
			return true
		}
	}
	return false
}

func validateConfig(cfg Config) error {
	if !cfg.CLOnly && cfg.Network == nil {
		return errors.New("network is required for an execution-layer discovery identity")
	}
	if cfg.PortV4 < 1 || cfg.PortV4 > 65535 || cfg.PortV5 < 1 || cfg.PortV5 > 65535 {
		return fmt.Errorf("discovery ports must be between 1 and 65535, got %d/%d", cfg.PortV4, cfg.PortV5)
	}
	if cfg.PortV4 != cfg.PortV5 {
		return fmt.Errorf("discv4 and discv5 must share one advertised UDP port, got %d and %d", cfg.PortV4, cfg.PortV5)
	}
	if cfg.TCP < 0 || cfg.TCP > 65535 {
		return fmt.Errorf("TCP port must be between 0 and 65535, got %d", cfg.TCP)
	}
	if cfg.QUIC < 0 || cfg.QUIC > 65535 {
		return fmt.Errorf("QUIC port must be between 0 and 65535, got %d", cfg.QUIC)
	}
	seen := make(map[string]bool, len(cfg.Families))
	for _, family := range cfg.Families {
		if family != "udp4" && family != "udp6" {
			return fmt.Errorf("unsupported discovery address family %q", family)
		}
		if seen[family] {
			return fmt.Errorf("duplicate discovery address family %q", family)
		}
		seen[family] = true
	}
	for _, ip := range cfg.StaticIPs {
		if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
			return fmt.Errorf("invalid static IP %q", ip)
		}
	}
	return nil
}

func listen(family string, port int) (*net.UDPConn, error) {
	ip := net.IPv4zero
	if family == "udp6" {
		ip = net.IPv6zero
	}
	return net.ListenUDP(family, &net.UDPAddr{IP: ip, Port: port})
}

func (c *Crawler) LocalID() enode.ID {
	return c.ln.ID()
}

func (c *Crawler) LocalNode() *enode.Node {
	return c.ln.Node()
}

func (c *Crawler) SetELForkID(id forkid.ID) {
	c.ln.Set(netconf.EthEntry{ForkID: id})
}

func (c *Crawler) SetCLForkID(eth2 []byte) {
	if len(eth2) > 0 {
		c.ln.Set(netconf.Eth2Entry(eth2))
	}
}

func (c *Crawler) SetCLForkState(eth2, nfd []byte) {
	c.SetCLForkID(eth2)
	if len(nfd) > 0 {
		c.ln.Set(netconf.NFDEntry(nfd))
	}
}

type Source struct {
	Iter   enode.Iterator
	Proto  string
	Family string
}

func (c *Crawler) Sources() []Source {
	srcs := make([]Source, len(c.endpoints))
	for i, e := range c.endpoints {
		srcs[i] = Source{Iter: e.iter, Proto: e.proto, Family: e.family}
	}
	return srcs
}

func (c *Crawler) Resolve(n *enode.Node) (*enode.Node, string, error) {
	fams := nodeFamilies(n)
	for _, proto := range []string{"v5", "v4"} {
		for _, e := range c.endpoints {
			if e.proto != proto || !fams[e.family] {
				continue
			}
			if rn, err := e.res.RequestENR(n); err == nil && rn != nil {
				return rn, e.proto, nil
			}
		}
	}
	return nil, "", errUnresolved
}

// ResolveProtocol limits ENR resolution to the discovery protocol that yielded
// the candidate. This avoids discv5 and cross-identity timeouts for unsigned
// discv4 neighbors before their RLPx Status fallback.
func (c *Crawler) ResolveProtocol(n *enode.Node, proto string) (*enode.Node, string, error) {
	fams := nodeFamilies(n)
	for _, e := range c.endpoints {
		if e.proto != proto || !fams[e.family] {
			continue
		}
		if rn, err := e.res.RequestENR(n); err == nil && rn != nil {
			return rn, e.proto, nil
		}
	}
	return nil, "", errUnresolved
}

func (c *Crawler) Close() {
	for _, cl := range c.closers {
		cl()
	}
	for _, conn := range c.conns {
		conn.Close()
	}
	if c.db != nil {
		c.db.Close()
	}
}

func nodeFamilies(n *enode.Node) map[string]bool {
	fams := make(map[string]bool, 2)
	var ip4 enr.IPv4
	if n.Load(&ip4) == nil {
		fams["udp4"] = true
	}
	var ip6 enr.IPv6
	if n.Load(&ip6) == nil {
		fams["udp6"] = true
	}
	if len(fams) == 0 {
		fams["udp4"] = true
	}
	return fams
}
