package awgrepair

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"net/netip"
	"testing"
	"time"
)

func generateKeyPair(t *testing.T) (privateHex, publicHex string) {
	t.Helper()
	curve := ecdh.X25519()
	priv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(priv.Bytes()), hex.EncodeToString(priv.PublicKey().Bytes())
}

func TestDeriveKeyAcceptsValidX25519Pair(t *testing.T) {
	clientPriv, clientPub := generateKeyPair(t)
	_, hubPub := generateKeyPair(t)
	if _, err := deriveKey(clientPriv, hubPub, true); err != nil {
		t.Fatalf("client derive: %v", err)
	}
	if _, err := deriveKey(clientPriv, hubPub, false); err != nil {
		t.Fatalf("hub derive: %v", err)
	}
	if _, err := deriveKey(clientPriv, clientPub, true); err == nil {
		t.Fatal("expected error deriving against own public key")
	}
}

func TestIsInnerControlRecognizesMarkedFrames(t *testing.T) {
	clientPriv, _ := generateKeyPair(t)
	_, hubPub := generateKeyPair(t)
	k, err := deriveKey(clientPriv, hubPub, true)
	if err != nil {
		t.Fatal(err)
	}
	var s innerSession
	src := netip.MustParseAddr("192.0.2.1")
	dst := netip.MustParseAddr("192.0.2.2")
	now := time.Unix(100, 0)
	out := s.begin(&k, src, dst, now, nil)
	if out == nil {
		t.Fatal("begin did not produce hello")
	}
	if !isInnerControl(out) {
		t.Fatal("hello not recognized as inner control")
	}
	if isInnerControl([]byte("ordinary AWG packet")) {
		t.Fatal("ordinary data mistaken for inner control")
	}
	bad := append([]byte(nil), out...)
	bad[20] ^= 0xff
	if isInnerControl(bad) {
		t.Fatal("corrupted marker still recognized")
	}
}

func TestSessionHandshake(t *testing.T) {
	clientPriv, clientPub := generateKeyPair(t)
	hubPriv, hubPub := generateKeyPair(t)
	clientKey, err := deriveKey(clientPriv, hubPub, true)
	if err != nil {
		t.Fatal(err)
	}
	hubKey, err := deriveKey(hubPriv, clientPub, false)
	if err != nil {
		t.Fatal(err)
	}

	clientAddr := netip.MustParseAddr("192.0.2.1")
	hubAddr := netip.MustParseAddr("192.0.2.2")
	now := time.Unix(100, 0)

	var client, hub innerSession
	var buf [innerMTU]byte

	hello := client.begin(&clientKey, clientAddr, hubAddr, now, buf[:0])
	if hello == nil {
		t.Fatal("client did not send hello")
	}
	if client.ready(now) {
		t.Fatal("client ready before handshake")
	}

	_, _, challenge, consumed := hub.receive(&hubKey, hubAddr, clientAddr, false, hello, now, buf[:0])
	if !consumed || challenge == nil {
		t.Fatalf("hub did not consume hello or produce challenge: consumed=%v challenge=%v", consumed, challenge)
	}

	_, _, confirm, consumed := client.receive(&clientKey, clientAddr, hubAddr, true, challenge, now, buf[:0])
	if !consumed || confirm == nil {
		t.Fatalf("client did not consume challenge or produce confirm: consumed=%v confirm=%v", consumed, confirm)
	}
	if !client.ready(now) {
		t.Fatal("client not ready after confirm")
	}
	gen0 := client.generation()
	if gen0 == 0 {
		t.Fatal("generation should be non-zero after handshake")
	}

	kind, payload, _, consumed := hub.receive(&hubKey, hubAddr, clientAddr, false, confirm, now, buf[:0])
	if !consumed || kind != 0 || payload != nil {
		t.Fatalf("hub did not establish after confirm: kind=%d payload=%v consumed=%v", kind, payload, consumed)
	}
	if !hub.ready(now) {
		t.Fatal("hub not ready after confirm")
	}
	if hub.generation() != gen0 {
		t.Fatalf("generation mismatch: client=%d hub=%d", gen0, hub.generation())
	}
}

