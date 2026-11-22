// Package nativepeer establishes authenticated direct paths for unwrapped UDP datagrams.
package nativepeer

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"bittorrent-tunnel/btawg/internal/carrier"
	"bittorrent-tunnel/btawg/internal/tunsetup"
)

const (
	magic      = "\x00BTNAT1:"
	packetSize = 8 + 1 + 16*3 + 32
	interval   = 150 * time.Millisecond
	maxPaths   = 64
)

type token [16]byte

type Offer struct {
	Secret     [32]byte
	Initiator  bool
	Candidates []netip.AddrPort
	Expires    time.Time
}

type Socket struct {
	v4, v6    *net.UDPConn
	underlay4 net.PacketConn
	underlay6 net.PacketConn
	port      uint16
	mu        sync.Mutex
	closed    bool
	attempts  map[token]*attempt
	paths     map[netip.AddrPort]*transport
	observers map[token]observation
	done      chan struct{}
	closeOnce sync.Once
	workers   sync.WaitGroup
}

type path struct {
	addr                     netip.AddrPort
	local, peer              token
	listed, proved, dialable bool
}
type attempt struct {
	key       [32]byte
	id        token
	initiator bool
	paths     map[netip.AddrPort]*path
	order     []*path
	chosen    *path
	result    chan *transport
}
type observation struct {
	source netip.AddrPort
	result chan netip.AddrPort
}

type transport struct {
	s    *Socket
	addr netip.AddrPort
	recv chan []byte
	done chan struct{}
	once sync.Once
	// Responder remembers the completed challenge to acknowledge lost nominations.
	key             [32]byte
	id, local, peer token
	responder       bool
}

