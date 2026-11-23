package carrier

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"github.com/anacrolix/torrent/bencode"
)

// extDataID is the legacy private ID used for carrier datagrams.
const extDataID = 1

// extLegacyName is the pre-BEP10 branded extension name accepted from old
// peers. Outbound handshakes never advertise it.
const extLegacyName = "btawg"

const maxExtHandshakeLen = 4098
const maxExtMapEntries = 64

// extHandshake is the parsed BEP 10 extension handshake dictionary.
type extHandshake struct {
	M        map[string]int64 `bencode:"m"`
	X        string           `bencode:"x,omitempty"`
	PaddedV1 int              `bencode:"padded_v1,omitempty"`
}

// extName returns the opaque 16-character hex extension name derived from
// the public infohash.
func extName(infoHash [20]byte) string {
	name := sha256.Sum256(infoHash[:])
	return hex.EncodeToString(name[:8])
}

// extHandshakeMsg builds a BEP 10 extension handshake advertising the carrier
// extension under the given local receiver-assigned ID and optional
// experiment mode. The legacy branded name is never advertised.
func extHandshakeMsg(infoHash [20]byte, localID byte, mode string) []byte {
	name := extName(infoHash)
	h := extHandshake{M: map[string]int64{name: int64(localID)}}
	if mode == "auto" {
		h.PaddedV1 = 1
	} else if mode != "" {
		h.X = mode
	}
	payload, err := bencode.Marshal(h)
	if err != nil {
		// This structure always marshals successfully.
		panic(err)
	}
	b := make([]byte, 4+2+len(payload))
	binary.BigEndian.PutUint32(b, uint32(2+len(payload)))
	b[4], b[5] = 20, 0
	copy(b[6:], payload)
	return b
}

// parseExtHandshake parses a BEP 10 extension handshake payload and extracts
// the ID the peer assigned to our extension and any experiment mode. It
// accepts both the infohash-derived name and the legacy branded name.
func parseExtHandshake(payload []byte, infoHash [20]byte) (remoteID byte, mode string, err error) {
	if len(payload) > maxExtHandshakeLen {
		return 0, "", fmt.Errorf("ext handshake too large")
	}
	var h extHandshake
	if err := bencode.Unmarshal(payload, &h); err != nil {
		var trailing bencode.ErrUnusedTrailingBytes
		if errors.As(err, &trailing) {
			return 0, "", fmt.Errorf("ext handshake has trailing bytes")
		}
		return 0, "", fmt.Errorf("ext handshake parse: %w", err)
	}
	if len(h.M) > maxExtMapEntries {
		return 0, "", fmt.Errorf("ext handshake m too large")
	}
	name := extName(infoHash)
	id, ok := h.M[name]
	if !ok {
		id, ok = h.M[extLegacyName]
	}
	if !ok {
		return 0, "", fmt.Errorf("ext handshake missing carrier id")
	}
	if id <= 0 || id > 255 {
		return 0, "", fmt.Errorf("ext handshake invalid id %d", id)
	}
	if h.X == "auto" {
		return 0, "", fmt.Errorf("invalid mandatory ext experiment mode")
	}
	if h.PaddedV1 == 1 && h.X == "" {
		return byte(id), "auto", nil
	}
	return byte(id), h.X, nil
}

func hubExtMode() string {
	switch mode := os.Getenv("BTAWG_EXT_EXPERIMENT"); mode {
	case "":
		return "auto"
	case "baseline":
		return ""
	default:
		return mode
	}
}

func negotiateExtMode(local, remote string, peerBEP10 bool) (string, error) {
	if local == "auto" && remote == "auto" {
		return "padded", nil
	}
	if local == "auto" {
		local = ""
	}
	if remote == "auto" {
		remote = ""
	}
	if local != remote {
		return "", fmt.Errorf("ext experiment mismatch: local %q, peer %q", local, remote)
	}
	if local != "" && !peerBEP10 {
		return "", fmt.Errorf("ext experiment requires BEP10 peer")
	}
	return local, nil
}

