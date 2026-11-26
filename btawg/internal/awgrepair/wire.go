package awgrepair

import (
	"bytes"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
)

const (
	innerProto      = 253
	ipv4HeaderLen   = 20
	innerMarkerLen  = 4
	innerKindLen    = 1
	innerSeqLen     = 8
	innerGenLen     = 8
	innerMacLen     = 16
	innerHeaderLen  = innerMarkerLen + innerKindLen + innerSeqLen + innerGenLen
	innerMTU        = 1280
	innerMinPayload = innerHeaderLen + innerMacLen
	innerMaxData    = innerMTU - ipv4HeaderLen - innerHeaderLen - innerMacLen

	hello        = byte(1)
	challenge    = byte(2)
	confirm      = byte(3)
	measure      = byte(4)
	feedback     = byte(5)
	wideRequest  = byte(6)
	wideAccept   = byte(7)
	codedRequest = byte(8)
	codedAccept  = byte(9)
	codedParity  = byte(10)
	autoRequest  = byte(11)
	autoAccept   = byte(12)
	autoSample   = byte(13)
	autoReport   = byte(14)
)

var innerMarker = [innerMarkerLen]byte{0x19, 0x86, 0x11, 0x18}

// key holds direction-oriented HMAC-SHA256 keys derived from an X25519 ECDH.
// For a client send is the client-to-server key and recv is server-to-client;
// for a hub the orientation is reversed.
type key struct {
	send [32]byte
	recv [32]byte
}

// Bind the inner handshake to the outer engine even if the enrollment uses
// the same X25519 identities for WG and AWG.
func deriveTransportKey(privateHex, publicHex string, client bool, transport string) (key, error) {
	private, err := hex.DecodeString(privateHex)
	if err != nil {
		return key{}, err
	}
	public, err := hex.DecodeString(publicHex)
	if err != nil {
		return key{}, err
	}
	curve := ecdh.X25519()
	priv, err := curve.NewPrivateKey(private)
	if err != nil {
		return key{}, err
	}
	pub, err := curve.NewPublicKey(public)
	if err != nil {
		return key{}, err
	}
	if bytes.Equal(pub.Bytes(), priv.PublicKey().Bytes()) {
		return key{}, errors.New("awgrepair: cannot derive with own public key")
	}
	secret, err := priv.ECDH(pub)
	if err != nil {
		return key{}, err
	}
	c2s := deriveHMACKey(secret, "tunnel/recovery/v3/"+transport+"/client-to-server")
	s2c := deriveHMACKey(secret, "tunnel/recovery/v3/"+transport+"/server-to-client")
	if client {
		return key{send: c2s, recv: s2c}, nil
	}
	return key{send: s2c, recv: c2s}, nil
}

func deriveHMACKey(secret []byte, label string) [32]byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(label))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// isInnerControl reports whether p looks like an inner IPv4 control frame.
// It validates the IPv4 header (protocol 253, no fragmentation, checksum,
// total length) and the marker, but performs no per-peer authentication.
func isInnerControl(p []byte) bool {
	if len(p) < ipv4HeaderLen+innerMinPayload {
		return false
	}
	if p[0] != 0x45 || p[9] != innerProto {
		return false
	}
	if int(binary.BigEndian.Uint16(p[2:4])) != len(p) {
		return false
	}
	if binary.BigEndian.Uint16(p[6:8]) != 0x4000 {
		return false
	}
	if ipv4Checksum(p[:ipv4HeaderLen]) != 0 {
		return false
	}
	return hmac.Equal(p[ipv4HeaderLen:ipv4HeaderLen+innerMarkerLen], innerMarker[:])
}