func TestSessionDataPassThrough(t *testing.T) {
	clientPriv, clientPub := generateKeyPair(t)
	hubPriv, hubPub := generateKeyPair(t)
	clientKey, _ := deriveKey(clientPriv, hubPub, true)
	hubKey, _ := deriveKey(hubPriv, clientPub, false)
	clientAddr := netip.MustParseAddr("192.0.2.1")
	hubAddr := netip.MustParseAddr("192.0.2.2")
	now := time.Unix(100, 0)
	var client, hub innerSession
	var buf [innerMTU]byte

	shakeHands(t, &client, &hub, &clientKey, &hubKey, clientAddr, hubAddr, now, buf[:])

	data := []byte("repair measure payload")
	frame := client.send(measure, clientAddr, hubAddr, data, buf[:0])
	if frame == nil {
		t.Fatal("send returned nil for established session")
	}
	if len(frame) > innerMTU {
		t.Fatalf("frame %d bytes exceeds innerMTU", len(frame))
	}

	if bytes.Contains(frame, data) {
		t.Fatal("control disclosed packet-derived payload after outer route reassignment")
	}
	kind, payload, _, consumed := hub.receive(&hubKey, hubAddr, clientAddr, false, frame, now, buf[:0])
	if !consumed {
		t.Fatal("measure frame not consumed")
	}
	if kind != measure {
		t.Fatalf("expected kind measure, got %d", kind)
	}
	if !bytes.Equal(payload, data) {
		t.Fatalf("payload mismatch: %q", payload)
	}

	fb := []byte("feedback payload")
	fbFrame := hub.send(feedback, hubAddr, clientAddr, fb, buf[:0])
	kind, payload, _, consumed = client.receive(&clientKey, clientAddr, hubAddr, true, fbFrame, now, buf[:0])
	if !consumed || kind != feedback || !bytes.Equal(payload, fb) {
		t.Fatalf("feedback failed kind=%d payload=%q consumed=%v", kind, payload, consumed)
	}
}

func TestReattachedClientRenegotiatesBeforeSessionExpiry(t *testing.T) {
	cp, cu := generateKeyPair(t)
	hp, hu := generateKeyPair(t)
	ck, _ := deriveKey(cp, hu, true)
	hk, _ := deriveKey(hp, cu, false)
	ca, ha := netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.1")
	now := time.Now()
	var old, hub, restarted innerSession
	var buf [innerMTU]byte
	shakeHands(t, &old, &hub, &ck, &hk, ca, ha, now, buf[:])
	oldGen := hub.gen
	stale := old.send(autoReport, ca, ha, []byte{1}, nil)
	shakeHands(t, &restarted, &hub, &ck, &hk, ca, ha, now.Add(time.Second), buf[:])
	if hub.gen == oldGen || hub.gen != restarted.gen {
		t.Fatal("replacement carrier retained old recovery generation")
	}
	kind, data, _, _ := hub.receive(&hk, ha, ca, false, stale, now.Add(time.Second), nil)
	if kind != 0 || data != nil {
		t.Fatal("replacement accepted old recovery payload")
	}
}

func TestSessionRejectsReplay(t *testing.T) {
	clientPriv, clientPub := generateKeyPair(t)
	hubPriv, hubPub := generateKeyPair(t)
	clientKey, _ := deriveKey(clientPriv, hubPub, true)
	hubKey, _ := deriveKey(hubPriv, clientPub, false)
	clientAddr := netip.MustParseAddr("192.0.2.1")
	hubAddr := netip.MustParseAddr("192.0.2.2")
	now := time.Unix(100, 0)
	var client, hub innerSession
	var buf [innerMTU]byte

	shakeHands(t, &client, &hub, &clientKey, &hubKey, clientAddr, hubAddr, now, buf[:])

	frame := client.send(measure, clientAddr, hubAddr, []byte("x"), buf[:0])
	kind, payload, _, consumed := hub.receive(&hubKey, hubAddr, clientAddr, false, frame, now, buf[:0])
	if !consumed || kind != measure {
		t.Fatal("first frame not accepted")
	}
	if len(payload) != 1 {
		t.Fatal("payload length mismatch")
	}

	kind, payload, _, consumed = hub.receive(&hubKey, hubAddr, clientAddr, false, frame, now, buf[:0])
	if kind != 0 || payload != nil {
		t.Fatal("replay exposed payload")
	}
	if !consumed {
		t.Fatal("authentic replay should be dropped, not passed through")
	}
}

