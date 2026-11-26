package awgrepair

import (
	"encoding/binary"
	"slices"
	"testing"
	"time"
)

// triggerPacket returns an eligible IPv4/TCP data packet with a payload that
// does not collide with the test fixtures.
func triggerPacket(t *testing.T) []byte {
	t.Helper()
	return makeTCP(t, 0x18, []byte("trigger"))
}

func TestAutoSignalInvalidInputs(t *testing.T) {
	now := time.Unix(1000, 0)
	m := &autoMeter{}

	// Non-eligible inputs must not crash or emit.
	var emitted int
	emit := func([]byte) { emitted++ }
	m.sent(nil, now, emit)
	m.sent([]byte{0x45}, now, emit)
	m.sent(makeTCP(t, 0x10, nil), now, emit) // ACK-only, no payload.
	m.received(nil, now)
	m.received([]byte{0x45}, now)
	m.received(makeTCP(t, 0x10, nil), now)
	if emitted != 0 {
		t.Fatalf("ineligible data caused %d probe emissions", emitted)
	}

	// Malformed probe payloads are rejected.
	bad := [][]byte{
		{},
		{0, 0, 0, 0},
		{0, 0, 0, 0, 0, 0}, // count 0 with trailing byte.
		{0, 0, 0, 0, byte(autoProbeMaxIDs + 1)},
		slices.Repeat([]byte{0}, 13), // count 1 but length 13 != 13.
	}
	for _, p := range bad {
		if _, ok := m.probe(p, now); ok {
			t.Fatalf("probe should reject malformed payload %x", p)
		}
	}
}

func TestAutoSignalMatchesAllReceived(t *testing.T) {
	now := time.Unix(1000, 0)
	m := &autoMeter{}
	pkts := make([][]byte, 5)
	for i := range pkts {
		pkts[i] = makeTCP(t, 0x18, slices.Repeat([]byte{byte(i)}, i+10))
	}

	var probes [][]byte
	emit := func(p []byte) { probes = append(probes, append([]byte(nil), p...)) }
	for i := range pkts {
		m.sent(pkts[i], now.Add(time.Duration(i)*time.Millisecond), emit)
	}
	m.sent(triggerPacket(t), now.Add(autoProbeInterval+time.Millisecond), emit)

	for _, p := range pkts {
		m.received(p, now.Add(2*autoProbeInterval))
	}

	probe := probes[len(probes)-1]
	ev, ok := m.probe(probe, now.Add(3*autoProbeInterval))
	if !ok {
		t.Fatal("valid probe rejected")
	}
	if ev.total != len(pkts) || ev.missing != 0 {
		t.Fatalf("total=%d missing=%d, want total=%d missing=0", ev.total, ev.missing, len(pkts))
	}
}

func TestAutoSignalDetectsRawLoss(t *testing.T) {
	now := time.Unix(1000, 0)
	m := &autoMeter{}
	pkts := make([][]byte, 5)
	for i := range pkts {
		pkts[i] = makeTCP(t, 0x18, slices.Repeat([]byte{byte(i)}, i+10))
	}

	var probes [][]byte
	emit := func(p []byte) { probes = append(probes, append([]byte(nil), p...)) }
	for i := range pkts {
		m.sent(pkts[i], now.Add(time.Duration(i)*time.Millisecond), emit)
	}
	m.sent(triggerPacket(t), now.Add(autoProbeInterval+time.Millisecond), emit)

	// Receive only packets 0, 2 and 4 (simulating loss/repair).
	for _, i := range []int{0, 2, 4} {
		m.received(pkts[i], now.Add(2*autoProbeInterval))
	}

	probe := probes[len(probes)-1]
	ev, ok := m.probe(probe, now.Add(3*autoProbeInterval))
	if !ok {
		t.Fatal("valid probe rejected")
	}
	if ev.total != len(pkts) || ev.missing != 2 {
		t.Fatalf("total=%d missing=%d, want total=%d missing=2", ev.total, ev.missing, len(pkts))
	}
}