// ipv4Checksum returns the Internet checksum of a 20-byte IPv4 header.
func ipv4Checksum(header []byte) uint16 {
	var sum uint32
	for i := 0; i < len(header); i += 2 {
		sum += uint32(header[i])<<8 | uint32(header[i+1])
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// sealInner builds a complete authenticated inner IPv4 control packet in out.
// It returns nil if the payload is too large for the inner MTU.
func sealInner(out []byte, k *key, src, dst [4]byte, kind byte, seq, gen uint64, payload []byte) []byte {
	pkt := innerFrame(out, src, dst, kind, seq, gen, len(payload))
	if pkt == nil {
		return nil
	}
	body := pkt[ipv4HeaderLen:]
	copy(body[innerHeaderLen:], payload)
	end := len(body) - innerMacLen
	tag := mac(k.send[:], pkt[:ipv4HeaderLen], body[:end])
	copy(body[end:], tag[:])
	return pkt
}

// innerFrame prepares the common header. Handshake nonces use a static-key
// MAC; packet-derived session data uses AEAD and authenticates this header.
func innerFrame(out []byte, src, dst [4]byte, kind byte, seq, gen uint64, n int) []byte {
	if n > innerMaxData {
		return nil
	}
	totalLen := ipv4HeaderLen + innerHeaderLen + n + innerMacLen
	pkt := out
	if cap(pkt) < totalLen {
		pkt = make([]byte, totalLen)
	}
	pkt = pkt[:totalLen]

	hdr := pkt[:ipv4HeaderLen]
	hdr[0] = 0x45
	hdr[1] = 0
	binary.BigEndian.PutUint16(hdr[2:4], uint16(totalLen))
	binary.BigEndian.PutUint16(hdr[4:6], 0)
	binary.BigEndian.PutUint16(hdr[6:8], 0x4000)
	hdr[8] = 64
	hdr[9] = innerProto
	hdr[10], hdr[11] = 0, 0
	copy(hdr[12:16], src[:])
	copy(hdr[16:20], dst[:])
	binary.BigEndian.PutUint16(hdr[10:12], ipv4Checksum(hdr))

	body := pkt[ipv4HeaderLen:]
	copy(body[0:innerMarkerLen], innerMarker[:])
	body[innerMarkerLen] = kind
	binary.BigEndian.PutUint64(body[innerMarkerLen+innerKindLen:], seq)
	binary.BigEndian.PutUint64(body[innerMarkerLen+innerKindLen+innerSeqLen:], gen)
	return pkt
}

// mac computes a domain-separated HMAC-SHA256 truncated to innerMacLen.
func mac(key, header, payload []byte) [innerMacLen]byte {
	h := hmac.New(sha256.New, key)
	h.Write(header)
	h.Write(payload)
	var out [innerMacLen]byte
	copy(out[:], h.Sum(nil))
	return out
}

// peekKind validates the IPv4 header, addresses and marker of an inner
// control packet and returns the kind byte without authenticating it.
func peekKind(packet []byte, local, remote [4]byte) (kind byte, hdr, body []byte, ok bool) {
	if len(packet) < ipv4HeaderLen+innerMinPayload || len(packet) > innerMTU {
		return 0, nil, nil, false
	}
	hdr = packet[:ipv4HeaderLen]
	if hdr[0] != 0x45 || hdr[9] != innerProto {
		return 0, nil, nil, false
	}
	if int(binary.BigEndian.Uint16(hdr[2:4])) != len(packet) {
		return 0, nil, nil, false
	}
	if binary.BigEndian.Uint16(hdr[6:8]) != 0x4000 {
		return 0, nil, nil, false
	}
	if ipv4Checksum(hdr) != 0 {
		return 0, nil, nil, false
	}
	if [4]byte(hdr[12:16]) != remote || [4]byte(hdr[16:20]) != local {
		return 0, nil, nil, false
	}
	body = packet[ipv4HeaderLen:]
	if !hmac.Equal(body[:innerMarkerLen], innerMarker[:]) {
		return 0, nil, nil, false
	}
	return body[innerMarkerLen], hdr, body, true
}

// verifyBody checks the HMAC on an inner control body and returns the
// sequence, generation and payload. The caller must already have validated
// the IPv4 header with peekKind.
func verifyBody(hdr, body []byte, recvKey []byte) (seq, gen uint64, payload []byte, ok bool) {
	macEnd := len(body) - innerMacLen
	if macEnd < innerHeaderLen {
		return 0, 0, nil, false
	}
	tag := mac(recvKey, hdr, body[:macEnd])
	if !hmac.Equal(tag[:], body[macEnd:]) {
		return 0, 0, nil, false
	}
	seq = binary.BigEndian.Uint64(body[innerMarkerLen+innerKindLen:])
	gen = binary.BigEndian.Uint64(body[innerMarkerLen+innerKindLen+innerSeqLen:])
	payload = body[innerHeaderLen:macEnd]
	return seq, gen, payload, true
}

// sessionKey derives a fresh per-session key from the oriented static key and
// the handshake nonces. Both directions use the same domain label so that a
// client's send key matches the hub's receive key and vice versa.
func (k *key) sessionKey(nonces []byte) key {
	return key{
		send: deriveSessionKey(k.send[:], nonces),
		recv: deriveSessionKey(k.recv[:], nonces),
	}
}

func deriveSessionKey(parent []byte, nonces []byte) [32]byte {
	h := hmac.New(sha256.New, parent)
	h.Write([]byte("awgrepair/v2/session"))
	h.Write(nonces)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// generationFromNonces derives a generation value from the concatenated
// handshake nonces.
func generationFromNonces(nonces []byte) uint64 {
	h := sha256.Sum256(nonces)
	return binary.BigEndian.Uint64(h[:8])
}

// readNonce fills b with random bytes.
func readNonce(b []byte) bool {
	_, err := rand.Read(b)
	return err == nil
}

// replay rejects recently seen sequence numbers.
type replay struct {
	latest uint64
	seen   uint64
}

func (w *replay) accept(seq uint64) bool {
	if seq == 0 {
		return false
	}
	if seq > w.latest {
		w.seen <<= seq - w.latest
		w.latest = seq
	}
	delta := w.latest - seq
	if delta >= 64 || w.seen&(1<<delta) != 0 {
		return false
	}
	w.seen |= 1 << delta
	return true
}
