package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"syscall"
	"time"

	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/hubrelay"
	"bittorrent-tunnel/btawg/internal/socks"
	"bittorrent-tunnel/btawg/internal/tunneldns"
)

func (c *Client) localProxy() *socks.Server {
	gateway := netip.MustParseAddr(c.cfg.ServerIP)
	dns := netip.AddrPortFrom(gateway, tunneldns.Port)
	prefixes := make([]netip.Prefix, 0, len(c.cfg.Routes)+1)
	for _, r := range c.cfg.Routes {
		if p, err := netip.ParsePrefix(r); err == nil && p.Bits() > 0 {
			prefixes = append(prefixes, p)
		}
	}
	if p, err := netip.ParsePrefix(c.cfg.Subnet); err == nil {
		prefixes = append(prefixes, p)
	}
	allowed := func(ip netip.Addr) bool {
		if ip == gateway || !config.PublicIPv4(ip) {
			return false
		}
		for _, p := range prefixes {
			if p.Contains(ip) {
				return false
			}
		}
		return true
	}
	resolve := func(ctx context.Context, host string) ([]netip.Addr, error) {
		if ip, err := netip.ParseAddr(host); err == nil {
			ip = ip.Unmap()
			if !allowed(ip) {
				return nil, fmt.Errorf("SOCKS destination is not public IPv4")
			}
			return []netip.Addr{ip}, nil
		}
		ips, err := tunneldns.LookupIPv4(ctx, func(ctx context.Context) (net.Conn, error) {
			return c.proxyTCP(ctx, dns)
		}, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if !allowed(ip) {
				return nil, fmt.Errorf("SOCKS hostname resolved to a prohibited address")
			}
		}
		return ips, nil
	}
	return socks.NewServer(func(ctx context.Context, _, addr string) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		host, service, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		port, err := strconv.ParseUint(service, 10, 16)
		if err != nil || port == 0 {
			return nil, fmt.Errorf("invalid SOCKS TCP port %q", service)
		}
		ips, err := resolve(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			conn, err := c.dialSOCKSData(ctx, gateway, netip.AddrPortFrom(ip, uint16(port)))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("no usable IPv4 address for %q", host)
		}
		return nil, lastErr
	}, c.proxyUDP).WithUDPResolver(func(ctx context.Context, ip netip.Addr, host string) (netip.Addr, error) {
		if ip.IsValid() {
			if !allowed(ip) {
				return netip.Addr{}, fmt.Errorf("SOCKS destination is not public IPv4")
			}
			return ip, nil
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		ips, err := resolve(ctx, host)
		if err != nil {
			return netip.Addr{}, err
		}
		if len(ips) == 0 {
			return netip.Addr{}, fmt.Errorf("no usable IPv4 address for %q", host)
		}
		return ips[0], nil
	})
}

func (c *Client) dialSOCKSData(ctx context.Context, gateway netip.Addr, dst netip.AddrPort) (net.Conn, error) {
	if c.nativeSOCKS == nil {
		return c.proxyTCP(ctx, dst)
	}
	conn, err := c.nativeSOCKS(ctx, netip.AddrPortFrom(gateway, hubrelay.Port))
	if errors.Is(err, syscall.ECONNREFUSED) {
		// Older hubs and overlay-only hubs have no TCP relay listener.
		// Once a relay accepts the socket, its decisions are authoritative.
		return c.nativeSOCKS(ctx, dst)
	}
	if err != nil {
		return nil, err
	}
	if err := hubrelay.Request(ctx, conn, dst); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}
