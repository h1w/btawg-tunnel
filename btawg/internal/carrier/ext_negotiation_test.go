package carrier

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

// readBTHandshake reads and validates the initial 68-byte BitTorrent handshake.
func readBTHandshake(t *testing.T, c net.Conn, infoHash [20]byte) ([20]byte, byte, error) {
	t.Helper()
	var got [68]byte
	if _, err := io.ReadFull(c, got[:]); err != nil {
		return [20]byte{}, 0, err
	}
	if got[0] != 19 || string(got[1:20]) != protocol {
		return [20]byte{}, 0, errors.New("bad magic")
	}
	if !bytes.Equal(got[28:48], infoHash[:]) {
		return [20]byte{}, 0, errors.New("infohash mismatch")
	}
	var pid [20]byte
	copy(pid[:], got[48:68])
	return pid, got[25], nil
}

// writeBTHandshake writes a baseline or BEP10-enabled BitTorrent handshake.
func writeBTHandshake(c net.Conn, infoHash, peerID [20]byte, bep10 bool) error {
	h := HandshakeBytes(infoHash, peerID)
	if bep10 {
		h[25] = bep10ExtensionFlag
	}
	return fullWrite(c, h)
}

// readExtHandshake reads a BEP 10 extension handshake and returns the ID
// assigned to our extension and any experiment mode.
func readExtHandshake(t *testing.T, c net.Conn, infoHash [20]byte) (byte, string, error) {
	t.Helper()
	var lenBuf [4]byte
	if _, err := io.ReadFull(c, lenBuf[:]); err != nil {
		return 0, "", err
	}
	n := int(binary.BigEndian.Uint32(lenBuf[:]))
	if n < 2 || n > maxExtHandshakeLen {
		return 0, "", errors.New("bad length")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(c, body); err != nil {
		return 0, "", err
	}
	if body[0] != 20 || body[1] != 0 {
		return 0, "", errors.New("bad header")
	}
	return parseExtHandshake(body[2:], infoHash)
}

// writeExtHandshake writes a BEP 10 extension handshake.
func writeExtHandshake(c net.Conn, infoHash [20]byte, localID byte, mode string) error {
	return fullWrite(c, extHandshakeMsg(infoHash, localID, mode))
}

func TestExtendedNegotiatesRemoteID(t *testing.T) {
	client, server := tcpPair(t)
	peerRemoteID := byte(42)
	ourLocalID := byte(0) // captured from peer's view

	srvErr := make(chan error, 1)
	go func() {
		defer close(srvErr)
		if _, _, err := readBTHandshake(t, server, testHash); err != nil {
			srvErr <- err
			return
		}
		if err := writeBTHandshake(server, testHash, peerB, true); err != nil {
			srvErr <- err
			return
		}
		var err error
		ourLocalID, _, err = readExtHandshake(t, server, testHash)
		if err != nil {
			srvErr <- err
			return
		}
		if err := writeExtHandshake(server, testHash, peerRemoteID, ""); err != nil {
			srvErr <- err
			return
		}
	}()

	cli, pid, err := NewExtended(client, testHash, peerA)
	if err != nil {
		t.Fatalf("NewExtended: %v", err)
	}
	defer cli.Wait()
	if pid != peerB {
		t.Fatalf("peer id %v, want %v", pid, peerB)
	}
	if err := <-srvErr; err != nil {
		t.Fatalf("server side: %v", err)
	}
	if ourLocalID == 0 || ourLocalID == extDataID {
		t.Fatalf("server saw local id %d, want a fresh nonzero id", ourLocalID)
	}

	// The peer sends using the ID it assigned to us.
	payload := []byte("hello from peer")
	if err := sendRawExtended(server, ourLocalID, payload, false); err != nil {
		t.Fatalf("send raw: %v", err)
	}
	select {
	case got := <-cli.Recv():
		if !bytes.Equal(got, payload) {
			t.Fatalf("recv %q, want %q", got, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for payload")
	}

	// We send using the ID the peer assigned to itself.
	if err := cli.Send([]byte("hello from us")); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := expectRawExtended(server, peerRemoteID, []byte("hello from us"), false); err != nil {
		t.Fatalf("expect raw: %v", err)
	}
}

func TestExtendedAcceptNegotiatesRemoteID(t *testing.T) {
	client, server := tcpPair(t)
	ourLocalID := byte(0)
	peerRemoteID := byte(77)

	srvCh := make(chan result, 1)
	go func() {
		tr, pid, _, err := Accept(server, peerB, map[[20]byte]string{testHash: "ext"})
		srvCh <- result{tr: tr, pid: pid, err: err}
	}()

	if err := writeBTHandshake(client, testHash, peerA, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readBTHandshake(t, client, testHash); err != nil {
		t.Fatal(err)
	}
	var err error
	ourLocalID, _, err = readExtHandshake(t, client, testHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeExtHandshake(client, testHash, peerRemoteID, ""); err != nil {
		t.Fatal(err)
	}

	r := <-srvCh
	if r.err != nil {
		t.Fatalf("Accept: %v", r.err)
	}
	defer r.tr.Wait()
	if ourLocalID == 0 || ourLocalID == extDataID {
		t.Fatalf("client saw local id %d, want a fresh nonzero id", ourLocalID)
	}

	if err := r.tr.Send([]byte("server payload")); err != nil {
		t.Fatal(err)
	}
	if err := expectRawExtended(client, peerRemoteID, []byte("server payload"), false); err != nil {
		t.Fatal(err)
	}

	if err := sendRawExtended(client, ourLocalID, []byte("client payload"), false); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-r.tr.Recv():
		if !bytes.Equal(got, []byte("client payload")) {
			t.Fatalf("recv %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func TestExtendedLegacyPeerKeepsIDOne(t *testing.T) {
	client, server := tcpPair(t)

	srvErr := make(chan error, 1)
	go func() {
		defer close(srvErr)
		if _, _, err := readBTHandshake(t, server, testHash); err != nil {
			srvErr <- err
			return
		}
		// Peer does not set the BEP10 flag.
		if err := writeBTHandshake(server, testHash, peerB, false); err != nil {
			srvErr <- err
			return
		}
		id, _, err := readExtHandshake(t, server, testHash)
		if err != nil {
			srvErr <- err
			return
		}
		if id != extDataID {
			srvErr <- errors.New("legacy outbound id != 1")
			return
		}
		// Reply with the old branded, malformed handshake the original code accepted.
		if _, err := server.Write([]byte("\x00\x00\x00\x12\x14\x00d1:md5:btawgi1ee")); err != nil {
			srvErr <- err
			return
		}
	}()

	cli, _, err := NewExtended(client, testHash, peerA)
	if err != nil {
		t.Fatalf("NewExtended: %v", err)
	}
	defer cli.Wait()
	if err := <-srvErr; err != nil {
		t.Fatalf("server side: %v", err)
	}

	// Traffic uses the fixed ID 1 in both directions.
	if err := cli.Send([]byte("legacy payload")); err != nil {
		t.Fatal(err)
	}
	if err := expectRawExtended(server, extDataID, []byte("legacy payload"), false); err != nil {
		t.Fatal(err)
	}
	if err := sendRawExtended(server, extDataID, []byte("peer payload"), false); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-cli.Recv():
		if !bytes.Equal(got, []byte("peer payload")) {
			t.Fatalf("recv %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func TestExtendedRejectsMalformedHandshake(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		{"missing m", []byte("de")},
		{"missing carrier id", []byte("d1:md4:nopei1eee")},
		{"invalid id zero", []byte("d1:md16:44547bca15eabfdei0eee")},
		{"invalid id huge", []byte("d1:md16:44547bca15eabfdei256eee")},
		{"trailing bytes", []byte("d1:md16:44547bca15eabfdei1eeextra")},
		{"oversized", make([]byte, maxExtHandshakeLen+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, server := tcpPair(t)
			errCh := make(chan error, 1)
			go func() {
				defer close(errCh)
				if _, _, err := readBTHandshake(t, server, testHash); err != nil {
					errCh <- err
					return
				}
				if err := writeBTHandshake(server, testHash, peerB, true); err != nil {
					errCh <- err
					return
				}
				if _, _, err := readExtHandshake(t, server, testHash); err != nil {
					errCh <- err
					return
				}
				// Write the malformed handshake directly.
				length := make([]byte, 4)
				binary.BigEndian.PutUint32(length, uint32(2+len(tc.payload)))
				if err := fullWrite(server, length); err != nil {
					errCh <- err
					return
				}
				if err := fullWrite(server, []byte{20, 0}); err != nil {
					errCh <- err
					return
				}
				if err := fullWrite(server, tc.payload); err != nil {
					errCh <- err
				}
			}()
			_, _, err := NewExtended(client, testHash, peerA)
			if err == nil {
				t.Fatal("expected handshake error")
			}
			select {
			case <-errCh:
			case <-time.After(5 * time.Second):
				t.Fatal("server goroutine did not finish")
			}
		})
	}
}

func TestExtendedExperimentNegotiation(t *testing.T) {
	t.Setenv("BTAWG_EXT_EXPERIMENT", "protected")
	client, server := tcpPair(t)

	var clientLocalID byte
	srvErr := make(chan error, 1)
	go func() {
		defer close(srvErr)
		if _, _, err := readBTHandshake(t, server, testHash); err != nil {
			srvErr <- err
			return
		}
		if err := writeBTHandshake(server, testHash, peerB, true); err != nil {
			srvErr <- err
			return
		}
		id, mode, err := readExtHandshake(t, server, testHash)
		if err != nil {
			srvErr <- err
			return
		}
		clientLocalID = id
		if id == 0 {
			srvErr <- errors.New("expected nonzero extension ID")
			return
		}
		if mode != "protected" {
			srvErr <- errors.New("missing protected mode")
			return
		}
		if err := writeExtHandshake(server, testHash, 99, "protected"); err != nil {
			srvErr <- err
			return
		}
	}()

	cli, _, err := NewHubExtended(client, testHash, peerA, true)
	if err != nil {
		t.Fatalf("NewHubExtended: %v", err)
	}
	defer cli.Wait()
	if err := <-srvErr; err != nil {
		t.Fatalf("server side: %v", err)
	}
	if ExtendedExperiment(cli) != "protected" {
		t.Fatalf("experiment %q, want protected", ExtendedExperiment(cli))
	}

	// Raw mode: payload has no uint16 length prefix.
	payload := []byte("protected payload")
	if err := cli.Send(payload); err != nil {
		t.Fatal(err)
	}
	if err := expectRawExtended(server, 99, payload, true); err != nil {
		t.Fatal(err)
	}

	if err := sendRawExtended(server, clientLocalID, []byte("peer protected"), true); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-cli.Recv():
		if !bytes.Equal(got, []byte("peer protected")) {
			t.Fatalf("recv %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func TestExtendedExperimentMismatchFails(t *testing.T) {
	t.Setenv("BTAWG_EXT_EXPERIMENT", "protected")
	client, server := tcpPair(t)

	go func() {
		if _, _, err := readBTHandshake(t, server, testHash); err != nil {
			return
		}
		if err := writeBTHandshake(server, testHash, peerB, true); err != nil {
			return
		}
		if _, _, err := readExtHandshake(t, server, testHash); err != nil {
			return
		}
		if err := writeExtHandshake(server, testHash, 9, "padded"); err != nil {
			return
		}
	}()

	_, _, err := NewHubExtended(client, testHash, peerA, true)
	if err == nil {
		t.Fatal("expected mismatch error")
	}
}

func TestExtendedExperimentOneAdvertisesFails(t *testing.T) {
	t.Setenv("BTAWG_EXT_EXPERIMENT", "protected")
	client, server := tcpPair(t)

	go func() {
		if _, _, err := readBTHandshake(t, server, testHash); err != nil {
			return
		}
		if err := writeBTHandshake(server, testHash, peerB, true); err != nil {
			return
		}
		if _, _, err := readExtHandshake(t, server, testHash); err != nil {
			return
		}
		// Peer does not advertise a mode.
		if err := writeExtHandshake(server, testHash, 9, ""); err != nil {
			return
		}
	}()

	_, _, err := NewHubExtended(client, testHash, peerA, true)
	if err == nil {
		t.Fatal("expected mismatch error")
	}
}

func TestExtendedExperimentRequiresBEP10Peer(t *testing.T) {
	t.Setenv("BTAWG_EXT_EXPERIMENT", "protected")
	client, server := tcpPair(t)

	go func() {
		if _, _, err := readBTHandshake(t, server, testHash); err != nil {
			return
		}
		if err := writeBTHandshake(server, testHash, peerB, false); err != nil {
			return
		}
		if _, _, err := readExtHandshake(t, server, testHash); err != nil {
			return
		}
		if _, err := server.Write([]byte("\x00\x00\x00\x12\x14\x00d1:md5:btawgi1ee")); err != nil {
			return
		}
	}()

	_, _, err := NewHubExtended(client, testHash, peerA, true)
	if err == nil {
		t.Fatal("expected BEP10 requirement error")
	}
}

func TestExtendedSkipsUnknownExtID(t *testing.T) {
	client, server := tcpPair(t)

	srvErr := make(chan error, 1)
	go func() {
		defer close(srvErr)
		if _, _, err := readBTHandshake(t, server, testHash); err != nil {
			srvErr <- err
			return
		}
		if err := writeBTHandshake(server, testHash, peerB, true); err != nil {
			srvErr <- err
			return
		}
		id, _, err := readExtHandshake(t, server, testHash)
		if err != nil {
			srvErr <- err
			return
		}
		if err := writeExtHandshake(server, testHash, id, ""); err != nil {
			srvErr <- err
			return
		}
	}()

	cli, _, err := NewExtended(client, testHash, peerA)
	if err != nil {
		t.Fatalf("NewExtended: %v", err)
	}
	defer cli.Wait()
	if err := <-srvErr; err != nil {
		t.Fatalf("server side: %v", err)
	}

	ourID := cli.(*extCarrier).localID

	// Send an unknown extended message, then a valid one.
	if err := sendRawExtended(server, ourID+1, []byte("ignored"), false); err != nil {
		t.Fatal(err)
	}
	if err := sendRawExtended(server, ourID, []byte("delivered"), false); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-cli.Recv():
		if !bytes.Equal(got, []byte("delivered")) {
			t.Fatalf("recv %q, want delivered", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
	if cli.(*extCarrier).Invalid() != 0 {
		t.Fatalf("invalid count %d, want 0", cli.(*extCarrier).Invalid())
	}
}

func TestExtendedInvalidDataShutsDown(t *testing.T) {
	client, server := tcpPair(t)

	srvErr := make(chan error, 1)
	go func() {
		defer close(srvErr)
		if _, _, err := readBTHandshake(t, server, testHash); err != nil {
			srvErr <- err
			return
		}
		if err := writeBTHandshake(server, testHash, peerB, true); err != nil {
			srvErr <- err
			return
		}
		id, _, err := readExtHandshake(t, server, testHash)
		if err != nil {
			srvErr <- err
			return
		}
		if err := writeExtHandshake(server, testHash, id, ""); err != nil {
			srvErr <- err
			return
		}
	}()

	cli, _, err := NewExtended(client, testHash, peerA)
	if err != nil {
		t.Fatalf("NewExtended: %v", err)
	}
	defer cli.Wait()
	if err := <-srvErr; err != nil {
		t.Fatalf("server side: %v", err)
	}

	ourID := cli.(*extCarrier).localID
	// Send a complete frame whose uint16 size exceeds the actual payload.
	length := make([]byte, 4)
	const payloadBytes = 10
	binary.BigEndian.PutUint32(length, uint32(2+2+payloadBytes))
	frame := make([]byte, 4+2+2+payloadBytes)
	copy(frame, length)
	frame[4], frame[5] = 20, ourID
	binary.BigEndian.PutUint16(frame[6:], 100) // claims 100 bytes, only 10 follow
	if err := fullWrite(server, frame); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cli.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("carrier did not shut down")
	}
	if cli.(*extCarrier).Invalid() != 1 {
		t.Fatalf("invalid count %d, want 1", cli.(*extCarrier).Invalid())
	}
}

func TestAcceptHubExtTLSIgnoresExperiment(t *testing.T) {
	t.Setenv("BTAWG_EXT_EXPERIMENT", "protected")
	client, server := tcpPair(t)

	srvCh := make(chan result, 1)
	go func() {
		tr, pid, _, err := AcceptHub(server, peerB, map[[20]byte]string{testHash: "ext-tls"})
		srvCh <- result{tr: tr, pid: pid, err: err}
	}()

	if err := writeBTHandshake(client, testHash, peerA, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readBTHandshake(t, client, testHash); err != nil {
		t.Fatal(err)
	}
	id, mode, err := readExtHandshake(t, client, testHash)
	if err != nil {
		t.Fatal(err)
	}
	if mode != "" {
		t.Fatalf("ext-tls advertised mode %q", mode)
	}
	if err := writeExtHandshake(client, testHash, id, ""); err != nil {
		t.Fatal(err)
	}

	r := <-srvCh
	if r.err != nil {
		t.Fatalf("AcceptHub: %v", r.err)
	}
	defer r.tr.Wait()
	if ExtendedExperiment(r.tr) != "" {
		t.Fatalf("experiment %q, want empty", ExtendedExperiment(r.tr))
	}
}

func TestSendKeepalive(t *testing.T) {
	client, server := tcpPair(t)

	srvErr := make(chan error, 1)
	go func() {
		defer close(srvErr)
		if _, _, err := readBTHandshake(t, server, testHash); err != nil {
			srvErr <- err
			return
		}
		if err := writeBTHandshake(server, testHash, peerB, true); err != nil {
			srvErr <- err
			return
		}
		id, _, err := readExtHandshake(t, server, testHash)
		if err != nil {
			srvErr <- err
			return
		}
		if err := writeExtHandshake(server, testHash, id, ""); err != nil {
			srvErr <- err
			return
		}
	}()

	cli, _, err := NewExtended(client, testHash, peerA)
	if err != nil {
		t.Fatalf("NewExtended: %v", err)
	}
	defer cli.Wait()
	if err := <-srvErr; err != nil {
		t.Fatalf("server side: %v", err)
	}

	if err := SendKeepalive(cli); err != nil {
		t.Fatalf("SendKeepalive: %v", err)
	}
	var ka [4]byte
	if _, err := io.ReadFull(server, ka[:]); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ka[:], []byte{0, 0, 0, 0}) {
		t.Fatalf("keepalive %v", ka)
	}
}

func TestSendKeepaliveRejectsNonExt(t *testing.T) {
	if err := SendKeepalive(fakeTransport{}); err == nil {
		t.Fatal("expected error for non-ext transport")
	}
}

type fakeTransport struct{}

func (fakeTransport) Send([]byte) error     { return nil }
func (fakeTransport) Recv() <-chan []byte   { return nil }
func (fakeTransport) Done() <-chan struct{} { return nil }
func (fakeTransport) Close()                {}
func (fakeTransport) Wait()                 {}

// sendRawExtended writes a carrier data frame directly to c.
func sendRawExtended(c net.Conn, subID byte, payload []byte, raw bool) error {
	length := make([]byte, 4)
	if raw {
		binary.BigEndian.PutUint32(length, uint32(2+len(payload)))
		frame := make([]byte, 6+len(payload))
		copy(frame, length)
		frame[4], frame[5] = 20, subID
		copy(frame[6:], payload)
		return fullWrite(c, frame)
	}
	size := len(payload)
	n := 6 + (size+2+31)&^31
	binary.BigEndian.PutUint32(length, uint32(n-4))
	frame := make([]byte, n)
	copy(frame, length)
	frame[4], frame[5] = 20, subID
	binary.BigEndian.PutUint16(frame[6:], uint16(size))
	copy(frame[8:], payload)
	if _, err := rand.Read(frame[8+size:]); err != nil {
		return err
	}
	return fullWrite(c, frame)
}

// expectRawExtended reads a carrier data frame from c and checks the sub-ID and payload.
func expectRawExtended(c net.Conn, wantSubID byte, wantPayload []byte, raw bool) error {
	var lenBuf [4]byte
	if _, err := io.ReadFull(c, lenBuf[:]); err != nil {
		return err
	}
	n := int(binary.BigEndian.Uint32(lenBuf[:]))
	body := make([]byte, n)
	if _, err := io.ReadFull(c, body); err != nil {
		return err
	}
	if n < 2 || body[0] != 20 || body[1] != wantSubID {
		return errors.New("bad header")
	}
	if raw {
		if !bytes.Equal(body[2:], wantPayload) {
			return errors.New("raw payload mismatch")
		}
		return nil
	}
	if n < 4 {
		return errors.New("short frame")
	}
	size := int(binary.BigEndian.Uint16(body[2:4]))
	if size > n-4 {
		return errors.New("bad size")
	}
	if !bytes.Equal(body[4:4+size], wantPayload) {
		return errors.New("payload mismatch")
	}
	return nil
}

func TestMain(m *testing.M) {
	// Ensure the env var is cleared for tests that do not explicitly set it.
	os.Unsetenv("BTAWG_EXT_EXPERIMENT")
	m.Run()
}
