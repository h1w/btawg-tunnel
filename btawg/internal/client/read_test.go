package client

import (
	"testing"
	"time"
)

func TestTunPathReadWithWakeInterruptsIdleRead(t *testing.T) {
	shared := &sharedTUN{mtu: 1280, done: make(chan struct{})}
	path := shared.newPath()
	wake := make(chan struct{})
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		buf := make([]byte, 1600)
		sizes := make([]int, 1)
		n, err := path.ReadWithWake([][]byte{buf}, sizes, 0, wake)
		done <- result{n: n, err: err}
	}()
	close(wake)
	select {
	case got := <-done:
		if got.n != 0 || got.err != nil {
			t.Fatalf("ReadWithWake returned (%d, %v), want (0, nil)", got.n, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("managed TUN read did not observe the control wakeup")
	}
}
