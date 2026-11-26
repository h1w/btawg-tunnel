package awgrepair

import (
	"bytes"
	"crypto/cipher"
	"encoding/binary"
	"net/netip"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	sessionLifetime = 30 * time.Second
	probeEvery      = 2 * time.Second
	probeLifetime   = 5 * time.Second
)

// innerSession authenticates inner IPv4 control frames and runs a 3-way
// handshake to establish a fresh per-session key.
type innerSession struct {
	established         bool
	client              bool
	gen                 uint64
	k                   key
	sendAEAD, recvAEAD  cipher.AEAD
	plaintext           [innerMaxData]byte // valid through the current receive callback; device state is locked
	seq                 uint64
	rx                  replay
	last                time.Time
	pending             [32]byte
	pendingAt           time.Time
	nextHello           time.Time
	handshakeRTT        time.Duration // locally timed hello/challenge or challenge/confirm
	offerCold, peerCold bool
}

// begin emits a hello handshake packet if the session is not established or
// has expired, respecting a rate limit and retrying while unaffirmed.
func (s *innerSession) begin(k *key, src, dst netip.Addr, now time.Time, out []byte) []byte {
	if s.established && s.ready(now) {
		return nil
	}
	if now.Before(s.nextHello) {
		return nil
	}
	s.client = true
	s.nextHello = now.Add(probeEvery)
	if !readNonce(s.pending[:16]) {
		return nil
	}
	s.pendingAt = now
	s.established = false
	src4, dst4 := src.As4(), dst.As4()
	return sealInner(out, k, src4, dst4, hello, s.coldFlag(), 0, s.pending[:16])
}

// Older peers use zero in the authenticated handshake sequence field and
// ignore it on receipt. Session data replay counters are unaffected.
func (s *innerSession) coldFlag() uint64 {
	if s.offerCold {
		return coldCapVersion
	}
	return 0
}
func (s *innerSession) challengeFlag() uint64 {
	if s.peerCold {
		return s.coldFlag()
	}
	return 0
}

// send emits an authenticated inner IPv4 control packet if the session is
// established. The returned packet is at most innerMTU bytes. Freshness is
// receive-driven; send does not extend the session lifetime.
func (s *innerSession) send(kind byte, src, dst netip.Addr, payload, out []byte) []byte {
	if !s.established {
		return nil
	}
	s.seq++
	if s.seq == 0 {
		s.established = false // never reuse a nonce; begin a new handshake
		return nil
	}
	src4, dst4 := src.As4(), dst.As4()
	frame := innerFrame(out, src4, dst4, kind, s.seq, s.gen, len(payload))
	if frame == nil {
		return nil
	}
	const head = ipv4HeaderLen + innerHeaderLen
	var nonce [chacha20poly1305.NonceSize]byte
	binary.BigEndian.PutUint64(nonce[4:], s.seq)
	return s.sendAEAD.Seal(frame[:head], nonce[:], payload, frame[:head])
}

