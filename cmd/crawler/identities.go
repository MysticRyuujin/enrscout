package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/p2p/netutil"

	"github.com/MysticRyuujin/enrscout/internal/discovery"
	"github.com/MysticRyuujin/enrscout/internal/enrich"
	"github.com/MysticRyuujin/enrscout/internal/netconf"
)

const (
	layerEL = "el"
	layerCL = "cl"
)

type identitySpec struct {
	Network       string
	Layer         string
	Index         int
	Walker        bool
	DiscoveryPort int
	TCPPort       int
	QUICPort      int
}

type runtimeIdentity struct {
	spec      identitySpec
	discovery *discovery.Crawler
	el        *enrich.Fingerprinter
	cl        *enrich.CLFingerprinter
}

func parseAdvertiserNetworks(raw string, devnetOnly bool) ([]string, error) {
	if devnetOnly {
		return []string{"devnet"}, nil
	}
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("--advertiser-networks must list at least one network")
	}
	seen := make(map[string]bool)
	var out []string
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("--advertiser-networks contains an empty network")
		}
		if seen[name] {
			return nil, fmt.Errorf("--advertiser-networks contains duplicate %q", name)
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}

// Non-walker identities stay DHT-findable (tables self-maintain and answer FINDNODE without RandomNodes consumption), so fewer walkers means less walk traffic with unchanged inbound reachability.
func identitySpecs(networks []string, basePort, elIdentitiesPerNetwork, walkerELIdentities int, withTransports bool) ([]identitySpec, error) {
	portsPerNetwork := elIdentitiesPerNetwork + 2
	if basePort < 1 || len(networks) == 0 || elIdentitiesPerNetwork < 1 || basePort+len(networks)*portsPerNetwork-1 > 65535 {
		return nil, fmt.Errorf("advertiser port range must fit within 1..65535, base=%d networks=%d el-identities=%d", basePort, len(networks), elIdentitiesPerNetwork)
	}
	specs := make([]identitySpec, 0, len(networks)*(elIdentitiesPerNetwork+1))
	port := basePort
	for _, network := range networks {
		for index := range elIdentitiesPerNetwork {
			el := identitySpec{Network: network, Layer: layerEL, Index: index, DiscoveryPort: port,
				Walker: walkerELIdentities == 0 || index < walkerELIdentities}
			if withTransports {
				el.TCPPort = port
			}
			specs = append(specs, el)
			port++
		}
		cl := identitySpec{Network: network, Layer: layerCL, DiscoveryPort: port, Walker: true}
		if withTransports {
			cl.TCPPort = port + 1
			cl.QUICPort = port + 1
		}
		specs = append(specs, cl)
		port += 2
	}
	return specs, nil
}

// walkSource limits CL walkers to discv5: the discv4 keyspace is global and already covered by each network's EL walker.
func walkSource(spec identitySpec, proto string) bool {
	if !spec.Walker {
		return false
	}
	return spec.Layer != layerCL || proto == "v5"
}

func identityKeyName(spec identitySpec) string {
	name := spec.Network + "-" + spec.Layer
	if spec.Index > 0 {
		name += fmt.Sprintf("-%d", spec.Index+1)
	}
	return name + ".key"
}

func walkerName(spec identitySpec) string {
	return strings.TrimSuffix(identityKeyName(spec), ".key")
}

func clListenAddrs(families []string, tcpPort, quicPort int) []string {
	var out []string
	for _, family := range families {
		host := "0.0.0.0"
		proto := "ip4"
		if family == "udp6" {
			host = "::"
			proto = "ip6"
		}
		if tcpPort > 0 {
			out = append(out, fmt.Sprintf("/%s/%s/tcp/%d", proto, host, tcpPort))
		}
		if quicPort > 0 {
			out = append(out, fmt.Sprintf("/%s/%s/udp/%d/quic-v1", proto, host, quicPort))
		}
	}
	return out
}

