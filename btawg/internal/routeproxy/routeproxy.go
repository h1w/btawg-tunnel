// Package routeproxy carries SOCKS sessions across already authenticated
// overlay TCP links. The caller owns peer authentication and route grants.
package routeproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"bittorrent-tunnel/btawg/internal/socks"
)

// Request identifies an authorized route and the hop expected to accept it.
// Hop is zero-based and Grant is opaque to the data protocol.
type Request struct {
	RouteID string
	Grant   string
	Hop     int
}

// OpenFunc dials the known authenticated overlay TCP endpoint for the
// indicated hop. It must honor the setup context without tying the returned
// connection's lifetime to it. The protocol writes the request itself.
type OpenFunc func(context.Context, Request) (net.Conn, error)

// Decision describes this hop's next step and the authorization lifetime.
// A nil Next identifies the final exit. Fallback, if set, must reach the
// same Next hop using another transport, and is used only before the
// invocation is acknowledged. Life must be non-nil and independent of
// the bounded AuthorizeFunc context. Release, if set, is called when the
// session or setup attempt ends.
// AllowDestination, if set, can only further restrict final-hop IPv4
// destinations. It must be safe to call concurrently and may change its
// answer for active UDP associations. Revoke Life to close existing TCP.
type Decision struct {
	Next             OpenFunc
	Fallback         OpenFunc
	Life             context.Context
	Release          func()
	AllowDestination func(netip.Addr) bool
}

// AuthorizeFunc validates the grant and hop against the actual previous peer
// from the authenticated listener. It must honor the bounded setup context.
type AuthorizeFunc func(context.Context, netip.Addr, Request) (Decision, error)

const (
	handshakeTimeout = 10 * time.Second
	primaryTimeout   = 3 * time.Second
	maxSessions      = 128
)

var (
	errRouteUnavailable = errors.New("route proxy: route unavailable")
	errUnacknowledged   = errors.New("route proxy: invocation not acknowledged")
)

// Serve runs the route service on an authenticated overlay TCP listener.
// Cancellation closes the listener and all sessions.
func Serve(ctx context.Context, ln net.Listener, auth AuthorizeFunc) error {
	return ServeWithBlockedPrefixes(ctx, ln, auth, nil)
}

// ServeWithBlockedPrefixes additionally denies destinations in the overlay's
// configured prefixes, including globally numbered userspace overlay routes.
func ServeWithBlockedPrefixes(ctx context.Context, ln net.Listener, auth AuthorizeFunc, blocked []netip.Prefix) error {
	return serve(ctx, ln, auth, exitPolicy{blocked: append([]netip.Prefix(nil), blocked...)})
}

func serve(ctx context.Context, ln net.Listener, auth AuthorizeFunc, policy exitPolicy) error {
	ctx, cancel := context.WithCancel(ctx)
	var active sync.WaitGroup
	defer func() {
		cancel()
		_ = ln.Close()
		active.Wait()
	}()
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	if auth == nil {
		return errRouteUnavailable
	}
	limit := make(chan struct{}, maxSessions)
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case limit <- struct{}{}:
			active.Add(1)
			go func() {
				defer active.Done()
				defer func() { <-limit }()
				serveConn(ctx, c, auth, policy)
			}()
		default:
			_ = c.Close()
		}
	}
}

