// Package awg builds AmneziaWG key material and the line-oriented IpcSet
// configuration strings consumed by the AWG engine: the client's full device
// config (one peer = server, allowed_ip 0.0.0.0/0) and the server's per-client
// peer stanzas. The endpoint port 51820 is a fixed overlay identifier — the
// real transport is the BitTorrent carrier, not this UDP address.
package awg

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/curve25519"

	"bittorrent-tunnel/btawg/internal/config"
)

// EndpointPort is the fixed overlay endpoint identifier used in IpcSet configs.
const EndpointPort = 51820

// GenerateKeyPair returns a fresh X25519 key pair as hex strings.
func GenerateKeyPair() (privHex, pubHex string, err error) {
	var priv [32]byte
	if _, err = rand.Read(priv[:]); err != nil {
		return "", "", fmt.Errorf("generate private key: %w", err)
	}
	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return "", "", fmt.Errorf("derive public key: %w", err)
	}
	return hex.EncodeToString(priv[:]), hex.EncodeToString(pub), nil
}

// DerivePubKey recomputes the public key of a hex private key.
func DerivePubKey(privHex string) (string, error) {
	priv, err := hex.DecodeString(privHex)
	if err != nil || len(priv) != 32 {
		return "", fmt.Errorf("bad private key hex")
	}
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return "", fmt.Errorf("derive public key: %w", err)
	}
	return hex.EncodeToString(pub), nil
}

// ClientConf renders the client device's complete IpcSet configuration: its
// private key, the AWG obfuscation profile, and a single peer (the server)
// that carries all traffic. The endpoint is the overlay hub address.
func ClientConf(c config.ClientConfig) string {
	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\n", c.ClientPrivKey)
	b.WriteString(c.Profile.DeviceIPC())
	b.WriteString("replace_peers=true\n")
	fmt.Fprintf(&b, "public_key=%s\n", c.ServerPubKey)
	b.WriteString("allowed_ip=0.0.0.0/0\n")
	fmt.Fprintf(&b, "endpoint=%s:%d\n", c.ServerIP, EndpointPort)
	if keepalive := c.Profile.PersistentKeepalive; keepalive != "" {
		fmt.Fprintf(&b, "persistent_keepalive_interval=%s\n", keepalive)
	}
	return b.String()
}

// ServerPeerLine renders one client peer stanza for the server hub's IpcSet.
// The AWG profile is a device-level setting and is applied once with the
// server's own private key, not per peer.
func ServerPeerLine(pubKeyHex, clientIP string) string {
	return fmt.Sprintf("public_key=%s\nallowed_ip=%s/32\nendpoint=%s:%d\n",
		pubKeyHex, clientIP, clientIP, EndpointPort)
}

// PeerID derives the 20-byte BitTorrent peer identifier (40 hex chars) from a
// client's public key. The server learns which client is calling by reading
// this value from the BitTorrent handshake.
func PeerID(pubKeyHex string) string {
	pub, err := hex.DecodeString(pubKeyHex)
	if err != nil || len(pub) != 32 {
		return ""
	}
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:20])
}

// PeerIDBytes converts a 40-char hex peer ID to its 20-byte form.
func PeerIDBytes(hexID string) ([20]byte, error) {
	var out [20]byte
	b, err := hex.DecodeString(hexID)
	if err != nil || len(b) != 20 {
		return out, fmt.Errorf("bad peer id")
	}
	copy(out[:], b)
	return out, nil
}

// InfoHashBytes converts a 40-char hex infohash to its 20-byte form.
func InfoHashBytes(hexHash string) ([20]byte, error) {
	var out [20]byte
	b, err := hex.DecodeString(hexHash)
	if err != nil || len(b) != 20 {
		return out, fmt.Errorf("bad infohash")
	}
	copy(out[:], b)
	return out, nil
}
