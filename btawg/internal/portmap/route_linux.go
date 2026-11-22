package portmap

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

func defaultRouter(ctx context.Context) (router, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return router{}, fmt.Errorf("read Linux IPv4 routes: %w", err)
	}
	defer f.Close()
	name, gateway, err := defaultRoute(f)
	if err != nil {
		return router{}, err
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return router{}, fmt.Errorf("find default route interface: %w", err)
	}
	r := router{gateway: gateway, ifindex: iface.Index, natPort: 5351, ssdp: netip.MustParseAddrPort("239.255.255.250:1900")}

	// A UDP connect selects the preferred source according to the kernel's
	// route to this gateway without transmitting a packet. Refuse to use it
	// if it selects a different interface (e.g. conflicting policy routing).
	var dial net.Dialer
	conn, err := dial.DialContext(ctx, "udp4", netip.AddrPortFrom(gateway, r.natPort).String())
	if err != nil {
		return router{}, fmt.Errorf("select IPv4 source for default gateway: %w", err)
	}
	r.source = conn.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap()
	_ = conn.Close()
	addrs, err := iface.Addrs()
	if err != nil {
		return router{}, fmt.Errorf("get default route interface addresses: %w", err)
	}
	for _, addr := range addrs {
		prefix, err := netip.ParsePrefix(addr.String())
		if err == nil && prefix.Addr() == r.source && r.source.Is4() && r.source.IsGlobalUnicast() {
			return r, nil
		}
	}
	return router{}, fmt.Errorf("no IPv4 source on default route interface %s for gateway %s", name, gateway)
}

func defaultRoute(input io.Reader) (string, netip.Addr, error) {
	const (
		rtfUp      = 0x001
		rtfGateway = 0x002
		rtfReject  = 0x200
	)
	var name string
	var gateway netip.Addr
	var best uint64
	s := bufio.NewScanner(input)
	for s.Scan() {
		f := strings.Fields(s.Text())
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(f[3], 16, 32)
		if err != nil || flags&(rtfUp|rtfGateway|rtfReject) != (rtfUp|rtfGateway) {
			continue
		}
		next, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil {
			continue
		}
		metric, err := strconv.ParseUint(f[6], 10, 32)
		if err != nil || gateway.IsValid() && metric >= best {
			continue
		}
		var ip [4]byte
		binary.NativeEndian.PutUint32(ip[:], uint32(next))
		candidate := netip.AddrFrom4(ip)
		if !candidate.IsGlobalUnicast() && !candidate.IsLinkLocalUnicast() {
			continue
		}
		name, gateway, best = f[0], candidate, metric
	}
	if err := s.Err(); err != nil {
		return "", netip.Addr{}, fmt.Errorf("parse Linux IPv4 routes: %w", err)
	}
	if !gateway.IsValid() {
		return "", netip.Addr{}, errors.New("no default Linux IPv4 gateway")
	}
	return name, gateway, nil
}
