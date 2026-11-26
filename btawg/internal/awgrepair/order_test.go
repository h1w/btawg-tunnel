package awgrepair

import (
	"bytes"
	"net/netip"
	"testing"
	"time"
)

// The low-level codec tests exercise one transport domain.
func deriveKey(private, public string, client bool) (key, error) {
	return deriveTransportKey(private, public, client, "socks5-awg-udp")
}

func TestLateOriginalDeliveredOnlyOnceAndSpoofedRecoveryRejected(t *testing.T) {
	for _, spoof := range []bool{false, true} {
		hub := newDevice(fixtureTUN(), false)
		clientAddr := netip.MustParseAddr("10.0.0.2")
		hubAddr := netip.MustParseAddr("10.0.0.1")
		foreign := netip.MustParseAddr("10.0.0.9")
		hc, hp := generateKeyPair(t)
		cc, cp := generateKeyPair(t)
		hk, err := deriveKey(hc, cp, false)
		if err != nil {
			t.Fatal(err)
		}
		ck, err := deriveKey(cc, hp, true)
		if err != nil {
			t.Fatal(err)
		}
		p := &path{k: hk, local: hubAddr, remote: clientAddr}
		hub.replace(map[netip.Addr]*path{clientAddr: p})
		now := time.Now()
		var cli innerSession
		var work [innerMTU]byte
		shakeHands(t, &cli, &p.session, &ck, &hk, clientAddr, hubAddr, now, work[:])
		s := hub.state(p, now)
		var packets [blockSize][]byte
		b := block{protected: true}
		for i := range packets {
			q := make([]byte, 256)
			q[0], q[2], q[3], q[8], q[9] = 0x45, 1, 0, 64, 17
			q[20] = byte(i)
			src := clientAddr
			if i == 3 && spoof {
				src = foreign
			}
			copy(q[12:16], src.AsSlice())
			copy(q[16:20], hubAddr.AsSlice())
			packets[i] = q
			b.add(q)
			if i != 3 {
				s.recent.remember(q)
			}
		}
		var recovered []byte
		b.parts(1, clock(now), true, func(payload []byte) bool {
			var wire [innerMTU]byte
			frame := cli.send(measure, clientAddr, hubAddr, payload, wire[:0])
			r, handled := hub.incoming(frame, wire[:], now, nil)
			if !handled {
				t.Fatal("did not consume valid repair control")
			}
			if r != nil {
				recovered = bytes.Clone(r)
			}
			return true
		})
		if spoof {
			if recovered != nil {
				t.Fatal("reconstructed source bypassed AWG allowed-IP policy")
			}
			continue
		}
		if !bytes.Equal(recovered, packets[3]) {
			t.Fatal("missing legitimate recovered packet")
		}
		if _, consumed := hub.incoming(packets[3], work[:], now.Add(time.Millisecond), nil); !consumed {
			t.Fatal("late original was delivered again after successful reconstruction")
		}
		if _, consumed := hub.incoming(packets[3], work[:], now.Add(2*time.Millisecond), nil); consumed {
			t.Fatal("a new identical application datagram must not be suppressed forever")
		}
	}
}

func TestQueuedRepairCannotCrossEnrollmentChange(t *testing.T) {
	private, pub := generateKeyPair(t)
	clientPrivate, clientPub := generateKeyPair(t)
	_, replacementPub := generateKeyPair(t)
	hub := NewHub(fixtureTUN(), private, "socks5-awg-udp")
	defer hub.Close()
	clientIP, gateway := netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.1")
	enrolled := Peer{Local: gateway, Remote: clientIP, Public: clientPub}
	if err := hub.SetPeers([]Peer{enrolled}); err != nil {
		t.Fatal(err)
	}
	remoteKey, err := deriveTransportKey(clientPrivate, pub, true, "socks5-awg-udp")
	if err != nil {
		t.Fatal(err)
	}
	p := hub.paths[clientIP]
	var remote innerSession
	var buf [innerMTU]byte
	now := time.Now()
	shakeHands(t, &remote, &p.session, &remoteKey, &p.k, clientIP, gateway, now, buf[:])
	hub.appendControl(p, now, func(dst []byte) []byte {
		return p.session.send(feedback, gateway, clientIP, []byte("old enrollment"), dst)
	})
	enrolled.Public = replacementPub
	if err = hub.SetPeers([]Peer{enrolled}); err != nil {
		t.Fatal(err)
	}
	if n, err := hub.drain([][]byte{buf[:]}, make([]int, 1), 0, now); n != 0 || err != nil {
		t.Fatalf("obsolete control reached encryption: n=%d err=%v", n, err)
	}
	if hub.paths[clientIP].session.ready(now) {
		t.Fatal("new enrollment inherited previous recovery session")
	}
}
