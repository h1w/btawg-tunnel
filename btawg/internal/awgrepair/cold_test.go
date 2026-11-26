package awgrepair

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"testing"
	"time"
)

// A real handshake measures the path before TCP carries its first payload.
// No forced active flags or synthetic loss reports: erase an original before
// any telemetry round trip, then verify the clean session stops adding parity.
func TestAutoColdFirstFlightSurvivesBeforeLossReport(t *testing.T) {
	for _, mode := range []string{"socks5-awg-udp", "socks5-awg-ext-tcp", "socks5-awg-ext-utp", "socks5-awg-req-tcp", "socks5-awg-req-utp"} {
		t.Run(mode, func(t *testing.T) { testColdFlight(t, mode) })
	}
}

func testColdFlight(t *testing.T, mode string) {
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
	var buf [innerMTU]byte
	var sz [1]int
	pump := func(from, to *Device, at time.Time, restore func([]byte)) {
		for {
			n, err := from.drain([][]byte{buf[:]}, sz[:], 0, at)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				break
			}
			to.incoming(buf[:sz[0]], buf[:], at.Add(100*time.Millisecond), restore)
		}
	}
	syn := flightPacket(2, 8, 4000, 32000, 100, 0, false, nil)
	copy(syn[12:16], ca.AsSlice())
	copy(syn[16:20], ha.AsSlice())
	cli.StartAuto()
	// TCP starts while the recovery hello is still in flight. Both SYNs
	// must be observed, but neither is delayed for the recovery handshake.
	cli.sent(syn, now)
	hub.incoming(syn, buf[:], now.Add(100*time.Millisecond), func([]byte) {})
	synAck := flightPacket(18, 7, 4000, 32000, 900, 101, true, nil)
	copy(synAck[12:16], ha.AsSlice())
	copy(synAck[16:20], ca.AsSlice())
	hub.sent(synAck, now.Add(100*time.Millisecond))
	cli.incoming(synAck, buf[:], now.Add(200*time.Millisecond), func([]byte) {})
	// No application packet or keepalive is needed to finish negotiation.
	pump(cli, hub, now, func([]byte) {})
	pump(hub, cli, now.Add(100*time.Millisecond), func([]byte) {})
	pump(cli, hub, now.Add(200*time.Millisecond), func([]byte) {})
	now = now.Add(400 * time.Millisecond)
	p := makeTCP(t, 0x18, bytes.Repeat([]byte{3}, 1100))
	copy(p[12:16], ca.AsSlice())
	copy(p[16:20], ha.AsSlice())
	var want, got []byte
	for i := range 8 {
		binary.BigEndian.PutUint32(p[24:], uint32(1100*i))
		cli.sent(p, now)
		if i == 2 {
			want = bytes.Clone(p)
		} else {
			hub.incoming(p, buf[:], now.Add(100*time.Millisecond), func([]byte) {})
		}
	}
	pump(cli, hub, now, func(b []byte) { got = bytes.Clone(b) })
	if mode != "socks5-awg-udp" {
		if cli.stats.CodedPieces != 0 || got != nil {
			t.Fatal("provisional parity stalled ordered carrier without observed inner loss")
		}
		return
	}
	if !bytes.Equal(got, want) {
		t.Fatal("first-flight erasure exposed to TCP before any loss report")
	}
	ack := flightPacket(16, -1, 4000, 65535, 901, 1100, true, nil)
	copy(ack[12:16], ha.AsSlice())
	copy(ack[16:20], ca.AsSlice())
	hub.sent(ack, now.Add(200*time.Millisecond))
	if binary.BigEndian.Uint16(ack[34:]) != flightBytes>>7 {
		t.Fatal("lost the TCP scale negotiated before confirmation")
	}
	// A stale LAN pulse may arrive behind a WAN report, and the original
	// behind its reconstruction. Neither should cause a duplicate delivery.
	hs := hub.paths[ca].state
	var sample [7]byte
	binary.BigEndian.PutUint16(sample[:], 200)
	binary.BigEndian.PutUint32(sample[2:], 300)
	hub.autoProbe(hub.paths[ca], hs, sample[:], now)
	binary.BigEndian.PutUint16(sample[:], 1)
	binary.BigEndian.PutUint32(sample[2:], 299)
	hub.autoProbe(hub.paths[ca], hs, sample[:], now)
	if hs.adapt.localRX {
		t.Fatal("late LAN pulse reset newer WAN evidence")
	}
	binary.BigEndian.PutUint32(sample[2:], 301)
	hub.autoProbe(hub.paths[ca], hs, sample[:], now)
	if _, drop := hub.incoming(want, buf[:], now.Add(200*time.Millisecond), func([]byte) { t.Fatal("duplicate reconstruction") }); !drop {
		t.Fatal("LAN transition exposed a late duplicate")
	}
	for i := range 5000 {
		now = now.Add(time.Millisecond)
		binary.BigEndian.PutUint32(p[24:], uint32((i+8)*1100))
		cli.sent(p, now)
		hub.incoming(p, buf[:], now.Add(100*time.Millisecond), func([]byte) {})
		pump(cli, hub, now, func([]byte) {})
		pump(hub, cli, now.Add(100*time.Millisecond), func([]byte) {})
	}
	count := cli.stats.CodedPieces
	for i := range 16 {
		binary.BigEndian.PutUint32(p[24:], uint32((i+5010)*1100))
		cli.sent(p, now)
	}
	if cli.stats.CodedPieces != count {
		t.Fatal("startup protection persisted on confirmed clean path")
	}
}

