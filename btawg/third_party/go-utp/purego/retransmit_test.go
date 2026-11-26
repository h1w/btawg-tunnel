package purego

import (
	"io"
	"net"
	"testing"
	"time"
)

// The test drives both protocol state machines with a simulated clock and
// explicitly delivers each queued datagram; no timer goroutines or UDP races.
type retransmitTestWire struct {
	net.PacketConn
	packets [][]byte
}

func (w *retransmitTestWire) WriteTo(b []byte, _ net.Addr) (int, error) {
	w.packets = append(w.packets, append([]byte(nil), b...))
	return len(b), nil
}

func (w *retransmitTestWire) take(t *testing.T) packet {
	t.Helper()
	if len(w.packets) == 0 {
		t.Fatal("no datagram sent")
	}
	p, err := parsePacket(w.packets[0])
	w.packets = w.packets[1:]
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Stamp a single queued datagram as if the socket handed it off at ms.
// The fake wire still observes the actual datagram sent by flushWrites.
func sendAt(s *Socket, ms uint64, send func()) {
	tx := &udpWriteBatch{active: true, stamp: ms * 1000}
	tx.messages[0].Buffers = tx.buffers[0][:]
	s.tx = tx
	send()
	s.flushWrites()
	s.tx = nil
}

func TestLostFastRetransmitExpiresDespiteLaterSACK(t *testing.T) {
	const oldest = 10
	var down, up retransmitTestWire
	sender := newConn(&Socket{pc: &down, optSndBuf: 1 << 20}, nil)
	receiver := newConn(&Socket{pc: &up, optRcvBuf: 1 << 20}, nil)
	receiver.s.ackTimer = time.AfterFunc(time.Hour, func() {})
	defer receiver.s.ackTimer.Stop()
	for _, c := range []*Conn{sender, receiver} {
		c.state = csConnected
		c.mtuLast, c.mtuFloor, c.mtuCeiling = 1402, 1402, 1402
		c.mtuDiscoverTime = 60_000
	}
	sender.seqNr, sender.fastResendSeqNr = oldest, oldest
	sender.maxWindow, sender.rto = 16_384, 1000
	receiver.ackNr = oldest - 1
	// Full packets avoid dependence on the application's Nagle policy.
	const size = 1382
	var want [5 * size]byte
	for i := range 5 {
		for j := range want[i*size : (i+1)*size] {
			want[i*size+j] = byte(i + 1)
		}
	}
	clock := nowMicros()
	advance := func(ms uint64) uint64 {
		sender.s.currentMS, receiver.s.currentMS = ms, ms
		return clock + ms*1000
	}
	advance(100)
	for i := range 5 {
		sendAt(sender.s, 100, func() { sender.writeOutgoingPacket(want[i*size:(i+1)*size], stData) })
	}
	if p := down.take(t); p.h.SeqNr != oldest {
		t.Fatalf("first DATA sequence = %d, want %d", p.h.SeqNr, oldest)
	} // Drop the original.
	now := advance(200)
	for range 3 {
		p := down.take(t)
		receiver.processIncoming(&p, false, now)
	}
	receiver.sendAck(false)
	ack := up.take(t)
	sender.processIncoming(&ack, false, now)
	later := down.take(t)
	if p := down.take(t); p.h.SeqNr != oldest {
		t.Fatalf("fast retransmit sequence = %d, want %d", p.h.SeqNr, oldest)
	} // Drop the fast retransmit as well.
	now = advance(900)
	receiver.processIncoming(&later, false, now)
	receiver.sendAck(false)
	ack = up.take(t)
	sender.processIncoming(&ack, false, now)

	advance(1099)
	sender.checkTimeouts()
	if len(down.packets) != 0 {
		t.Fatal("retransmitted before the oldest packet's deadline")
	}
	now = advance(1100)
	sender.checkTimeouts()
	if len(down.packets) == 0 {
		t.Fatal("oldest DATA not retransmitted at its deadline after a later SACK")
	}
	p := down.take(t)
	if p.h.Type != stData || p.h.SeqNr != oldest {
		t.Fatalf("timeout sent %v sequence %d, want DATA %d", p.h.Type, p.h.SeqNr, oldest)
	}
	receiver.processIncoming(&p, false, now)
	receiver.gotEOF = true // Make an incomplete read fail rather than block.
	var got [len(want)]byte
	if _, err := io.ReadFull(receiver, got[:]); err != nil || got != want {
		t.Fatalf("ordered payload was not restored after retransmission: %v", err)
	}
}

func TestReverseDataDoesNotCancelSelectiveAck(t *testing.T) {
	var wire retransmitTestWire
	c := newConn(&Socket{pc: &wire, optSndBuf: 1 << 20, optRcvBuf: 1 << 20}, nil)
	c.state = csConnected
	c.mtuFloor, c.mtuLast, c.mtuCeiling = 1402, 1402, 1402
	c.mtuDiscoverTime = 60_000
	c.maxWindow = 16_384
	c.ackNr = 100
	c.seqNr = 50
	// DATA 101 is missing. A reverse DATA packet can piggyback ACK 100
	// but has no extension to report that DATA 102 arrived.
	c.processIncoming(&packet{h: header{Type: stData, SeqNr: 102, AckNr: 49, WndSize: 1 << 20}, payload: []byte("later")}, false, nowMicros())
	c.writeOutgoingPacket(make([]byte, 1382), stData)
	reverse := wire.take(t)
	if reverse.h.Type != stData || reverse.h.AckNr != 100 {
		t.Fatalf("reverse packet = %v/ACK %d, want DATA/ACK 100", reverse.h.Type, reverse.h.AckNr)
	}
	c.s.issueDeferredAcks()
	ack := wire.take(t)
	if ack.h.Type != stState || ack.h.AckNr != 100 || len(ack.selAck) == 0 || ack.selAck[0]&1 == 0 {
		t.Fatalf("deferred packet = %v/ACK %d/SACK %x, want STATE/ACK 100 with DATA 102 reported", ack.h.Type, ack.h.AckNr, ack.selAck)
	}
}

func TestDuplicateOutOfOrderDataRepeatsSelectiveAck(t *testing.T) {
	var wire retransmitTestWire
	c := newConn(&Socket{pc: &wire, optSndBuf: 1 << 20, optRcvBuf: 1 << 20}, nil)
	c.state = csConnected
	c.ackNr, c.seqNr = 100, 50
	data := packet{h: header{Type: stData, SeqNr: 102, AckNr: 49, WndSize: 1 << 20}, payload: []byte("later")}
	for attempt := range 2 {
		c.processIncoming(&data, false, nowMicros())
		c.s.issueDeferredAcks()
		ack := wire.take(t) // Drop the first SACK on its way back to the sender.
		if ack.h.Type != stState || ack.h.AckNr != 100 || len(ack.selAck) == 0 || ack.selAck[0]&1 == 0 {
			t.Fatalf("attempt %d returned ACK %d/SACK %x, want ACK 100 reporting DATA 102", attempt, ack.h.AckNr, ack.selAck)
		}
	}
}

func TestReadAdvertisesReopenedNonzeroWindow(t *testing.T) {
	var wire retransmitTestWire
	c := newConn(&Socket{pc: &wire, optRcvBuf: 4096}, nil)
	c.state = csConnected
	c.readBuf.write(make([]byte, 4095))
	c.sendAck(false)
	if got := wire.take(t).h.WndSize; got != 1 {
		t.Fatalf("initial advertised window = %d, want 1", got)
	}
	// The peer cannot write its next segment and has no DATA in flight.
	// Draining must send an update without waiting for a new datagram.
	c.s.ackTimer = time.AfterFunc(time.Hour, func() {})
	defer c.s.ackTimer.Stop()
	var drained [2048]byte
	if _, err := io.ReadFull(c, drained[:]); err != nil {
		t.Fatal(err)
	}
	c.s.ackTimerFunc()
	if got := wire.take(t).h.WndSize; got != 2049 {
		t.Fatalf("reopened advertised window = %d, want 2049", got)
	}
}

func TestRetransmitAdvertisesCurrentReceiveWindow(t *testing.T) {
	var wire retransmitTestWire
	c := newConn(&Socket{pc: &wire, optSndBuf: 8192, optRcvBuf: 4096}, nil)
	c.state = csConnected
	c.mtuFloor, c.mtuLast, c.mtuCeiling = 1402, 1402, 1402
	c.maxWindow = 8192
	c.readBuf.write(make([]byte, 4095))
	seq := c.seqNr
	c.writeOutgoingPacket(make([]byte, 1382), stData)
	if p := wire.take(t); p.h.WndSize != 1 || p.h.SeqNr != seq {
		t.Fatalf("initial flight = seq %d window %d, want seq %d window 1", p.h.SeqNr, p.h.WndSize, seq)
	}
	var read [4095]byte
	c.readBuf.read(read[:])
	c.sendAck(false)
	if got := wire.take(t).h.WndSize; got != 4096 {
		t.Fatalf("window update = %d, want 4096", got)
	}
	c.sendPacket(c.outbuf.get(uint32(seq)))
	if p := wire.take(t); p.h.SeqNr != seq || p.h.WndSize != 4096 {
		t.Fatalf("retry = seq %d window %d, want seq %d window 4096", p.h.SeqNr, p.h.WndSize, seq)
	}
}

func TestWindowUpdateSendsQueuedPacketWithoutAckProgress(t *testing.T) {
	var wire retransmitTestWire
	c := newConn(&Socket{pc: &wire, optSndBuf: 8192, optRcvBuf: 8192}, nil)
	c.state = csConnected
	c.mtuFloor, c.mtuLast, c.mtuCeiling = 1402, 1402, 1402
	c.maxWindow, c.maxWindowUser = 8192, 0
	c.seqNr, c.ackNr = 20, 100
	// With the window closed, neither segment can be sent or acknowledged.
	// The receiver sends a standalone window update after draining its buffer.
	for range 2 {
		c.writeOutgoingPacket(make([]byte, 1382), stData)
	}
	if len(wire.packets) != 0 {
		t.Fatal("sent DATA before the receiver opened its window")
	}
	update := packet{h: header{Type: stState, SeqNr: 101, AckNr: 19, WndSize: 4096}}
	c.processIncoming(&update, false, nowMicros())
	for want := uint16(20); want <= 21; want++ {
		if sent := wire.take(t); sent.h.Type != stData || sent.h.SeqNr != want {
			t.Fatalf("window update sent %v/%d, want DATA/%d", sent.h.Type, sent.h.SeqNr, want)
		}
	}
}

func TestAckDoesNotSendQueuedTailWithoutReceiveCredit(t *testing.T) {
	var wire retransmitTestWire
	c := newConn(&Socket{pc: &wire, optSndBuf: 8192, optRcvBuf: 8192}, nil)
	c.state = csConnected
	c.mtuFloor, c.mtuLast, c.mtuCeiling = 1402, 1402, 1402
	c.mtuDiscoverTime = 60_000
	c.maxWindow, c.maxWindowUser = 8192, 8192
	c.seqNr, c.ackNr = 20, 100

	c.writeOutgoingPacket(make([]byte, 1382), stData)
	if sent := wire.take(t); sent.h.SeqNr != 20 {
		t.Fatalf("first DATA sequence = %d, want 20", sent.h.SeqNr)
	}
	c.writeOutgoingPacket([]byte("tail"), stData)
	if len(wire.packets) != 0 {
		t.Fatal("short tail was not held behind the full segment")
	}

	ack := packet{h: header{Type: stState, SeqNr: 101, AckNr: 20}}
	c.processIncoming(&ack, false, nowMicros())
	if len(wire.packets) != 0 {
		t.Fatal("sent queued DATA after ACK advertised zero receive credit")
	}
	ack.h.WndSize = 3
	c.processIncoming(&ack, false, nowMicros())
	if len(wire.packets) != 0 {
		t.Fatal("sent four bytes with only three bytes of receive credit")
	}
	ack.h.WndSize = 4
	c.processIncoming(&ack, false, nowMicros())
	if sent := wire.take(t); sent.h.Type != stData || sent.h.SeqNr != 21 || string(sent.payload) != "tail" {
		t.Fatalf("after credit update got %v/%d %q, want DATA/21 tail", sent.h.Type, sent.h.SeqNr, sent.payload)
	}
}

func TestShortWriteResumesWithExactReceiveCredit(t *testing.T) {
	var wire retransmitTestWire
	s := &Socket{pc: &wire, optSndBuf: 8192, optRcvBuf: 8192}
	c := newConn(s, nil)
	c.state = csConnected
	c.mtuFloor, c.mtuLast, c.mtuCeiling = 1402, 1402, 1402
	c.mtuDiscoverTime = nowMillis() + 60_000
	c.maxWindow, c.maxWindowUser = 8192, 0
	c.seqNr, c.ackNr = 20, 100
	deadline := time.Now().Add(time.Second)
	if err := c.SetWriteDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := io.WriteString(c, "tail")
		done <- err
	}()
	waiting := false
	for time.Now().Before(deadline) {
		s.mu.Lock()
		waiting = c.state == csConnectedFull
		if waiting {
			ack := packet{h: header{Type: stState, SeqNr: 101, AckNr: 19, WndSize: 4}}
			c.processIncoming(&ack, false, nowMicros())
		}
		s.mu.Unlock()
		if waiting {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatalf("Write with four bytes of receive credit: %v", err)
	}
	if !waiting {
		t.Fatal("write did not wait for credit")
	}
	if sent := wire.take(t); sent.h.Type != stData || string(sent.payload) != "tail" {
		t.Fatalf("sent %v %q, want DATA tail", sent.h.Type, sent.payload)
	}
}

func TestNewFlightUsesActualTransmitTimeForRTO(t *testing.T) {
	var wire retransmitTestWire
	c := newConn(&Socket{pc: &wire, currentMS: 1000, optSndBuf: 8192}, nil)
	c.state = csConnected
	c.rto = 1000
	c.mtuFloor, c.mtuLast, c.mtuCeiling = 1402, 1402, 1402
	c.mtuDiscoverTime = c.s.currentMS + 60_000
	c.maxWindow = 8192
	sendAt(c.s, 1100, func() { c.writeOutgoingPacket(make([]byte, 1382), stData) })
	wire.take(t)
	c.s.currentMS = 2099
	c.checkTimeouts()
	if len(wire.packets) != 0 {
		t.Fatal("first DATA retried before a full timeout had passed since transmission")
	}
	c.s.currentMS = 2100
	c.checkTimeouts()
	if p := wire.take(t); p.h.Type != stData || p.h.SeqNr != 1 {
		t.Fatalf("RTO sent %v/%d, want retry of DATA/1", p.h.Type, p.h.SeqNr)
	}
}

func TestUnsentTailWaitsForCreditAcrossRetransmitTimeout(t *testing.T) {
	var wire retransmitTestWire
	s := &Socket{pc: &wire, currentMS: 1000, optSndBuf: 8192, optRcvBuf: 8192}
	c := newConn(s, nil)
	c.state = csConnected
	c.rto = 1000
	c.mtuFloor, c.mtuLast, c.mtuCeiling = 1402, 1402, 1402
	c.mtuDiscoverTime = 60_000
	c.maxWindow, c.maxWindowUser = 8192, 8192
	c.seqNr, c.ackNr = 20, 100

	// A full segment goes out; the short tail waits behind it.
	sendAt(s, 1000, func() { c.writeOutgoingPacket(make([]byte, 1382), stData) })
	wire.take(t)
	c.writeOutgoingPacket([]byte("tail"), stData)
	ack := packet{h: header{Type: stState, SeqNr: 101, AckNr: 20}}
	c.processIncoming(&ack, false, 1000)
	if len(wire.packets) != 0 {
		t.Fatal("tail was sent before peer credit closed")
	}

	s.currentMS = 2000
	c.checkTimeouts()
	if len(wire.packets) != 0 {
		t.Fatal("timeout sent previously unsent DATA through zero peer credit")
	}
	if c.err != nil {
		t.Fatalf("waiting for credit closed the connection: %v", c.err)
	}

	s.currentMS = 3000
	ack.h.WndSize = 4
	sendAt(s, 3000, func() { c.processIncoming(&ack, false, 3_000_000) })
	if sent := wire.take(t); sent.h.Type != stData || sent.h.SeqNr != 21 || string(sent.payload) != "tail" {
		t.Fatalf("with credit got %v/%d %q, want DATA/21 tail", sent.h.Type, sent.h.SeqNr, sent.payload)
	}
	s.currentMS = 3999
	c.checkTimeouts()
	if len(wire.packets) != 0 {
		t.Fatal("tail retransmitted before a full RTO after first send")
	}
	s.currentMS = 4000
	c.checkTimeouts()
	if sent := wire.take(t); sent.h.SeqNr != 21 || string(sent.payload) != "tail" {
		t.Fatalf("timeout got %v/%d %q, want retry of DATA/21 tail", sent.h.Type, sent.h.SeqNr, sent.payload)
	}
}

func TestMTUProbeAcknowledgesPacketSentFromQueue(t *testing.T) {
	var wire retransmitTestWire
	s := &Socket{pc: &wire, currentMS: 1000, optSndBuf: 8192}
	c := newConn(s, nil)
	c.state = csConnected
	c.mtuFloor, c.mtuLast, c.mtuCeiling = 1200, 1402, 1402
	c.mtuDiscoverTime = 60_000
	c.maxWindow, c.maxWindowUser = 8192, 0
	c.seqNr, c.ackNr = 20, 100

	// A later, small segment is already queued when credit admits the probe.
	c.writeOutgoingPacket(make([]byte, 1382), stData)
	c.writeOutgoingPacket([]byte("tail"), stData)
	ack := packet{h: header{Type: stState, SeqNr: 101, AckNr: 19, WndSize: 1382}}
	c.processIncoming(&ack, false, nowMicros())
	if sent := wire.take(t); sent.h.SeqNr != 20 || len(sent.payload) != 1382 || len(wire.packets) != 0 {
		t.Fatalf("window update did not send only the full-size probe: seq=%d size=%d", sent.h.SeqNr, len(sent.payload))
	}

	ack.h.AckNr, ack.h.WndSize = 20, 0
	c.processIncoming(&ack, false, nowMicros())
	if c.mtuFloor != 1402 {
		t.Fatalf("ACK for the transmitted 1402-byte probe left the MTU floor at %d", c.mtuFloor)
	}
}

func TestMTUProbeAcceptsSelectiveAcknowledgement(t *testing.T) {
	var wire retransmitTestWire
	s := &Socket{pc: &wire, currentMS: 1000, optSndBuf: 8192}
	c := newConn(s, nil)
	c.state = csConnected
	c.mtuFloor, c.mtuLast, c.mtuCeiling = 1200, 1402, 1402
	c.mtuDiscoverTime = 60_000
	c.maxWindow, c.maxWindowUser = 8192, 8192
	c.seqNr, c.ackNr = 20, 100

	c.writeOutgoingPacket([]byte("lost"), stData)
	wire.take(t)
	c.writeOutgoingPacket(make([]byte, 1382), stData)
	if sent := wire.take(t); sent.h.SeqNr != 21 || len(sent.payload) != 1382 {
		t.Fatalf("expected DATA/21 with 1382 payload bytes, got %d/%d", sent.h.SeqNr, len(sent.payload))
	}
	ack := packet{
		h:      header{Type: stState, SeqNr: 101, AckNr: 19, WndSize: 8192},
		selAck: []byte{1, 0, 0, 0}, // DATA/21 arrived, but DATA/20 is missing.
	}
	s.currentMS = 1200
	c.processIncoming(&ack, false, nowMicros())
	if c.mtuFloor != 1402 {
		t.Fatalf("SACK for DATA/21 left the MTU floor at %d, want 1402", c.mtuFloor)
	}
	if len(wire.packets) != 0 {
		t.Fatal("single SACK unexpectedly triggered retransmission")
	}
}
