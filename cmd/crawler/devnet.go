package main

import (
	"errors"
	"fmt"
	"net"

	"github.com/ethereum/go-ethereum/p2p/enode"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
)

// enclaveIP is the crawler's enclave-network address (172.16.0.0/12); clients dial it, not the co-attached infra net.
func enclaveIP() (net.IP, error) {
	_, block, err := net.ParseCIDR("172.16.0.0/12")
	if err != nil {
		return nil, err
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("list interface addresses: %w", err)
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			if ip4 := ipnet.IP.To4(); ip4 != nil && block.Contains(ip4) {
				return ip4, nil
			}
		}
	}
	return nil, errors.New("no enclave IPv4 address in 172.16.0.0/12")
}

type clIdentity struct {
	eth2, nfd, attnets, syncnets, cgc []byte
}

// clIdentityFromSeeds retains network-specific subnet and custody fields while
// the current fork entry itself is generated from netconf.
func clIdentityFromSeeds(seeds []*enode.Node) (clIdentity, bool) {
	for _, s := range seeds {
		var eth2 netconf.Eth2Entry
		if s.Load(&eth2) != nil || len(eth2) < 4 {
			continue
		}
		var att netconf.AttnetsEntry
		var sync netconf.SyncnetsEntry
		var cgc netconf.CGCEntry
		var nfd netconf.NFDEntry
		s.Load(&att)
		s.Load(&sync)
		s.Load(&cgc)
		s.Load(&nfd)
		return clIdentity{eth2: []byte(eth2), nfd: []byte(nfd), attnets: []byte(att), syncnets: []byte(sync), cgc: []byte(cgc)}, true
	}
	return clIdentity{}, false
}
