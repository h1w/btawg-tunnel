package tunneldns

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func runServer(t *testing.T, allowed func(remote, local net.Addr) bool) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = Serve(ctx, ln, allowed)
		close(done)
	}()
	return ln.Addr().String(), func() {
		cancel()
		<-done
	}
}

func setResolver(t *testing.T, fn func(ctx context.Context, network, host string) ([]netip.Addr, error)) {
	t.Helper()
	old := lookupNetIP
	lookupNetIP = fn
	t.Cleanup(func() { lookupNetIP = old })
}

func dialTCP(addr string) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
}

func TestLookupIPv4Success(t *testing.T) {
	want := []netip.Addr{
		netip.MustParseAddr("10.0.0.1"),
		netip.MustParseAddr("10.0.0.2"),
	}
	setResolver(t, func(_ context.Context, _, host string) ([]netip.Addr, error) {
		if host != "example.com." {
			return nil, &net.DNSError{Err: "absolute name required", Name: host, IsNotFound: true}
		}
		return []netip.Addr{
			netip.MustParseAddr("10.0.0.1"),
			netip.MustParseAddr("2001:db8::1"),
			netip.MustParseAddr("::ffff:10.0.0.2"),
		}, nil
	})
	addr, stop := runServer(t, nil)
	defer stop()

	addrs, err := LookupIPv4(context.Background(), dialTCP(addr), "example.com")
	if err != nil {
		t.Fatalf("LookupIPv4: %v", err)
	}
	if len(addrs) != len(want) {
		t.Fatalf("got %d addrs, want %d: %v", len(addrs), len(want), addrs)
	}
	for i, a := range addrs {
		if a != want[i] {
			t.Fatalf("addr[%d] = %v, want %v", i, a, want[i])
		}
	}
}

func TestLookupIPv4NXDOMAIN(t *testing.T) {
	setResolver(t, func(context.Context, string, string) ([]netip.Addr, error) {
		return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
	})
	addr, stop := runServer(t, nil)
	defer stop()

	_, err := LookupIPv4(context.Background(), dialTCP(addr), "missing.example")
	var rc RCodeError
	if !errors.As(err, &rc) {
		t.Fatalf("expected RCodeError, got %T: %v", err, err)
	}
	if dnsmessage.RCode(rc) != dnsmessage.RCodeNameError {
		t.Fatalf("got RCode %v, want NXDOMAIN", dnsmessage.RCode(rc))
	}
}

func TestLookupIPv4ServerFailure(t *testing.T) {
	setResolver(t, func(context.Context, string, string) ([]netip.Addr, error) {
		return nil, errors.New("resolver unreachable")
	})
	addr, stop := runServer(t, nil)
	defer stop()

	_, err := LookupIPv4(context.Background(), dialTCP(addr), "example.com")
	var rc RCodeError
	if !errors.As(err, &rc) {
		t.Fatalf("expected RCodeError, got %T: %v", err, err)
	}
	if dnsmessage.RCode(rc) != dnsmessage.RCodeServerFailure {
		t.Fatalf("got RCode %v, want SERVFAIL", dnsmessage.RCode(rc))
	}
}

func TestServeDeniedSourceGetsNoReply(t *testing.T) {
	setResolver(t, func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
	})
	addr, stop := runServer(t, func(remote, local net.Addr) bool { return false })
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// The server may close before the write reaches it; tolerate that.
	_ = writeTCPMessage(conn, validQuery(t, "example.com."))
	_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, err = readTCPMessage(conn)
	if err == nil {
		t.Fatal("denied source received a DNS reply")
	}
}

func TestServeMalformedClosesConnection(t *testing.T) {
	setResolver(t, func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
	})
	addr, stop := runServer(t, nil)
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// Send a length prefix that does not match the payload.
	if _, err := conn.Write([]byte{0, 2, 0}); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, err = readTCPMessage(conn)
	if err == nil {
		t.Fatal("malformed query received a reply")
	}
}

func TestLookupIPv4Cancellation(t *testing.T) {
	addr, stop := runServer(t, nil)
	defer stop()

	// Make the server hang so cancellation is observable.
	setResolver(t, func(ctx context.Context, network, host string) ([]netip.Addr, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := LookupIPv4(ctx, dialTCP(addr), "example.com")
		errCh <- err
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error for cancelled lookup")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lookup did not return after cancellation")
	}
}

