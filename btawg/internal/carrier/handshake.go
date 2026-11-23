package carrier

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strings"
)

// protocol is the BitTorrent protocol identifier sent in every handshake.
const protocol = "BitTorrent protocol"

// bep10ExtensionFlag is reserved byte 5 bit 0x10, advertising support for
// BEP 10 extended messaging. Exported Handshake/HandshakeBytes keep the
// baseline reserved-zero semantics; carrier framing entrypoints set this bit.
const bep10ExtensionFlag = byte(0x10)

// HandshakeBytes renders the 68-byte BitTorrent protocol handshake for
// infoHash and peerID: length-prefixed protocol name, 8 reserved zero bytes,
// the 20-byte infohash, the 20-byte peer ID.
func HandshakeBytes(infoHash, peerID [20]byte) []byte {
	h := make([]byte, 68)
	h[0] = 19
	copy(h[1:], protocol)
	copy(h[28:], infoHash[:])
	copy(h[48:], peerID[:])
	return h
}

// Handshake performs the BitTorrent protocol handshake on c: it writes its own
// handshake for infoHash/peerID concurrently with reading the peer's, and
// fails on a short exchange, wrong magic, or a mismatched infohash. The peer's
// handshake bytes are returned for caller-side routing.
func Handshake(c net.Conn, infoHash, peerID [20]byte) ([20]byte, error) {
	theirs, _, peerFeatures, err := doHandshake(c, infoHash, peerID, 0, false)
	_ = peerFeatures
	return theirs, err
}

// handshake is the historical request/piece handshake helper. It is unchanged
// so that requestpiece.go continues to compile without modification.
func handshake(c net.Conn, infoHash, peerID [20]byte, features byte) ([20]byte, byte, error) {
	theirs, _, peerFeatures, err := doHandshake(c, infoHash, peerID, features, false)
	return theirs, peerFeatures, err
}

// handshakeBEP10 performs the BitTorrent handshake with the BEP 10 extension
// flag set in reserved byte 5. It returns the peer's ID and reserved byte 5.
func handshakeBEP10(c net.Conn, infoHash, peerID [20]byte) ([20]byte, byte, error) {
	theirs, peerBEPByte, _, err := doHandshake(c, infoHash, peerID, 0, true)
	return theirs, peerBEPByte, err
}

// doHandshake performs the BitTorrent handshake. bep10 controls whether the
// BEP 10 extension flag is advertised. It returns the peer ID, the peer's
// reserved byte 5, and the peer's reserved byte 0.
func doHandshake(c net.Conn, infoHash, peerID [20]byte, features byte, bep10 bool) ([20]byte, byte, byte, error) {
	var zero [20]byte
	sendErr := make(chan error, 1)
	go func() {
		h := HandshakeBytes(infoHash, peerID)
		h[20] = features
		if bep10 {
			h[25] = bep10ExtensionFlag
		}
		sendErr <- fullWrite(c, h)
	}()
	var got [68]byte
	if _, err := io.ReadFull(c, got[:]); err != nil {
		c.Close()
		<-sendErr
		return zero, 0, 0, fmt.Errorf("read peer handshake: %w", err)
	}
	if err := <-sendErr; err != nil {
		return zero, 0, 0, fmt.Errorf("write handshake: %w", err)
	}
	if got[0] != 19 || string(got[1:20]) != protocol {
		return zero, 0, 0, fmt.Errorf("invalid BT handshake magic")
	}
	if !bytes.Equal(got[28:48], infoHash[:]) {
		return zero, 0, 0, fmt.Errorf("infohash mismatch")
	}
	var theirs [20]byte
	copy(theirs[:], got[48:68])
	return theirs, got[25], got[20], nil
}

// Accept reads the first BitTorrent handshake once on a server connection,
// dispatches to the carrier framing selected by the infohash in hashes
// ("ext" or "req", optionally with a payload suffix), replies, and
// negotiates the existing request/piece feature bits. The caller is
// responsible for any deadline on c; Accept does not reset it. The returned
// peer ID and family identify the enrollment and payload protocol.
func Accept(c net.Conn, serverID [20]byte, hashes map[[20]byte]string) (Transport, [20]byte, string, error) {
	return acceptHubLike(c, serverID, hashes, false)
}

// AcceptHub enables the explicitly configured experiments on AWG hub
// carriers. TLS payload variants still use their baseline negotiation.
func AcceptHub(c net.Conn, serverID [20]byte, hashes map[[20]byte]string) (Transport, [20]byte, string, error) {
	return acceptHubLike(c, serverID, hashes, true)
}

func acceptHubLike(c net.Conn, serverID [20]byte, hashes map[[20]byte]string, hub bool) (Transport, [20]byte, string, error) {
	var zero [20]byte
	var got [68]byte
	if _, err := io.ReadFull(c, got[:]); err != nil {
		c.Close()
		return nil, zero, "", fmt.Errorf("read peer handshake: %w", err)
	}
	if got[0] != 19 || string(got[1:20]) != protocol {
		c.Close()
		return nil, zero, "", fmt.Errorf("invalid BT handshake magic")
	}
	var infoHash [20]byte
	copy(infoHash[:], got[28:48])
	mode, ok := hashes[infoHash]
	if !ok {
		c.Close()
		return nil, zero, "", fmt.Errorf("unknown infohash")
	}
	var theirs [20]byte
	copy(theirs[:], got[48:68])
	peerFeatures := got[20]
	peerBEP10 := got[25]&bep10ExtensionFlag != 0

	framing, _, _ := strings.Cut(mode, "-")
	switch framing {
	case "ext":
		if err := acceptSendHandshake(c, infoHash, serverID, 0, true); err != nil {
			c.Close()
			return nil, theirs, mode, fmt.Errorf("write handshake: %w", err)
		}
		var tr Transport
		var err error
		if hub && mode == "ext" {
			tr, err = acceptExtendedWithMode(c, infoHash, peerBEP10, hubExtMode())
		} else {
			tr, err = acceptExtended(c, infoHash, peerBEP10)
		}
		return tr, theirs, mode, err
	case "req":
		features := byte(wideRequestFeature | compactRequestFeature)
		var err error
		if hub && mode == "req" {
			features, err = RequestFeatures()
			if err != nil {
				c.Close()
				return nil, theirs, mode, err
			}
		}
		if err := acceptSendHandshake(c, infoHash, serverID, features, false); err != nil {
			c.Close()
			return nil, theirs, mode, fmt.Errorf("write handshake: %w", err)
		}
		if hub && mode == "req" {
			tr, err := acceptHubRequestPiece(c, features, peerFeatures)
			return tr, theirs, mode, err
		}
		tr, err := acceptRequestPiece(c, peerFeatures)
		return tr, theirs, mode, err
	default:
		c.Close()
		return nil, theirs, mode, fmt.Errorf("unknown carrier mode %q", mode)
	}
}

func acceptSendHandshake(c net.Conn, infoHash, peerID [20]byte, features byte, bep10 bool) error {
	h := HandshakeBytes(infoHash, peerID)
	h[20] = features
	if bep10 {
		h[25] = bep10ExtensionFlag
	}
	return fullWrite(c, h)
}