// Eight streams on the clean WAN used to share a 2 MiB window for four
// seconds even when every original in the protected groups arrived.
func TestCleanFlightReleasesOnDeliveredOriginals(t *testing.T) {
	cp, cu := generateKeyPair(t)
	hp, hu := generateKeyPair(t)
	ca, ha := netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.1")
	cli, err := NewNode(fixtureTUN(), cp, Peer{Local: ca, Remote: ha, Public: hu}, "socks5-awg-udp")
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub(fixtureTUN(), hp, "socks5-awg-udp")
	if err := hub.SetPeers([]Peer{{Local: ha, Remote: ca, Public: cu}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var buf [innerMTU]byte
	var lens [1]int
	pump := func(a, b *Device, at time.Time) {
		for {
			n, err := a.drain([][]byte{buf[:]}, lens[:], 0, at)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				return
			}
			b.incoming(buf[:lens[0]], buf[:], at.Add(100*time.Millisecond), func([]byte) { t.Fatal("recovered an intact original") })
		}
	}
	cli.StartAuto()
	pump(cli, hub, now)
	pump(hub, cli, now.Add(100*time.Millisecond))
	pump(cli, hub, now.Add(200*time.Millisecond))
	now = now.Add(400 * time.Millisecond)
	p := makeTCP(t, 0x18, bytes.Repeat([]byte{9}, 1100))
	copy(p[12:16], ca.AsSlice())
	copy(p[16:20], ha.AsSlice())
	var before uint64
	for i := range 2048 {
		now = now.Add(500 * time.Microsecond)
		binary.BigEndian.PutUint32(p[24:], uint32(i*1100))
		cli.sent(p, now)
		hub.incoming(p, buf[:], now.Add(100*time.Millisecond), func([]byte) {})
		pump(cli, hub, now)
		pump(hub, cli, now.Add(100*time.Millisecond))
		if i == 1791 {
			before = cli.stats.CodedPieces
		}
	}
	if before == 0 || cli.stats.CodedPieces != before {
		s := hub.paths[ca].state
		t.Fatalf("clean path still charged startup parity: %d -> %d; intact=%d clean=%v rx=%+v txRTT=%d stats=%+v", before, cli.stats.CodedPieces, s.decoder.intactGroups, s.adapt.cleanStart, s.adapt.rx, cli.upstream.state.adapt.rttMS, hub.stats)
	}
}

func TestAutoColdBudgetDoesNotRearmOnIdle(t *testing.T) {
	var f coldFlight
	now := time.Now()
	for left := coldFlightBytes; left > 0; {
		n := min(1280, left)
		if !f.data(now, n) {
			t.Fatal("stopped before byte budget")
		}
		left -= n
	}
	if f.data(now, 1) || f.data(now.Add(time.Minute), 1280) {
		t.Fatal("byte-limited startup renewed without a fresh session")
	}
}
