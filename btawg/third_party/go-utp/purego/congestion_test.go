package purego

import (
	"net"
	"testing"
	"time"
)

func TestSlowStartGrowsWithAcknowledgedBytes(t *testing.T) {
	const initialWindow = 128 << 10
	s := &Socket{currentMS: 10, optSndBuf: 4 << 20}
	c := newConn(s, nil)
	c.maxWindow = initialWindow
	c.applyCControl(initialWindow/2, 96_000, 0, 0)
	if c.maxWindow != initialWindow+initialWindow/2 {
		t.Fatalf("window after acknowledging half the flight = %d, want %d", c.maxWindow, initialWindow+initialWindow/2)
	}
}

func TestCongestionAvoidanceAccumulatesSmallACKs(t *testing.T) {
	const start = 8 << 20
	s := &Socket{currentMS: 10, optSndBuf: 16 << 20}
	c := newConn(s, nil)
	c.slowStart = false
	c.maxWindow = start
	// Each ACK adds less than one byte, but an acknowledged flight still
	// needs to grow by almost 3000 bytes at zero queuing delay.
	for range start / 1382 {
		c.applyCControl(1382, 96_000, 0, 0)
	}
	if got := c.maxWindow - start; got < 2950 || got > 3000 {
		t.Fatalf("congestion window grew by %d bytes after one flight, want 2950–3000", got)
	}
}

func TestWideWindowSendsBeyond1024Packets(t *testing.T) {
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	s := &Socket{pc: pc, optSndBuf: 16 << 20, optRcvBuf: 16 << 20}
	c := newConn(s, peer.LocalAddr())
	c.mtuLast = 1402
	c.state = csConnected
	c.maxWindow = s.optSndBuf
	c.maxWindowUser = s.optRcvBuf
	c.mtuDiscoverTime = 1
	const packets = 2048
	data := make([]byte, packets*c.packetSize())
	if n := c.writeNoWait(data); n != len(data) {
		t.Fatalf("queued %d of %d bytes inside an open send window", n, len(data))
	}
	if c.curWindow != len(data) {
		t.Fatalf("sent %d of %d bytes inside an open send window", c.curWindow, len(data))
	}
}

func TestSelectiveAckReportsPacketsBeyondFirstWord(t *testing.T) {
	local := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
	pc, err := net.ListenUDP("udp4", local)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	peer, err := net.ListenUDP("udp4", local)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	s := &Socket{pc: pc, optRcvBuf: 4 << 20}
	c := newConn(s, peer.LocalAddr())
	c.ackNr = 65520
	const gap = 390
	seq := (uint32(c.ackNr) + 2 + gap) & seqNrMask
	c.inbuf.ensureSize(seq+1, gap+3)
	c.inbuf.put(seq, &inboundPacket{})
	c.reorderCount = 1
	c.sendAck(false)

	var wire [512]byte
	n, _, err := peer.ReadFromUDP(wire[:])
	if err != nil {
		t.Fatal(err)
	}
	p, err := parsePacket(wire[:n])
	if err != nil {
		t.Fatal(err)
	}
	if len(p.selAck) <= gap/8 || p.selAck[gap/8] != 1<<(gap%8) {
		t.Fatalf("SACK %x does not report received packet %d past cumulative ack", p.selAck, gap+2)
	}
}

func TestPacedWindowRecoversOnlyWithLowQueueDelay(t *testing.T) {
	for _, tt := range []struct {
		name  string
		rate  int64
		queue uint32
		want  int
	}{
		{"paced low delay", 50_000_000, 1_000, 1_800_000},
		{"paced high delay", 50_000_000, 110_000, 20_000},
		{"unpaced low delay", 0, 1_000, 20_000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &Socket{currentMS: 2_000, optSndBuf: 4 << 20, paceBytesPerSecond: tt.rate}
			c := newConn(s, nil)
			c.slowStart = false
			c.maxWindow = 20_000
			c.rttHist.addSample(40, s.currentMS)
			c.ourHist.addSample(1_000, s.currentMS)
			for range len(c.ourHist.curDelayHist) {
				c.ourHist.addSample(1_000+tt.queue, s.currentMS)
			}
			c.applyCControl(1_000, 120_000, 0, 0)
			if tt.queue > 100_000 {
				if c.maxWindow >= tt.want {
					t.Fatalf("window = %d, want congestion backoff below %d", c.maxWindow, tt.want)
				}
			} else if c.maxWindow != tt.want {
				t.Fatalf("window = %d, want %d", c.maxWindow, tt.want)
			}
		})
	}
}

func TestFastLossBackoffWaitsOneRTT(t *testing.T) {
	for _, tt := range []struct {
		name string
		rtt  uint32
		wait uint64
	}{
		{"unknown RTT", 0, 100},
		{"short RTT", 30, 100},
		{"WAN RTT", 200, 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const firstLoss = 1000
			c := newConn(&Socket{}, nil)
			c.rtt = tt.rtt
			c.maxWindow = 64 << 10
			c.maybeDecayWin(firstLoss)
			if c.maxWindow != 32<<10 {
				t.Fatalf("first loss left window %d, want %d", c.maxWindow, 32<<10)
			}
			c.maybeDecayWin(firstLoss + tt.wait - 1)
			if c.maxWindow != 32<<10 {
				t.Fatalf("another loss before the next round reduced window to %d", c.maxWindow)
			}
			c.maybeDecayWin(firstLoss + tt.wait)
			if c.maxWindow != 16<<10 {
				t.Fatalf("loss in the next round left window %d, want %d", c.maxWindow, 16<<10)
			}
		})
	}
}

