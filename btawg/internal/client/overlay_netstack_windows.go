package client

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"bittorrent-tunnel/btawg/internal/mesh"
)

// The Windows host has no per-interface policy route equivalent to Linux's
// SOCKS route. Give explicit overlay sockets their own IPv4 stack per path.
// The Windows kernel still reserves their source ports on the real adapter.
type windowsOverlay struct {
	path      *tunPath
	local     netip.Addr
	stack     *stack.Stack
	link      *channel.Endpoint
	worker    chan struct{}
	cancel    context.CancelFunc
	life      context.Context
	dials     sync.WaitGroup
	mu        sync.RWMutex
	closed    bool
	probe     net.Listener
	probeDone chan struct{}
}

// The default gVisor qdisc ignores a short link write. Report a full link
// queue explicitly instead of silently admitting another UDP datagram.
type windowsOverlayQueue struct {
	link   *channel.Endpoint
	shared *sharedTUN
}

func (q *windowsOverlayQueue) WritePacket(pkt stack.PacketBufferPtr) tcpip.Error {
	var packets stack.PacketBufferList
	packets.PushBack(pkt)
	n, err := q.link.WritePackets(packets)
	if err == nil && n == 0 {
		q.shared.overlayQueueStalls.Add(1)
		return &tcpip.ErrNoBufferSpace{}
	}
	return err
}

func (*windowsOverlayQueue) Close() {}

func newWindowsOverlay(path *tunPath, local netip.Addr) (*windowsOverlay, error) {
	link := channel.New(pathQueueSize, uint32(path.shared.mtu), "")
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})
	start := func() tcpip.Error {
		sack := tcpip.TCPSACKEnabled(true)
		if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &sack); err != nil {
			return err
		}
		if err := s.CreateNICWithOptions(1, link, stack.NICOptions{QDisc: &windowsOverlayQueue{link: link, shared: path.shared}}); err != nil {
			return err
		}
		return s.AddProtocolAddress(1, tcpip.ProtocolAddress{
			Protocol:          ipv4.ProtocolNumber,
			AddressWithPrefix: tcpip.AddrFrom4(local.As4()).WithPrefix(),
		}, stack.AddressProperties{})
	}
	if err := start(); err != nil {
		s.Destroy()
		link.Close()
		return nil, fmt.Errorf("initialize overlay IP stack: %s", err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
	life, cancel := context.WithCancel(context.Background())
	o := &windowsOverlay{path: path, local: local, stack: s, link: link, worker: make(chan struct{}), life: life, cancel: cancel}
	if path.directLife != nil && !path.peerOutbound {
		probeAddr := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(local.As4()), Port: mesh.PeerProbePort}
		ln, err := gonet.ListenTCP(s, probeAddr, ipv4.ProtocolNumber)
		if err != nil {
			cancel()
			s.Destroy()
			link.Close()
			return nil, fmt.Errorf("listen for direct peer probes: %w", err)
		}
		o.probe, o.probeDone = ln, make(chan struct{})
		go func() { defer close(o.probeDone); acceptPeerProbes(ln) }()
	}
	go o.forward()
	return o, nil
}

func (o *windowsOverlay) forward() {
	defer close(o.worker)
	for {
		pkt := o.link.ReadContext(o.life)
		if pkt.IsNil() {
			return
		}
		b := o.path.shared.pool.Get().(*[]byte)
		buf := (*b)[:cap(*b)]
		data := pkt.ToBuffer()
		n, _ := data.ReadAt(buf[packetHeadroom:], 0)
		data.Release()
		pkt.DecRef()
		if n == 0 || !o.path.shared.enqueueOverlayPacket(o.life, o.path, b, buf[:packetHeadroom+n], buf[packetHeadroom:packetHeadroom+n]) {
			o.path.shared.pool.Put(b)
		}
	}
}

func (o *windowsOverlay) deliver(packet []byte) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if !o.closed {
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(packet)})
		o.link.InjectInbound(ipv4.ProtocolNumber, pkt)
		pkt.DecRef()
	}
}

func (o *windowsOverlay) close() {
	o.mu.Lock()
	o.closed = true
	o.cancel()
	o.mu.Unlock()
	if o.probe != nil {
		o.probe.Close()
		<-o.probeDone
	}
	o.dials.Wait()
	o.stack.Destroy()
	o.link.Close()
	<-o.worker
}

func reserveOverlayPort(_ netip.Addr, proto byte) (uint16, func(), error) {
	kind := windows.SOCK_STREAM
	if proto == 17 {
		kind = windows.SOCK_DGRAM
	}
	sock, err := windows.Socket(windows.AF_INET, kind, int(proto))
	if err != nil {
		return 0, nil, err
	}
	release := func() { _ = windows.Closesocket(sock) }
	// A specific-address exclusive bind still allows a later wildcard bind.
	// Reserve the ephemeral port on every IPv4 interface instead.
	const soExclusiveAddrUse = ^0x0004 // ~SO_REUSEADDR in Winsock.
	if err := windows.SetsockoptInt(sock, windows.SOL_SOCKET, soExclusiveAddrUse, 1); err != nil {
		release()
		return 0, nil, err
	}
	if err := windows.Bind(sock, &windows.SockaddrInet4{}); err != nil {
		release()
		return 0, nil, err
	}
	addr, err := windows.Getsockname(sock)
	if err != nil {
		release()
		return 0, nil, err
	}
	return uint16(addr.(*windows.SockaddrInet4).Port), release, nil
}