func serveConn(ctx context.Context, c net.Conn, auth AuthorizeFunc, policy exitPolicy) {
	defer c.Close()
	stop := closeOnCancel(ctx, c)
	defer stop()
	setup, stopSetup := context.WithTimeout(ctx, handshakeTimeout)
	defer stopSetup()
	deadline, _ := setup.Deadline()
	if err := c.SetDeadline(deadline); err != nil {
		return
	}
	stopSetupIO := closeOnCancel(setup, c)
	defer stopSetupIO()

	q, err := readCommand(c)
	if err != nil {
		_ = writeBytes(c, []byte{statusBad})
		return
	}
	peer, ok := c.RemoteAddr().(*net.TCPAddr)
	if !ok || peer == nil || peer.Port <= 0 || peer.Port > 65535 {
		_ = writeBytes(c, []byte{statusBad})
		return
	}
	previous := peer.AddrPort().Addr().Unmap()
	if !previous.Is4() || previous.IsUnspecified() {
		_ = writeBytes(c, []byte{statusBad})
		return
	}
	if writeBytes(c, []byte{statusReceived}) != nil {
		return
	}
	var confirmation [1]byte
	if _, err := io.ReadFull(c, confirmation[:]); err != nil || confirmation[0] != statusProceed || setup.Err() != nil {
		return
	}
	decision, err := auth(setup, previous, q.req)
	if decision.Release != nil {
		defer decision.Release()
	}
	if err != nil || decision.Life == nil || decision.Life.Err() != nil ||
		(q.req.Hop == 1 && decision.Next != nil) || (decision.Next == nil && decision.Fallback != nil) {
		_ = writeBytes(c, []byte{statusBad})
		return
	}

	// Life cancellation is synchronous on the context used by dial and UDP
	// policy. Closing c also propagates revocation through an active cascade.
	life, end := context.WithCancel(decision.Life)
	defer end()
	stop = context.AfterFunc(ctx, end)
	defer stop()
	stop = closeOnCancel(life, c)
	defer stop()
	dial, stopDial := context.WithDeadline(life, deadline)
	defer stopDial()
	if ctx.Err() != nil || dial.Err() != nil {
		return
	}

	policy.allowDestination = decision.AllowDestination
	var peerConn net.Conn
	var udp *net.UDPConn
	switch {
	case decision.Next != nil:
		q.req.Hop++
		peerConn, err = openSession(dial, decision.Next, decision.Fallback, q)
	case q.mode == modeTCP:
		peerConn, err = policy.dialTCP(dial, q.dst)
	case q.mode == modeUDP:
		udp, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	}
	if peerConn != nil {
		defer peerConn.Close()
		stop = closeOnCancel(life, peerConn)
		defer stop()
	}
	if udp != nil {
		defer udp.Close()
	}
	if err != nil || dial.Err() != nil {
		_ = writeBytes(c, []byte{statusBad})
		return
	}
	if !stopSetupIO() || setup.Err() != nil || life.Err() != nil || ctx.Err() != nil {
		return
	}
	// The original socket deadline still bounds the readiness reply.
	if writeBytes(c, []byte{statusOK}) != nil || c.SetDeadline(time.Time{}) != nil {
		return
	}
	stopSetup()
	stopDial()
	if udp != nil {
		policy.pumpUDP(life, c, udp)
	} else {
		socks.RelayTCP(life, c, peerConn)
	}
}

// Proxy builds a local SOCKS server pinned to one route. Each CONNECT or UDP
// ASSOCIATE establishes its own first overlay hop before reporting readiness.
func Proxy(open OpenFunc, req Request) *socks.Server {
	return ProxyWithFallback(open, nil, req)
}

// ProxyWithFallback is Proxy with an optional alternate transport for the
// same authenticated overlay endpoint. It never retries once the peer
// acknowledges the invocation, including explicit refusal or readiness.
func ProxyWithFallback(primary, fallback OpenFunc, req Request) *socks.Server {
	return proxy(primary, fallback, req, publicIPv4)
}

// Proxies may be constructed per SOCKS request, so admission is shared
// across them within the originating process.
var originSlots = make(chan struct{}, maxSessions)

