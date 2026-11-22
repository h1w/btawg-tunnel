package client

import (
	"net/netip"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"

	"bittorrent-tunnel/btawg/internal/config"
)

func openSharedClientTUN(cfg config.ClientConfig, name string) (*sharedTUN, error) {
	dev, err := openClientTUN(cfg, name)
	if err != nil {
		return nil, err
	}
	return newSharedTUN(dev, name, config.DefaultMTU), nil
}

func (c *Client) configureForegroundTUN(dev tun.Device, name string) (tun.Device, error) {
	shared := newSharedTUN(dev, name, config.DefaultMTU)
	path := shared.newPath()
	if err := path.proxySockets(c, netip.MustParseAddr(c.cfg.ClientIP)); err != nil {
		_ = path.Close()
		_ = shared.close()
		return nil, err
	}
	shared.primary.Store(path)
	c.closeTUN = shared.close
	return path, nil
}
