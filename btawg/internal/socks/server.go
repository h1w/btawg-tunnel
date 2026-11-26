// Package socks implements the client-side SOCKS5 proxy (CONNECT and UDP
// ASSOCIATE) and helpers for applications speaking to it.
package socks

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// DialFunc dials one TCP connection to the destination.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// DialUDPFunc returns an upstream UDP socket through which the caller sends
// raw datagrams to arbitrary destinations and receives raw datagrams back.
type DialUDPFunc func(ctx context.Context) (net.PacketConn, error)

// NamedUDPConn exchanges UDP payloads without resolving outbound names in
// the SOCKS server. Each reply carries the source IP and port.
type NamedUDPConn interface {
	ReadPacket([]byte) (int, netip.AddrPort, error)
	WritePacket(host string, port uint16, data []byte) error
	Close() error
}

// DialNamedUDPFunc opens an association and returns only after it is ready.
type DialNamedUDPFunc func(context.Context) (NamedUDPConn, error)

// Server is a SOCKS5 server with optional RFC 1929 authentication.
type Server struct {
	dial               DialFunc
	dialUDP            DialUDPFunc
	dialNamedUDP       DialNamedUDPFunc
	resolveUDP         func(context.Context, netip.Addr, string) (netip.Addr, error)
	selectUpstream     func() (*Server, context.Context)
	selectObserved     func(bool) (*Server, context.Context, func(bool, int64, time.Duration, time.Time))
	greetingMethod     byte
	username, password []byte
}

// NewServer builds a local SOCKS5 server with the given upstream dialers.
func NewServer(dial DialFunc, dialUDP DialUDPFunc) *Server {
	return &Server{dial: dial, dialUDP: dialUDP}
}

// NewNamedUDPServer builds a proxy that sends UDP names to its upstream.
func NewNamedUDPServer(dial DialFunc, dialUDP DialNamedUDPFunc) *Server {
	return &Server{dial: dial, dialNamedUDP: dialUDP}
}

// NewBalancer admits SOCKS requests using its own credentials, then selects
// one upstream for the whole TCP connection or UDP association.
func NewBalancer(selectUpstream func() (*Server, context.Context)) *Server {
	return &Server{selectUpstream: selectUpstream}
}

// NewObservedBalancer selects once per request and observes completed TCP directions.
func NewObservedBalancer(selectUpstream func(bool) (*Server, context.Context, func(bool, int64, time.Duration, time.Time))) *Server {
	return &Server{selectObserved: selectUpstream}
}

// WithCredentials requires username/password authentication on this server.
// Configure it before accepting connections. Empty credentials fail closed.
func (s *Server) WithCredentials(username, password string) *Server {
	s.greetingMethod = 2
	s.username, s.password = []byte(username), []byte(password)
	return s
}

// WithUDPResolver handles both IP literals and domain targets for UDP.
func (s *Server) WithUDPResolver(resolve func(context.Context, netip.Addr, string) (netip.Addr, error)) *Server {
	s.resolveUDP = resolve
	return s
}

// ServeConn handles one accepted SOCKS5 session. It owns c until the
// session ends and closes it when ctx is canceled.
func (s *Server) ServeConn(ctx context.Context, c net.Conn) {
	s.handle(ctx, c)
}

// Serve accepts SOCKS5 connections on ln until ctx is done.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	defer ln.Close()
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(ctx, c)
	}
}