func validateExtMode(mode string) error {
	switch mode {
	case "", "auto", "protected", "packed", "padded":
		return nil
	default:
		return fmt.Errorf("invalid ext experiment mode %q", mode)
	}
}

// chooseLocalID picks a receiver-assigned nonzero extension ID. Legacy peers
// (no BEP 10 flag) keep the fixed ID 1.
func chooseLocalID(peerBEP10 bool) (byte, error) {
	if !peerBEP10 {
		return extDataID, nil
	}
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	id := b[0]
	if id == 0 {
		id = 1
	}
	return id, nil
}

const extFrameSize = 8 + BlockSize

type extCarrier struct {
	c          net.Conn
	writeMu    sync.Mutex
	writeFrame [extFrameSize]byte
	writeBatch []byte
	received   chan []byte
	done       chan struct{}
	once       sync.Once
	wg         sync.WaitGroup
	invalid    atomic.Int64
	localID    byte
	remoteID   byte
	experiment string
}

// NewExtended wraps an established connection with BEP 10 extended framing:
// BitTorrent handshake (the peer's ID is returned for caller-side routing),
// extension handshake, then private messages whose payloads are
// uint16-length-prefixed AWG datagrams with bounded padding.
func NewExtended(c net.Conn, infoHash, peerID [20]byte) (Transport, [20]byte, error) {
	return newExtendedWithMode(c, infoHash, peerID, "")
}

// NewHubExtended offers protected records only when the client has an
// existing server pin. An explicit experiment still requires matching peers.
func NewHubExtended(c net.Conn, infoHash, peerID [20]byte, hasPin bool) (Transport, [20]byte, error) {
	mode := hubExtMode()
	if !hasPin {
		if mode != "auto" && mode != "" {
			return nil, [20]byte{}, fmt.Errorf("protected ext requires a pinned server key")
		}
		mode = ""
	}
	return newExtendedWithMode(c, infoHash, peerID, mode)
}

func newExtendedWithMode(c net.Conn, infoHash, peerID [20]byte, mode string) (Transport, [20]byte, error) {
	if err := validateExtMode(mode); err != nil {
		return nil, [20]byte{}, err
	}
	theirs, peerBEPByte, err := handshakeBEP10(c, infoHash, peerID)
	if err != nil {
		return nil, theirs, err
	}
	peerBEP10 := peerBEPByte&bep10ExtensionFlag != 0

	localID, err := chooseLocalID(peerBEP10)
	if err != nil {
		c.Close()
		return nil, theirs, err
	}

	sendErr := make(chan error, 1)
	go func() { sendErr <- fullWrite(c, extHandshakeMsg(infoHash, localID, mode)) }()

	var lenBuf [4]byte
	if _, err := io.ReadFull(c, lenBuf[:]); err != nil {
		c.Close()
		<-sendErr
		return nil, theirs, fmt.Errorf("read ext handshake: %w", err)
	}
	n := int(binary.BigEndian.Uint32(lenBuf[:]))
	if n == 0 || n > maxExtHandshakeLen {
		c.Close()
		<-sendErr
		return nil, theirs, fmt.Errorf("bad ext handshake length")
	}
	h := make([]byte, n)
	if _, err := io.ReadFull(c, h); err != nil {
		c.Close()
		<-sendErr
		return nil, theirs, fmt.Errorf("read ext handshake body: %w", err)
	}
	if err := <-sendErr; err != nil {
		return nil, theirs, fmt.Errorf("write ext handshake: %w", err)
	}
	if n < 2 || h[0] != 20 || h[1] != 0 {
		return nil, theirs, fmt.Errorf("bad ext handshake")
	}
	var remoteID byte
	var peerMode string
	if peerBEP10 {
		var err error
		remoteID, peerMode, err = parseExtHandshake(h[2:], infoHash)
		if err != nil {
			return nil, theirs, err
		}
	} else {
		// Legacy peers keep the fixed ID and never advertise an experiment.
		remoteID = extDataID
		peerMode = ""
	}
	mode, err = negotiateExtMode(mode, peerMode, peerBEP10)
	if err != nil {
		return nil, theirs, err
	}

	x := &extCarrier{
		c:          c,
		received:   make(chan []byte, receiveQueuePackets),
		done:       make(chan struct{}),
		localID:    localID,
		remoteID:   remoteID,
		experiment: mode,
	}
	x.wg.Add(1)
	go func() {
		defer x.wg.Done()
		x.readLoop()
	}()
	return x, theirs, nil
}

