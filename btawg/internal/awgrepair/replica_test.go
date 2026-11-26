package awgrepair

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

// makeTCP builds a minimal IPv4/TCP packet with the given TCP flags and payload.
func makeTCP(t *testing.T, flags byte, payload []byte) []byte {
	t.Helper()
	ipLen := 20
	tcpLen := 20
	total := ipLen + tcpLen + len(payload)
	p := make([]byte, total)
	// IPv4 header.
	p[0] = 0x45 // version 4, IHL 5.
	p[1] = 0
	p[2] = byte(total >> 8)
	p[3] = byte(total)
	p[4] = 0x12 // identification
	p[5] = 0x34
	p[6] = 0x40 // DF set, fragment offset 0.
	p[7] = 0
	p[8] = 64 // TTL
	p[9] = 6  // TCP
	p[10] = 0 // header checksum (ignored)
	p[11] = 0
	p[12] = 10
	p[13] = 0
	p[14] = 0
	p[15] = 1
	p[16] = 10
	p[17] = 0
	p[18] = 0
	p[19] = 2
	// TCP header.
	p[20] = 0x1a // source port 6666
	p[21] = 0x0a
	p[22] = 0x1f // dest port 8080
	p[23] = 0x90
	p[24] = 0
	p[25] = 0
	p[26] = 0
	p[27] = 1 // seq
	p[28] = 0
	p[29] = 0
	p[30] = 0
	p[31] = 1               // ack
	p[32] = 0x50 | (5 << 4) // data offset 5
	p[33] = flags
	p[34] = 0x10
	p[35] = 0
	p[36] = 0 // checksum (ignored)
	p[37] = 0
	p[38] = 0
	p[39] = 0
	copy(p[40:], payload)
	return p
}

func TestEligibleReplicaValidData(t *testing.T) {
	p := makeTCP(t, 0x18, []byte("hello")) // PSH+ACK, payload present.
	if !eligibleReplica(p) {
		t.Fatal("expected IPv4 TCP data packet to be eligible")
	}
}

func TestEligibleReplicaRejectsControlFlags(t *testing.T) {
	for _, flags := range []byte{0x02, 0x01, 0x04, 0x12, 0x19} { // SYN, FIN, RST, SYN+ACK, FIN+PSH+ACK.
		p := makeTCP(t, flags, []byte("x"))
		if eligibleReplica(p) {
			t.Fatalf("flags 0x%02x should not be eligible", flags)
		}
	}
}

func TestEligibleReplicaRejectsAckOnly(t *testing.T) {
	p := makeTCP(t, 0x10, nil) // ACK only, no payload.
	if eligibleReplica(p) {
		t.Fatal("ack-only packet should not be eligible")
	}
}

func TestEligibleReplicaRejectsUDP(t *testing.T) {
	p := makeTCP(t, 0x10, []byte("payload"))
	p[9] = 17 // UDP
	if eligibleReplica(p) {
		t.Fatal("UDP packet should not be eligible")
	}
}

func TestEligibleReplicaRejectsIPv6(t *testing.T) {
	p := makeTCP(t, 0x18, []byte("payload"))
	p[0] = 0x60 // IPv6
	if eligibleReplica(p) {
		t.Fatal("IPv6 packet should not be eligible")
	}
}

func TestEligibleReplicaRejectsFragmented(t *testing.T) {
	p := makeTCP(t, 0x18, []byte("payload"))
	p[6] = 0x40 // DF set, MF clear, offset 0: non-fragmented.
	if !eligibleReplica(p) {
		t.Fatal("DF non-fragmented packet should be eligible")
	}
	p[6] = 0x00 // DF clear, MF clear, offset 0: non-fragmented.
	if !eligibleReplica(p) {
		t.Fatal("non-fragmented packet without DF should still be eligible")
	}
	p[6] = 0x60 // DF set, MF set.
	if eligibleReplica(p) {
		t.Fatal("fragmented packet (DF+MF) should not be eligible")
	}
	p[6] = 0x40
	p[7] = 0x08 // DF set, non-zero fragment offset.
	if eligibleReplica(p) {
		t.Fatal("fragmented packet (DF+offset) should not be eligible")
	}
	p[6] = 0x20 // DF clear, MF set.
	p[7] = 0x00
	if eligibleReplica(p) {
		t.Fatal("fragmented packet (MF) should not be eligible")
	}
}

func TestEligibleReplicaRejectsIncomplete(t *testing.T) {
	p := makeTCP(t, 0x18, []byte("payload"))
	p[2] = 0
	p[3] = byte(len(p) + 10) // total length larger than buffer.
	if eligibleReplica(p) {
		t.Fatal("packet with total length mismatch should not be eligible")
	}
}

func TestEligibleReplicaRejectsOversized(t *testing.T) {
	p := makeTCP(t, 0x18, make([]byte, 2000))
	if len(p) <= 1280 {
		t.Fatal("test packet should exceed MTU")
	}
	if eligibleReplica(p) {
		t.Fatal("packet larger than MTU should not be eligible")
	}
}