func TestCongestionBackoffKeepsOneSegmentSendable(t *testing.T) {
	for _, tc := range []struct {
		name             string
		peerWindow, want int
	}{
		{"peer has room", 4096, 1382},
		{"peer window closed", 0, 0},
		{"peer window too small", 500, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire retransmitTestWire
			s := &Socket{pc: &wire, currentMS: 1000, optSndBuf: 8192}
			c := newConn(s, nil)
			c.state = csConnected
			c.mtuFloor, c.mtuLast, c.mtuCeiling = 1402, 1402, 1402
			c.mtuDiscoverTime = 60_000
			c.maxWindow, c.maxWindowUser = 1382, tc.peerWindow
			c.maybeDecayWin(s.currentMS)
			if got := c.writeNoWait(make([]byte, 1382)); got != tc.want {
				t.Fatalf("after backoff with no data in flight, Write queued %d bytes, want %d", got, tc.want)
			}
			if tc.want > 0 && len(wire.take(t).payload) != tc.want {
				t.Fatal("segment was queued but not transmitted")
			}
		})
	}
}

func TestWindowAfterPropagationStep(t *testing.T) {
	for _, tc := range []struct {
		name        string
		outstanding bool
		reverse     uint32
		baseRTT     uint32
		vary        bool
		wantGrow    bool
	}{
		{"symmetric route change", false, 100_000, 0, false, true},
		{"symmetric route change from WAN", false, 100_000, 40, false, true},
		{"forward FIFO", false, 0, 0, false, false},
		{"fluctuating bidirectional queue", false, 100_000, 0, true, false},
		{"old flight has not drained", true, 100_000, 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Socket{currentMS: 1000, optSndBuf: 4 << 20}
			c := newConn(s, nil)
			c.mtuLast = 1402
			c.slowStart = false
			c.maxWindow, c.maxWindowUser = c.packetSize(), s.optSndBuf
			if tc.outstanding {
				c.curWindow = 12 * c.packetSize()
				c.rtt = 3600 // loss backlog: ACKs for individual retransmissions can still be fast
			} else {
				c.rtt = tc.baseRTT + uint32((100_100+tc.reverse)/1000)
			}
			c.ourHist.addSample(1000, s.currentMS)
			c.theirHist.addSample(2000, s.currentMS)
			c.rttHist.addSample(tc.baseRTT, s.currentMS)
			for i := range 32 {
				s.currentMS += 200
				jitter := uint32(0)
				if tc.vary && i%2 == 1 {
					jitter = 25_000
				}
				for range curDelaySize {
					c.ourHist.addSample(101_100+jitter, s.currentMS)
					c.theirHist.addSample(2000+tc.reverse+jitter, s.currentMS)
				}
				c.lastMaxedOutWindow = s.currentMS
				rtt := int64(tc.baseRTT*1000 + 100_100 + tc.reverse + 2*jitter)
				c.applyCControl(c.packetSize(), rtt, rtt, 2000+tc.reverse+jitter)
			}
			if grew := c.maxWindow >= 2*c.packetSize(); grew != tc.wantGrow {
				t.Fatalf("repeated ACKs left window %d (MSS %d), want growth %v", c.maxWindow, c.packetSize(), tc.wantGrow)
			}
		})
	}
}

func TestPropagationStepRequiresFreshBidirectionalACK(t *testing.T) {
	for _, tc := range []struct {
		name          string
		transmissions uint32
		reverse       bool
		wantGrow      bool
	}{
		{"fresh bidirectional ACK", 1, true, true},
		{"retransmitted data", 2, true, false},
		{"no reverse timestamp", 1, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := uint64(10_000_000)
			s := &Socket{currentMS: 1_000, optSndBuf: 4 << 20}
			c := newConn(s, nil)
			c.state, c.mtuLast, c.slowStart = csConnected, 1402, false
			c.maxWindow, c.maxWindowUser = c.packetSize(), s.optSndBuf
			c.rtt = 200
			c.rttHist.addSample(1, s.currentMS)
			c.ourHist.addSample(1000, s.currentMS)
			c.theirHist.addSample(2000, s.currentMS)
			for range curDelaySize {
				c.ourHist.addSample(101_000, s.currentMS)
				c.theirHist.addSample(102_000, s.currentMS)
			}
			for i := range 16 {
				s.currentMS += 200
				now := clock + s.currentMS*1000
				seq := uint16(10 + i)
				c.seqNr, c.curWindowPackets, c.curWindow = seq+1, 1, c.packetSize()
				pkt := &outgoingPacket{payload: c.packetSize(), transmissions: 1, timeSent: now - 200_000}
				p := packet{h: header{Type: stState, SeqNr: 1, AckNr: seq, WndSize: 4 << 20, TimestampMicros: uint32(now - 102_000), TimestampDiffMicros: 101_000}}
				if i >= 10 {
					pkt.transmissions = tc.transmissions
					if !tc.reverse {
						p.h.TimestampMicros = 0
					}
				}
				c.outbuf.put(uint32(seq), pkt)
				c.lastMaxedOutWindow = s.currentMS
				c.processIncoming(&p, false, now)
			}
			if grew := c.maxWindow >= 2*c.packetSize(); grew != tc.wantGrow {
				t.Fatalf("window = %d, want growth %v", c.maxWindow, tc.wantGrow)
			}
		})
	}
}