func proxy(primary, fallback OpenFunc, req Request, allowReply func(netip.Addr) bool) *socks.Server {
	start := func(ctx context.Context, q command) (net.Conn, error) {
		select {
		case originSlots <- struct{}{}:
		default:
			return nil, errRouteUnavailable
		}
		c, err := startSession(ctx, primary, fallback, q)
		if err != nil {
			<-originSlots
			return nil, err
		}
		return &admittedConn{Conn: c}, nil
	}
	return socks.NewNamedUDPServer(func(ctx context.Context, _, addr string) (net.Conn, error) {
		host, portString, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		port, err := strconv.ParseUint(portString, 10, 16)
		if err != nil {
			return nil, err
		}
		return start(ctx, command{req: req, mode: modeTCP, dst: destination{host: host, port: uint16(port)}})
	}, func(ctx context.Context) (socks.NamedUDPConn, error) {
		c, err := start(ctx, command{req: req, mode: modeUDP})
		if err != nil {
			return nil, err
		}
		return &udpStream{Conn: c, allowReply: allowReply}, nil
	})
}

type admittedConn struct {
	net.Conn
	once sync.Once
}

func (c *admittedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { <-originSlots })
	return err
}

func (c *admittedConn) CloseWrite() error { return closeWrite(c.Conn) }

// Forward io.Copy fast paths (not part of the net.Conn interface) to the
// actual socket, including kernel splice for TCP where the OS supports it.
func (c *admittedConn) ReadFrom(r io.Reader) (int64, error) { return io.Copy(c.Conn, r) }
func (c *admittedConn) WriteTo(w io.Writer) (int64, error)  { return io.Copy(w, c.Conn) }

func startSession(ctx context.Context, primary, fallback OpenFunc, q command) (net.Conn, error) {
	if q.req.Hop != 0 {
		return nil, errInvalidFrame
	}
	setup, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	return openSession(setup, primary, fallback, q)
}

func openSession(ctx context.Context, primary, fallback OpenFunc, q command) (net.Conn, error) {
	if !q.valid() {
		return nil, errInvalidFrame
	}
	if fallback == nil {
		return attemptSession(ctx, ctx, primary, q)
	}
	try, cancel := context.WithTimeout(ctx, primaryTimeout)
	c, err := attemptSession(try, ctx, primary, q)
	cancel()
	if errors.Is(err, errUnacknowledged) && ctx.Err() == nil {
		return attemptSession(ctx, ctx, fallback, q)
	}
	return c, err
}

func attemptSession(transport, setup context.Context, open OpenFunc, q command) (net.Conn, error) {
	if open == nil || transport.Err() != nil {
		return nil, errUnacknowledged
	}
	c, err := open(transport, q.req)
	if err != nil || c == nil {
		if c != nil {
			_ = c.Close()
		}
		return nil, errUnacknowledged
	}
	ready := false
	defer func() {
		if !ready {
			_ = c.Close()
		}
	}()
	stopTransport := closeOnCancel(transport, c)
	defer stopTransport()
	deadline, _ := transport.Deadline()
	if c.SetDeadline(deadline) != nil || q.writeTo(c) != nil {
		return nil, errUnacknowledged
	}
	var status [1]byte
	if _, err := io.ReadFull(c, status[:]); err != nil {
		return nil, errUnacknowledged
	}
	// Once any acknowledgment is seen, no repeat authorization or WAN
	// operation may be attempted via the other transport.
	if status[0] != statusReceived || !stopTransport() || setup.Err() != nil {
		return nil, errRouteUnavailable
	}
	stopSetup := closeOnCancel(setup, c)
	defer stopSetup()
	deadline, _ = setup.Deadline()
	if c.SetDeadline(deadline) != nil || writeBytes(c, []byte{statusProceed}) != nil {
		return nil, errRouteUnavailable
	}
	if _, err := io.ReadFull(c, status[:]); err != nil || status[0] != statusOK || !stopSetup() || setup.Err() != nil {
		return nil, errRouteUnavailable
	}
	if c.SetDeadline(time.Time{}) != nil {
		return nil, errRouteUnavailable
	}
	ready = true
	return c, nil
}

func closeOnCancel(ctx context.Context, c net.Conn) func() bool {
	return context.AfterFunc(ctx, func() { _ = c.Close() })
}
