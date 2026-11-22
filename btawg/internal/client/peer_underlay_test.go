package client

import (
	"net/netip"
	"testing"

	"bittorrent-tunnel/btawg/internal/mesh"
)

func TestPeerUnderlayOmitsOwnVPNAddress(t *testing.T) {
	vpn := netip.MustParseAddr("10.77.0.2")
	virtual := mesh.Candidate{Addr: netip.MustParseAddrPort("10.77.0.2:6881"), Kind: "lan"}
	physical := mesh.Candidate{Addr: netip.MustParseAddrPort("192.168.50.4:6881"), Kind: "lan"}
	got := peerUnderlayCandidates([]mesh.Candidate{virtual, physical}, vpn)
	if len(got) != 1 || got[0] != physical {
		t.Fatalf("advertised candidates: %v, want only %v", got, physical)
	}
}