func TestEligibleReplicaRejectsShortHeaders(t *testing.T) {
	if eligibleReplica([]byte{0x45}) {
		t.Fatal("truncated IP header should not be eligible")
	}
	p := makeTCP(t, 0x18, []byte("payload"))
	// IHL claims 6 (24 bytes) but packet only has 20-byte IP header.
	p[0] = 0x46
	if eligibleReplica(p) {
		t.Fatal("IP header too short for claimed IHL should not be eligible")
	}
}

func TestReplicaCacheFirstPacketForwarded(t *testing.T) {
	var c replicaCache
	now := time.Unix(0, 0)
	p := makeTCP(t, 0x18, []byte("data"))
	if c.duplicate(p, now) {
		t.Fatal("first packet should not be duplicate")
	}
}

func TestReplicaCacheSuppressesIdenticalWithinWindow(t *testing.T) {
	var c replicaCache
	now := time.Unix(0, 0)
	p := makeTCP(t, 0x18, []byte("data"))
	if c.duplicate(p, now) {
		t.Fatal("first packet should not be duplicate")
	}
	for i := range 3 {
		if !c.duplicate(p, now.Add(time.Duration(i+1)*time.Millisecond)) {
			t.Fatalf("identical packet %d should be suppressed", i+1)
		}
	}
}

func TestReplicaCacheExpiresAfterWindow(t *testing.T) {
	var c replicaCache
	now := time.Unix(0, 0)
	p := makeTCP(t, 0x18, []byte("data"))
	if c.duplicate(p, now) {
		t.Fatal("first packet should not be duplicate")
	}
	if !c.duplicate(p, now.Add(24*time.Millisecond)) {
		t.Fatal("packet just inside window should be suppressed")
	}
	if c.duplicate(p, now.Add(25*time.Millisecond)) {
		t.Fatal("packet at original window boundary should be forwarded again")
	}
	// The forward at 25 ms started a fresh window; duplicates inside it are suppressed.
	if !c.duplicate(p, now.Add(26*time.Millisecond)) {
		t.Fatal("packet inside the fresh window should be suppressed")
	}
	if c.duplicate(p, now.Add(50*time.Millisecond)) {
		t.Fatal("packet at fresh window boundary should be forwarded again")
	}
}

func TestReplicaCacheDoesNotExtendExpiryOnRepeat(t *testing.T) {
	var c replicaCache
	now := time.Unix(0, 0)
	p := makeTCP(t, 0x18, []byte("data"))
	c.duplicate(p, now)
	c.duplicate(p, now.Add(10*time.Millisecond))
	// If the repeat at 10ms extended expiry, 30ms would be suppressed.
	if c.duplicate(p, now.Add(30*time.Millisecond)) {
		t.Fatal("repeat should not extend expiry")
	}
}

func TestReplicaCacheDifferentPacketNotDuplicate(t *testing.T) {
	var c replicaCache
	now := time.Unix(0, 0)
	p1 := makeTCP(t, 0x18, []byte("data"))
	p2 := makeTCP(t, 0x18, []byte("other"))
	c.duplicate(p1, now)
	if c.duplicate(p2, now.Add(time.Millisecond)) {
		t.Fatal("different packet should not be treated as duplicate")
	}
}

func TestReplicaCacheExactBytesVerified(t *testing.T) {
	var c replicaCache
	now := time.Unix(0, 0)
	p := makeTCP(t, 0x18, []byte("data"))
	c.duplicate(p, now)
	p2 := makeTCP(t, 0x18, []byte("data"))
	p2[40] ^= 0xff // mutate payload byte.
	if c.duplicate(p2, now.Add(time.Millisecond)) {
		t.Fatal("packet with different bytes should not be suppressed even if metadata collides")
	}
}

func TestReplicaCacheCollisionEvictsSlot(t *testing.T) {
	var c replicaCache
	now := time.Unix(0, 0)
	a := makeTCP(t, 0x18, []byte("a-payload"))
	slotA := packetID(a) & (replicaSlots - 1)

	// Find a different packet that maps to the same direct-mapped slot.
	var b []byte
	for i := range 1 << 16 {
		candidate := makeTCP(t, 0x18, []byte("b-payload"))
		binary.BigEndian.PutUint16(candidate[4:6], uint16(i))
		if packetID(candidate)&(replicaSlots-1) == slotA && !bytes.Equal(candidate, a) {
			b = candidate
			break
		}
	}
	if b == nil {
		t.Fatal("could not find a colliding packet")
	}

	if c.duplicate(a, now) {
		t.Fatal("first packet should be forwarded")
	}
	if c.duplicate(b, now.Add(time.Millisecond)) {
		t.Fatal("different packet in same slot should be forwarded, not suppressed")
	}
	// a was evicted by b due to direct mapping; false-negative is safe for TCP.
	if c.duplicate(a, now.Add(2*time.Millisecond)) {
		t.Fatal("evicted packet should be forwarded again, not falsely suppressed")
	}
}

func TestReplicaCacheNoCrossSessionState(t *testing.T) {
	var c1, c2 replicaCache
	now := time.Unix(0, 0)
	p := makeTCP(t, 0x18, []byte("data"))
	c1.duplicate(p, now)
	if c2.duplicate(p, now.Add(time.Millisecond)) {
		t.Fatal("independent caches must not share state")
	}
}