func (s *Server) handle(ctx context.Context, c net.Conn) {
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	if !s.serveGreeting(c) {
		return
	}
	cmd, dst, err := readRequest(c)
	if err != nil {
		writeReply(c, replyGeneralFailure)
		return
	}
	var observe func(bool, int64, time.Duration, time.Time)
	if (s.selectUpstream != nil || s.selectObserved != nil) && (cmd == cmdConnect || cmd == cmdUDPAssociate) {
		var upstream *Server
		var life context.Context
		if s.selectObserved != nil {
			upstream, life, observe = s.selectObserved(cmd == cmdConnect)
		} else {
			upstream, life = s.selectUpstream()
		}
		if upstream == nil || life.Err() != nil {
			writeReply(c, replyHostUnreachable)
			return
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(life, cancel)
		defer stop()
		closeConn := context.AfterFunc(ctx, func() { _ = c.Close() })
		defer closeConn()
		s = upstream
	}
	switch cmd {
	case cmdConnect:
		s.handleConnect(ctx, c, dst, observe)
	case cmdUDPAssociate:
		s.handleUDPAssociate(ctx, c, dst)
	default:
		writeReply(c, replyCommandUnsupported)
	}
}

func (s *Server) handleConnect(ctx context.Context, c net.Conn, dst target, observe func(bool, int64, time.Duration, time.Time)) {
	upstream, err := s.dial(ctx, "tcp", dst.String())
	if err != nil {
		writeReply(c, replyHostUnreachable)
		return
	}
	defer upstream.Close()
	boundLoopbackBuffers(c)
	boundLoopbackBuffers(upstream)
	if err := writeReply(c, replySucceeded); err != nil {
		return
	}
	relayTCP(ctx, c, upstream, observe)
}

// RelayTCP copies both directions, propagates clean EOF as a write half-close,
// and closes both sockets on an I/O error or cancellation.
func RelayTCP(ctx context.Context, client, upstream net.Conn) {
	relayTCP(ctx, client, upstream, nil)
}

func relayTCP(ctx context.Context, client, upstream net.Conn, observe func(bool, int64, time.Duration, time.Time)) {
	closeBoth := func() {
		_ = upstream.Close()
		_ = client.Close()
	}
	stop := context.AfterFunc(ctx, closeBoth)
	defer stop()
	if observe != nil {
		callback := observe
		observe = func(up bool, n int64, duration time.Duration, at time.Time) {
			if ctx.Err() == nil {
				callback(up, n, duration, at)
			}
		}
	}
	copyOne := func(dst, src net.Conn, upload bool) {
		var err error
		if observe == nil {
			_, err = io.Copy(dst, src)
		} else {
			err = copyObserved(dst, src, upload, observe)
		}
		if err == nil {
			if tcp, ok := dst.(interface{ CloseWrite() error }); ok && tcp.CloseWrite() == nil {
				return
			}
		}
		// For connections without half-close, preserve the old full-close
		// behavior rather than leave the opposite copy blocked indefinitely.
		closeBoth()
	}
	done := make(chan struct{})
	go func() { copyOne(upstream, client, true); close(done) }()
	copyOne(client, upstream, false)
	<-done
}

func copyObserved(dst io.Writer, src io.Reader, upload bool, observe func(bool, int64, time.Duration, time.Time)) error {
	r := &transferReader{Reader: src, upload: upload}
	w := &transferWriter{Writer: dst, reader: r}
	n, err := io.Copy(w, r)
	if err == nil && !r.idle && n > 0 {
		duration := r.duration
		if upload {
			duration += w.duration
		}
		// A lone long gap is indistinguishable from an idle application;
		// repeated stalls still contribute to a sustained download sample.
		if !upload && r.maxGap > time.Second && duration < 2*r.maxGap {
			return nil
		}
		observe(upload, n, duration, r.lastPayload)
	}
	return err
}

// Upload pauses may be application think time. Download gaps can instead be
// carrier stalls and must remain in the measured interval.
type transferReader struct {
	io.Reader
	upload            bool
	last, lastPayload time.Time
	duration          time.Duration
	maxGap            time.Duration
	idle              bool
}

func (r *transferReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		now := time.Now()
		if !r.last.IsZero() {
			d := now.Sub(r.last)
			if d > r.maxGap {
				r.maxGap = d
			}
			if r.upload && d > time.Second {
				r.idle = true
			}
			r.duration += d
		}
		r.last, r.lastPayload = now, now
	}
	return n, err
}

type transferWriter struct {
	io.Writer
	reader   *transferReader
	duration time.Duration
}

func (w *transferWriter) Write(p []byte) (int, error) {
	start := time.Now()
	n, err := w.Writer.Write(p)
	d := time.Since(start)
	w.duration += d
	w.reader.last = w.reader.last.Add(d)
	if n > 0 {
		w.reader.lastPayload = time.Now()
	}
	return n, err
}

