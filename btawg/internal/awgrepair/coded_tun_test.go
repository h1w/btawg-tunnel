package awgrepair

import (
	"net/netip"
	"testing"
	"time"
)

func TestAWGEnginesCannotExchangeRecoveryHandshakes(t *testing.T) {
	cp, cu := generateKeyPair(t)
	hp, hu := generateKeyPair(t)
	ca, ha := netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.1")
	node, err := NewNode(fixtureTUN(), cp, Peer{Local: ca, Remote: ha, Public: hu}, "socks5-awg-ext-utp")
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	now := time.Now()
	var frame [innerMTU]byte
	peer := node.upstream
	hello := peer.session.begin(&peer.k, ca, ha, now, frame[:0])
	for _, mode := range []string{"socks5-awg-ext-utp", "socks5-awg-udp", "socks5-awg-req-tcp"} {
		hub := NewHub(fixtureTUN(), hp, mode)
		defer hub.Close()
		if err := hub.SetPeers([]Peer{{Local: ha, Remote: ca, Public: cu}}); err != nil {
			t.Fatal(err)
		}
		hub.incoming(hello, nil, now, nil)
		var reply [innerMTU]byte
		var lens [1]int
		n, err := hub.drain([][]byte{reply[:]}, lens[:], 0, now)
		if err != nil {
			t.Fatal(err)
		}
		if (n == 1) != (mode == "socks5-awg-ext-utp") {
			t.Fatalf("%s answered Extended/uTP recovery hello: %t", mode, n == 1)
		}
	}
}
