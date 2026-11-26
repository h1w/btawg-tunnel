package server

import (
	"net/netip"
	"testing"
	"time"
)

func TestEngineReadWithWakeInterruptsIdleRead(t *testing.T) {
	r := &packetRouter{closed: make(chan struct{})}
	e := r.newEngine(netip.Addr{})
	wake := make(chan struct{})
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		buf := make([]byte, 1600)
		sizes := make([]int, 1)
		n, err := e.ReadWithWake([][]byte{buf}, sizes, 0, wake)
		done <- result{n: n, err: err}
	}()
	close(wake)
	select {
	case got := <-done:
		if got.n != 0 || got.err != nil {
			t.Fatalf("ReadWithWake returned (%d, %v), want (0, nil)", got.n, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("engine read did not observe the control wakeup")
	}
}
