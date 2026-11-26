package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"syscall"
	"time"

	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/hubrelay"
	"bittorrent-tunnel/btawg/internal/routeproxy"
	"bittorrent-tunnel/btawg/internal/socks"
	"bittorrent-tunnel/btawg/internal/tunsetup"
)

const maxTCPEgress = 128

type tcpEgressSession struct {
	source, gateway, target netip.Addr
	id, key                 string
	cancel                  context.CancelFunc
}

func (p *policySnapshot) tcpEgress(src, gateway, dst netip.Addr) (tcpEgressSession, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	rec, found := p.byIP[src]
	_, local := p.localIPs[dst]
	// Admission uses the current snapshot, not the import's claimed policy.

	if !found || !p.gateway(gateway) || !p.internet(rec) || !config.PublicIPv4(dst) || local || p.gateway(dst) || p.overlay(dst) {
		return tcpEgressSession{}, false
	}
	if _, peer := p.byIP[dst]; peer {
		return tcpEgressSession{}, false
	}
	return tcpEgressSession{source: src, gateway: gateway, target: dst, id: rec.ID, key: rec.PubKey}, true
}

// revalidateTCPEgress runs under applyMu, including after a failed reload.
func (s *Server) revalidateTCPEgress() {
	for session := range s.egress {
		now, ok := s.policy.tcpEgress(session.source, session.gateway, session.target)
		if !ok || now.id != session.id || now.key != session.key {
			session.cancel()
		}
	}
}

func (s *Server) serveTCPEgress(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	bind := tunsetup.BindDevice(s.tunName)
	lc := net.ListenConfig{Control: func(network, address string, raw syscall.RawConn) error {
		if err := bind(network, address, raw); err != nil {
			return err
		}
		return tunsetup.RelayTCP(network, address, raw)
	}}
	ln, err := lc.Listen(ctx, "tcp4", ":"+strconv.Itoa(hubrelay.Port))
	if err != nil {
		return fmt.Errorf("overlay TCP relay: %w", err)
	}
	defer ln.Close()
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	limit := make(chan struct{}, maxTCPEgress)
	sendBuffer := tunsetup.RelayWANSendBuffer()
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		select {
		case limit <- struct{}{}:
			wg.Go(func() {
				defer func() { <-limit }()
				s.handleTCPEgress(ctx, c, c.RemoteAddr().(*net.TCPAddr).AddrPort().Addr().Unmap(), c.LocalAddr().(*net.TCPAddr).AddrPort().Addr().Unmap(), s.dialTCPEgress, sendBuffer)
			})
		default:
			_ = c.Close()
		}
	}
}

func (s *Server) dialTCPEgress(ctx context.Context, target netip.AddrPort) (net.Conn, error) {
	s.applyMu.Lock()
	wan := s.kernel.WAN()
	s.applyMu.Unlock()
	d := net.Dialer{Control: tunsetup.BindDevice(wan)}
	return d.DialContext(ctx, "tcp4", target.String())
}

func (s *Server) handleTCPEgress(ctx context.Context, c net.Conn, src, gateway netip.Addr, dial func(context.Context, netip.AddrPort) (net.Conn, error), sendBuffer int) {
	defer c.Close()
	// Pin the identity before reading the request: a slow handshake must
	// not inherit permissions if the overlay address is reassigned.
	s.policy.mu.RLock()
	identity, known := s.policy.byIP[src]
	eligible := known && s.policy.gateway(gateway) && s.policy.internet(identity)
	s.policy.mu.RUnlock()
	if !eligible {
		return
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	deadline := time.Now().Add(hubrelay.Timeout)
	if c.SetDeadline(deadline) != nil {
		return
	}
	target, err := hubrelay.Read(c)
	if err != nil {
		return
	}
	if routeproxy.CheckPublicTCPDestination(target.Addr()) != nil {
		_ = hubrelay.Reply(c, hubrelay.Denied)
		return
	}
	life, cancel := context.WithCancel(ctx)
	defer cancel()
	s.applyMu.Lock()
	session, allowed := s.policy.tcpEgress(src, gateway, target.Addr())
	allowed = allowed && session.id == identity.ID && session.key == identity.PubKey
	if allowed {
		session.cancel = cancel
		if s.egress == nil {
			s.egress = make(map[*tcpEgressSession]struct{})
		}
		s.egress[&session] = struct{}{}
	}
	s.applyMu.Unlock()
	if !allowed {
		_ = hubrelay.Reply(c, hubrelay.Denied)
		return
	}
	defer func() {
		s.applyMu.Lock()
		delete(s.egress, &session)
		s.applyMu.Unlock()
	}()
	setup, endSetup := context.WithDeadline(life, deadline)
	upstream, err := dial(setup, target)
	endSetup()
	if err != nil {
		_ = hubrelay.Reply(c, hubrelay.Unreachable)
		return
	}
	defer upstream.Close()
	if life.Err() != nil || hubrelay.Reply(c, hubrelay.Ready) != nil || c.SetDeadline(time.Time{}) != nil {
		return
	}
	if tcp, ok := c.(*net.TCPConn); ok {
		tunsetup.TuneRelayTCP(life, tcp, sendBuffer)
	}
	socks.RelayTCP(life, c, upstream)
}
