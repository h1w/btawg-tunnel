package tunsetup

import (
	"context"
	"net"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestNativeSOCKSUsesBBRWithBoundedBuffer(t *testing.T) {
	available, err := os.ReadFile("/proc/sys/net/ipv4/tcp_available_congestion_control")
	if err != nil || !slices.Contains(strings.Fields(string(available)), "bbr") {
		t.Skip("kernel BBR unavailable")
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	for _, tc := range []struct {
		control func(string, string, syscall.RawConn) error
		want    string
	}{{OverlayTCP, "reno"}, {NativeSOCKSTCP, "bbr"}} {
		d := net.Dialer{Control: tc.control}
		c, err := d.DialContext(context.Background(), "tcp4", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		raw, err := c.(*net.TCPConn).SyscallConn()
		if err != nil {
			c.Close()
			t.Fatal(err)
		}
		var cc string
		var send int
		if err = raw.Control(func(fd uintptr) {
			cc, _ = unix.GetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION)
			send, _ = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF)
		}); err != nil {
			c.Close()
			t.Fatal(err)
		}
		c.Close()
		if cc != tc.want || send <= 0 || send > 2<<20 {
			t.Fatalf("policy = %s, sndbuf = %d; want %s and at most 2 MiB", cc, send, tc.want)
		}
	}
}
