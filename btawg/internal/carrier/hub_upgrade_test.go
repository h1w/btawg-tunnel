package carrier

import (
	"net"
	"testing"
)

func TestHubUpgradeNegotiatesDefaultCarriers(t *testing.T) {
	t.Setenv("BTAWG_EXT_EXPERIMENT", "")
	t.Setenv("BTAWG_REQ_EXPERIMENT", "")
	for _, framing := range []string{"ext", "req"} {
		for _, tc := range []struct {
			name        string
			client, hub bool
			pinless     bool
		}{
			{name: "both-upgraded", client: true, hub: true},
			{name: "hub-upgraded", hub: true},
			{name: "client-upgraded", client: true},
			{name: "old-import", client: true, hub: true, pinless: true},
		} {
			if framing == "req" && tc.pinless {
				continue
			}
			t.Run(framing+"/"+tc.name, func(t *testing.T) {
				dial := func(c net.Conn) (Transport, [20]byte, error) {
					if framing == "ext" {
						if tc.client {
							return NewHubExtended(c, testHash, peerA, !tc.pinless)
						}
						return NewExtended(c, testHash, peerA)
					}
					if tc.client {
						return NewHubRequestPiece(c, testHash, peerA, false)
					}
					return NewRequestPiece(c, testHash, peerA, false)
				}
				accept := func(c net.Conn) (Transport, [20]byte, error) {
					open := Accept
					if tc.hub {
						open = AcceptHub
					}
					tr, id, _, err := open(c, peerB, map[[20]byte]string{testHash: framing})
					return tr, id, err
				}
				a, b := pair(t, dial, accept)
				roundTrip(t, a, b)
				for _, tr := range []Transport{a, b} {
					if framing == "ext" {
						want := ""
						if tc.client && tc.hub && !tc.pinless {
							want = "padded"
						}
						if got := ExtendedExperiment(tr); got != want {
							t.Fatalf("ext format = %q, want %q", got, want)
						}
					} else {
						varied := tr.(*requestPieceCarrier).maxBlockSize() == BlockSize
						if want := tc.client && tc.hub; varied != want {
							t.Fatalf("variable req blocks = %v, want %v", varied, want)
						}
					}
				}
			})
		}
	}
}