func TestAutoSignalReorderedPacketsNotLost(t *testing.T) {
	now := time.Unix(1000, 0)
	m := &autoMeter{}
	pkts := make([][]byte, 10)
	for i := range pkts {
		pkts[i] = makeTCP(t, 0x18, slices.Repeat([]byte{byte(i)}, i+10))
	}

	var probes [][]byte
	emit := func(p []byte) { probes = append(probes, append([]byte(nil), p...)) }
	for i := range pkts {
		m.sent(pkts[i], now.Add(time.Duration(i)*time.Millisecond), emit)
	}
	m.sent(triggerPacket(t), now.Add(autoProbeInterval+time.Millisecond), emit)

	// Receive in reverse order.
	for i := len(pkts) - 1; i >= 0; i-- {
		m.received(pkts[i], now.Add(2*autoProbeInterval))
	}

	probe := probes[len(probes)-1]
	ev, ok := m.probe(probe, now.Add(3*autoProbeInterval))
	if !ok {
		t.Fatal("valid probe rejected")
	}
	if ev.missing != 0 {
		t.Fatalf("reordering caused %d false missing", ev.missing)
	}
}

func TestAutoSignalIgnoresIneligibleInCounts(t *testing.T) {
	now := time.Unix(1000, 0)
	m := &autoMeter{}
	data := makeTCP(t, 0x18, []byte("payload"))
	ack := makeTCP(t, 0x10, nil)

	var probes [][]byte
	emit := func(p []byte) { probes = append(probes, append([]byte(nil), p...)) }
	m.sent(data, now, emit)
	m.sent(ack, now, emit)
	m.sent(triggerPacket(t), now.Add(autoProbeInterval+time.Millisecond), emit)
	m.received(data, now.Add(time.Millisecond))

	probe := probes[len(probes)-1]
	ev, ok := m.probe(probe, now.Add(2*autoProbeInterval))
	if !ok {
		t.Fatal("valid probe rejected")
	}
	if ev.total != 1 || ev.missing != 0 {
		t.Fatalf("total=%d missing=%d, want 1/0", ev.total, ev.missing)
	}
}

func TestAutoSignalIdentityIgnoresTransitHeaderChanges(t *testing.T) {
	now := time.Unix(1000, 0)
	m := &autoMeter{}
	sent := makeTCP(t, 0x18, []byte("x"))
	received := append([]byte(nil), sent...)
	// Typical wire changes that must not alter identity.
	received[8] = 63 // TTL decremented.
	received[10] = 0 // IP checksum (ignored).
	received[11] = 0 // IP checksum.
	received[36] = 0 // TCP checksum (ignored).
	received[37] = 0 // TCP checksum.

	var probes [][]byte
	emit := func(p []byte) { probes = append(probes, append([]byte(nil), p...)) }
	m.sent(sent, now, emit)
	m.sent(triggerPacket(t), now.Add(autoProbeInterval+time.Millisecond), emit)
	m.received(received, now.Add(time.Millisecond))

	probe := probes[len(probes)-1]
	ev, ok := m.probe(probe, now.Add(2*autoProbeInterval))
	if !ok {
		t.Fatal("valid probe rejected")
	}
	if ev.missing != 0 {
		t.Fatalf("transit header change caused false loss: missing=%d", ev.missing)
	}
}

