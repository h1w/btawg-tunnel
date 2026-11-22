package purego

import (
	"net"
	"sync"
	"testing"
	"time"
)

type fallbackPacketConn struct{ *net.UDPConn }

func (*fallbackPacketConn) SendUDPSegments(_ [][]byte, _ *net.UDPAddr) (bool, error) {
	return false, nil
}

func TestWindowsUDPBatchFallbackPreservesOrderAndBoundaries(t *testing.T) {
	receiver, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	pc := &fallbackPacketConn{sender}
	s := &Socket{pc: pc, tx: newWindowsUDPWriteBatch(pc)}
	s.tx.active = true
	for _, p := range []string{"aaaa", "bbbb", "longer", "cccc", "dddd"} {
		s.send([]byte(p), receiver.LocalAddr(), false)
	}
	s.flushWrites()
	if err := receiver.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"aaaa", "bbbb", "longer", "cccc", "dddd"} {
		var b [64]byte
		n, _, err := receiver.ReadFromUDP(b[:])
		if err != nil || string(b[:n]) != want {
			t.Fatalf("got %q, %v; want %q", b[:n], err, want)
		}
	}
}

type blockingPacketConn struct {
	net.PacketConn
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (c *blockingPacketConn) SendUDPSegments(_ [][]byte, _ *net.UDPAddr) (bool, error) {
	close(c.started)
	<-c.closed
	return false, net.ErrClosed
}
func (c *blockingPacketConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.PacketConn.Close()
}

func TestWindowsUDPBatchCloseInterruptsBlockedSend(t *testing.T) {
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	pc := &blockingPacketConn{PacketConn: udp, started: make(chan struct{}), closed: make(chan struct{})}
	s, err := NewSocketFromPacketConn(pc)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		s.mu.Lock()
		s.tx.active = true
		peer := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}
		s.send([]byte("aaaa"), peer, false)
		s.send([]byte("bbbb"), peer, false)
		s.flushWrites()
		s.mu.Unlock()
		close(done)
	}()
	select {
	case <-pc.started:
	case <-time.After(time.Second):
		t.Fatal("send did not start")
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close blocked behind send")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("send did not stop")
	}
}