// Loopback has negligible bandwidth-delay product. Large autotuned buffers
// there compete for the same TCP memory as the actual WAN carrier.
func boundLoopbackBuffers(c net.Conn) {
	tcp, ok := c.(*net.TCPConn)
	if !ok || !tcp.LocalAddr().(*net.TCPAddr).IP.IsLoopback() || !tcp.RemoteAddr().(*net.TCPAddr).IP.IsLoopback() {
		return
	}
	const loopbackBuffer = 192 << 10
	// These are performance hints; a socket stays usable without them.
	_ = tcp.SetReadBuffer(loopbackBuffer)
	_ = tcp.SetWriteBuffer(loopbackBuffer)
}

func (s *Server) handleUDPAssociate(ctx context.Context, c net.Conn, _ target) {
	if s.dialUDP == nil && s.dialNamedUDP == nil {
		writeReply(c, replyCommandUnsupported)
		return
	}
	peer, ok := c.RemoteAddr().(*net.TCPAddr)
	if !ok {
		writeReply(c, replyGeneralFailure)
		return
	}
	relay, err := listenUDPFor(c.LocalAddr())
	if err != nil {
		writeReply(c, replyGeneralFailure)
		return
	}
	defer relay.Close()
	var upstream net.PacketConn
	var named NamedUDPConn
	if s.dialNamedUDP != nil {
		named, err = s.dialNamedUDP(ctx)
	} else {
		upstream, err = s.dialUDP(ctx)
	}
	if err != nil {
		writeReply(c, replyGeneralFailure)
		return
	}
	var closeOnce sync.Once
	closeUpstream := func() {
		closeOnce.Do(func() {
			if named != nil {
				_ = named.Close()
			} else {
				_ = upstream.Close()
			}
		})
	}
	defer closeUpstream()
	if err := writeUDPReply(c, relay.LocalAddr()); err != nil {
		return
	}
	s.pumpUDP(ctx, relay, upstream, named, c, peer.AddrPort().Addr().Unmap(), closeUpstream)
}

func (s *Server) pumpUDP(ctx context.Context, relay *net.UDPConn, upstream net.PacketConn, named NamedUDPConn, ctrl net.Conn, peer netip.Addr, closeUpstream func()) {
	type association struct{ addr netip.AddrPort }
	var appAddr atomic.Pointer[association]
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(ctx, func() {
		_ = ctrl.Close()
		_ = relay.Close()
		closeUpstream()
	})
	defer stop()
	ctrlDone := make(chan struct{})
	go func() {
		defer close(ctrlDone)
		_, _ = io.Copy(io.Discard, ctrl)
		cancel()
	}()
	readDone := make(chan struct{})
	defer func() {
		cancel()
		<-readDone
		<-ctrlDone
	}()
	go func() {
		defer close(readDone)
		defer cancel()
		// Reserve header space so a reply does not require copying its payload.
		const head = 22
		buf := make([]byte, head+65536)
		for {
			var n int
			var err error
			var src netip.AddrPort
			if named != nil {
				n, src, err = named.ReadPacket(buf[head:])
				if err == nil && (!src.IsValid() || src.Port() == 0) {
					continue
				}
			} else {
				var raw net.Addr
				n, raw, err = upstream.ReadFrom(buf[head:])
				src = udpAddrPort(raw)
			}
			if err != nil {
				return
			}
			app := appAddr.Load()
			if app == nil {
				continue
			}
			ip := src.Addr().Unmap()
			size := head
			if ip.Is4() {
				size = 10
			}
			start := head - size
			_ = appendUDPHeader(buf[start:start], ip, src.Port())
			if _, err := relay.WriteToUDPAddrPort(buf[start:head+n], app.addr); err != nil {
				return
			}
		}
	}()
	buf := make([]byte, 65536)
	for {
		n, app, err := relay.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		app = netip.AddrPortFrom(app.Addr().Unmap(), app.Port())
		if app.Addr() != peer {
			continue
		}
		if last := appAddr.Load(); last != nil && last.addr != app {
			continue
		}
		dst, data, err := parseUDPTarget(buf[:n])
		if err != nil || dst.port == 0 {
			continue
		}
		if named != nil {
			if appAddr.Load() == nil {
				appAddr.Store(&association{app})
			}
			host := dst.host
			if dst.ip.IsValid() {
				host = dst.ip.String()
			}
			_ = named.WritePacket(host, dst.port, data)
			continue
		}
		ip := dst.ip
		if s.resolveUDP != nil {
			ip, err = s.resolveUDP(ctx, ip, dst.host)
		}
		if err != nil || !ip.IsValid() {
			continue
		}
		if appAddr.Load() == nil {
			appAddr.Store(&association{app})
		}
		// A failed DNS lookup or unreachable destination is per datagram.
		_, _ = upstream.WriteTo(data, net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, dst.port)))
	}
}

