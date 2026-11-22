package portmap

import "net/netip"

type gatewayRoute struct {
	index                        int
	luid                         uint64
	via                          netip.Addr
	routeMetric, interfaceMetric uint32
}

func pickGateway(routes []gatewayRoute) (gatewayRoute, bool) {
	var best gatewayRoute
	var bestMetric uint64
	for _, route := range routes {
		if route.index == 0 || !route.via.Is4() || (!route.via.IsGlobalUnicast() && !route.via.IsLinkLocalUnicast()) {
			continue
		}
		metric := uint64(route.routeMetric) + uint64(route.interfaceMetric)
		if best.index == 0 || metric < bestMetric || metric == bestMetric && route.index < best.index {
			best, bestMetric = route, metric
		}
	}
	return best, best.index != 0
}
