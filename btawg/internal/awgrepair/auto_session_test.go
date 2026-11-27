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

func TestEstablishedLANGrantsOnlyWithReturnPathDelay(t *testing.T) {
	for _, tc := range []struct {
		name    string
		forward time.Duration
		recent  uint16
		grant   bool
	}{
		{"shared forward FIFO", 119 * time.Millisecond, 120, false},
		{"longer symmetric path", 60 * time.Millisecond, 120, true},
		{"second symmetric step", 120 * time.Millisecond, 240, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var d Device
			var p path
			p.session.peerStep = true
			var s stream
			start := time.Now()
			var pulse [7]byte
			binary.BigEndian.PutUint16(pulse[:2], 2)
			binary.BigEndian.PutUint32(pulse[2:6], clock(start))
			d.autoProbe(&p, &s, pulse[:], start.Add(time.Millisecond))
			packet := makeTCP(t, 0x18, bytes.Repeat([]byte{7}, 1100))
			for batch := 1; batch <= 8; batch++ {
				stamp := start.Add(time.Duration(batch) * 50 * time.Millisecond)
				var probe [7 + 16*8 + 2]byte
				binary.BigEndian.PutUint16(probe[:2], 120)
				binary.BigEndian.PutUint32(probe[2:6], clock(stamp))
				probe[6] = 16
				binary.BigEndian.PutUint16(probe[len(probe)-2:], tc.recent)
				for i := 0; i < 16; i++ {
					binary.BigEndian.PutUint32(packet[24:], uint32((batch*16+i)*1100))
					id, _, _ := packetIdentity(packet)
					binary.BigEndian.PutUint64(probe[7+i*8:], id)
					if i != 15 { // one original lost, others arrive in a fast train
						s.adapt.meter.received(packet, stamp.Add(tc.forward-time.Millisecond+time.Duration(i)*time.Microsecond))
					}
				}
				d.autoProbe(&p, &s, probe[:], stamp.Add(tc.forward))
			}
			if (d.stats.AutoRxOn != 0) != tc.grant {
				t.Fatalf("repeated raw loss with %v forward delay: grants %d, want active %v", tc.forward, d.stats.AutoRxOn, tc.grant)
			}
		})
	}
}

func TestEstablishedLANQueueHoldSurvivesIdleAndClockWrap(t *testing.T) {
	for _, tc := range []struct {
		name string
		base uint32
		idle time.Duration
	}{
		{"expired loss sample", 1, 4 * time.Second},
		{"signed clock wrap", 0x7fffffe0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var d Device
			var p path
			p.session.peerStep = true
			var s stream
			now := origin.Add(24 * time.Hour)
			feed := func(at time.Time, rtt uint16, delta uint32, count byte) {
				var sample [23]byte
				binary.BigEndian.PutUint16(sample[:2], rtt)
				binary.BigEndian.PutUint32(sample[2:6], clock(at)-delta)
				sample[6] = count
				if count != 0 {
					s.adapt.meter.seen = map[uint64]struct{}{0: {}}
					for i := range 15 {
						s.adapt.meter.train.record(at.Add(time.Duration(i-15)*time.Microsecond), 1100)
					}
				}
				binary.BigEndian.PutUint64(sample[15:], 1)
				d.autoProbe(&p, &s, sample[:7+int(count)*8], at)
			}
			feed(now, 2, tc.base, 0)
			feed(now.Add(time.Second), 120, tc.base+60, 2)
			at := now.Add(2*time.Second + tc.idle)
			feed(at, 120, tc.base+118, 2)
			feed(at.Add(100*time.Millisecond), 120, tc.base+60, 2)
			if tc.idle != 0 {
				feed(at.Add(200*time.Millisecond), 120, tc.base+60, 2)
			}
			if d.stats.AutoRxOn != 0 {
				t.Fatal("granted parity during the forward-queue hold")
			}
		})
	}
}

func TestLegacyAutoPeerKeepsConservativeRTTFloor(t *testing.T) {
	var d Device
	var s stream
	var report [5]byte
	start := origin.Add(time.Second)
	binary.BigEndian.PutUint32(report[:], clock(start.Add(-2*time.Millisecond)))
	d.autoFeedback(&s, report[:], start)
	for i := 1; i <= 12; i++ {
		at := start.Add(time.Duration(i) * 250 * time.Millisecond)
		binary.BigEndian.PutUint32(report[:], clock(at.Add(-200*time.Millisecond)))
		d.autoFeedback(&s, report[:], at)
	}
	if !s.adapt.localTX {
		t.Fatal("enabled early loss probes without the peer capability")
	}
}

func TestFastPathStepNegotiationPreservesLegacyStartup(t *testing.T) {
	clientPrivate, clientPublic := generateKeyPair(t)
	hubPrivate, hubPublic := generateKeyPair(t)
	clientKey, err := deriveKey(clientPrivate, hubPublic, true)
	if err != nil {
		t.Fatal(err)
	}
	hubKey, err := deriveKey(hubPrivate, clientPublic, false)
	if err != nil {
		t.Fatal(err)
	}
	ca, ha := netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.1")
	for _, tc := range []struct {
		name                    string
		legacyClient, legacyHub bool
	}{
		{"updated pair", false, false},
		{"legacy client", true, false},
		{"legacy hub", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := innerSession{offerCold: true}
			hub := innerSession{offerCold: true}
			now := origin.Add(time.Second)
			helloFrame := client.begin(&clientKey, ca, ha, now, nil)
			if tc.legacyClient {
				helloFrame = sealInner(nil, &clientKey, ca.As4(), ha.As4(), hello, 1, 0, client.pending[:16])
			}
			_, _, response, ok := hub.receive(&hubKey, ha, ca, false, helloFrame, now, nil)
			if !ok || response == nil {
				t.Fatal("hub did not challenge the hello")
			}
			if tc.legacyHub {
				response = sealInner(nil, &hubKey, ha.As4(), ca.As4(), challenge, 1, 0, hub.pending[:32])
			}
			_, _, reply, ok := client.receive(&clientKey, ca, ha, true, response, now.Add(2*time.Millisecond), nil)
			if !ok || reply == nil {
				t.Fatal("client did not confirm the challenge")
			}
			_, _, _, ok = hub.receive(&hubKey, ha, ca, false, reply, now.Add(3*time.Millisecond), nil)
			if !ok || !client.peerCold || !hub.peerCold {
				t.Fatal("fast-step negotiation broke the existing cold-start protocol")
			}
			if want := !tc.legacyClient && !tc.legacyHub; (!tc.legacyHub && hub.peerStep != want) || (!tc.legacyClient && client.peerStep != want) {
				t.Fatalf("fast step: client=%v hub=%v, want %v on updated peers", client.peerStep, hub.peerStep, want)
			}
		})
	}
}