// receive validates an inner IPv4 control packet, advances the handshake if
// needed, and returns any handshake reply. If the packet is not authentic or
// does not belong to this session it reports consumed==false.
func (s *innerSession) receive(k *key, local, remote netip.Addr, client bool, packet []byte, now time.Time, out []byte) (kind byte, payload []byte, reply []byte, consumed bool) {
	local4, remote4 := local.As4(), remote.As4()
	kind, hdr, body, ok := peekKind(packet, local4, remote4)
	if !ok {
		return 0, nil, nil, false
	}

	var seq, gen uint64
	var data []byte
	switch kind {
	case hello, challenge, confirm:
		seq, gen, data, ok = verifyBody(hdr, body, k.recv[:])
	case measure, feedback, wideRequest, wideAccept, codedRequest, codedAccept, codedParity, autoRequest, autoAccept, autoSample, autoReport:
		if !s.established {
			return 0, nil, nil, false
		}
		seq = binary.BigEndian.Uint64(body[innerMarkerLen+innerKindLen:])
		gen = binary.BigEndian.Uint64(body[innerMarkerLen+innerKindLen+innerSeqLen:])
		if gen != s.gen {
			return 0, nil, nil, false
		}
		var nonce [chacha20poly1305.NonceSize]byte
		binary.BigEndian.PutUint64(nonce[4:], seq)
		var err error
		ciphertext := body[innerHeaderLen:]
		data, err = s.recvAEAD.Open(s.plaintext[:0], nonce[:], ciphertext, packet[:ipv4HeaderLen+innerHeaderLen])
		ok = err == nil
	default:
		seq, gen, data, ok = verifyBody(hdr, body, k.recv[:])
	}

	if !ok {
		return 0, nil, nil, false
	}

	s.client = client

	switch kind {
	case hello:
		if client || len(data) != 16 {
			return 0, nil, nil, true
		}
		// A reconnect may bring a new client nonce before the previous session
		// expires. Challenge it without changing the live key or replay window;
		// only confirmation of the fresh hub nonce can replace the generation.
		if s.established && s.ready(now) && bytes.Equal(s.pending[:16], data) && generationFromNonces(s.pending[:]) == s.gen {
			return 0, nil, nil, true
		}
		// Retransmitted hello before timeout: echo the existing challenge.
		if bytes.Equal(s.pending[:16], data) && now.Sub(s.pendingAt) <= probeLifetime {
			return 0, nil, sealInner(out, k, local4, remote4, challenge, s.challengeFlag(), 0, s.pending[:32]), true
		}
		var hubNonce [16]byte
		if !readNonce(hubNonce[:]) {
			return 0, nil, nil, true
		}
		copy(s.pending[:16], data)
		copy(s.pending[16:], hubNonce[:])
		s.peerCold = s.offerCold && seq == coldCapVersion
		s.pendingAt = now
		return 0, nil, sealInner(out, k, local4, remote4, challenge, s.challengeFlag(), 0, s.pending[:32]), true

	case challenge:
		if !client || len(data) != 32 || !bytes.Equal(data[:16], s.pending[:16]) {
			return 0, nil, nil, true
		}
		// Retransmitted challenge for the current session: re-send confirm
		// without resetting keys or replay state.
		if s.established && generationFromNonces(data) == s.gen {
			return 0, nil, sealInner(out, k, local4, remote4, confirm, 0, s.gen, data), true
		}
		if now.Sub(s.pendingAt) > probeLifetime {
			return 0, nil, nil, true
		}
		copy(s.pending[16:], data[16:])
		s.establish(k)
		s.handshakeRTT = now.Sub(s.pendingAt)
		s.peerCold = s.offerCold && seq == coldCapVersion
		s.last = now
		return 0, nil, sealInner(out, k, local4, remote4, confirm, 0, s.gen, s.pending[:32]), true

	case confirm:
		if client || len(data) != 32 {
			return 0, nil, nil, true
		}
		// Duplicate confirm for an already-established session: ignore.
		if s.established && generationFromNonces(data) == s.gen {
			return 0, nil, nil, true
		}
		if !bytes.Equal(data, s.pending[:]) || now.Sub(s.pendingAt) > probeLifetime {
			return 0, nil, nil, true
		}
		s.establish(k)
		s.handshakeRTT = now.Sub(s.pendingAt)
		s.last = now
		return 0, nil, nil, true

	case measure, feedback, wideRequest, wideAccept, codedRequest, codedAccept, codedParity, autoRequest, autoAccept, autoSample, autoReport:
		if gen != s.gen || !s.ready(now) {
			return 0, nil, nil, true
		}
		if !s.rx.accept(seq) {
			return 0, nil, nil, true
		}
		s.last = now
		return kind, data, nil, true

	default:
		// Authentic but unknown kind: drop without exposing to the parent.
		return 0, nil, nil, true
	}
}

func (s *innerSession) establish(static *key) {
	s.k = static.sessionKey(s.pending[:32])
	// Both traffic keys are 32 bytes by construction; New can fail only on length.
	s.sendAEAD, _ = chacha20poly1305.New(s.k.send[:])
	s.recvAEAD, _ = chacha20poly1305.New(s.k.recv[:])
	s.gen = generationFromNonces(s.pending[:32])
	s.established = true
	s.seq = 0
	s.rx = replay{}
}

// ready reports whether the session is established and has not expired.
func (s *innerSession) ready(now time.Time) bool {
	return s.established && now.Sub(s.last) < sessionLifetime
}

// generation returns a value that changes on every completed handshake so the
// parent can reset codec state.
func (s *innerSession) generation() uint64 {
	return s.gen
}