// serveGreeting selects only the configured method, even if a client offers
// both no authentication and RFC 1929 authentication.
func (s *Server) serveGreeting(c net.Conn) bool {
	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil || hdr[0] != socksVersion {
		return false
	}
	var methods [255]byte
	if _, err := io.ReadFull(c, methods[:hdr[1]]); err != nil {
		return false
	}
	for _, m := range methods[:hdr[1]] {
		if m == s.greetingMethod {
			_, err := c.Write([]byte{socksVersion, m})
			return err == nil && (m == 0 || s.authenticate(c))
		}
	}
	_, _ = c.Write([]byte{socksVersion, 0xff})
	return false
}

func (s *Server) authenticate(c net.Conn) bool {
	var hdr [2]byte
	var user, pass [255]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil || hdr[0] != 1 || hdr[1] == 0 {
		return false
	}
	username := user[:hdr[1]]
	if _, err := io.ReadFull(c, username); err != nil {
		return false
	}
	if _, err := io.ReadFull(c, hdr[:1]); err != nil || hdr[0] == 0 {
		return false
	}
	password := pass[:hdr[0]]
	if _, err := io.ReadFull(c, password); err != nil {
		return false
	}
	match := subtle.ConstantTimeCompare(username, s.username) & subtle.ConstantTimeCompare(password, s.password)
	_, err := c.Write([]byte{1, byte(1 - match)})
	return err == nil && match == 1
}

const (
	socksVersion            = 5
	cmdConnect              = 0x01
	cmdUDPAssociate         = 0x03
	replySucceeded          = 0x00
	replyGeneralFailure     = 0x01
	replyHostUnreachable    = 0x04
	replyCommandUnsupported = 0x07
	atypIPv4                = 0x01
	atypDomain              = 0x03
	atypIPv6                = 0x04
)

// request is one parsed SOCKS5 request.
type request struct {
	cmd byte
	dst target
}

// target is the parsed destination of a SOCKS5 request: an IP or a domain
// plus a port. String is the host:port form the dialer receives.
type target struct {
	host string
	port int
}

func (t target) String() string { return net.JoinHostPort(t.host, strconv.Itoa(t.port)) }
func readRequest(c net.Conn) (byte, target, error) {
	var head [4]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return 0, target{}, err
	}
	if head[0] != socksVersion {
		return 0, target{}, fmt.Errorf("bad SOCKS version %d", head[0])
	}
	host, err := readAddr(c, head[3])
	if err != nil {
		return 0, target{}, err
	}
	var portBuf [2]byte
	if _, err := io.ReadFull(c, portBuf[:]); err != nil {
		return 0, target{}, err
	}
	port := binary.BigEndian.Uint16(portBuf[:])
	return head[1], target{host: host, port: int(port)}, nil
}

// readAddr reads a SOCKS5 address field following the given ATYP and returns
// it as a host string: IP text, or the domain itself.
func readAddr(c net.Conn, atyp byte) (string, error) {
	switch atyp {
	case atypIPv4:
		var ip [4]byte
		if _, err := io.ReadFull(c, ip[:]); err != nil {
			return "", err
		}
		return net.IP(ip[:]).String(), nil
	case atypIPv6:
		var ip [16]byte
		if _, err := io.ReadFull(c, ip[:]); err != nil {
			return "", err
		}
		return net.IP(ip[:]).String(), nil
	case atypDomain:
		var lenBuf [1]byte
		if _, err := io.ReadFull(c, lenBuf[:]); err != nil {
			return "", err
		}
		name := make([]byte, lenBuf[0])
		if _, err := io.ReadFull(c, name); err != nil {
			return "", err
		}
		return string(name), nil
	default:
		return "", fmt.Errorf("unsupported ATYP %d", atyp)
	}
}

