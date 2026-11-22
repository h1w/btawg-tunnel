package portmap

import (
	"net/netip"
	"testing"
)

func TestGatewayUsesCombinedMetricAndUsableInterface(t *testing.T) {
	first := gatewayRoute{index: 5, via: netip.MustParseAddr("192.0.2.1"), routeMetric: 1, interfaceMetric: 50}
	second := gatewayRoute{index: 8, via: netip.MustParseAddr("198.51.100.1"), routeMetric: 20, interfaceMetric: 8}
	onLink := gatewayRoute{index: 9, via: netip.IPv4Unspecified()}
	got, ok := pickGateway([]gatewayRoute{first, second, onLink})
	if !ok || got.index != 8 || got.via.String() != "198.51.100.1" {
		t.Fatalf("selected %+v, want interface 8 via 198.51.100.1", got)
	}
	if _, ok := pickGateway([]gatewayRoute{onLink}); ok {
		t.Fatal("on-link default without a router is not a NAT mapping gateway")
	}
}