func TestSessionExpires(t *testing.T) {
	clientPriv, clientPub := generateKeyPair(t)
	hubPriv, hubPub := generateKeyPair(t)
	clientKey, _ := deriveKey(clientPriv, hubPub, true)
	hubKey, _ := deriveKey(hubPriv, clientPub, false)
	clientAddr := netip.MustParseAddr("192.0.2.1")
	hubAddr := netip.MustParseAddr("192.0.2.2")
	now := time.Unix(100, 0)
	var client, hub innerSession
	var buf [innerMTU]byte

	shakeHands(t, &client, &hub, &clientKey, &hubKey, clientAddr, hubAddr, now, buf[:])

	later := now.Add(sessionLifetime + time.Second)
	if client.ready(later) || hub.ready(later) {
		t.Fatal("session should be expired")
	}

	// A data frame arriving after expiry is authentic but must not expose a
	// payload to the parent.
	frame := client.send(measure, clientAddr, hubAddr, []byte("x"), buf[:0])
	if frame == nil {
		t.Fatal("send produced no frame")
	}
	kind, payload, _, consumed := hub.receive(&hubKey, hubAddr, clientAddr, false, frame, later, buf[:0])
	if kind != 0 || payload != nil {
		t.Fatal("expired session exposed payload")
	}
	if !consumed {
		t.Fatal("authentic expired frame should be dropped, not passed through")
	}
}

func TestSessionReconnectIncrementsGeneration(t *testing.T) {
	clientPriv, clientPub := generateKeyPair(t)
	hubPriv, hubPub := generateKeyPair(t)
	clientKey, _ := deriveKey(clientPriv, hubPub, true)
	hubKey, _ := deriveKey(hubPriv, clientPub, false)
	clientAddr := netip.MustParseAddr("192.0.2.1")
	hubAddr := netip.MustParseAddr("192.0.2.2")
	now := time.Unix(100, 0)
	var client, hub innerSession
	var buf [innerMTU]byte

	shakeHands(t, &client, &hub, &clientKey, &hubKey, clientAddr, hubAddr, now, buf[:])
	gen0 := client.generation()

	// Let session expire and reconnect.
	now = now.Add(sessionLifetime + time.Second)
	shakeHands(t, &client, &hub, &clientKey, &hubKey, clientAddr, hubAddr, now, buf[:])
	gen1 := client.generation()
	if gen1 == gen0 {
		t.Fatal("generation should change after reconnect")
	}
}

func TestSessionMTULimit(t *testing.T) {
	clientPriv, clientPub := generateKeyPair(t)
	hubPriv, hubPub := generateKeyPair(t)
	clientKey, _ := deriveKey(clientPriv, hubPub, true)
	hubKey, _ := deriveKey(hubPriv, clientPub, false)
	clientAddr := netip.MustParseAddr("192.0.2.1")
	hubAddr := netip.MustParseAddr("192.0.2.2")
	now := time.Unix(100, 0)
	var client, hub innerSession
	var buf [innerMTU]byte

	shakeHands(t, &client, &hub, &clientKey, &hubKey, clientAddr, hubAddr, now, buf[:])

	large := make([]byte, innerMaxData+1)
	if client.send(measure, clientAddr, hubAddr, large, buf[:0]) != nil {
		t.Fatal("send should reject oversized payload")
	}

	ok := make([]byte, innerMaxData)
	frame := client.send(measure, clientAddr, hubAddr, ok, buf[:0])
	if frame == nil || len(frame) != innerMTU {
		t.Fatalf("max payload produced frame %d bytes, want %d", len(frame), innerMTU)
	}
}

