package carrier

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func TestRequestFeatures(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		want    byte
		wantErr bool
	}{
		{"default", "", wideRequestFeature | compactRequestFeature | autoVariedRequestFeature, false},
		{"baseline", "baseline", wideRequestFeature | compactRequestFeature, false},
		{"window", "window", wideRequestFeature | compactRequestFeature | windowRequestFeature, false},
		{"varied", "varied", wideRequestFeature | compactRequestFeature | windowRequestFeature | variedRequestFeature, false},
		{"invalid", "unknown", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BTAWG_REQ_EXPERIMENT", tc.env)
			got, err := RequestFeatures()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("features=%02x, want %02x", got, tc.want)
			}
		})
	}
}

func handshakeReply(t *testing.T, c net.Conn, features byte) [20]byte {
	t.Helper()
	var got [68]byte
	if _, err := io.ReadFull(c, got[:]); err != nil {
		t.Fatalf("read handshake: %v", err)
	}
	h := HandshakeBytes(testHash, peerB)
	h[20] = features
	if err := fullWrite(c, h); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	var theirs [20]byte
	copy(theirs[:], got[48:68])
	return theirs
}

func TestRequestPieceBaselineRejectsExperiment(t *testing.T) {
	a, b := tcpPair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		handshakeReply(t, b, wideRequestFeature|compactRequestFeature|windowRequestFeature)
	}()
	_, _, err := NewRequestPiece(a, testHash, peerA, false)
	if err == nil {
		t.Fatal("baseline client accepted experimental peer")
	}
	<-done
}

func TestRequestPieceHubMismatch(t *testing.T) {
	t.Setenv("BTAWG_REQ_EXPERIMENT", "window")
	a, b := tcpPair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		handshakeReply(t, b, wideRequestFeature|compactRequestFeature)
	}()
	_, _, err := NewHubRequestPiece(a, testHash, peerA, false)
	if err == nil {
		t.Fatal("window hub accepted baseline peer")
	}
	<-done
}

func TestRequestPieceVariedMismatch(t *testing.T) {
	t.Setenv("BTAWG_REQ_EXPERIMENT", "varied")
	a, b := tcpPair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		handshakeReply(t, b, wideRequestFeature|compactRequestFeature|windowRequestFeature)
	}()
	_, _, err := NewHubRequestPiece(a, testHash, peerA, false)
	if err == nil {
		t.Fatal("varied hub accepted window peer")
	}
	<-done
}

func TestRequestPieceHubWindowRoundTrip(t *testing.T) {
	t.Setenv("BTAWG_REQ_EXPERIMENT", "window")
	ta, tb := pair(t,
		func(c net.Conn) (Transport, [20]byte, error) { return NewHubRequestPiece(c, testHash, peerA, false) },
		func(c net.Conn) (Transport, [20]byte, error) { return NewHubRequestPiece(c, testHash, peerB, true) },
	)
	roundTrip(t, ta, tb)
}

func TestRequestPieceWindowStartupLimit(t *testing.T) {
	t.Setenv("BTAWG_REQ_EXPERIMENT", "window")
	a, b := tcpPair(t)
	ready := make(chan result, 1)
	go func() {
		tr, pid, err := NewHubRequestPiece(a, testHash, peerA, false)
		ready <- result{tr: tr, pid: pid, err: err}
	}()
	remote, _, err := NewHubRequestPiece(b, testHash, peerB, true)
	if err != nil {
		t.Fatalf("remote carrier: %v", err)
	}
	t.Cleanup(remote.Wait)
	r := <-ready
	if r.err != nil {
		t.Fatalf("local carrier: %v", r.err)
	}
	t.Cleanup(r.tr.Wait)

	rc := remote.(*requestPieceCarrier)
	// Credits are primed asynchronously after the peer's bitfield + unchoke.
	deadline := time.Now().Add(2 * time.Second)
	for len(rc.requests) < legacyRequestWindow && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(rc.requests); got != legacyRequestWindow {
		t.Fatalf("initial window %d, want %d", got, legacyRequestWindow)
	}
	// The window must not grow past the startup limit without traffic.
	time.Sleep(200 * time.Millisecond)
	if got := len(rc.requests); got != legacyRequestWindow {
		t.Fatalf("window grew to %d without traffic, want %d", got, legacyRequestWindow)
	}
}

