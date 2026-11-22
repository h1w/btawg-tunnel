package client

import (
	"context"
	"io"
	"runtime"
	"testing"
	"time"
)

func TestOverlayQueuePreservesPacketWhenFull(t *testing.T) {
	p := fullOverlayQueue(t)
	result := sendToFullOverlayQueue(t, p, t.Context())
	readOverlayQueueByte(t, p, 1)
	select {
	case ok := <-result:
		if !ok {
			t.Fatal("waiting packet was dropped")
		}
	case <-time.After(time.Second):
		t.Fatal("waiting packet did not resume after a read")
	}
	readOverlayQueueByte(t, p, 2)
	if got := p.shared.overlayQueueStalls.Load(); got != 1 {
		t.Fatalf("queue stalls = %d, want 1", got)
	}
}

func TestOverlayFullQueueStopsAtPathClose(t *testing.T) {
	p := fullOverlayQueue(t)
	result := sendToFullOverlayQueue(t, p, t.Context())
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case ok := <-result:
		if ok {
			t.Fatal("waiting packet was queued on a closed path")
		}
	case <-time.After(time.Second):
		t.Fatal("closing the path did not release the waiting packet")
	}
}

func fullOverlayQueue(t *testing.T) *tunPath {
	t.Helper()
	s := &sharedTUN{mtu: 1280, done: make(chan struct{})}
	s.pool.New = func() any { return newPacketBuffer(s.mtu) }
	p := s.newPath()
	p.packets = make(chan *[]byte, 1)
	if p.queueSpace == nil {
		p.queueSpace = make(chan struct{}, 1)
	}
	b := s.pool.Get().(*[]byte)
	buf := (*b)[:packetHeadroom+1]
	buf[packetHeadroom] = 1
	if !s.enqueueOverlayPacket(t.Context(), p, b, buf, buf[packetHeadroom:]) {
		t.Fatal("failed to prime the packet queue")
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func sendToFullOverlayQueue(t *testing.T, p *tunPath, ctx context.Context) <-chan bool {
	t.Helper()
	s := p.shared
	b := s.pool.Get().(*[]byte)
	buf := (*b)[:packetHeadroom+1]
	buf[packetHeadroom] = 2
	result := make(chan bool, 1)
	go func() {
		ok := s.enqueueOverlayPacket(ctx, p, b, buf, buf[packetHeadroom:])
		if !ok {
			s.pool.Put(b)
		}
		result <- ok
	}()
	deadline := time.Now().Add(time.Second)
	for s.overlayQueueStalls.Load() == 0 {
		select {
		case <-result:
			t.Fatal("sender returned before a queue slot became available")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("full queue was not reported")
		}
		runtime.Gosched()
	}
	return result
}

func readOverlayQueueByte(t *testing.T, p *tunPath, want byte) {
	t.Helper()
	result := make(chan error, 1)
	go func() {
		var buf [1]byte
		var sizes [1]int
		n, err := p.Read([][]byte{buf[:]}, sizes[:], 0)
		if err == nil && (n != 1 || sizes[0] != 1 || buf[0] != want) {
			err = io.ErrUnexpectedEOF
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("reading packet %d: %v", want, err)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out reading packet %d", want)
	}
}
