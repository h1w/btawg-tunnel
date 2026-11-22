package client

import (
	"context"
	"log"
	"net"
	"net/netip"
	"time"

	"bittorrent-tunnel/btawg/internal/mesh"
	"bittorrent-tunnel/btawg/internal/tunsetup"
)

func (n *meshNode) servePeerProbe() {
	if n.owner.shared == nil {
		return
	}
	ip := netip.MustParseAddr(n.owner.cfg.ClientIP)
	lc := net.ListenConfig{Control: tunsetup.BindOwnedDevice(n.owner.shared.dev, n.owner.shared.name)}
	ln, err := lc.Listen(n.ctx, "tcp4", netip.AddrPortFrom(ip, mesh.PeerProbePort).String())
	if err != nil {
		if n.ctx.Err() == nil {
			log.Printf("btawg-client: peer probe listener: %v", err)
		}
		return
	}
	defer ln.Close()
	stop := context.AfterFunc(n.ctx, func() { ln.Close() })
	defer stop()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		c.Close()
	}
}

func (n *meshNode) probeDirect(ctx context.Context, p *tunPath, ip netip.Addr) {
	var c Client
	if err := p.proxySockets(&c, netip.MustParseAddr(n.owner.cfg.ClientIP)); err != nil {
		if ctx.Err() == nil {
			log.Printf("btawg-client: peer probe sockets: %v", err)
		}
		return
	}
	addr := netip.AddrPortFrom(ip, mesh.PeerProbePort)
	tick := time.NewTicker(probeInterval)
	defer tick.Stop()
	var health probeHealth
	for ctx.Err() == nil {
		began := time.Now()
		deadline, cancel := context.WithTimeout(ctx, probeTimeout)
		conn, err := c.proxyTCP(deadline, addr)
		cancel()
		var elapsed time.Duration
		if err == nil {
			elapsed = max(time.Since(began), time.Nanosecond)
			conn.Close()
		}
		p.shared.probePeer(ip, p, health.observe(elapsed))
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
