// Package direct establishes authenticated peer-to-peer BitTorrent carriers.
package direct

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"bittorrent-tunnel/btawg/internal/carrier"
	"bittorrent-tunnel/btawg/internal/tunsetup"

	"github.com/anacrolix/go-utp/purego"
)

// Candidate is an advertised endpoint. Empty Protocol denotes UDP.
type Candidate struct {
	Addr     netip.AddrPort
	Kind     string
	Protocol string
}

// Offer is a short-lived grant for one pair. Both peers have the same secret
// and exactly one is Initiator. Expires limits establishment, not the lifetime
// of an already returned carrier, which must be closed by its owner on revocation.
type Offer struct {
	Secret     [32]byte
	Initiator  bool
	Candidates []Candidate
	Expires    time.Time
	Mode       string
}

// Socket owns the long-lived UDP endpoints and the streams established on them.
type Socket struct {
	v4, v6  *purego.Socket
	port    uint16
	tcp     net.Listener
	tcpPort uint16
	ctx     context.Context
	cancel  context.CancelFunc

	mu         sync.Mutex // Never call µTP while holding mu: its firewall takes mu.
	closed     bool
	attempts   map[token]*attempt
	observers  map[token]observation
	transports map[*ownedConn]carrier.Transport
	accepting  chan struct{}
	workers    sync.WaitGroup
	closeOnce  sync.Once
	closeErr   error
}

// Listen binds UDP4 and, where available, UDP6 to the same port on all local
// addresses. Zero tries the BitTorrent port range and then an ephemeral port.
func Listen(port uint16) (*Socket, error) { return ListenWithTCP(port, false) }

// ListenWithTCP optionally binds a separate TCP listener alongside the UDP socket.
func ListenWithTCP(port uint16, tcp bool) (*Socket, error) {
	pc, err := bind4(port)
	if err != nil {
		return nil, fmt.Errorf("listen on direct UDP4: %w", err)
	}
	v4, err := carrier.NewUTPSocket(pc)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Socket{
		v4:         v4,
		port:       pc.LocalAddr().(*net.UDPAddr).AddrPort().Port(),
		ctx:        ctx,
		cancel:     cancel,
		attempts:   make(map[token]*attempt),
		observers:  make(map[token]observation),
		transports: make(map[*ownedConn]carrier.Transport),
		accepting:  make(chan struct{}, 32),
	}
	if tcp {
		s.tcp, err = net.Listen("tcp", net.JoinHostPort("", fmt.Sprint(port)))
		if err == nil {
			s.tcpPort = uint16(s.tcp.Addr().(*net.TCPAddr).Port)
			s.workers.Go(func() { s.acceptTCP() })
		}
	}
	if pc6, err := net.ListenUDP("udp6", &net.UDPAddr{Port: int(s.port)}); err == nil {
		if s.v6, err = carrier.NewUTPSocket(pc6); err != nil {
			_ = pc6.Close()
		}
	}
	for _, f := range []*purego.Socket{s.v4, s.v6} {
		if f == nil {
			continue
		}
		f.SetFirewallCallback(s.rejectSYN)
		s.workers.Go(func() { s.readPackets(f) })
		s.workers.Go(func() { s.accept(f) })
	}
	return s, nil
}

func bind4(port uint16) (*net.UDPConn, error) {
	if port == 0 {
		for port := 6881; port <= 6889; port++ {
			if pc, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port}); err == nil {
				return pc, nil
			}
		}
	}
	return net.ListenUDP("udp4", &net.UDPAddr{Port: int(port)})
}

// Port is the bound UDP port, also used by the optional IPv6 socket.
func (s *Socket) Port() uint16 { return s.port }

// TCPPort returns the TCP listener port, or zero when TCP is disabled.
func (s *Socket) TCPPort() uint16 { return s.tcpPort }