// acceptExtended finishes a server-side extended carrier after the initial
// BitTorrent handshake has already been consumed by Accept. It performs the
// BEP 10 extension handshake and starts the read loop.
func acceptExtended(c net.Conn, infoHash [20]byte, peerBEP10 bool) (Transport, error) {
	return acceptExtendedWithMode(c, infoHash, peerBEP10, "")
}

func acceptExtendedWithMode(c net.Conn, infoHash [20]byte, peerBEP10 bool, mode string) (Transport, error) {
	if err := validateExtMode(mode); err != nil {
		return nil, err
	}
	localID, err := chooseLocalID(peerBEP10)
	if err != nil {
		c.Close()
		return nil, err
	}

	sendErr := make(chan error, 1)
	go func() { sendErr <- fullWrite(c, extHandshakeMsg(infoHash, localID, mode)) }()

	var lenBuf [4]byte
	if _, err := io.ReadFull(c, lenBuf[:]); err != nil {
		c.Close()
		<-sendErr
		return nil, fmt.Errorf("read ext handshake: %w", err)
	}
	n := int(binary.BigEndian.Uint32(lenBuf[:]))
	if n == 0 || n > maxExtHandshakeLen {
		c.Close()
		<-sendErr
		return nil, fmt.Errorf("bad ext handshake length")
	}
	h := make([]byte, n)
	if _, err := io.ReadFull(c, h); err != nil {
		c.Close()
		<-sendErr
		return nil, fmt.Errorf("read ext handshake body: %w", err)
	}
	if err := <-sendErr; err != nil {
		return nil, fmt.Errorf("write ext handshake: %w", err)
	}
	if n < 2 || h[0] != 20 || h[1] != 0 {
		return nil, fmt.Errorf("bad ext handshake")
	}
	var remoteID byte
	var peerMode string
	if peerBEP10 {
		var err error
		remoteID, peerMode, err = parseExtHandshake(h[2:], infoHash)
		if err != nil {
			return nil, err
		}
	} else {
		remoteID = extDataID
		peerMode = ""
	}
	mode, err = negotiateExtMode(mode, peerMode, peerBEP10)
	if err != nil {
		return nil, err
	}

	x := &extCarrier{
		c:          c,
		received:   make(chan []byte, receiveQueuePackets),
		done:       make(chan struct{}),
		localID:    localID,
		remoteID:   remoteID,
		experiment: mode,
	}
	x.wg.Add(1)
	go func() {
		defer x.wg.Done()
		x.readLoop()
	}()
	return x, nil
}

func (x *extCarrier) readLoop() {
	defer x.Close()
	reader := bufio.NewReaderSize(x.c, streamReadBuffer)
	rawMode := x.experiment != ""
	for {
		h, err := reader.Peek(4)
		if err != nil {
			return
		}
		length := binary.BigEndian.Uint32(h)
		reader.Discard(4)
		if length == 0 {
			continue // keepalive
		}
		if length > 4+BlockSize {
			x.invalid.Add(1)
			return
		}
		n := int(length)
		b, err := reader.Peek(n)
		if err != nil {
			return
		}
		if n < 2 || b[0] != 20 {
			x.invalid.Add(1)
			return
		}
		switch b[1] {
		case 0:
			reader.Discard(n)
			continue // re-handshake, ignored
		case x.localID:
			if rawMode {
				if n <= 2 || n-2 > BlockSize-2 {
					x.invalid.Add(1)
					return
				}
				d := append([]byte(nil), b[2:n]...)
				select {
				case x.received <- d:
				case <-x.done:
					return
				}
			} else {
				if n < 4 {
					x.invalid.Add(1)
					return
				}
				size := int(binary.BigEndian.Uint16(b[2:4]))
				if size == 0 || size > n-4 {
					x.invalid.Add(1)
					return
				}
				d := append([]byte(nil), b[4:4+size]...)
				select {
				case x.received <- d:
				case <-x.done:
					return
				}
			}
		default:
			// Unknown extension sub-ID: skip without delivering as carrier data.
		}
		reader.Discard(n)
	}
}

