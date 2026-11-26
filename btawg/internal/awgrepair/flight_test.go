package awgrepair

import (
	"encoding/binary"
	"testing"
	"time"
)

// Create real IPv4/TCP option and checksum layouts independently of the limiter.
func flightPacket(flags byte, ws int, port uint16, window uint16, seq, ack uint32, reverse bool, options []byte) []byte {
	opts := make([]byte, 0, len(options)+8)
	if ws >= 0 {
		opts = append(opts, 1, 3, 3, byte(ws))
	}
	opts = append(opts, options...)
	for len(opts)%4 != 0 {
		opts = append(opts, 0)
	}
	n := 40 + len(opts)
	p := make([]byte, n)
	p[0] = 0x45
	p[2] = byte(n >> 8)
	p[3] = byte(n)
	p[9] = 6
	p[12], p[13], p[14], p[15] = 10, 0, 0, 1
	p[16], p[17], p[18], p[19] = 10, 0, 0, 2
	sport, dport := port, uint16(80)
	if reverse {
		copy(p[12:16], p[16:20])
		p[16], p[17], p[18], p[19] = 10, 0, 0, 1
		sport, dport = dport, sport
	}
	binary.BigEndian.PutUint16(p[20:], sport)
	binary.BigEndian.PutUint16(p[22:], dport)
	binary.BigEndian.PutUint32(p[24:], seq)
	binary.BigEndian.PutUint32(p[28:], ack)
	p[32] = byte((n - 20) / 4 << 4)
	p[33] = flags
	binary.BigEndian.PutUint16(p[34:], window)
	copy(p[40:], opts)
	binary.BigEndian.PutUint16(p[36:], flightChecksum(p))
	return p
}

func flightACK(flags byte, ws int, port uint16, window uint16) []byte {
	return flightPacket(flags, ws, port, window, 0, 0, false, nil)
}

func flightNegotiate(f *flightGuard, now time.Time, port uint16, localScale, peerScale int) {
	f.observe(flightPacket(2, localScale, port, 32768, 100, 0, false, nil), now)
	f.peerHandshake(flightPacket(18, peerScale, port, 32768, 900, 101, true, nil), now)
}

func flightChecksum(p []byte) uint16 {
	sum := uint32(6 + len(p) - 20)
	for i := 12; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(p[i:]))
	}
	for i := 20; i < len(p); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(p[i:]))
	}
	for sum>>16 != 0 {
		sum = sum&65535 + sum>>16
	}
	return ^uint16(sum)
}

func TestFlightBudgetPreservesWindowAndTCPChecksum(t *testing.T) {
	now := time.Now()
	var f flightGuard
	flightNegotiate(&f, now, 4000, 8, 8)
	ack := flightACK(16, -1, 4000, 65535)
	if !f.observe(ack, now.Add(time.Millisecond)) {
		t.Fatal("did not report the window write")
	}
	w := binary.BigEndian.Uint16(ack[34:])
	if int(w)<<8 != 2<<20 || flightChecksum(ack) != 0 {
		t.Fatalf("one-flow credit=%d checksum=%d", int(w)<<8, flightChecksum(ack))
	}
	small := flightACK(16, -1, 4000, 12)
	if f.observe(small, now.Add(2*time.Millisecond)) || binary.BigEndian.Uint16(small[34:]) != 12 || flightChecksum(small) != 0 {
		t.Fatal("receiver flow control was enlarged")
	}
	flightNegotiate(&f, now, 4001, 8, 8)
	ack = flightACK(16, -1, 4000, 65535)
	f.observe(ack, now)
	if int(binary.BigEndian.Uint16(ack[34:]))<<8 != 1<<20 {
		t.Fatal("each flow received a whole link's budget")
	}
	unknown := flightACK(16, -1, 4002, 65535)
	f.observe(unknown, now)
	if binary.BigEndian.Uint16(unknown[34:]) != 65535 {
		t.Fatal("guessed scale for established connection")
	}
}

func TestFlightWaitsForAssociatedPeerHandshake(t *testing.T) {
	now := time.Now()
	var f flightGuard
	f.observe(flightPacket(2, 8, 4000, 32768, 100, 0, false, nil), now)
	ack := flightACK(16, -1, 4000, 65535)
	f.observe(ack, now)
	if binary.BigEndian.Uint16(ack[34:]) != 65535 {
		t.Fatal("clamped before the peer negotiated window scaling")
	}
	f.peerHandshake(flightPacket(18, 8, 4000, 32768, 900, 999, true, nil), now)
	f.observe(ack, now)
	if binary.BigEndian.Uint16(ack[34:]) != 65535 || len(f.flows) != 0 {
		t.Fatal("accepted a SYN-ACK with an unrelated acknowledgement")
	}
	f.peerHandshake(flightPacket(18, 8, 4000, 32768, 900, 101, true, nil), now)
	f.observe(ack, now)
	if int(binary.BigEndian.Uint16(ack[34:]))<<8 != 2<<20 || flightChecksum(ack) != 0 {
		t.Fatal("associated handshake did not enable scaled credit")
	}
}

