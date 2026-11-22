package client

import (
	"net"
	"net/netip"
	"testing"
	"time"

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

type countingBind struct {
	conn.Bind
	binds int
}

func (b *countingBind) Open(uint16) ([]conn.ReceiveFunc, uint16, error) { return nil, 1234, nil }
func (b *countingBind) Close() error                                    { return nil }
func (b *countingBind) BindSocketToInterface4(uint32, bool) error       { b.binds++; return nil }
func (b *countingBind) BindSocketToInterface6(uint32, bool) error       { b.binds++; return nil }
func (b *countingBind) Send(_ [][]byte, _ conn.Endpoint) error          { return nil }

type loopbackEndpoint struct{ conn.Endpoint }

func (loopbackEndpoint) DstIP() netip.Addr { return netip.MustParseAddr("127.0.0.1") }
func (loopbackEndpoint) ClearSrc()         {}

func TestNativeUnderlayRebindsAfterSocketReopen(t *testing.T) {
	inner := &countingBind{}
	b := &underlayBind{Bind: inner}
	if _, _, err := b.Open(0); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := b.Send([][]byte{{1}}, loopbackEndpoint{}); err != nil {
			t.Fatal(err)
		}
	}
	if inner.binds != 1 {
		t.Fatalf("stable sends performed %d bindings, want 1", inner.binds)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Open(0); err != nil {
		t.Fatal(err)
	}
	if err := b.Send([][]byte{{1}}, loopbackEndpoint{}); err != nil {
		t.Fatal(err)
	}
	if inner.binds != 2 {
		t.Fatalf("reopened socket has %d bindings, want 2", inner.binds)
	}
}

func TestNativeUnderlayClearsCachedEndpointSource(t *testing.T) {
	inner := &countingBind{}
	b := &underlayBind{Bind: inner}
	ep := &sourceEndpoint{}
	if err := b.Send([][]byte{{1}}, ep); err != nil {
		t.Fatal(err)
	}
	if !ep.cleared {
		t.Fatal("cached source remained set")
	}
}

type sourceEndpoint struct {
	loopbackEndpoint
	cleared bool
}

func (e *sourceEndpoint) ClearSrc() { e.cleared = true }

type blockingBind struct {
	countingBind
	started chan struct{}
	closed  chan struct{}
}

func (b *blockingBind) Send(_ [][]byte, _ conn.Endpoint) error {
	close(b.started)
	<-b.closed
	return net.ErrClosed
}
func (b *blockingBind) Close() error { close(b.closed); return nil }

func TestNativeUnderlayCloseInterruptsSend(t *testing.T) {
	inner := &blockingBind{started: make(chan struct{}), closed: make(chan struct{})}
	b := &underlayBind{Bind: inner}
	sent := make(chan error, 1)
	go func() { sent <- b.Send([][]byte{{1}}, loopbackEndpoint{}) }()
	select {
	case <-inner.started:
	case <-time.After(time.Second):
		t.Fatal("send did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- b.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close blocked behind send")
	}
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("send did not terminate")
	}
}