func TestSessionCrossPeerAuthentication(t *testing.T) {
	clientPriv, clientPub := generateKeyPair(t)
	hubPriv, hubPub := generateKeyPair(t)
	clientKey, _ := deriveKey(clientPriv, hubPub, true)
	hubKey, _ := deriveKey(hubPriv, clientPub, false)

	_, otherClientPub := generateKeyPair(t)
	otherHubPriv, _ := generateKeyPair(t)
	otherHubKey, _ := deriveKey(otherHubPriv, otherClientPub, false)

	clientAddr := netip.MustParseAddr("192.0.2.1")
	hubAddr := netip.MustParseAddr("192.0.2.2")
	now := time.Unix(100, 0)
	var client, hub innerSession
	var buf [innerMTU]byte

	shakeHands(t, &client, &hub, &clientKey, &hubKey, clientAddr, hubAddr, now, buf[:])

	frame := client.send(measure, clientAddr, hubAddr, []byte("x"), buf[:0])

	// A different hub peer/session must not consume a frame authenticated for
	// the first peer, even when the addresses match.
	var otherHub innerSession
	_, _, _, consumed := otherHub.receive(&otherHubKey, hubAddr, clientAddr, false, frame, now, buf[:0])
	if consumed {
		t.Fatal("cross-peer frame authenticated")
	}
}

func TestSessionWrongAddressNotConsumed(t *testing.T) {
	clientPriv, clientPub := generateKeyPair(t)
	hubPriv, hubPub := generateKeyPair(t)
	clientKey, _ := deriveKey(clientPriv, hubPub, true)
	hubKey, _ := deriveKey(hubPriv, clientPub, false)
	clientAddr := netip.MustParseAddr("192.0.2.1")
	hubAddr := netip.MustParseAddr("192.0.2.2")
	wrongAddr := netip.MustParseAddr("192.0.2.3")
	now := time.Unix(100, 0)
	var client, hub innerSession
	var buf [innerMTU]byte

	shakeHands(t, &client, &hub, &clientKey, &hubKey, clientAddr, hubAddr, now, buf[:])

	frame := client.send(measure, clientAddr, hubAddr, []byte("x"), buf[:0])
	_, _, _, consumed := hub.receive(&hubKey, hubAddr, wrongAddr, false, frame, now, buf[:0])
	if consumed {
		t.Fatal("frame with wrong remote address was consumed")
	}
}

func TestDuplicateConfirmDoesNotResetReplay(t *testing.T) {
	clientPriv, clientPub := generateKeyPair(t)
	hubPriv, hubPub := generateKeyPair(t)
	clientKey, _ := deriveKey(clientPriv, hubPub, true)
	hubKey, _ := deriveKey(hubPriv, clientPub, false)
	clientAddr := netip.MustParseAddr("192.0.2.1")
	hubAddr := netip.MustParseAddr("192.0.2.2")
	now := time.Unix(100, 0)
	var client, hub innerSession
	var buf [innerMTU]byte

	shakeHands(t, &client, &hub, &clientKey, &hubKey, clientAddr, hubAddr, now, buf[:])

	// Client replays the confirm (e.g., because it never got an ACK).
	// Re-obtain the confirm from the stored handshake transcript.
	confirm := sealInner(nil, &clientKey, clientAddr.As4(), hubAddr.As4(), confirm, 0, hub.generation(), client.pending[:32])
	seq0 := hub.seq
	rx0 := hub.rx
	_, _, _, consumed := hub.receive(&hubKey, hubAddr, clientAddr, false, confirm, now, buf[:0])
	if !consumed {
		t.Fatal("duplicate confirm should be consumed (dropped)")
	}
	if hub.seq != seq0 || hub.rx != rx0 {
		t.Fatal("duplicate confirm reset session state")
	}
}

func TestDuplicateChallengeDoesNotResetReplay(t *testing.T) {
	clientPriv, clientPub := generateKeyPair(t)
	hubPriv, hubPub := generateKeyPair(t)
	clientKey, _ := deriveKey(clientPriv, hubPub, true)
	hubKey, _ := deriveKey(hubPriv, clientPub, false)
	clientAddr := netip.MustParseAddr("192.0.2.1")
	hubAddr := netip.MustParseAddr("192.0.2.2")
	now := time.Unix(100, 0)
	var client, hub innerSession
	var buf [innerMTU]byte

	hello := client.begin(&clientKey, clientAddr, hubAddr, now, buf[:0])
	_, _, challenge, _ := hub.receive(&hubKey, hubAddr, clientAddr, false, hello, now, buf[:0])
	challenge = bytes.Clone(challenge)
	_, _, confirm, _ := client.receive(&clientKey, clientAddr, hubAddr, true, challenge, now, buf[:0])
	_, _, _, _ = hub.receive(&hubKey, hubAddr, clientAddr, false, confirm, now, buf[:0])

	seq0 := client.seq
	rx0 := client.rx

	// Hub retransmits the same challenge.
	_, _, reply, consumed := client.receive(&clientKey, clientAddr, hubAddr, true, challenge, now, buf[:0])
	if !consumed || reply == nil {
		t.Fatal("retransmitted challenge should elicit a confirm")
	}
	if client.seq != seq0 || client.rx != rx0 {
		t.Fatal("retransmitted challenge reset session state")
	}
}

