package awgrepair

import (
	"bytes"
	"net/netip"
	"testing"
	"time"
)

func TestWideRequiresBothEndpointsAndFreshSession(t *testing.T) {
	for _, allow := range []bool{false, true} {
		client, hub := newDevice(fixtureTUN(), true), newDevice(fixtureTUN(), false)
		client.profile = wideRequest
		if allow {
			hub.profile = wideRequest
		}
		cp, cu := generateKeyPair(t)
		hp, hu := generateKeyPair(t)
		ca, ha := netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.1")
		ck, _ := deriveKey(cp, hu, true)
		hk, _ := deriveKey(hp, cu, false)
		c, h := &path{k: ck, local: ca, remote: ha}, &path{k: hk, local: ha, remote: ca}
		client.replace(map[netip.Addr]*path{ha: c})
		hub.replace(map[netip.Addr]*path{ca: h})
		now := time.Now()
		var buf [innerMTU]byte
		var sz [1]int
		shakeHands(t, &c.session, &h.session, &ck, &hk, ca, ha, now, buf[:])
		pkt := make([]byte, 256)
		pkt[0], pkt[2], pkt[8], pkt[9], pkt[32], pkt[33] = 0x45, 1, 64, 6, 0x50, 0x18
		copy(pkt[12:16], ca.AsSlice())
		copy(pkt[16:20], ha.AsSlice())
		client.sent(pkt, now)
		if n, _ := client.drain([][]byte{buf[:]}, sz[:], 0, now); n != 1 || bytes.Equal(buf[:sz[0]], pkt) {
			t.Fatal("client copied data before hub accepted bandwidth cost")
		}
		hub.incoming(buf[:sz[0]], buf[:], now, nil)
		n, _ := hub.drain([][]byte{buf[:]}, sz[:], 0, now)
		if !allow {
			if n != 0 {
				t.Fatal("disabled hub accepted wide mode")
			}
			continue
		}
		if n != 1 {
			t.Fatal("no authenticated reply")
		}
		client.incoming(buf[:sz[0]], buf[:], now, nil)
		client.sent(pkt, now)
		n, _ = client.drain([][]byte{buf[:]}, sz[:], 0, now)
		if n != 1 || !bytes.Equal(buf[:sz[0]], pkt) {
			t.Fatal("negotiated replica did not reach normal AWG encryption")
		}
		// Lose the original and its first replica: one independent transmission remains.
		n, _ = client.drain([][]byte{buf[:]}, sz[:], 0, now)
		if n != 1 || !bytes.Equal(buf[:sz[0]], pkt) {
			t.Fatal("no surviving data after two erasures")
		}
		if n, _ = client.drain([][]byte{buf[:]}, sz[:], 0, now); n != 0 {
			t.Fatal("exceeded the negotiated two-copy budget")
		}
		if _, drop := hub.incoming(pkt, buf[:], now, nil); drop {
			t.Fatal("original dropped")
		}
		if _, drop := hub.incoming(pkt, buf[:], now, nil); !drop {
			t.Fatal("TCP received a duplicate despite negotiated protection")
		}
		later := now.Add(6 * time.Second)
		client.sent(pkt, later)
		for {
			n, err := client.drain([][]byte{buf[:]}, sz[:], 0, later)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				break
			}
			if bytes.Equal(buf[:sz[0]], pkt) {
				t.Fatal("continued copying after unrenewed lease")
			}
		}
		if _, drop := hub.incoming(pkt, buf[:], later, nil); drop {
			t.Fatal("continued suppressing originals after lease expiry")
		}
	}
}