func (x *extCarrier) Send(p []byte) error {
	x.writeMu.Lock()
	defer x.writeMu.Unlock()
	remoteID := x.remoteID
	if remoteID == 0 {
		remoteID = extDataID
	}
	b := x.writeFrame[:]
	n, err := frameExtended(b, p, remoteID, x.experiment != "")
	if err != nil {
		return err
	}
	return fullWrite(x.c, b[:n])
}

// SendBatch sends packets already available together without a coalescing timer.
func (x *extCarrier) SendBatch(packets [][]byte) error {
	x.writeMu.Lock()
	defer x.writeMu.Unlock()
	const chunk = 32
	raw := x.experiment != ""
	remoteID := x.remoteID
	if remoteID == 0 {
		remoteID = extDataID
	}
	if x.writeBatch == nil {
		x.writeBatch = make([]byte, chunk*extFrameSize)
	}
	for len(packets) > 0 {
		n := min(len(packets), chunk)
		used := 0
		for _, p := range packets[:n] {
			size, err := frameExtended(x.writeBatch[used:], p, remoteID, raw)
			if err != nil {
				return err
			}
			used += size
		}
		if err := fullWrite(x.c, x.writeBatch[:used]); err != nil {
			return err
		}
		packets = packets[n:]
	}
	return nil
}

func frameExtended(b, p []byte, remoteID byte, raw bool) (int, error) {
	if len(p) == 0 || len(p) > BlockSize-2 {
		return 0, fmt.Errorf("invalid AWG datagram size: %d", len(p))
	}
	if raw {
		// Opaque stream bytes: outer length, extended ID, sub ID, payload.
		n := 6 + len(p)
		if n > extFrameSize {
			return 0, fmt.Errorf("raw frame exceeds extFrameSize")
		}
		binary.BigEndian.PutUint32(b, uint32(n-4))
		b[4], b[5] = 20, remoteID
		copy(b[6:], p)
		return n, nil
	}
	// Round up the data region, retaining at most 31 bytes of random padding.
	n := 6 + (len(p)+2+31)&^31
	binary.BigEndian.PutUint32(b, uint32(n-4))
	b[4], b[5] = 20, remoteID
	binary.BigEndian.PutUint16(b[6:], uint16(len(p)))
	copy(b[8:], p)
	_, err := rand.Read(b[8+len(p) : n])
	return n, err
}

// SendKeepalive writes a BitTorrent keepalive message on an ext carrier.
func SendKeepalive(t Transport) error {
	x, ok := t.(*extCarrier)
	if !ok {
		return fmt.Errorf("SendKeepalive unsupported for %T", t)
	}
	return fullWrite(x.c, []byte{0, 0, 0, 0})
}

// ExtendedExperiment returns the negotiated ext experiment mode for an
// ext carrier, or the empty string for baseline or non-ext transports.
func ExtendedExperiment(t Transport) string {
	if x, ok := t.(*extCarrier); ok {
		return x.experiment
	}
	return ""
}

// Invalid reports how many malformed wire messages were rejected.
func (x *extCarrier) Invalid() int64 { return x.invalid.Load() }

func (x *extCarrier) Close() {
	x.once.Do(func() {
		close(x.done)
		x.c.Close()
	})
}

func (x *extCarrier) Wait() {
	x.Close()
	x.wg.Wait()
}

func (x *extCarrier) Recv() <-chan []byte   { return x.received }
func (x *extCarrier) Done() <-chan struct{} { return x.done }