func TestSendDoesNotRefreshLast(t *testing.T) {
	clientPriv, clientPub := generateKeyPair(t)
	hubPriv, hubPub := generateKeyPair(t)
	clientKey, _ := deriveKey(clientPriv, hubPub, true)
	hubKey, _ := deriveKey(hubPriv, clientPub, false)
	clientAddr := netip.MustParseAddr("192.0.2.1")
	hubAddr := netip.MustParseAddr("192.0.2.2")
	now := time.Unix(100, 0)
	var client, hub innerSession
	var buf [innerMTU]byte

	shakeHands(t, &client, &hub, &clientKey, &hubKey, clientAddr, hubAddr, now, buf[:])
	hub.last = now // freeze receive-side freshness

	// Sending data must not make an otherwise idle session appear ready.
	_ = hub.send(feedback, hubAddr, clientAddr, []byte("x"), buf[:0])
	if hub.ready(now.Add(sessionLifetime + time.Second)) {
		t.Fatal("send refreshed session lifetime")
	}
}

func TestHelloRetransmitEchosChallenge(t *testing.T) {
	clientPriv, clientPub := generateKeyPair(t)
	hubPriv, hubPub := generateKeyPair(t)
	clientKey, _ := deriveKey(clientPriv, hubPub, true)
	hubKey, _ := deriveKey(hubPriv, clientPub, false)
	clientAddr := netip.MustParseAddr("192.0.2.1")
	hubAddr := netip.MustParseAddr("192.0.2.2")
	now := time.Unix(100, 0)
	var client, hub innerSession
	var buf [innerMTU]byte

	hello := client.begin(&clientKey, clientAddr, hubAddr, now, buf[:0])
	hello = bytes.Clone(hello)
	_, _, challenge1, _ := hub.receive(&hubKey, hubAddr, clientAddr, false, hello, now, buf[:0])
	challenge1 = bytes.Clone(challenge1)
	_, _, challenge2, _ := hub.receive(&hubKey, hubAddr, clientAddr, false, hello, now, buf[:0])
	if !bytes.Equal(challenge1, challenge2) {
		t.Fatalf("repeated hello did not echo the same challenge")
	}

	// After the pending lifetime expires the hub starts fresh.
	later := now.Add(probeLifetime + time.Second)
	_, _, challenge3, consumed := hub.receive(&hubKey, hubAddr, clientAddr, false, hello, later, buf[:0])
	if !consumed || bytes.Equal(challenge1, challenge3) {
		t.Fatal("hub did not restart handshake after pending timeout")
	}
}

func shakeHands(t *testing.T, client, hub *innerSession, clientKey, hubKey *key, clientAddr, hubAddr netip.Addr, now time.Time, buf []byte) {
	t.Helper()
	hello := client.begin(clientKey, clientAddr, hubAddr, now, buf[:0])
	if hello == nil {
		t.Fatal("client did not produce hello")
	}
	_, _, challenge, ok := hub.receive(hubKey, hubAddr, clientAddr, false, hello, now, buf[:0])
	if !ok || challenge == nil {
		t.Fatal("hub did not produce challenge")
	}
	_, _, confirm, ok := client.receive(clientKey, clientAddr, hubAddr, true, challenge, now, buf[:0])
	if !ok || confirm == nil {
		t.Fatal("client did not produce confirm")
	}
	_, _, _, ok = hub.receive(hubKey, hubAddr, clientAddr, false, confirm, now, buf[:0])
	if !ok {
		t.Fatal("hub did not establish")
	}
}
