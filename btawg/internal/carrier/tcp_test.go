package carrier

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestTCPControlCarriesDuplexTraffic(t *testing.T) {
	ctx := t.Context()
	listener := net.ListenConfig{Control: TCPControl}
	ln, err := listener.Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dialer := net.Dialer{Control: TCPControl, Timeout: time.Second}
	a, err := dialer.DialContext(ctx, "tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for _, pair := range [][2]net.Conn{{a, b}, {b, a}} {
		if err := pair[1].SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(pair[0], "peer"); err != nil {
			t.Fatal(err)
		}
		var msg [4]byte
		if _, err := io.ReadFull(pair[1], msg[:]); err != nil || string(msg[:]) != "peer" {
			t.Fatalf("received %q: %v", msg, err)
		}
	}
}