func TestRequestPieceWindowAdaptiveGrowth(t *testing.T) {
	t.Setenv("BTAWG_REQ_EXPERIMENT", "window")
	a, b := pair(t,
		func(c net.Conn) (Transport, [20]byte, error) { return NewHubRequestPiece(c, testHash, peerA, false) },
		func(c net.Conn) (Transport, [20]byte, error) { return NewHubRequestPiece(c, testHash, peerB, true) },
	)
	const count = 30000
	payload := []byte{1, 2, 3, 4}
	sent := make(chan error, 1)
	go func() {
		batch := make([][]byte, count)
		for i := range batch {
			batch[i] = payload
		}
		sent <- a.(interface{ SendBatch([][]byte) error }).SendBatch(batch)
	}()
	for i := range count {
		select {
		case <-b.Recv():
		case <-b.Done():
			t.Fatalf("carrier closed at datagram %d", i)
		case <-time.After(20 * time.Second):
			t.Fatalf("stalled at datagram %d", i)
		}
	}
	if err := <-sent; err != nil {
		t.Fatalf("send: %v", err)
	}
	a.Wait()
	b.Wait()
	bc := b.(*requestPieceCarrier)
	if bc.curWindow <= legacyRequestWindow {
		t.Fatalf("window did not grow: %d", bc.curWindow)
	}
	if bc.curWindow != bc.window {
		t.Fatalf("window did not reach max: got %d, want %d", bc.curWindow, bc.window)
	}
}

func TestRequestPieceVariedRoundTrip(t *testing.T) {
	t.Setenv("BTAWG_REQ_EXPERIMENT", "varied")
	ta, tb := pair(t,
		func(c net.Conn) (Transport, [20]byte, error) { return NewHubRequestPiece(c, testHash, peerA, false) },
		func(c net.Conn) (Transport, [20]byte, error) { return NewHubRequestPiece(c, testHash, peerB, true) },
	)
	roundTrip(t, ta, tb)

	sizes := []int{1, 100, 512, 1500, 2046}
	for _, sz := range sizes {
		p := bytes.Repeat([]byte{byte(sz)}, sz)
		if err := ta.Send(p); err != nil {
			t.Fatalf("send size %d: %v", sz, err)
		}
		select {
		case got := <-tb.Recv():
			if !bytes.Equal(got, p) {
				t.Fatalf("size %d mismatch", sz)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for size %d", sz)
		}
	}
}

func readMessage(t *testing.T, r io.Reader) []byte {
	t.Helper()
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		t.Fatalf("read prefix: %v", err)
	}
	body := make([]byte, binary.BigEndian.Uint32(prefix[:]))
	if _, err := io.ReadFull(r, body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}

func sendMessage(t *testing.T, w io.Writer, id byte, body []byte) {
	t.Helper()
	msg := make([]byte, 5+len(body))
	binary.BigEndian.PutUint32(msg, uint32(len(body)+1))
	msg[4] = id
	copy(msg[5:], body)
	if err := fullWrite(w, msg); err != nil {
		t.Fatalf("write message: %v", err)
	}
}

func sendVariedStartup(t *testing.T, c net.Conn, ownOdd bool) {
	t.Helper()
	bitfield := make([]byte, totalPieces/8)
	for i := range totalPieces {
		if (i%2 == 1) == ownOdd {
			bitfield[i/8] |= 1 << uint(7-i%8)
		}
	}
	sendMessage(t, c, 5, bitfield)
	sendMessage(t, c, 2, nil)
	sendMessage(t, c, 1, nil)
}

func TestRequestPieceVariedNonZeroOffset(t *testing.T) {
	t.Setenv("BTAWG_REQ_EXPERIMENT", "varied")
	a, b := tcpPair(t)
	if err := b.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}

	ready := make(chan result, 1)
	go func() {
		tr, pid, err := NewHubRequestPiece(a, testHash, peerA, false)
		ready <- result{tr: tr, pid: pid, err: err}
	}()

	handshakeReply(t, b, wideRequestFeature|compactRequestFeature|windowRequestFeature|variedRequestFeature)
	for _, id := range []byte{5, 2, 1} {
		if got := readMessage(t, b)[0]; got != id {
			t.Fatalf("startup message ID %d, want %d", got, id)
		}
	}
	sendVariedStartup(t, b, true)

	// Read one request issued by the client; varied requests must use a
	// nonzero begin offset and a bounded block length.
	req := readMessage(t, b)
	if len(req) != 13 || req[0] != 6 {
		t.Fatalf("expected 13-byte request, got %d bytes id=%d", len(req), req[0])
	}
	idx := binary.BigEndian.Uint32(req[1:])
	begin := binary.BigEndian.Uint32(req[5:])
	length := binary.BigEndian.Uint32(req[9:])
	if begin == 0 {
		t.Fatal("varied request had zero begin offset")
	}
	if length < 2 || int(length) > BlockSize || begin+length > uint32(BlockSize) {
		t.Fatalf("invalid varied request begin=%d length=%d", begin, length)
	}

	// Send a valid piece matching the outstanding tuple.
	payload := []byte("varied offset payload")
	block := make([]byte, length)
	binary.BigEndian.PutUint16(block[0:], uint16(len(payload)))
	copy(block[2:], payload)
	pieceBody := make([]byte, 9+length)
	pieceBody[0] = 7
	binary.BigEndian.PutUint32(pieceBody[1:], idx)
	binary.BigEndian.PutUint32(pieceBody[5:], begin)
	copy(pieceBody[9:], block)
	piece := make([]byte, 4+len(pieceBody))
	binary.BigEndian.PutUint32(piece, uint32(len(pieceBody)))
	copy(piece[4:], pieceBody)
	if err := fullWrite(b, piece); err != nil {
		t.Fatalf("write piece: %v", err)
	}

	r := <-ready
	if r.err != nil {
		t.Fatalf("local carrier: %v", r.err)
	}
	t.Cleanup(r.tr.Wait)
	select {
	case got := <-r.tr.Recv():
		if !bytes.Equal(got, payload) {
			t.Fatalf("received %q, want %q", got, payload)
		}
	case <-r.tr.Done():
		t.Fatal("carrier closed before delivering valid varied piece")
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for varied piece")
	}
}

