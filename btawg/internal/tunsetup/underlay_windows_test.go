package tunsetup

import (
	"net"
	"net/netip"
	"testing"
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
