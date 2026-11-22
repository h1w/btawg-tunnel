package portmap

import (
	"encoding/binary"
	"net/netip"
	"strings"
	"testing"
)

func TestLinuxDefaultRouteSelectsLowestMetricGateway(t *testing.T) {
	routes := strings.NewReader(`Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT
wg0 00000000 00000000 0001 0 0 1 00000000 0 0 0
eth1 00000000 0102A8C0 0003 0 0 99 00000000 0 0 0
eth0 00000000 0101A8C0 0003 0 0 20 00000000 0 0 0
eth2 00000000 0100A8C0 0002 0 0 0 00000000 0 0 0
eth3 00000000 0100A8C0 0203 0 0 0 00000000 0 0 0
eth4 00000000 0100A8C0 0003 0 0 0 0000FFFF 0 0 0
`)
	// The /proc addresses are displayed in native byte order.
	var b [4]byte
	binary.NativeEndian.PutUint32(b[:], 0x0101a8c0)
	iface, addr, err := defaultRoute(routes)
	if iface != "eth0" || addr != netip.AddrFrom4(b) || err != nil {
		t.Fatalf("route = (%q, %v, %v), want eth0/%v", iface, addr, err, netip.AddrFrom4(b))
	}
}
