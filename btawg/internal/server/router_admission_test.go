package server

import (
	"testing"
	"time"
)

func TestAdmitBurstUntilDeadlineOrShutdown(t *testing.T) {
	r := &packetRouter{closed: make(chan struct{})}
	e := &engineTun{router: r, packets: make(chan *routerPacket), closed: make(chan struct{})}
	pkt := new(routerPacket)
	expired := make(chan time.Time)
	close(expired)
	if accepted, full := e.admitBurst(pkt, expired); accepted || !full {
		t.Fatal("a stalled engine must release the shared dispatcher at the batch deadline")
	}
	result := make(chan bool, 1)
	go func() { got, full := e.admitBurst(pkt, nil); result <- got && !full }()
	select {
	case got := <-e.packets:
		if got != pkt {
			t.Fatal("different packet admitted")
		}
	case <-time.After(time.Second):
		t.Fatal("temporary pressure dropped an original before the reader could take it")
	}
	if !<-result {
		t.Fatal("admitted original was counted as dropped")
	}
	close(r.closed)
	if got, full := e.admitBurst(pkt, nil); got || full {
		t.Fatal("shutdown should release blocked admission without counting congestion")
	}
}