func TestRequestPieceVariedRejectsMismatch(t *testing.T) {
	t.Setenv("BTAWG_REQ_EXPERIMENT", "varied")
	a, b := tcpPair(t)
	if err := b.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}

	ready := make(chan result, 1)
	go func() {
		tr, pid, err := NewHubRequestPiece(a, testHash, peerA, false)
		ready <- result{tr: tr, pid: pid, err: err}
	}()

	handshakeReply(t, b, wideRequestFeature|compactRequestFeature|windowRequestFeature|variedRequestFeature)
	for _, id := range []byte{5, 2, 1} {
		if got := readMessage(t, b)[0]; got != id {
			t.Fatalf("startup message ID %d, want %d", got, id)
		}
	}
	sendVariedStartup(t, b, true)

	req := readMessage(t, b)
	if len(req) != 13 || req[0] != 6 {
		t.Fatalf("expected 13-byte request, got %d bytes id=%d", len(req), req[0])
	}
	idx := binary.BigEndian.Uint32(req[1:])
	begin := binary.BigEndian.Uint32(req[5:])
	length := binary.BigEndian.Uint32(req[9:])

	// Send a piece with a mismatched begin offset.
	block := make([]byte, length)
	binary.BigEndian.PutUint16(block[0:], 1)
	block[2] = 0xee
	pieceBody := make([]byte, 9+length)
	pieceBody[0] = 7
	binary.BigEndian.PutUint32(pieceBody[1:], idx)
	binary.BigEndian.PutUint32(pieceBody[5:], begin+1)
	copy(pieceBody[9:], block)
	piece := make([]byte, 4+len(pieceBody))
	binary.BigEndian.PutUint32(piece, uint32(len(pieceBody)))
	copy(piece[4:], pieceBody)
	if err := fullWrite(b, piece); err != nil {
		t.Fatalf("write piece: %v", err)
	}

	r := <-ready
	if r.err != nil {
		t.Fatalf("local carrier: %v", r.err)
	}
	t.Cleanup(r.tr.Wait)
	select {
	case <-r.tr.Done():
		if r.tr.(*requestPieceCarrier).Invalid() == 0 {
			t.Fatal("carrier closed without reporting invalid message")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("carrier did not close on mismatched piece")
	}
}

func TestRequestPieceCloseUnblocksSend(t *testing.T) {
	t.Setenv("BTAWG_REQ_EXPERIMENT", "window")
	a, b := pair(t,
		func(c net.Conn) (Transport, [20]byte, error) { return NewHubRequestPiece(c, testHash, peerA, false) },
		func(c net.Conn) (Transport, [20]byte, error) { return NewHubRequestPiece(c, testHash, peerB, true) },
	)
	done := make(chan error, 1)
	go func() {
		// Drain the sender's credits so the next Send has to wait; the peer
		// will not replenish because no pieces are flowing.
		ac := a.(*requestPieceCarrier)
		deadline := time.Now().Add(2 * time.Second)
		for len(ac.requests) < legacyRequestWindow && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		for len(ac.requests) > 0 {
			<-ac.requests
		}
		done <- a.Send(make([]byte, 100))
	}()
	time.Sleep(100 * time.Millisecond)
	b.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("send succeeded after peer close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not unblock sender")
	}
	a.Wait()
}