func TestLookupIPv4RejectsMismatchedID(t *testing.T) {
	old := newQueryID
	newQueryID = func() uint16 { return 0x1234 }
	defer func() { newQueryID = old }()

	name, _ := dnsmessage.NewName("example.com.")
	bad := responseBytes(t, dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:       0x9999,
			Response: true,
			RCode:    dnsmessage.RCodeSuccess,
		},
		Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	})

	addr, stop := runBadServer(t, bad)
	defer stop()

	_, err := LookupIPv4(context.Background(), dialTCP(addr), "example.com")
	if err == nil || !contains(err, "ID mismatch") {
		t.Fatalf("expected ID mismatch error, got %v", err)
	}
}

func TestLookupIPv4RejectsMismatchedQuestion(t *testing.T) {
	name, _ := dnsmessage.NewName("other.example.")
	bad := responseBytes(t, dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:       0x1234,
			Response: true,
			RCode:    dnsmessage.RCodeSuccess,
		},
		Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	})

	old := newQueryID
	newQueryID = func() uint16 { return 0x1234 }
	defer func() { newQueryID = old }()

	addr, stop := runBadServer(t, bad)
	defer stop()

	_, err := LookupIPv4(context.Background(), dialTCP(addr), "example.com")
	if err == nil || !contains(err, "question mismatch") {
		t.Fatalf("expected question mismatch error, got %v", err)
	}
}

func TestLookupIPv4RejectsNonSuccessRCode(t *testing.T) {
	name, _ := dnsmessage.NewName("example.com.")
	bad := responseBytes(t, dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:       0x1234,
			Response: true,
			RCode:    dnsmessage.RCodeRefused,
		},
		Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	})

	old := newQueryID
	newQueryID = func() uint16 { return 0x1234 }
	defer func() { newQueryID = old }()

	addr, stop := runBadServer(t, bad)
	defer stop()

	_, err := LookupIPv4(context.Background(), dialTCP(addr), "example.com")
	var rc RCodeError
	if !errors.As(err, &rc) {
		t.Fatalf("expected RCodeError, got %T: %v", err, err)
	}
	if dnsmessage.RCode(rc) != dnsmessage.RCodeRefused {
		t.Fatalf("got RCode %v, want REFUSED", dnsmessage.RCode(rc))
	}
}

func validQuery(t *testing.T, host string) []byte {
	t.Helper()
	name, err := dnsmessage.NewName(host)
	if err != nil {
		t.Fatalf("new name: %v", err)
	}
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 1, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	}
	b, err := msg.Pack()
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	return b
}

func responseBytes(t *testing.T, msg dnsmessage.Message) []byte {
	t.Helper()
	b, err := msg.Pack()
	if err != nil {
		t.Fatalf("pack response: %v", err)
	}
	return b
}

func runBadServer(t *testing.T, response []byte) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				if ctx.Err() != nil {
					break
				}
				continue
			}
			go func(c net.Conn) {
				defer c.Close()
				// Consume the two-byte length prefix and query body.
				var lenBuf [2]byte
				if _, err := c.Read(lenBuf[:]); err != nil {
					return
				}
				n := int(binary.BigEndian.Uint16(lenBuf[:]))
				if n > 0 {
					buf := make([]byte, n)
					_, _ = c.Read(buf)
				}
				// Send the canned response with DNS-over-TCP length prefix.
				out := make([]byte, 2+len(response))
				binary.BigEndian.PutUint16(out, uint16(len(response)))
				copy(out[2:], response)
				_, _ = c.Write(out)
			}(conn)
		}
		close(done)
	}()
	return ln.Addr().String(), func() {
		cancel()
		ln.Close()
		<-done
	}
}

func contains(err error, sub string) bool {
	return err != nil && errors.Is(err, fmt.Errorf("contains %s", sub)) || (err != nil && len(sub) > 0 && containsString(err.Error(), sub))
}

func containsString(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || len(s) > 0 && containsStringHelper(s, sub))
}

func containsStringHelper(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