func (o *windowsOverlay) reserve(key localPort) (localPort, *socketPath, error) {
	port, release, err := reserveOverlayPort(o.local, key.proto)
	if err != nil {
		return key, nil, fmt.Errorf("reserve overlay source port: %w", err)
	}
	key.port = port
	owner := &socketPath{path: o.path, release: release}
	if err := o.path.shared.track(key, owner); err != nil {
		release()
		return key, nil, err
	}
	return key, owner, nil
}

func (o *windowsOverlay) tcp(ctx context.Context, dst netip.AddrPort) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !dst.Addr().Is4() {
		return nil, fmt.Errorf("overlay TCP requires an IPv4 destination: %s", dst)
	}
	o.mu.RLock()
	if o.closed {
		o.mu.RUnlock()
		return nil, net.ErrClosed
	}
	key, owner, err := o.reserve(localPort{proto: 6, remote: dst})
	if err == nil {
		o.dials.Add(1)
	}
	o.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	defer o.dials.Done()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(o.life, cancel)
	defer cancel()
	defer stop()
	local := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(o.local.As4()), Port: key.port}
	remote := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(dst.Addr().As4()), Port: dst.Port()}
	c, err := o.dialTCP(ctx, owner, local, remote)
	if err != nil {
		o.path.shared.untrack(key, owner)
		return nil, err
	}
	return &windowsTCP{TCPConn: c, path: o.path, key: key, owner: owner}, nil
}

func (o *windowsOverlay) dialTCP(ctx context.Context, owner *socketPath, local, remote tcpip.FullAddress) (*gonet.TCPConn, error) {
	var wq waiter.Queue
	ep, err := o.stack.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if err != nil {
		return nil, fmt.Errorf("create overlay TCP endpoint: %s", err)
	}
	o.path.shared.mu.Lock()
	owner.retired = func() bool {
		state := tcp.EndpointState(ep.State())
		return state == tcp.StateClose || state == tcp.StateError
	}
	o.path.shared.mu.Unlock()
	if err := ep.Bind(local); err != nil {
		ep.Close()
		return nil, fmt.Errorf("bind overlay TCP endpoint: %s", err)
	}
	entry, ready := waiter.NewChannelEntry(waiter.WritableEvents)
	wq.EventRegister(&entry)
	defer wq.EventUnregister(&entry)
	if err = ep.Connect(remote); err != nil {
		if _, pending := err.(*tcpip.ErrConnectStarted); pending {
			select {
			case <-ctx.Done():
				ep.Close()
				return nil, ctx.Err()
			case <-ready:
				err = ep.LastError()
			}
		}
		if err != nil {
			ep.Close()
			if _, refused := err.(*tcpip.ErrConnectionRefused); refused {
				return nil, fmt.Errorf("connect overlay TCP endpoint: %w", syscall.ECONNREFUSED)
			}
			return nil, fmt.Errorf("connect overlay TCP endpoint: %s", err)
		}
	}
	return gonet.NewTCPConn(&wq, ep), nil
}

func (o *windowsOverlay) udp(ctx context.Context) (net.PacketConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.closed {
		return nil, net.ErrClosed
	}
	key, owner, err := o.reserve(localPort{proto: 17})
	if err != nil {
		return nil, err
	}
	local := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(o.local.As4()), Port: key.port}
	var wq waiter.Queue
	ep, endpointErr := o.stack.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if endpointErr != nil {
		o.path.shared.untrack(key, owner)
		return nil, fmt.Errorf("create overlay UDP endpoint: %s", endpointErr)
	}
	// Without IP_RECVERR gVisor emulates Linux and suppresses ENOBUFS.
	ep.SocketOptions().SetIPv4RecvError(true)
	if bindErr := ep.Bind(local); bindErr != nil {
		ep.Close()
		o.path.shared.untrack(key, owner)
		return nil, fmt.Errorf("bind overlay UDP endpoint: %s", bindErr)
	}
	c := gonet.NewUDPConn(o.stack, &wq, ep)
	return &windowsUDP{UDPConn: c, path: o.path, key: key, owner: owner}, nil
}

type windowsTCP struct {
	*gonet.TCPConn
	path  *tunPath
	key   localPort
	owner *socketPath
}

func (c *windowsTCP) Close() error {
	err := c.TCPConn.Close()
	c.path.shared.untrack(c.key, c.owner)
	return err
}

type windowsUDP struct {
	*gonet.UDPConn
	path  *tunPath
	key   localPort
	owner *socketPath
}

func (c *windowsUDP) Close() error {
	err := c.UDPConn.Close()
	c.path.shared.untrack(c.key, c.owner)
	return err
}