// LocalCandidates enumerates current interface addresses, so callers may use
// it again following network changes. External mappings and observations are
// obtained separately. Never advertise an interface confined to this host.
func (s *Socket) LocalCandidates() []Candidate {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var result []Candidate
	seen := make(map[netip.Addr]bool)
	for _, intf := range interfaces {
		if intf.Flags&net.FlagUp == 0 || intf.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 || !tunsetup.IsUnderlayInterface(intf.Index) {
			continue
		}
		addrs, err := intf.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip netip.Addr
			switch v := addr.(type) {
			case *net.IPNet:
				ip, _ = netip.AddrFromSlice(v.IP)
			case *net.IPAddr:
				ip, _ = netip.AddrFromSlice(v.IP)
			}
			ip = ip.Unmap()
			kind := localKind(ip)
			if kind == "" || seen[ip] || s.familyFor(ip) == nil {
				continue
			}
			seen[ip] = true
			result = append(result, Candidate{Addr: netip.AddrPortFrom(ip, s.port), Kind: kind})
			if s.tcpPort != 0 {
				result = append(result, Candidate{Addr: netip.AddrPortFrom(ip, s.tcpPort), Kind: kind, Protocol: "tcp"})
			}
		}
	}
	return result
}

var sharedIPv4 = netip.MustParsePrefix("100.64.0.0/10")

func localKind(ip netip.Addr) string {
	switch {
	case ip.Is4() && (ip.IsPrivate() || ip.IsLinkLocalUnicast() || sharedIPv4.Contains(ip)):
		return "lan"
	case ip.Is4() && ip.IsGlobalUnicast():
		return "public"
	case ip.Is6() && ip.IsGlobalUnicast() && !ip.IsPrivate():
		return "ipv6"
	default:
		return ""
	}
}

func (s *Socket) familyFor(ip netip.Addr) *purego.Socket {
	if ip.Unmap().Is4() {
		return s.v4
	}
	if ip.Is6() {
		return s.v6
	}
	return nil
}

func remoteAddr(addr net.Addr) netip.AddrPort {
	if udp, ok := addr.(*net.UDPAddr); ok {
		return normalize(udp.AddrPort())
	}
	return netip.AddrPort{}
}

func normalize(addr netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
}

func usable(addr netip.AddrPort) bool {
	ip := addr.Addr()
	return addr.Port() != 0 && (ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() && ip.Is4())
}

// Close stops all pending operations and closes every carrier and UDP endpoint.
func (s *Socket) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		transports := s.transports
		s.transports = nil
		s.mu.Unlock()
		s.cancel()
		for _, tr := range transports {
			tr.Close()
		}
		s.closeErr = s.v4.Close()
		if s.v6 != nil {
			s.closeErr = errors.Join(s.closeErr, s.v6.Close())
		}
		if s.tcp != nil {
			s.closeErr = errors.Join(s.closeErr, s.tcp.Close())
		}
		for _, tr := range transports {
			tr.Wait()
		}
		s.workers.Wait()
	})
	return s.closeErr
}

// The carrier can close itself on read failure. Keep it registered while
// live without adding a goroutine solely to watch its Done channel.
type ownedConn struct {
	net.Conn
	s      *Socket
	closed bool // guarded by s.mu
}

func (c *ownedConn) Close() error {
	c.s.mu.Lock()
	c.closed = true
	delete(c.s.transports, c)
	c.s.mu.Unlock()
	return c.Conn.Close()
}

func (s *Socket) streamCarrier(ctx context.Context, c net.Conn, key [32]byte, framing string, dialer bool) (carrier.Transport, error) {
	var infoHash, peerID [20]byte
	hash := mac(key, "btawg/direct/infohash/1", nil)
	copy(infoHash[:], hash[:])
	random(peerID[:])
	owned := &ownedConn{Conn: c, s: s}
	var tr carrier.Transport
	var err error
	if framing == "req" {
		tr, _, err = carrier.NewRequestPiece(owned, infoHash, peerID, !dialer)
	} else {
		tr, _, err = carrier.NewExtended(owned, infoHash, peerID)
	}
	if err != nil {
		return nil, err
	}
	// Negotiation deadlines do not apply to the returned data stream.
	if err := owned.SetDeadline(time.Time{}); err != nil {
		tr.Wait()
		return nil, err
	}
	s.mu.Lock()
	if s.closed || owned.closed || ctx.Err() != nil {
		s.mu.Unlock()
		tr.Wait()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, net.ErrClosed
	}
	s.transports[owned] = tr
	s.mu.Unlock()
	return tr, nil
}
