package server

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"bittorrent-tunnel/btawg/internal/mesh"
	"bittorrent-tunnel/btawg/internal/routeproxy"
	"bittorrent-tunnel/btawg/internal/socks"
	"bittorrent-tunnel/btawg/internal/tunsetup"
)

func (s *Server) serveRouteRelay(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	ln, err := s.meshListen(ctx, mesh.RelayPort)
	if err != nil {
		cancel()
		return err
	}
	defer ln.Close()
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	limit := make(chan struct{}, 128)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		select {
		case limit <- struct{}{}:
			wg.Go(func() { defer func() { <-limit }(); s.relayRoute(ctx, conn) })
		default:
			conn.Close()
		}
	}
}

func (s *Server) relayRoute(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if conn.SetDeadline(time.Now().Add(5*time.Second)) != nil {
		return
	}
	remote, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return
	}
	req, err := mesh.ReadRelay(conn)
	if err != nil {
		return
	}
	s.applyMu.Lock()
	caller, ok := s.meshClient(remote.AddrPort().Addr().Unmap())
	var next netip.Addr
	if ok {
		next, err = s.checkEgress(caller, req, time.Now(), true)
	}
	s.applyMu.Unlock()
	if !ok || err != nil {
		_, _ = conn.Write([]byte{1})
		return
	}
	dialCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var dst net.Conn
	if s.ns != nil {
		dst, err = s.ns.DialContextTCPAddrPort(dialCtx, netip.AddrPortFrom(next, mesh.EgressPort))
	} else {
		d := net.Dialer{Control: tunsetup.BindDevice(s.tunName)}
		dst, err = d.DialContext(dialCtx, "tcp4", net.JoinHostPort(next.String(), strconv.Itoa(mesh.EgressPort)))
	}
	if err != nil {
		_, _ = conn.Write([]byte{1})
		return
	}
	defer dst.Close()
	end := context.AfterFunc(ctx, func() { dst.Close() })
	defer end()
	if _, err := conn.Write([]byte{0}); err != nil {
		return
	}
	if conn.SetDeadline(time.Time{}) != nil {
		return
	}
	if conn.SetReadDeadline(time.Now().Add(5*time.Second)) != nil {
		return
	}
	invocation, header, err := routeproxy.ReadInvocation(conn)
	if err != nil || invocation.RouteID != req.ID || invocation.Grant != req.Grant || invocation.Hop != req.Hop {
		return
	}
	if dst.SetWriteDeadline(time.Now().Add(5*time.Second)) != nil {
		return
	}
	if n, err := dst.Write(header); err != nil || n != len(header) {
		return
	}
	if dst.SetWriteDeadline(time.Time{}) != nil {
		return
	}
	if conn.SetReadDeadline(time.Time{}) != nil {
		return
	}
	socks.RelayTCP(ctx, conn, dst)
}
