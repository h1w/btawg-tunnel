// Package tunneldns provides a small DNS-over-TCP resolver used between the
// local SOCKS client and the hub. The client sends absolute A queries over a
// TCP connection inside the overlay; the hub answers them with its OS
// resolver. No client-side DNS is used.
package tunneldns

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Port is the dedicated overlay-only TCP listener port for DNS-over-TCP.
const Port = 1053

const defaultQueryTimeout = 10 * time.Second

// lookupNetIP is the OS resolver used by Serve. It is a package-level hook so
// tests can inject deterministic behaviour without touching public internet.
var lookupNetIP func(ctx context.Context, network, host string) ([]netip.Addr, error) = net.DefaultResolver.LookupNetIP

// Serve runs the hub-side DNS-over-TCP endpoint on ln until ctx is done.
//
// allowed reports whether a source may use the resolver. Denied sources are
// closed before any DNS bytes are read, so they receive no successful reply.
// Each connection carries one standard IN A query. The hub's OS resolver
// answers it; authorization is checked before lookup and before reply.
func Serve(ctx context.Context, ln net.Listener, allowed func(remote, local net.Addr) bool) error {
	defer ln.Close()

	var wg sync.WaitGroup
	active := make(map[net.Conn]struct{})
	var mu sync.Mutex
	const maxConnections = 128

	stop := context.AfterFunc(ctx, func() {
		ln.Close()
		mu.Lock()
		for c := range active {
			c.Close()
		}
		mu.Unlock()
	})
	defer stop()

	var acceptErr error
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				acceptErr = fmt.Errorf("tunneldns accept: %w", err)
			}
			break
		}
		canResolve := func() bool {
			return allowed == nil || allowed(conn.RemoteAddr(), conn.LocalAddr())
		}
		if !canResolve() {
			conn.Close()
			continue
		}
		mu.Lock()
		if ctx.Err() != nil || len(active) >= maxConnections {
			mu.Unlock()
			conn.Close()
			continue
		}
		active[conn] = struct{}{}
		mu.Unlock()
		wg.Go(func() {
			defer func() {
				mu.Lock()
				delete(active, conn)
				mu.Unlock()
				conn.Close()
			}()
			serviceConn(ctx, conn, canResolve)
		})
	}
	mu.Lock()
	for conn := range active {
		conn.Close()
	}
	mu.Unlock()
	wg.Wait()
	return acceptErr
}

func serviceConn(ctx context.Context, conn net.Conn, allowed func() bool) {
	ctx, cancel := context.WithTimeout(ctx, defaultQueryTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return
	}
	req, err := readTCPMessage(conn)
	if err != nil || !allowed() {
		return
	}
	_ = answerQuery(ctx, conn, req, allowed)
}

func answerQuery(ctx context.Context, conn net.Conn, req []byte, allowed func() bool) error {
	var p dnsmessage.Parser
	h, err := p.Start(req)
	if err != nil {
		return err
	}

	if h.Response || h.OpCode != 0 || h.Truncated {
		return errors.New("malformed DNS query")
	}

	questions, err := p.AllQuestions()
	if err != nil || len(questions) != 1 {
		return errors.New("malformed DNS query")
	}
	q := questions[0]
	if q.Type != dnsmessage.TypeA || q.Class != dnsmessage.ClassINET {
		return errors.New("unsupported DNS query")
	}

	addrs, err := lookupNetIP(ctx, "ip4", q.Name.String())

	var rcode dnsmessage.RCode
	switch {
	case err == nil:
		rcode = dnsmessage.RCodeSuccess
	case isNotFound(err):
		rcode = dnsmessage.RCodeNameError
	default:
		rcode = dnsmessage.RCodeServerFailure
	}

	resp := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:                 h.ID,
			Response:           true,
			OpCode:             0,
			RecursionDesired:   h.RecursionDesired,
			RecursionAvailable: true,
			RCode:              rcode,
		},
		Questions: []dnsmessage.Question{q},
	}

	if err == nil {
		for _, a := range addrs {
			a = a.Unmap()
			if !a.Is4() {
				continue
			}
			resp.Answers = append(resp.Answers, dnsmessage.Resource{
				Header: dnsmessage.ResourceHeader{
					Name:  q.Name,
					Type:  dnsmessage.TypeA,
					Class: dnsmessage.ClassINET,
					TTL:   300,
				},
				Body: &dnsmessage.AResource{A: a.As4()},
			})
		}
	}

	b, packErr := resp.Pack()
	if packErr != nil {
		return packErr
	}

	if !allowed() {
		return net.ErrClosed
	}
	return writeTCPMessage(conn, b)
}

func isNotFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

func readTCPMessage(conn net.Conn) ([]byte, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(lenBuf[:]))
	if n == 0 || n > 65535 {
		return nil, errors.New("invalid DNS-over-TCP length")
	}
	msg := make([]byte, n)
	if _, err := io.ReadFull(conn, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

func writeTCPMessage(conn net.Conn, msg []byte) error {
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(msg)))
	if _, err := conn.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err := conn.Write(msg)
	return err
}
