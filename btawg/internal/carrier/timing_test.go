package carrier

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"testing/synctest"
	"time"
)

func TestCarrierTimingCloseUnblocksQueuedSend(t *testing.T) {
	a, b := pair(t,
		func(c net.Conn) (Transport, [20]byte, error) { return NewExtended(c, testHash, peerA) },
		func(c net.Conn) (Transport, [20]byte, error) { return NewExtended(c, testHash, peerB) },
	)
	timed := newTimedTransport(a, a, timingOptions{bytesPerSecond: 1})
	t.Cleanup(timed.Wait)
	batch := [][]byte{[]byte("first"), []byte("second")}
	err := make(chan error, 1)
	go func() { err <- timed.SendBatch(batch) }()
	select {
	case got := <-b.Recv():
		if string(got) != "first" {
			t.Fatalf("first datagram = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first datagram stalled")
	}
	select {
	case got := <-b.Recv():
		t.Fatalf("backlog was not paced: %q", got)
	default:
	}
	timed.Close()
	select {
	case got := <-err:
		if !errors.Is(got, net.ErrClosed) {
			t.Fatalf("blocked send ended with %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not cancel pacing")
	}
}

func TestCarrierTimingIsolatedSendDoesNotWait(t *testing.T) {
	a, b := pair(t,
		func(c net.Conn) (Transport, [20]byte, error) { return NewExtended(c, testHash, peerA) },
		func(c net.Conn) (Transport, [20]byte, error) { return NewExtended(c, testHash, peerB) },
	)
	timed := newTimedTransport(a, a, timingOptions{bytesPerSecond: 1})
	t.Cleanup(timed.Wait)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	for _, msg := range []string{"first", "second"} {
		err := make(chan error, 1)
		go func() { err <- timed.Send([]byte(msg)) }()
		select {
		case got := <-err:
			if got != nil {
				t.Fatal(got)
			}
		case <-ctx.Done():
			t.Fatal("isolated send waited for pacing", ctx.Err())
		}
		select {
		case got := <-b.Recv():
			if string(got) != msg {
				t.Fatalf("datagram = %q, want %q", got, msg)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestCarrierTimingFiniteIdleBudget(t *testing.T) {
	// synctest advances time deterministically; the writer's observable output
	// is the four-byte BitTorrent keepalive, never a tunneled datagram.
	synctest.Test(t, func(t *testing.T) {
		a, b := net.Pipe()
		defer b.Close()
		base := &extCarrier{c: a, done: make(chan struct{})}
		x := newTimedTransport(base, base, timingOptions{idleCount: 2, idleInterval: time.Second})
		defer x.Wait()
		if err := b.SetReadDeadline(time.Now().Add(4 * time.Second)); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			var frame [4]byte
			if _, err := io.ReadFull(b, frame[:]); err != nil || frame != ([4]byte{}) {
				t.Fatalf("idle message = %x, %v", frame, err)
			}
		}
		var next [1]byte
		if _, err := b.Read(next[:]); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("idle budget exhausted; read returned %v", err)
		}
	})
}
