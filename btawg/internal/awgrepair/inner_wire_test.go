package awgrepair

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"net/netip"
	"testing"
	"time"
)

func TestDeriveKeyRejectsMalformedInput(t *testing.T) {
	priv, pub := generateKeyPair(t)
	if _, err := deriveKey("not-hex", pub, true); err == nil {
		t.Fatal("expected error for invalid private hex")
	}
	if _, err := deriveKey(priv, "not-hex", true); err == nil {
		t.Fatal("expected error for invalid public hex")
	}
	if _, err := deriveKey(priv, "aabb", true); err == nil {
		t.Fatal("expected error for short public key")
	}
}

func TestDeriveKeyRejectsOwnPublicKey(t *testing.T) {
	priv, pub := generateKeyPair(t)
	if _, err := deriveKey(priv, pub, true); err == nil {
		t.Fatal("expected error deriving against own public key")
	}
}

func TestIsInnerControlRejectsInvalidIPv4Headers(t *testing.T) {
	priv, _ := generateKeyPair(t)
	_, pub := generateKeyPair(t)
	k, err := deriveKey(priv, pub, true)
	if err != nil {
		t.Fatal(err)
	}
	var s innerSession
	src := netip.MustParseAddr("192.0.2.1")
	dst := netip.MustParseAddr("192.0.2.2")
	base := s.begin(&k, src, dst, epoch, nil)
	if base == nil {
		t.Fatal("begin did not produce packet")
	}

	cases := []struct {
		name string
		mut  func([]byte) []byte
	}{
		{"short", func(p []byte) []byte { return p[:len(p)-1] }},
		{"wrong version", func(p []byte) []byte { p[0] = 0x55; return p }},
		{"wrong protocol", func(p []byte) []byte { p[9] = 17; return p }},
		{"wrong total length", func(p []byte) []byte { binary.BigEndian.PutUint16(p[2:4], uint16(len(p)+1)); return p }},
		{"fragmented", func(p []byte) []byte { binary.BigEndian.PutUint16(p[6:8], 0x0000); return p }},
		{"bad checksum", func(p []byte) []byte { p[10] ^= 0xff; return p }},
		{"bad marker", func(p []byte) []byte { p[ipv4HeaderLen] ^= 0xff; return p }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := c.mut(append([]byte(nil), base...))
			if isInnerControl(p) {
				t.Fatal("invalid packet recognized as inner control")
			}
		})
	}
}

func TestReceiveRejectsTamperedPackets(t *testing.T) {
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
	for off := range frame {
		tampered := append([]byte(nil), frame...)
		tampered[off] ^= 0xff
		_, _, _, consumed := hub.receive(&hubKey, hubAddr, clientAddr, false, tampered, now, buf[:0])
		if consumed {
			t.Fatalf("tampered byte %d was consumed", off)
		}
	}
}

func TestSealUsesProvidedBuffer(t *testing.T) {
	priv, _ := generateKeyPair(t)
	_, pub := generateKeyPair(t)
	k, err := deriveKey(priv, pub, true)
	if err != nil {
		t.Fatal(err)
	}
	var s innerSession
	src := netip.MustParseAddr("192.0.2.1")
	dst := netip.MustParseAddr("192.0.2.2")
	buf := make([]byte, innerMTU)
	out := s.begin(&k, src, dst, epoch, buf[:0])
	if !bytes.Equal(out, buf[:len(out)]) {
		t.Fatal("seal did not use provided buffer")
	}
}

func TestSessionKeyIsDirectionallyConsistent(t *testing.T) {
	curve := ecdh.X25519()
	cp, _ := curve.GenerateKey(rand.Reader)
	hp, _ := curve.GenerateKey(rand.Reader)
	client, _ := deriveKey(hex.EncodeToString(cp.Bytes()), hex.EncodeToString(hp.PublicKey().Bytes()), true)
	hub, _ := deriveKey(hex.EncodeToString(hp.Bytes()), hex.EncodeToString(cp.PublicKey().Bytes()), false)

	nonces := make([]byte, 32)
	rand.Read(nonces)
	cs := client.sessionKey(nonces)
	hs := hub.sessionKey(nonces)

	if !bytes.Equal(cs.send[:], hs.recv[:]) {
		t.Fatal("client send key does not match hub recv key")
	}
	if !bytes.Equal(cs.recv[:], hs.send[:]) {
		t.Fatal("client recv key does not match hub send key")
	}
}

var epoch = time.Unix(100, 0)
