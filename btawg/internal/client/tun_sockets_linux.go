package client

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"bittorrent-tunnel/btawg/internal/tunsetup"
)

const windowsAssemblesIngress = false
const kernelPeerProbe = true

func (p *tunPath) inboundPacket(packet []byte) []byte { return packet }

// Register the bound source port before TCP connect can send its first SYN.
// Binding explicitly without SO_REUSEADDR reserves the port for this socket.
func (p *tunPath) bindTCP(ctx context.Context, network string, raw syscall.RawConn, source [4]byte, key *localPort) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tunsetup.BindDevice(p.shared.name)(network, "", raw); err != nil {
		return err
	}
	if err := tunsetup.OverlayTCP(network, "", raw); err != nil {
		return err
	}
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		socketErr = unix.Bind(int(fd), &unix.SockaddrInet4{Addr: source})
		if socketErr != nil {
			return
		}
		addr, err := unix.Getsockname(int(fd))
		if err != nil {
			socketErr = err
			return
		}
		key.port = uint16(addr.(*unix.SockaddrInet4).Port)
	}); err != nil {
		return err
	}
	return socketErr
}

func (s *sharedTUN) track(key localPort, owner *socketPath) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous := s.ports[key]; previous != nil && previous.until.IsZero() {
		return fmt.Errorf("overlay source port %d is still owned", key.port)
	}
	s.ports[key] = owner
	return nil
}

func (s *sharedTUN) untrack(key localPort, owner *socketPath) {
	s.mu.Lock()
	if s.ports[key] == owner {
		// TCP can still emit FIN retransmissions after Close. UDP only needs
		// enough overlap to read datagrams already queued on the TUN.
		drain := time.Second
		if key.proto == unix.IPPROTO_TCP {
			drain = time.Minute
		}
		if owner.until.IsZero() {
			owner.until = time.Now().Add(drain)
		}
	}
	s.mu.Unlock()
}

func (s *sharedTUN) pruneSockets() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case now := <-tick.C:
			s.mu.Lock()
			for key, owner := range s.ports {
				if !owner.until.IsZero() && !now.Before(owner.until) {
					delete(s.ports, key)
				}
			}
			s.mu.Unlock()
		}
	}
}

func (p *tunPath) proxySockets(c *Client, source netip.Addr) error {
	c.proxyTCP = func(ctx context.Context, dst netip.AddrPort) (net.Conn, error) {
		key := localPort{proto: unix.IPPROTO_TCP, remote: dst}
		owner := &socketPath{path: p}
		dialer := net.Dialer{ControlContext: func(ctx context.Context, network, _ string, raw syscall.RawConn) error {
			if err := p.bindTCP(ctx, network, raw, source.As4(), &key); err != nil {
				return err
			}
			return p.shared.track(key, owner)
		}}
		conn, err := dialer.DialContext(ctx, "tcp4", dst.String())
		if err != nil {
			p.shared.untrack(key, owner)
			return nil, err
		}
		return &trackedTCP{TCPConn: conn.(*net.TCPConn), shared: p.shared, key: key, owner: owner}, nil
	}
	listen := net.ListenConfig{Control: tunsetup.BindDevice(p.shared.name)}
	addr := netip.AddrPortFrom(source, 0).String()
	c.proxyUDP = func(ctx context.Context) (net.PacketConn, error) {
		conn, err := listen.ListenPacket(ctx, "udp4", addr)
		if err != nil {
			return nil, err
		}
		key := localPort{port: conn.LocalAddr().(*net.UDPAddr).AddrPort().Port(), proto: unix.IPPROTO_UDP}
		owner := &socketPath{path: p}
		if err := p.shared.track(key, owner); err != nil {
			conn.Close()
			return nil, err
		}
		return &trackedUDP{PacketConn: conn, shared: p.shared, key: key, owner: owner}, nil
	}
	return nil
}

func (p *tunPath) deliverPackets(bufs [][]byte, offset int) (int, error) {
	return p.shared.dev.Write(bufs, offset)
}

type trackedTCP struct {
	*net.TCPConn
	shared *sharedTUN
	key    localPort
	owner  *socketPath
}

func (c *trackedTCP) Close() error {
	err := c.TCPConn.Close()
	c.shared.untrack(c.key, c.owner)
	return err
}

type trackedUDP struct {
	net.PacketConn
	shared *sharedTUN
	key    localPort
	owner  *socketPath
}

func (c *trackedUDP) Close() error {
	err := c.PacketConn.Close()
	c.shared.untrack(c.key, c.owner)
	return err
}