func TestFlightUsesZeroScaleWhenPeerOmitsScaling(t *testing.T) {
	now := time.Now()
	var f flightGuard
	flightNegotiate(&f, now, 4000, 8, -1)
	key := ackFlow{src: 10<<24 | 1, dst: 10<<24 | 2, sport: 4000, dport: 80}
	e, ok := f.flows[key]
	if !ok || e.scale != 0 {
		t.Fatal("local SYN option enabled scaling without peer negotiation")
	}
	ack := flightACK(16, -1, 4000, 65535)
	f.observe(ack, now)
	if binary.BigEndian.Uint16(ack[34:]) != 65535 || flightChecksum(ack) != 0 {
		t.Fatal("applied a scale factor to an unscaled flow")
	}
}

func TestFlightPeerInitiatedHandshake(t *testing.T) {
	now := time.Now()
	var f flightGuard
	f.peerHandshake(flightPacket(2, 7, 4000, 32768, 900, 0, true, nil), now)
	f.observe(flightPacket(18, 8, 4000, 32768, 100, 901, false, nil), now)
	ack := flightACK(16, -1, 4000, 65535)
	f.observe(ack, now)
	if int(binary.BigEndian.Uint16(ack[34:]))<<8 != 2<<20 {
		t.Fatal("did not use the locally negotiated scale for a peer-initiated flow")
	}
}

func TestFlightRejectsUnsafeLaterOptions(t *testing.T) {
	tests := map[string][]byte{
		"MD5":                 {19, 18, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		"AO":                  {29, 4, 0, 0},
		"malformed timestamp": {8, 9, 0, 0, 0, 0, 0, 0, 0},
		"malformed SACK":      {5, 11, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		"truncated option":    {8, 10, 0, 0},
		"unsupported":         {30, 4, 0, 0},
	}
	for name, options := range tests {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			var f flightGuard
			flightNegotiate(&f, now, 4000, 8, 8)
			ack := flightPacket(16, -1, 4000, 65535, 0, 0, false, options)
			f.observe(ack, now)
			if binary.BigEndian.Uint16(ack[34:]) != 65535 || flightChecksum(ack) != 0 || len(f.flows) != 0 {
				t.Fatal("modified or retained a flow with unsafe TCP options")
			}
		})
	}
}

func TestFlightAcceptsTimestampAndSACK(t *testing.T) {
	tests := map[string][]byte{
		"timestamp": {1, 1, 8, 10, 0, 0, 0, 1, 0, 0, 0, 2},
		"SACK":      {1, 1, 5, 10, 0, 0, 0, 1, 0, 0, 0, 2},
		"combined":  {1, 1, 8, 10, 0, 0, 0, 1, 0, 0, 0, 2, 1, 1, 5, 18, 0, 0, 0, 1, 0, 0, 0, 2, 0, 0, 0, 3, 0, 0, 0, 4},
	}
	for name, options := range tests {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			var f flightGuard
			flightNegotiate(&f, now, 4000, 8, 8)
			ack := flightPacket(16, -1, 4000, 65535, 0, 0, false, options)
			f.observe(ack, now)
			if int(binary.BigEndian.Uint16(ack[34:]))<<8 != 2<<20 || flightChecksum(ack) != 0 {
				t.Fatal("rejected ordinary Linux TCP options")
			}
		})
	}
}

func TestFlightPendingHandshakesAreBoundedAndOutsideFairShare(t *testing.T) {
	now := time.Now()
	var f flightGuard
	flightNegotiate(&f, now, 4000, 8, 8)
	for i := 0; i < 256; i++ {
		f.observe(flightPacket(2, 8, uint16(5000+i), 32768, uint32(i+1), 0, false, nil), now)
	}
	if len(f.pending) > 128 || len(f.flows) != 1 {
		t.Fatalf("unbounded or established pending state: pending=%d flows=%d", len(f.pending), len(f.flows))
	}
	ack := flightACK(16, -1, 4000, 65535)
	f.observe(ack, now)
	if int(binary.BigEndian.Uint16(ack[34:]))<<8 != 2<<20 {
		t.Fatal("pending handshakes consumed established flows' fair share")
	}
	f.observe(flightACK(16, -1, 4999, 65535), now.Add(4*time.Second))
	if len(f.pending) != 0 {
		t.Fatal("stale pending handshakes were not expired")
	}
}

func TestFlightIdleThenBulkRetainsFairShare(t *testing.T) {
	start := time.Now()
	var f flightGuard
	for i := 0; i < 8; i++ {
		flightNegotiate(&f, start.Add(time.Duration(i)*200*time.Millisecond), uint16(4000+i), 8, 8)
	}
	now := start.Add(4 * time.Second)
	for i := 0; i < 8; i++ {
		ack := flightACK(16, -1, uint16(4000+i), 65535)
		if !f.observe(ack, now) {
			t.Fatalf("flow %d lost its handshake while idle", i)
		}
		got := int(binary.BigEndian.Uint16(ack[34:])) << 8
		if got != flightBytes/8 || flightChecksum(ack) != 0 {
			t.Fatalf("flow %d restarted with %d bytes instead of its fair share", i, got)
		}
	}
}
