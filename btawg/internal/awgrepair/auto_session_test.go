package awgrepair

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"testing"
	"time"
)

// Deliver actual authenticated control frames; never set an activation flag in
// the test. Data and probe losses are deliberately independent here.
func TestAutoTransitionsOnOneLiveSession(t *testing.T) {
	for _, mode := range []string{"socks5-awg-udp", "socks5-awg-ext-tcp", "socks5-awg-ext-utp", "socks5-awg-req-tcp", "socks5-awg-req-utp"} {
		t.Run(mode, func(t *testing.T) { testLiveLoss(t, mode) })
	}
}

func testLiveLoss(t *testing.T, mode string) {
	cp, cu := generateKeyPair(t)
	hp, hu := generateKeyPair(t)
	ca := netip.MustParseAddr("10.0.0.2")
	ha := netip.MustParseAddr("10.0.0.1")
	cli, err := NewNode(fixtureTUN(), cp, Peer{Local: ca, Remote: ha, Public: hu}, mode)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	hub := NewHub(fixtureTUN(), hp, mode)
	defer hub.Close()
	if err := hub.SetPeers([]Peer{{Local: ha, Remote: ca, Public: cu}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	c, h := cli.upstream, hub.paths[ca]
	var buf [innerMTU]byte
	shakeHands(t, &c.session, &h.session, &c.k, &h.k, ca, ha, now, buf[:])
	generation := c.session.generation()
	controls := func(a, b *Device, at time.Time) {
		var scratch [innerMTU]byte
		var sz [1]int
		for {
			n, err := a.drain([][]byte{scratch[:]}, sz[:], 0, at)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				break
			}
			b.incoming(scratch[:sz[0]], scratch[:], at.Add(100*time.Millisecond), func([]byte) {})
		}
	}
	packet := makeTCP(t, 0x18, bytes.Repeat([]byte{7}, 1100))
	copy(packet[12:16], ca.AsSlice())
	copy(packet[16:20], ha.AsSlice())
	i := 0
	drive := func(n int, loss bool) {
		for range n {
			i++
			if i%24 == 0 {
				now = now.Add(20 * time.Millisecond)
			} else {
				now = now.Add(100 * time.Microsecond)
			}
			binary.BigEndian.PutUint32(packet[24:], uint32(i*1100))
			cli.sent(packet, now)
			if !loss || i%89 != 0 {
				hub.incoming(packet, buf[:], now.Add(100*time.Millisecond), func([]byte) {})
			}
			controls(cli, hub, now)
			controls(hub, cli, now.Add(100*time.Millisecond))
		}
	}
	drive(600, false)
	if cli.stats.CodedPieces != 0 {
		t.Fatal("clean session emitted parity")
	}
	drive(9000, true)
	if cli.stats.CodedPieces == 0 || hub.stats.Recovered == 0 || cli.stats.AutoTxOn == 0 || hub.stats.AutoTxOn != 0 {
		t.Fatalf("did not protect only the impaired path: tx %+v rx %+v", cli.stats, hub.stats)
	}
	drive(6000, false)
	pieces := cli.stats.CodedPieces
	drive(2500, false)
	if cli.stats.CodedPieces != pieces || cli.stats.AutoTxOff == 0 || c.session.generation() != generation {
		t.Fatal("did not release protection on original live session")
	}
}
