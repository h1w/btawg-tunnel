package portmap

import (
	"net/netip"
	"testing"
)

func TestGatewaySnapshotRejectsReusedInterfaceOrSource(t *testing.T) {
	r := router{ifindex: 4, luid: 42, source: netip.MustParseAddr("192.0.2.4")}
	if !gatewaySnapshotMatches(r, 42, 4, r.source, true) {
		t.Fatal("valid gateway snapshot rejected")
	}
	for _, tc := range []struct {
		luid   uint64
		index  uint32
		source netip.Addr
		up     bool
	}{
		{43, 4, r.source, true}, {42, 5, r.source, true}, {42, 4, netip.MustParseAddr("192.0.2.5"), true}, {42, 4, r.source, false},
	} {
		if gatewaySnapshotMatches(r, tc.luid, tc.index, tc.source, tc.up) {
			t.Fatalf("accepted changed snapshot: %+v", tc)
		}
	}
}
