package client

import (
	"net/netip"
	"testing"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
)

// An unavailable route must stop before invoking the wrapped bind's Send.
type guardTestBind struct {
	conn.Bind
	sent bool
}

func (b *guardTestBind) Send(_ [][]byte, _ conn.Endpoint) error { b.sent = true; return nil }

type guardTestEndpoint struct{ conn.Endpoint }

func (guardTestEndpoint) DstIP() netip.Addr { return netip.Addr{} }

func TestNativeUnderlayRejectsUnroutableEndpoint(t *testing.T) {
	inner := &guardTestBind{}
	b := &underlayBind{Bind: inner}
	if err := b.Send([][]byte{{1}}, guardTestEndpoint{}); err == nil {
		t.Fatal("accepted invalid destination")
	}
	if inner.sent {
		t.Fatal("sent packet without a safe route")
	}
}