func Listen(port uint16) (*Socket, error) {
	v4, err := net.ListenUDP("udp4", &net.UDPAddr{Port: int(port)})
	if err != nil {
		return nil, fmt.Errorf("listen native UDP4: %w", err)
	}
	s := &Socket{v4: v4, port: v4.LocalAddr().(*net.UDPAddr).AddrPort().Port(), attempts: make(map[token]*attempt), paths: make(map[netip.AddrPort]*transport), observers: make(map[token]observation), done: make(chan struct{})}
	s.underlay4 = tunsetup.ProtectUnderlayPacket(v4)
	s.v6, _ = net.ListenUDP("udp6", &net.UDPAddr{Port: int(s.port)})
	if s.v6 != nil {
		s.underlay6 = tunsetup.ProtectUnderlayPacket(s.v6)
	}
	for _, conn := range []*net.UDPConn{s.v4, s.v6} {
		if conn != nil {
			s.workers.Add(1)
			go s.read(conn)
		}
	}
	return s, nil
}
func (s *Socket) Port() uint16 { return s.port }
func (s *Socket) family(ip netip.Addr) *net.UDPConn {
	if ip.Is4() {
		return s.v4
	}
	if ip.Is6() {
		return s.v6
	}
	return nil
}
func (s *Socket) writeTo(b []byte, addr netip.AddrPort) (int, error) {
	if addr.Addr().Unmap().Is4() {
		return s.underlay4.WriteTo(b, net.UDPAddrFromAddrPort(addr))
	}
	return s.underlay6.WriteTo(b, net.UDPAddrFromAddrPort(addr))
}
func normalized(a netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(a.Addr().Unmap(), a.Port())
}
func usable(a netip.AddrPort) bool {
	ip := a.Addr()
	return a.Port() != 0 && (ip.IsGlobalUnicast() || ip.IsLoopback() || ip.Is4() && ip.IsLinkLocalUnicast())
}
func (s *Socket) LocalCandidates() []netip.AddrPort {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var result []netip.AddrPort
	seen := make(map[netip.Addr]bool)
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 || !tunsetup.IsUnderlayInterface(iface.Index) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			prefix, err := netip.ParsePrefix(a.String())
			if err != nil {
				continue
			}
			ip := prefix.Addr().Unmap()
			if !ip.IsGlobalUnicast() || s.family(ip) == nil || seen[ip] {
				continue
			}
			seen[ip] = true
			result = append(result, netip.AddrPortFrom(ip, s.port))
		}
	}
	return result
}
func (s *Socket) Close() error {
	var result error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.done)
		for _, tr := range s.paths {
			tr.closeLocked()
		}
		s.mu.Unlock()
		result = s.v4.Close()
		if s.v6 != nil {
			result = errors.Join(result, s.v6.Close())
		}
		s.workers.Wait()
	})
	return result
}
func randomToken() (token, error) { var t token; _, err := rand.Read(t[:]); return t, err }
func digest(key [32]byte, domain string, msg []byte) [32]byte {
	h := hmac.New(sha256.New, key[:])
	_, _ = h.Write([]byte(domain))
	_, _ = h.Write(msg)
	var out [32]byte
	h.Sum(out[:0])
	return out
}
func (s *Socket) addPath(a *attempt, addr netip.AddrPort, listed bool) (*path, error) {
	if p := a.paths[addr]; p != nil {
		p.listed = p.listed || listed
		return p, nil
	}
	if len(a.order) >= maxPaths {
		return nil, errors.New("too many native paths")
	}
	nonce, err := randomToken()
	if err != nil {
		return nil, err
	}
	p := &path{addr: addr, local: nonce, listed: listed}
	a.paths[addr] = p
	a.order = append(a.order, p)
	return p, nil
}
func (s *Socket) Connect(ctx context.Context, offer Offer) (carrier.Transport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !time.Now().Before(offer.Expires) {
		return nil, context.DeadlineExceeded
	}
	if offer.Secret == ([32]byte{}) {
		return nil, errors.New("empty native offer secret")
	}
	ctx, cancel := context.WithDeadline(ctx, offer.Expires)
	defer cancel()
	key := digest(offer.Secret, "btawg/nativepeer/key/1", nil)
	selector := digest(key, "btawg/nativepeer/selector/1", nil)
	a := &attempt{key: key, id: token(selector[:16]), initiator: offer.Initiator, paths: make(map[netip.AddrPort]*path), result: make(chan *transport, 1)}
	for _, addr := range offer.Candidates {
		addr = normalized(addr)
		if !usable(addr) || s.family(addr.Addr()) == nil {
			continue
		}
		if _, err := s.addPath(a, addr, true); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, net.ErrClosed
	}
	if s.attempts[a.id] != nil {
		s.mu.Unlock()
		return nil, errors.New("native offer already active")
	}
	s.attempts[a.id] = a
	s.mu.Unlock()
	accepted := false
	defer func() {
		s.mu.Lock()
		if s.attempts[a.id] == a {
			delete(s.attempts, a.id)
		}
		if !accepted {
			select {
			case tr := <-a.result:
				tr.closeLocked()
			default:
			}
		}
		s.mu.Unlock()
	}()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		s.mu.Lock()
		for _, p := range a.order {
			if p.listed || p.proved {
				flags := byte(0)
				if a.initiator {
					flags = 1
				}
				if p.proved {
					flags |= 2
				}
				if a.chosen == p {
					flags |= 4
				}
				s.sendControl(a.key, a.id, p.local, p.peer, flags, p.addr)
			}
		}
		s.mu.Unlock()
		select {
		case tr := <-a.result:
			if ctx.Err() != nil {
				tr.Close()
				return nil, ctx.Err()
			}
			select {
			case <-s.done:
				tr.Close()
				return nil, net.ErrClosed
			default:
			}
			accepted = true
			return tr, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.done:
			return nil, net.ErrClosed
		case <-tick.C:
		}
	}
}
func (s *Socket) sendControl(key [32]byte, id, local, peer token, flags byte, addr netip.AddrPort) {
	var b [packetSize]byte
	copy(b[:8], magic)
	b[8] = flags
	copy(b[9:25], id[:])
	copy(b[25:41], local[:])
	copy(b[41:57], peer[:])
	tag := digest(key, "btawg/nativepeer/punch/1", b[8:57])
	copy(b[57:], tag[:])
	_, _ = s.writeTo(b[:], addr)
}
func (s *Socket) read(conn *net.UDPConn) {
	defer s.workers.Done()
	buf := make([]byte, 65535)
	for {
		n, addr, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		addr = normalized(addr)
		if !usable(addr) {
			continue
		}
		b := buf[:n]
		if s.control(b, addr) || s.observeReply(b, addr) {
			continue
		}
		s.mu.Lock()
		tr := s.paths[addr]
		if tr != nil {
			packet := append([]byte(nil), b...)
			select {
			case tr.recv <- packet:
			default:
			}
		}
		s.mu.Unlock()
	}
}
func (s *Socket) control(b []byte, addr netip.AddrPort) bool {
	if len(b) != packetSize || string(b[:8]) != magic {
		return false
	}
	// The reserved control framing never enters a native data stream,
	// including delayed packets from retired or unknown attempts.
	if b[8] > 7 {
		return true
	}
	id, sender, echo := token(b[9:25]), token(b[25:41]), token(b[41:57])
	if sender == (token{}) {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.attempts[id]
	if a == nil {
		tr := s.paths[addr]
		if tr == nil || !tr.responder || tr.id != id || b[8] != 7 || sender != tr.peer || echo != tr.local {
			return true
		}
		tag := digest(tr.key, "btawg/nativepeer/punch/1", b[8:57])
		if !hmac.Equal(b[57:], tag[:]) {
			return true
		}
		s.sendControl(tr.key, tr.id, tr.local, tr.peer, 6, addr)
		return true
	}
	if a.initiator == (b[8]&1 != 0) {
		return true
	}
	tag := digest(a.key, "btawg/nativepeer/punch/1", b[8:57])
	if !hmac.Equal(b[57:], tag[:]) {
		return true
	}
	p := a.paths[addr]
	if p == nil {
		var err error
		p, err = s.addPath(a, addr, false)
		if err != nil {
			return true
		}
	}
	proved := echo == p.local
	changed := p.peer != sender || (proved && !p.proved) || (proved && b[8]&2 != 0 && !p.dialable)
	if proved || !p.proved {
		p.peer = sender
		p.proved = proved
		p.dialable = proved && b[8]&2 != 0
	}
	flags := byte(0)
	if a.initiator {
		flags = 1
	}
	if p.proved {
		flags |= 2
	}
	if a.initiator && p.dialable && a.chosen == nil && s.paths[addr] == nil {
		a.chosen = p
	}
	if a.chosen == p {
		flags |= 4
	}
	if !a.initiator && b[8]&4 != 0 && p.dialable && s.paths[addr] == nil {
		tr := s.establish(a, p, true)
		s.sendControl(a.key, a.id, p.local, p.peer, 6, addr)
		select {
		case a.result <- tr:
		default:
		}
		return true
	}
	if a.initiator && a.chosen == p && b[8]&4 != 0 && p.dialable && s.paths[addr] == nil {
		tr := s.establish(a, p, false)
		select {
		case a.result <- tr:
		default:
		}
		return true
	}
	if changed {
		s.sendControl(a.key, a.id, p.local, p.peer, flags, addr)
	}
	return true
}
func (s *Socket) establish(a *attempt, p *path, responder bool) *transport {
	tr := &transport{s: s, addr: p.addr, recv: make(chan []byte, 2048), done: make(chan struct{}), key: a.key, id: a.id, local: p.local, peer: p.peer, responder: responder}
	s.paths[p.addr] = tr
	delete(s.attempts, a.id)
	return tr
}
func (t *transport) Send(b []byte) error {
	select {
	case <-t.done:
		return net.ErrClosed
	default:
	}
	_, err := t.s.writeTo(b, t.addr)
	return err
}
func (t *transport) Recv() <-chan []byte   { return t.recv }
func (t *transport) Done() <-chan struct{} { return t.done }
func (t *transport) Close()                { t.s.mu.Lock(); t.closeLocked(); t.s.mu.Unlock() }
func (t *transport) closeLocked() {
	t.once.Do(func() {
		if t.s.paths[t.addr] == t {
			delete(t.s.paths, t.addr)
		}
		close(t.done)
		close(t.recv)
	})
}
func (t *transport) Wait() { <-t.done }
