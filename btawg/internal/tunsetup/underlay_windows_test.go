package tunsetup

import (
	"net"
	"net/netip"
	"os"
	"testing"
	"time"
)

func TestUnderlayRouteExcludesOwnedAdapterAndChoosesLongestMatch(t *testing.T) {
	dst := netip.MustParseAddr("10.1.2.3")
	routes := []underlayRoute{
		{prefix: netip.MustParsePrefix("0.0.0.0/0"), index: 2, luid: 12, metric: 30},
		{prefix: netip.MustParsePrefix("10.1.0.0/16"), index: 3, luid: 13, metric: 1},
		{prefix: netip.MustParsePrefix("10.1.2.0/24"), index: 4, luid: 14, metric: 90},
	}
	got, ok := selectUnderlayRoute(routes, dst, map[uint64]bool{13: true})
	if !ok || got.index != 4 {
		t.Fatalf("route = %+v, %v; want interface 4", got, ok)
	}
	got, ok = selectUnderlayRoute(routes, dst, map[uint64]bool{13: true, 14: true})
	if !ok || got.index != 2 {
		t.Fatalf("route = %+v, %v; want interface 2", got, ok)
	}
	if _, ok = selectUnderlayRoute(routes[1:], dst, map[uint64]bool{13: true, 14: true}); ok {
		t.Fatal("selected tunnel without an underlay route")
	}
}

func TestUnderlayPacketRejectsInvalidDestination(t *testing.T) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	protected := ProtectUnderlayPacket(c)
	if _, err := protected.WriteTo([]byte("x"), &net.UDPAddr{}); err == nil {
		t.Fatal("invalid destination was sent without route validation")
	}
}

func TestUnderlayStreamClosesWhenAdapterBecomesOwned(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	dst := netip.MustParseAddr("127.0.0.1")
	protected, err := ProtectUnderlayStream(client, dst)
	if err != nil {
		t.Fatal(err)
	}
	stream := protected.(*underlayStream)
	registerUnderlayAdapter(stream.luid, true)
	defer registerUnderlayAdapter(stream.luid, false)
	if _, err := stream.Write([]byte("blocked")); err == nil {
		t.Fatal("write succeeded on owned adapter")
	}
	if _, err := client.Write([]byte("closed")); err == nil {
		t.Fatal("stale stream was not closed")
	}
}

func TestUnderlayUSOPreservesDatagramBoundaries(t *testing.T) {
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
	protected := ProtectUnderlayPacket(sender).(*UnderlayPacketConn)
	parts := [][]byte{[]byte("first"), []byte("other"), []byte("third")}
	sent, err := protected.SendUDPSegments(parts, receiver.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	if !sent {
		if os.Getenv("BTAWG_WINDOWS_NETWORK_TESTS") == "1" {
			t.Fatal("Windows USO-capable smoke test did not send a segmented message")
		}
		t.Skip("Windows provider does not support USO")
	}
	if err := receiver.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"first", "other", "third"} {
		var buf [64]byte
		n, _, err := receiver.ReadFromUDP(buf[:])
		if err != nil || string(buf[:n]) != want {
			t.Fatalf("datagram %d = %q, %v; want %q", i, buf[:n], err, want)
		}
	}
}

func TestUnderlayUSORejectsUnequalSegmentsWithoutSending(t *testing.T) {
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
	protected := ProtectUnderlayPacket(sender).(*UnderlayPacketConn)
	sent, err := protected.SendUDPSegments([][]byte{[]byte("one"), []byte("four")}, receiver.LocalAddr().(*net.UDPAddr))
	if err != nil || sent {
		t.Fatalf("unequal segments: sent %v, err %v", sent, err)
	}
	if err := receiver.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var buf [64]byte
	if _, _, err := receiver.ReadFromUDP(buf[:]); err == nil {
		t.Fatal("ineligible batch sent data")
	}
}

func TestUnderlayUSOHonorsExpiredWriteDeadline(t *testing.T) {
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
	if err := sender.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	protected := ProtectUnderlayPacket(sender).(*UnderlayPacketConn)
	sent, err := protected.SendUDPSegments([][]byte{[]byte("first"), []byte("other")}, receiver.LocalAddr().(*net.UDPAddr))
	if !sent && err == nil {
		_, err = protected.WriteTo([]byte("first"), receiver.LocalAddr())
	}
	if sent || err == nil {
		t.Fatalf("expired deadline sent=%v err=%v", sent, err)
	}
	if err := receiver.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var buf [64]byte
	if _, _, err := receiver.ReadFromUDP(buf[:]); err == nil {
		t.Fatal("datagram sent after write deadline")
	}
}

func TestUnderlayRejectsStaleCachePublication(t *testing.T) {
	old := underlayEpoch.Load()
	underlayEpoch.Add(1)
	if publishUnderlayRoute(netip.MustParseAddr("192.0.2.44"), old, cachedUnderlayRoute{index: 1, luid: 2}) {
		t.Fatal("published a route after its generation was invalidated")
	}
}

func TestUnderlayDialRejectsChangedAdapterIdentity(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var guard UnderlayDialGuard
	dialer := net.Dialer{Control: guard.Control}
	client, err := dialer.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	guard.luid++ // Simulate index reuse by a different adapter.
	if _, err := guard.Protect(client, netip.MustParseAddr("127.0.0.1")); err == nil {
		t.Fatal("accepted a socket connected on a different adapter identity")
	}
}
