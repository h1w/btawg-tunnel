package client

import (
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestWindowsOverlayBackpressureReachesUDPSocket(t *testing.T) {
	s := &sharedTUN{mtu: 1280, ports: make(map[localPort]*socketPath), done: make(chan struct{})}
	s.pool.New = func() any { return newPacketBuffer(s.mtu) }
	p := s.newPath()
	p.packets = make(chan *[]byte, 1)
	t.Cleanup(func() { _ = p.Close() })
	var c Client
	if err := p.proxySockets(&c, netip.MustParseAddr("10.77.0.2")); err != nil {
		t.Fatal(err)
	}
	udp, err := c.proxyUDP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = p.Close()
		_ = udp.Close()
	})
	dst := net.UDPAddrFromAddrPort(netip.MustParseAddrPort("203.0.113.8:1053"))
	if _, err := udp.WriteTo([]byte{1}, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := udp.WriteTo([]byte{2}, dst); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for s.overlayQueueStalls.Load() == 0 {
		select {
		case <-deadline.C:
			t.Fatal("second packet did not encounter backpressure")
		case <-tick.C:
		}
	}
	// The forwarder holds packet 2 while the transport queue holds packet 1.
	// Further writes fill the link queue; an overflow must not report success.
	for range pathQueueSize {
		if _, err := udp.WriteTo([]byte{3}, dst); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := udp.WriteTo([]byte{4}, dst); err == nil {
		t.Fatal("UDP write reported success although its packet could not be queued")
	}
	if got := s.overlayQueueStalls.Load(); got != 2 {
		t.Fatalf("saturation events = %d, want one transport stall and one link rejection", got)
	}
	for _, want := range []byte{1, 2} {
		var buf [1280]byte
		var sizes [1]int
		n, err := p.Read([][]byte{buf[:]}, sizes[:], 0)
		if err != nil || n != 1 || sizes[0] < 29 || buf[sizes[0]-1] != want {
			t.Fatalf("read packet %d: %d packets, size %d, err %v", want, n, sizes[0], err)
		}
	}
}
