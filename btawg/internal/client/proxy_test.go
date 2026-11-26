package client

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"

	"bittorrent-tunnel/btawg/internal/hubrelay"
)

func TestNativeSOCKSRelayOnlyFallsBackOnConnectionRefused(t *testing.T) {
	dst := netip.MustParseAddrPort("1.1.1.1:80")
	gateway := netip.MustParseAddr("10.77.0.1")
	for _, status := range []string{"refused", "denied"} {
		t.Run(status, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			direct := 0
			c := &Client{nativeSOCKS: func(_ context.Context, addr netip.AddrPort) (net.Conn, error) {
				if addr == dst {
					direct++
					a, b := net.Pipe()
					b.Close()
					return a, nil
				}
				if addr != netip.AddrPortFrom(gateway, hubrelay.Port) {
					return nil, fmt.Errorf("unexpected dial %s", addr)
				}
				if status == "refused" {
					return nil, syscall.ECONNREFUSED
				}
				a, b := net.Pipe()
				go func() {
					defer b.Close()
					got, err := hubrelay.Read(b)
					if err == nil && got == dst {
						_ = hubrelay.Reply(b, hubrelay.Denied)
					}
				}()
				return a, nil
			}}
			conn, err := c.dialSOCKSData(ctx, gateway, dst)
			if conn != nil {
				conn.Close()
			}
			if status == "denied" && (err == nil || direct != 0) {
				t.Fatalf("relay denial: error=%v, direct=%d", err, direct)
			}
			if status == "refused" && (err != nil || direct != 1) {
				t.Fatalf("legacy hub fallback: error=%v, direct=%d", err, direct)
			}
		})
	}
}