func writeReply(c net.Conn, rep byte) error {
	// VER REP RSV ATYP=IPv4 BND.ADDR=0.0.0.0 BND.PORT=0
	_, err := c.Write([]byte{socksVersion, rep, 0, atypIPv4, 0, 0, 0, 0, 0, 0})
	return err
}

func writeUDPReply(c net.Conn, bind net.Addr) error {
	udp, ok := bind.(*net.UDPAddr)
	if !ok {
		return writeReply(c, replyGeneralFailure)
	}
	ip := udp.IP.To4()
	atyp := byte(atypIPv4)
	if ip == nil {
		ip = udp.IP.To16()
		atyp = atypIPv6
	}
	b := []byte{socksVersion, replySucceeded, 0, atyp}
	b = append(b, ip...)
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], uint16(udp.Port))
	b = append(b, p[:]...)
	_, err := c.Write(b)
	return err
}

// listenUDPFor binds a UDP relay socket on the same host as the TCP listener.
func listenUDPFor(local net.Addr) (*net.UDPConn, error) {
	host := "127.0.0.1"
	if tcp, ok := local.(*net.TCPAddr); ok && tcp.IP != nil && !tcp.IP.IsUnspecified() {
		host = tcp.IP.String()
	}
	return net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(host)})
}

// buildUDPHeader renders the SOCKS5 UDP header for addr: RSV(2) FRAG ATYP
// DST.ADDR DST.PORT.
func buildUDPHeader(addr net.Addr) []byte {
	var b [22]byte
	src := udpAddrPort(addr)
	return appendUDPHeader(b[:0], src.Addr(), src.Port())
}

func udpAddrPort(addr net.Addr) netip.AddrPort {
	var ip netip.AddrPort
	switch a := addr.(type) {
	case *net.UDPAddr:
		ip = a.AddrPort()
	case *net.TCPAddr:
		ip = a.AddrPort()
	}
	if !ip.IsValid() {
		return netip.AddrPortFrom(netip.IPv4Unspecified(), 0)
	}
	return netip.AddrPortFrom(ip.Addr().Unmap(), ip.Port())
}

func appendUDPHeader(b []byte, ip netip.Addr, port uint16) []byte {
	ip = ip.Unmap()
	if ip.Is4() {
		v4 := ip.As4()
		b = append(b, 0, 0, 0, atypIPv4)
		b = append(b, v4[:]...)
	} else {
		v6 := ip.As16()
		b = append(b, 0, 0, 0, atypIPv6)
		b = append(b, v6[:]...)
	}
	return binary.BigEndian.AppendUint16(b, port)
}

type udpTarget struct {
	ip   netip.Addr
	host string
	port uint16
}

// parseUDPTarget splits the header from the payload. SOCKS fragmentation is
// not supported; domain targets remain names until the caller resolves them.
func parseUDPTarget(b []byte) (udpTarget, []byte, error) {
	if len(b) < 4 || b[0] != 0 || b[1] != 0 || b[2] != 0 {
		return udpTarget{}, nil, fmt.Errorf("invalid SOCKS UDP header")
	}
	atyp := b[3]
	b = b[4:]
	var dst udpTarget
	switch atyp {
	case atypIPv4:
		if len(b) < 4 {
			return udpTarget{}, nil, io.ErrUnexpectedEOF
		}
		dst.ip, b = netip.AddrFrom4([4]byte(b[:4])), b[4:]
	case atypIPv6:
		if len(b) < 16 {
			return udpTarget{}, nil, io.ErrUnexpectedEOF
		}
		dst.ip, b = netip.AddrFrom16([16]byte(b[:16])).Unmap(), b[16:]
	case atypDomain:
		if len(b) == 0 || b[0] == 0 || len(b) < 1+int(b[0]) {
			return udpTarget{}, nil, io.ErrUnexpectedEOF
		}
		end := 1 + int(b[0])
		dst.host, b = string(b[1:end]), b[end:]
	default:
		return udpTarget{}, nil, fmt.Errorf("unsupported UDP address type %d", atyp)
	}
	if len(b) < 2 {
		return udpTarget{}, nil, io.ErrUnexpectedEOF
	}
	dst.port = binary.BigEndian.Uint16(b[:2])
	return dst, b[2:], nil
}