func TestAutoSignalNoOverlappingEmissions(t *testing.T) {
	now := time.Unix(1000, 0)
	m := &autoMeter{}
	pkts := make([][]byte, 3)
	for i := range pkts {
		pkts[i] = makeTCP(t, 0x18, []byte{byte(i)})
	}

	var probes [][]byte
	emit := func(p []byte) { probes = append(probes, append([]byte(nil), p...)) }
	for i := range pkts {
		m.sent(pkts[i], now.Add(time.Duration(i)*time.Millisecond), emit)
	}
	trigger1 := triggerPacket(t)
	m.sent(trigger1, now.Add(autoProbeInterval+time.Millisecond), emit)

	// A second interval later must not resample the original three IDs.
	trigger2 := makeTCP(t, 0x18, []byte("trigger2"))
	m.sent(trigger2, now.Add(2*autoProbeInterval+2*time.Millisecond), emit)

	if len(probes) < 2 {
		t.Fatalf("expected at least 2 probes, got %d", len(probes))
	}
	first := probes[len(probes)-2]
	second := probes[len(probes)-1]

	ev1, ok := m.probe(first, now.Add(3*autoProbeInterval))
	if !ok {
		t.Fatal("first probe rejected")
	}
	if ev1.total != len(pkts) {
		t.Fatalf("first probe total=%d, want %d", ev1.total, len(pkts))
	}
	ev2, ok := m.probe(second, now.Add(3*autoProbeInterval))
	if !ok {
		t.Fatal("second probe rejected")
	}
	if ev2.total != 1 {
		t.Fatalf("second probe should contain exactly the flushed trigger, got %d", ev2.total)
	}
}

func TestAutoSignalRateHeadroomAndQueue(t *testing.T) {
	now := time.Unix(1000, 0)
	m := &autoMeter{}

	// Feed enough full-size packets across distinct reads to signal headroom.
	payload := make([]byte, 1240)
	for i := range 11 {
		p := makeTCP(t, 0x18, payload)
		m.received(p, now.Add(time.Duration(i)*300*time.Microsecond))
	}

	// Build a probe payload manually so we can exercise queue timing deterministically.
	stamp := clock(now)
	probe := make([]byte, 5+8)
	binary.BigEndian.PutUint32(probe[0:4], stamp)
	probe[4] = 1
	// The ID is irrelevant for rate/queue; use zero.

	ev1, ok := m.probe(probe, now.Add(2*time.Millisecond))
	if !ok {
		t.Fatal("valid probe rejected")
	}
	if ev1.rate == 0 {
		t.Fatal("expected non-zero receive rate")
	}
	if !ev1.headroom {
		t.Fatal("expected serialization headroom")
	}
	if ev1.queue != 0 {
		t.Fatalf("first probe queue should be 0, got %v", ev1.queue)
	}

	ev2, ok := m.probe(probe, now.Add(25*time.Millisecond))
	if !ok {
		t.Fatal("second valid probe rejected")
	}
	if ev2.queue < 20*time.Millisecond || ev2.queue > 25*time.Millisecond {
		t.Fatalf("queue delay out of range: %v", ev2.queue)
	}
}

func TestAutoSignalDoesNotImplictlyRepair(t *testing.T) {
	now := time.Unix(1000, 0)
	m := &autoMeter{}
	pkt := makeTCP(t, 0x18, []byte("data"))

	var probes [][]byte
	emit := func(p []byte) { probes = append(probes, append([]byte(nil), p...)) }
	m.sent(pkt, now, emit)
	m.sent(triggerPacket(t), now.Add(autoProbeInterval+time.Millisecond), emit)

	// Never call received; probe should report the packet missing.
	probe := probes[len(probes)-1]
	ev, ok := m.probe(probe, now.Add(2*autoProbeInterval))
	if !ok {
		t.Fatal("valid probe rejected")
	}
	if ev.total != 1 || ev.missing != 1 {
		t.Fatalf("total=%d missing=%d, want 1/1", ev.total, ev.missing)
	}
}

func TestRawLossIgnoresDuplicateArrivalBurst(t *testing.T) {
	var m autoMeter
	now := time.Now()
	first := makeTCP(t, 0x18, []byte("one"))
	other := makeTCP(t, 0x18, []byte("other"))
	m.received(first, now)
	for range autoReceiveWindow + 40 {
		m.received(other, now)
	}
	id, _, _ := packetIdentity(first)
	var probe [13]byte
	probe[4] = 1
	binary.BigEndian.PutUint64(probe[5:], id)
	ev, ok := m.probe(probe[:], now)
	if !ok || ev.missing != 0 {
		t.Fatal("duplicate arrivals incorrectly marked a previously delivered original missing")
	}
}
