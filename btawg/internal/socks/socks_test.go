package socks

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func startEgress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := NewServer(
		func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
		func(ctx context.Context) (net.PacketConn, error) {
			return net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
		},
	)
	go srv.Serve(ctx, ln)
	return ln.Addr().String()
}

func tcpEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

func udpEcho(t *testing.T) *net.UDPAddr {
	t.Helper()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("udp listen: %v", err)
	}
	go func() {
		buf := make([]byte, 65536)
		for {
			n, from, err := pc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if _, err := pc.WriteToUDP(buf[:n], from); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { pc.Close() })
	return pc.LocalAddr().(*net.UDPAddr)
}

func TestConnectRoundTrip(t *testing.T) {
	remote := startEgress(t)
	echo := tcpEcho(t)
	cli := NewClient(remote)
	conn, err := cli.Dial(context.Background(), "tcp", echo)
	if err != nil {
		t.Fatalf("chain Dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := bufio.NewReader(conn).ReadString('g')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "ping" {
		t.Fatalf("echo = %q, want ping", got)
	}
}

func TestConnectPreservesResponseAfterHalfClose(t *testing.T) {
	for _, observed := range []bool{false, true} {
		name := "ordinary"
		if observed {
			name = "observed"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			origin, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer origin.Close()
			go func() {
				c, err := origin.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(2 * time.Second))
				b, err := io.ReadAll(c)
				if err == nil && string(b) == "request" {
					_, _ = c.Write([]byte("response after EOF"))
				}
			}()
			ln, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			upstream := NewServer((&net.Dialer{}).DialContext, nil)
			srv := upstream
			if observed {
				srv = NewObservedBalancer(func(bool) (*Server, context.Context, func(bool, int64, time.Duration, time.Time)) {
					return upstream, ctx, func(bool, int64, time.Duration, time.Time) {}
				})
			}
			go srv.Serve(ctx, ln)
			conn, err := NewClient(ln.Addr().String()).Dial(ctx, "tcp", origin.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			if _, err := conn.Write([]byte("request")); err != nil {
				t.Fatal(err)
			}
			if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if got, err := io.ReadAll(conn); err != nil || string(got) != "response after EOF" {
				t.Fatalf("response after CloseWrite = %q, %v", got, err)
			}
		})
	}
}

func TestObservedBalancerReportsCompletedTCPDirection(t *testing.T) {
	echo := tcpEcho(t)
	upstream := NewServer(func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}, nil)
	type sample struct {
		upload bool
		bytes  int64
	}
	samples := make(chan sample, 2)
	b := NewObservedBalancer(func(tcp bool) (*Server, context.Context, func(bool, int64, time.Duration, time.Time)) {
		return upstream, context.Background(), func(upload bool, n int64, _ time.Duration, _ time.Time) { samples <- sample{upload, n} }
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go b.Serve(ctx, ln)
	c, err := NewClient(ln.Addr().String()).Dial(ctx, "tcp", echo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	c.Close()
	select {
	case got := <-samples:
		if got.bytes != 5 {
			t.Fatalf("completed transfer = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("missing completed transfer sample")
	}
}

func TestUDPAssociateRoundTrip(t *testing.T) {
	remote := startEgress(t)
	echo := udpEcho(t)
	cli := NewClient(remote)
	pc, err := cli.DialUDP(context.Background())
	if err != nil {
		t.Fatalf("chain DialUDP: %v", err)
	}
	defer pc.Close()
	if _, err := pc.WriteTo([]byte("hello"), echo); err != nil {
		t.Fatalf("udp write: %v", err)
	}
	buf := make([]byte, 128)
	pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, from, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("udp read: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("udp echo = %q, want hello", buf[:n])
	}
	if from.(*net.UDPAddr).Port != echo.Port {
		t.Fatalf("udp from %v, want echo %v", from, echo)
	}
}

func TestNoAuthMethods(t *testing.T) {
	remote := startEgress(t)
	c, err := net.Dial("tcp", remote)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	// Offer only an unacceptable auth method.
	if _, err := c.Write([]byte{socksVersion, 1, 0x7F}); err != nil {
		t.Fatalf("write: %v", err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var reply [2]byte
	if _, err := io.ReadFull(c, reply[:]); err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if reply[0] != socksVersion || reply[1] != 0xFF {
		t.Fatalf("reply = %v, want [5 FF]", reply)
	}
}

type failingTransferWriter struct{}

func (failingTransferWriter) Write([]byte) (int, error) { return 0, errors.New("receiver aborted") }

func TestTransferObservationRejectsAbortedWrite(t *testing.T) {
	called := false
	copyObserved(failingTransferWriter{}, bytes.NewReader(make([]byte, 1024)), true, func(bool, int64, time.Duration, time.Time) { called = true })
	if called {
		t.Fatal("aborted copy produced a sample")
	}
}

func TestTransferObservationUsesLastProductiveTimestamp(t *testing.T) {
	before := time.Now()
	var at time.Time
	copyObserved(io.Discard, bytes.NewReader([]byte("payload")), false, func(_ bool, _ int64, _ time.Duration, last time.Time) { at = last })
	if at.Before(before) || at.After(time.Now()) {
		t.Fatalf("last payload timestamp = %v", at)
	}
}

func TestDownloadStallRemainsMeasuredWithoutPenalizingIdleUpload(t *testing.T) {
	for _, tc := range []struct{ upload, wantIdle bool }{{true, true}, {false, false}} {
		r := &transferReader{Reader: bytes.NewReader([]byte("data")), upload: tc.upload, last: time.Now().Add(-2 * time.Second)}
		if _, err := r.Read(make([]byte, 4)); err != nil {
			t.Fatal(err)
		}
		if r.idle != tc.wantIdle {
			t.Fatalf("upload %v idle = %v", tc.upload, r.idle)
		}
		if !tc.upload && r.duration < 2*time.Second {
			t.Fatal("download stall omitted from goodput")
		}
	}
}
