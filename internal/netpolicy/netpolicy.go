// Package netpolicy centralizes the address admission rules used before a
// peer-controlled ENR can become a dial target.
package netpolicy

import (
	"net"

	"github.com/ethereum/go-ethereum/p2p/netutil"
)

// Neither Go's IsPrivate nor geth's IsSpecialNetwork covers these unroutable IPv4 ranges.
var (
	cgnat  = net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}
	classE = net.IPNet{IP: net.IPv4(240, 0, 0, 0).To4(), Mask: net.CIDRMask(4, 32)}
)

// Usable reports whether ip is safe to retain or dial. Private addresses are
// accepted only for an explicitly isolated devnet; loopback, link-local,
// multicast, unspecified, documentation, and other special-use ranges never are.
func Usable(ip net.IP, allowPrivate bool) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	if ip.IsPrivate() {
		return allowPrivate
	}
	if cgnat.Contains(ip) || classE.Contains(ip) {
		return false
	}
	return !netutil.IsSpecialNetwork(ip)
}
