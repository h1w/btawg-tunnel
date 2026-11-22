package nativepeer

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// Observe asks the hub's direct observer to report this socket's actual source.
func (s *Socket) Observe(ctx context.Context, observer netip.AddrPort) (netip.AddrPort, error) {
	if err := ctx.Err(); err != nil {
		return netip.AddrPort{}, err
	}
	observer = normalized(observer)
	conn := s.family(observer.Addr())
	if !usable(observer) || conn == nil {
		return netip.AddrPort{}, fmt.Errorf("unsupported native observer endpoint %v", observer)
	}
	nonce, err := randomToken()
	if err != nil {
		return netip.AddrPort{}, err
	}
	ob := observation{source: observer, result: make(chan netip.AddrPort, 1)}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return netip.AddrPort{}, net.ErrClosed
	}
	s.observers[nonce] = ob
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.observers, nonce); s.mu.Unlock() }()
	var query [43]byte
	copy(query[:8], "\x00BTOBS1?")
	copy(query[8:24], nonce[:])
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if _, err := s.writeTo(query[:], observer); err != nil {
			select {
			case <-s.done:
				return netip.AddrPort{}, net.ErrClosed
			default:
				return netip.AddrPort{}, err
			}
		}
		select {
		case result := <-ob.result:
			if err := ctx.Err(); err != nil {
				return netip.AddrPort{}, err
			}
			select {
			case <-s.done:
				return netip.AddrPort{}, net.ErrClosed
			default:
				return result, nil
			}
		case <-ctx.Done():
			return netip.AddrPort{}, ctx.Err()
		case <-s.done:
			return netip.AddrPort{}, net.ErrClosed
		case <-tick.C:
		}
	}
}
func (s *Socket) observeReply(b []byte, source netip.AddrPort) bool {
	if len(b) != 43 || string(b[:8]) != "\x00BTOBS1!" {
		return false
	}
	s.mu.Lock()
	ob, ok := s.observers[token(b[8:24])]
	s.mu.Unlock()
	if !ok || ob.source != source {
		return false
	}
	var ip netip.Addr
	switch b[24] {
	case 4:
		ip = netip.AddrFrom4([4]byte(b[25:29]))
	case 6:
		ip = netip.AddrFrom16([16]byte(b[25:41]))
	default:
		return true
	}
	addr := netip.AddrPortFrom(ip.Unmap(), uint16(b[41])<<8|uint16(b[42]))
	if usable(addr) && addr.Addr().Is4() == source.Addr().Is4() {
		select {
		case ob.result <- addr:
		default:
		}
	}
	return true
}