// newIdentityRuntime constructs the advertiser runtime. fp, clfp and nodeDBAssigned are
// loop-carried state: the first EL identity creates the shared fingerprinter and takes the
// node DB, and later identities reuse them.
func newIdentityRuntime(ctx context.Context, cr *crawler, families []string, restrict *netutil.Netlist, staticIPs []net.IP) (*identityRuntime, context.Context) {
	conf, set, geo := cr.conf, cr.set, cr.geo
	var fp *enrich.Fingerprinter
	var clfp *enrich.CLFingerprinter
	nodeDBAssigned := false
	identityCtx, cancelIdentities := context.WithCancel(ctx)
	rt := &identityRuntime{cancel: cancelIdentities}
	rt.build = func(spec identitySpec) (*runtimeIdentity, error) {
		keyPath := filepath.Join(conf.identityDir, identityKeyName(spec))
		key, err := loadOrCreateNodeKey(keyPath)
		if err != nil {
			return nil, fmt.Errorf("load %s %s identity: %w", spec.Network, spec.Layer, err)
		}
		nw, _ := netconf.Get(spec.Network)
		cfg := discovery.Config{
			Network: nw, Families: families, PortV4: spec.DiscoveryPort, PortV5: spec.DiscoveryPort,
			Key: key, NetRestrict: restrict, StaticIPs: staticIPs, TCP: spec.TCPPort, QUIC: spec.QUICPort,
		}
		if spec.Layer == layerEL {
			cfg.Bootnodes, err = netconf.Bootnodes(spec.Network)
			if !nodeDBAssigned {
				cfg.NodeDBPath = conf.nodeDB
				nodeDBAssigned = true
			}
		} else {
			cfg.CLOnly = true
			cfg.Bootnodes, err = netconf.CLBootnodes(spec.Network)
			if err == nil {
				cl, ok := clIdentityFromSeeds(cfg.Bootnodes)
				if !ok {
					err = fmt.Errorf("no consensus ENR identity fields in %s bootnodes", spec.Network)
				} else {
					cfg.Eth2, err = netconf.CurrentCLForkENR(spec.Network)
					if err == nil {
						cfg.NFD, err = netconf.CurrentCLNFD(spec.Network)
					}
					cfg.Attnets, cfg.Syncnets, cfg.CGC = cl.attnets, cl.syncnets, cl.cgc
					if len(cfg.Attnets) == 0 {
						cfg.Attnets = []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
					}
				}
			}
		}
		if err != nil {
			return nil, fmt.Errorf("configure %s %s identity: %w", spec.Network, spec.Layer, err)
		}
		disc, err := discovery.New(cfg)
		if err != nil {
			return nil, fmt.Errorf("start %s %s discovery identity: %w", spec.Network, spec.Layer, err)
		}
		identity := &runtimeIdentity{spec: spec, discovery: disc}
		rt.track(discoveryCloser{disc})

		if !conf.fingerprint {
			return identity, nil
		}
		if spec.Layer == layerEL {
			if fp == nil {
				fp, err = enrich.NewFingerprinterWithPolicy(conf.fpTimeout, conf.fpMaxInflight, key, conf.allowPrivate || conf.devnetOnly)
				identity.el, cr.fp = fp, fp
			} else {
				identity.el, err = fp.WithIdentity(key)
			}
			if err != nil {
				return nil, err
			}
			listener, err := identity.el.ListenInbound(identityCtx, fmt.Sprintf(":%d", spec.TCPPort), nw, func(result enrich.InboundFingerprint) {
				defer recoverPeerCallback(spec.Network, layerEL)
				if result.Err != nil {
					observeFingerprintAttempt(layerEL, "inbound", result.Duration, result.Err)
					if result.Fingerprint.Client != "" {
						// A failed Status exchange can still have decoded a network; clear it so
						// membership stays ENR-claimed, keeping only the Hello-proven client identity.
						fp := result.Fingerprint
						fp.Network = ""
						mAdvertiserInbound.WithLabelValues(spec.Network, layerEL, "hello_only").Inc()
						cr.applyInbound(result.NodeID, layerEL, fp)
						return
					}
					mAdvertiserInbound.WithLabelValues(spec.Network, layerEL, "failed").Inc()
					observeFingerprintFailure(layerEL, "inbound", spec.Network, "", result.Err)
					return
				}
				observeFingerprintAttempt(layerEL, "inbound", result.Duration, nil)
				if set.LayerOf(result.NodeID) == "" && result.Fingerprint.Network != "" {
					candidate, via := legacyInboundCandidate(set, cr.pendingLegacy, result, time.Now())
					if candidate != nil {
						if cr.applyLegacyFingerprint(candidate, via, "inbound", result.Fingerprint) {
							mAdvertiserInbound.WithLabelValues(spec.Network, layerEL, "identified_new").Inc()
							return
						}
						mAdvertiserInbound.WithLabelValues(spec.Network, layerEL, "rejected").Inc()
						return
					}
				}
				mAdvertiserInbound.WithLabelValues(spec.Network, layerEL, "identified").Inc()
				cr.applyInbound(result.NodeID, layerEL, result.Fingerprint)
			})
			if err != nil {
				return nil, fmt.Errorf("listen for %s EL fingerprints: %w", spec.Network, err)
			}
			rt.track(listener)
			slog.Info("EL advertiser started", "network", spec.Network, "node", disc.LocalID(), "enr", disc.LocalNode(), "discovery-port", spec.DiscoveryPort, "tcp-port", spec.TCPPort)
		} else {
			listenAddrs := clListenAddrs(families, spec.TCPPort, spec.QUICPort)
			identity.cl, err = enrich.NewCLFingerprinterWithLimits(conf.fpTimeout, key, conf.allowPrivate || conf.devnetOnly, conf.fpMaxInflight, listenAddrs...)
			if err != nil {
				return nil, err
			}
			rt.track(clCloser{identity.cl})
			if clfp == nil {
				clfp, cr.clfp = identity.cl, identity.cl
			} else {
				identity.cl.ShareInboundBudget(clfp)
			}
			if err := identity.cl.WatchInbound(cfg.Eth2, func(result enrich.InboundCLFingerprint) {
				defer recoverPeerCallback(spec.Network, layerCL)
				if result.Err != nil {
					observeFingerprintAttempt(layerCL, "inbound", 0, result.Err)
					mAdvertiserInbound.WithLabelValues(spec.Network, layerCL, "failed").Inc()
					observeFingerprintFailure(layerCL, "inbound", spec.Network, "", result.Err)
					return
				}
				observeFingerprintAttempt(layerCL, "inbound", 0, nil)
				mCLStatus.WithLabelValues("inbound", clStatusOutcome(result.Fingerprint)).Inc()
				if set.LayerOf(result.NodeID) == "" && result.Fingerprint.Network != "" {
					now := time.Now()
					if candidate := consensusInboundCandidate(set, result); candidate != nil {
						observed := set.ObserveAuthenticatedCL(candidate, result.Fingerprint.Network, result.Fingerprint.ForkHash, now)
						if observed.Accepted {
							if observed.Changed && geo != nil {
								addr := candidate.IP()
								g := geo.Lookup(addr)
								set.SetGeo(result.NodeID, addr, g.Country, g.City, g.Subdivision, g.Lat, g.Lon, g.ASN, g.Org, g.Hosting, g.HostingKnown, g.Geolocated, g.AccuracyRadiusKM)
							}
							set.SetFingerprint(result.NodeID, result.Fingerprint.Client, result.Fingerprint.Version, result.Fingerprint.OS, result.Fingerprint.Lang, result.Fingerprint.Caps, "inbound")
							mAdvertiserInbound.WithLabelValues(spec.Network, layerCL, "identified_new").Inc()
							slog.Info("inbound CL node identified", "node", result.NodeID, "network", result.Fingerprint.Network, "client", result.Fingerprint.Client)
							return
						}
					}
				}
				mAdvertiserInbound.WithLabelValues(spec.Network, layerCL, "identified").Inc()
				cr.applyInbound(result.NodeID, layerCL, result.Fingerprint)
			}); err != nil {
				return nil, fmt.Errorf("watch %s CL inbound peers: %w", spec.Network, err)
			}
			slog.Info("CL advertiser started", "network", spec.Network, "node", disc.LocalID(), "enr", disc.LocalNode(), "peer-id", identity.cl.PeerID(), "discovery-port", spec.DiscoveryPort, "tcp-port", spec.TCPPort, "quic-port", spec.QUICPort)
		}
		return identity, nil
	}
	return rt, identityCtx
}
