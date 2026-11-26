package routeproxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"

	"bittorrent-tunnel/btawg/internal/config"
)

var (
	errDestination = errors.New("route proxy: destination is not public IPv4")
	specialIPv4    = [...]netip.Prefix{
		netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
		netip.MustParsePrefix("192.88.99.0/24"),  // deprecated 6to4 relay anycast
		netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
		netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	}
)

func publicIPv4(ip netip.Addr) bool {
	if !config.PublicIPv4(ip) {
		return false
	}
	for _, p := range specialIPv4 {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// CheckPublicTCPDestination rejects non-public addresses and addresses on
// local interfaces or directly attached subnets before an Internet dial.
func CheckPublicTCPDestination(ip netip.Addr) error {
	return (exitPolicy{}).check([]netip.Addr{ip})
}

// Serve always uses the system resolver and interface list. Tests in this
// package can use private sockets without exposing a policy bypass to callers.
type exitPolicy struct {
	blocked          []netip.Prefix
	lookupIPv4       func(context.Context, string, string) ([]netip.Addr, error)
	interfaceAddrs   func() ([]net.Addr, error)
	allow            func(netip.Addr) bool
	allowDestination func(netip.Addr) bool
}

func (p exitPolicy) check(ips []netip.Addr) error {
	if len(ips) == 0 {
		return errDestination
	}
	for _, raw := range ips {
		ip := raw.Unmap()
		if !ip.Is4() || (p.allow == nil && !publicIPv4(ip)) || (p.allow != nil && !p.allow(ip)) {
			return errDestination
		}
		for _, blocked := range p.blocked {
			if blocked.Contains(ip) {
				return errDestination
			}
		}
	}
	if p.allowDestination != nil {
		for _, ip := range ips {
			if !p.allowDestination(ip.Unmap()) {
				return errDestination
			}
		}
	}
	if p.allow != nil { // private sockets used only by package tests
		return nil
	}
	interfaceAddrs := p.interfaceAddrs
	if interfaceAddrs == nil {
		interfaceAddrs = net.InterfaceAddrs
	}
	local, err := interfaceAddrs()
	if err != nil {
		return err
	}
	for _, addr := range local {
		var ip net.IP
		var network netip.Prefix
		switch a := addr.(type) {
		case *net.IPNet:
			ip = a.IP
			if ones, bits := a.Mask.Size(); bits == 32 {
				if addr, ok := netip.AddrFromSlice(ip); ok && addr.Unmap().Is4() {
					network = netip.PrefixFrom(addr.Unmap(), ones)
				}
			}
		case *net.IPAddr:
			ip = a.IP
		default:
			return errDestination
		}
		localIP, ok := netip.AddrFromSlice(ip)
		if !ok {
			return errDestination
		}
		localIP = localIP.Unmap()
		for _, dest := range ips {
			dest = dest.Unmap()
			if localIP == dest || network.Contains(dest) {
				return errDestination
			}
		}
	}
	return nil
}

func (p exitPolicy) destinationAllowed(ip netip.Addr) bool {
	return p.allowDestination == nil || p.allowDestination(ip)
}

func (p exitPolicy) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip.Unmap()}, nil
	}
	if !validHostname(host) {
		return nil, errDestination
	}
	lookup := p.lookupIPv4
	if lookup == nil {
		lookup = net.DefaultResolver.LookupNetIP
	}
	return lookup(ctx, "ip4", host)
}

func validHostname(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func (p exitPolicy) dialTCP(ctx context.Context, dst destination) (net.Conn, error) {
	ips, err := p.resolve(ctx, dst.host)
	if err != nil {
		return nil, err
	}
	if err := p.check(ips); err != nil {
		return nil, err
	}
	var dialer net.Dialer
	var c net.Conn
	for _, ip := range ips {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ip = ip.Unmap()
		if !p.destinationAllowed(ip) {
			return nil, errDestination
		}
		c, err = dialer.DialContext(ctx, "tcp4", netip.AddrPortFrom(ip, dst.port).String())
		if err == nil {
			return c, nil
		}
	}
	return nil, err
}
