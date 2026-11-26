package server

import (
	"context"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"bittorrent-tunnel/btawg/internal/hubrelay"
)

func TestTCPRelayRejectsDestinationsBeforeDial(t *testing.T) {
	r, client, gateway := testRouterWithClient(t)
	s := &Server{policy: r.policy}
	for _, tc := range []struct {
		name               string
		src, local, target netip.Addr
		want               bool
	}{
		{"allowed", client, gateway, netip.MustParseAddr("1.1.1.1"), true},
		{"unknown", netip.MustParseAddr("10.77.0.99"), gateway, netip.MustParseAddr("1.1.1.1"), false},
		{"non-gateway", client, netip.MustParseAddr("10.77.0.99"), netip.MustParseAddr("1.1.1.1"), false},
		{"overlay", client, gateway, client, false},
		{"loopback", client, gateway, netip.MustParseAddr("127.0.0.1"), false},
		{"private", client, gateway, netip.MustParseAddr("192.168.10.2"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := s.policy.tcpEgress(tc.src, tc.local, tc.target); ok != tc.want {
				t.Fatalf("relay authorization = %v, want %v", ok, tc.want)
			}
		})
	}
}

func TestTCPRelayClosesOnInternetRevocation(t *testing.T) {
	r, client, gateway := testRouterWithClient(t)
	s := &Server{policy: r.policy}
	a, b := net.Pipe()
	defer b.Close()
	x, y := net.Pipe()
	defer y.Close()
	target := netip.MustParseAddrPort("1.1.1.1:443")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_ = b.SetDeadline(time.Now().Add(2 * time.Second))
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		s.handleTCPEgress(ctx, a, client, gateway, func(_ context.Context, dst netip.AddrPort) (net.Conn, error) {
			if dst != target {
				t.Errorf("dial = %s, want %s", dst, target)
			}
			return x, nil
		}, 0)
	}()
	if err := hubrelay.Request(ctx, b, target); err != nil {
		t.Fatal(err)
	}
	s.applyMu.Lock()
	s.policy.mu.Lock()
	rec := s.policy.byIP[client]
	internet := false
	rec.Internet = &internet
	s.policy.byIP[client] = rec
	s.policy.mu.Unlock()
	s.revalidateTCPEgress()
	s.applyMu.Unlock()
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("forwarding survived revocation")
	}
	if _, err := b.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("revoked socket: %v", err)
	}
}
