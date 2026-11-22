package client

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"

	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/tunsetup"
)

func openSharedClientTUN(cfg config.ClientConfig, name string) (*sharedTUN, error) {
	dev, err := openClientTUN(cfg, name)
	if err != nil {
		return nil, err
	}
	route, err := tunsetup.NewProxyRoute(name)
	if err != nil {
		_ = dev.Close()
		return nil, err
	}
	shared := newSharedTUN(dev, name, config.DefaultMTU)
	shared.closeRoute = route.Close
	return shared, nil
}

func (c *Client) configureForegroundTUN(dev tun.Device, name string) (tun.Device, error) {
	route, err := tunsetup.NewProxyRoute(name)
	if err != nil {
		return nil, fmt.Errorf("create SOCKS TUN route: %w", err)
	}
	c.closeTUN = route.Close
	local := netip.AddrPortFrom(netip.MustParseAddr(c.cfg.ClientIP), 0)
	dialer := net.Dialer{
		LocalAddr: net.TCPAddrFromAddrPort(local),
		Control: func(network, address string, socket syscall.RawConn) error {
			if err := route.Control(network, address, socket); err != nil {
				return err
			}
			return tunsetup.OverlayTCP(network, address, socket)
		},
	}
	c.proxyTCP = func(ctx context.Context, dst netip.AddrPort) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp4", dst.String())
	}
	listen := net.ListenConfig{Control: route.Control}
	c.proxyUDP = func(ctx context.Context) (net.PacketConn, error) {
		return listen.ListenPacket(ctx, "udp4", local.String())
	}
	return dev, nil
}
