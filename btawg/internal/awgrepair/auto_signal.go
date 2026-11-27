package awgrepair

import (
	"encoding/binary"
	"time"
)

const (
	autoProbeMaxIDs   = 32
	autoProbeInterval = 40 * time.Millisecond
	autoIdleReset     = 200 * time.Millisecond
	autoRateWindow    = 100 * time.Millisecond
	autoReceiveWindow = 1024
)

// autoEvidence is the result of matching a remote probe list against locally
// observed pre-repair packets. All fields are derived from tiny fingerprints;
// no original packet data is retained.
type autoEvidence struct {
	stamp        uint32        // probe timestamp (sender milliseconds, raw)
	total        int           // IDs carried in this probe
	missing      int           // IDs not found in the local receive window
	rate         uint64        // observed receive rate in bytes/sec
	queue        time.Duration // queue delay relative to the minimum probe delta
	forwardQueue bool          // most of a measured RTT was added in this direction
	headroom     bool          // spare serialization capacity observed recently
}

// autoMeter observes one direction of a lossy path. It samples eligible
// sent packets into compact probe lists and matches inbound probes against a
// bounded cache of recently received packet fingerprints.
//
// The zero value is ready to use. All methods should be called from one
// goroutine; the type does not synchronize internally.
type autoMeter struct {
	// Sent side: a bounded ring of recent packet identities and a scratch
	// buffer used once per emission. The callback must copy the slice if it
	// retains it beyond the call.
	sentIDs  [autoProbeMaxIDs]uint64
	sentHead int
	sentN    int
	lastSent time.Time
	nextEmit time.Time
	probeBuf [5 + autoProbeMaxIDs*8]byte

	// Membership is distinct: a burst of retransmissions cannot evict a
	// different delivered packet before the delayed probe that names it.
	seen     map[uint64]struct{}
	seenRing [autoReceiveWindow]uint64
	seenHead int
	seenN    int
	rx       autoRate
	train    arrivalTrain

	// Queue baseline: minimum (receiver time - probe stamp) seen so far.
	minDelta    uint32
	minDeltaSet bool
	sawLANProbe bool // a low-RTT probe established a one-way reference for this session
}

// autoRate estimates a simple bytes-per-second rate over the latest window.
type autoRate struct {
	since time.Time
	bytes int64
	rate  uint64
}

func (r *autoRate) add(now time.Time, n int) {
	if r.since.IsZero() {
		r.since = now
	}
	if d := now.Sub(r.since); d >= autoRateWindow {
		r.rate = uint64(r.bytes) * uint64(time.Second) / uint64(d)
		r.since = now
		r.bytes = 0
	}
	r.bytes += int64(n)
}

func (r *autoRate) current(now time.Time) uint64 {
	if r.since.IsZero() {
		return 0
	}
	d := now.Sub(r.since)
	// Retain the previous completed interval while the new bucket fills;
	// a few packets at the same timestamp cannot imply unlimited capacity.
	if d < autoRateWindow && r.rate != 0 {
		return r.rate
	}
	return uint64(r.bytes) * uint64(time.Second) / uint64(max(d, autoRateWindow))
}

// sent records an eligible packet being forwarded and, at low overhead,
// emits a compact probe list of recent packet identities. emit is called
// with a payload that can be passed to the peer's probe method.
//
// IDs are emitted at most once. A new interval starts on the first eligible
// packet; when the interval expires the accumulated batch is flushed before
// the triggering packet is added, and after a long idle any pending batch is
// flushed with the last packet's timestamp.
func (m *autoMeter) sent(data []byte, now time.Time, emit func([]byte)) {
	id, _, ok := packetIdentity(data)
	if !ok {
		return
	}
	// Flush a stale pending batch after idle so IDs are not held forever.
	// Stamp it with the current time to keep queue deltas meaningful.
	if !m.lastSent.IsZero() && now.Sub(m.lastSent) > autoIdleReset {
		if m.sentN > 0 {
			m.emitProbe(now, emit)
		}
		m.sentHead, m.sentN = 0, 0
		m.nextEmit = time.Time{}
	}
	// Expire the previous interval before taking this packet, ensuring IDs
	// are never sampled twice.
	if !m.nextEmit.IsZero() && now.After(m.nextEmit) && m.sentN > 0 {
		m.emitProbe(now, emit)
		m.sentHead, m.sentN = 0, 0
	}
	if m.nextEmit.IsZero() || now.After(m.nextEmit) {
		m.nextEmit = now.Add(autoProbeInterval)
	}
	m.appendSent(id)
	m.lastSent = now
}

func (m *autoMeter) appendSent(id uint64) {
	if m.sentN == autoProbeMaxIDs {
		m.sentIDs[m.sentHead] = id
		m.sentHead = (m.sentHead + 1) % autoProbeMaxIDs
		return
	}
	idx := (m.sentHead + m.sentN) % autoProbeMaxIDs
	m.sentIDs[idx] = id
	m.sentN++
}

func (m *autoMeter) emitProbe(now time.Time, emit func([]byte)) {
	if m.sentN == 0 {
		return
	}
	n := m.sentN
	if n > autoProbeMaxIDs {
		n = autoProbeMaxIDs
	}
	out := m.probeBuf[:5+n*8]
	binary.BigEndian.PutUint32(out[0:4], clock(now))
	out[4] = byte(n)
	for i := range n {
		idx := (m.sentHead + i) % autoProbeMaxIDs
		binary.BigEndian.PutUint64(out[5+i*8:5+(i+1)*8], m.sentIDs[idx])
	}
	emit(out)
}

// received remembers a tiny fingerprint for an eligible pre-repair packet and
// updates the observed receive rate and serialization headroom.
func (m *autoMeter) received(data []byte, now time.Time) {
	id, _, ok := packetIdentity(data)
	if !ok {
		return
	}
	if m.seen == nil {
		m.seen = make(map[uint64]struct{}, autoReceiveWindow)
	}
	if _, ok := m.seen[id]; !ok {
		if m.seenN == autoReceiveWindow {
			delete(m.seen, m.seenRing[m.seenHead])
		} else {
			m.seenN++
		}
		m.seen[id] = struct{}{}
		m.seenRing[m.seenHead] = id
		m.seenHead = (m.seenHead + 1) % autoReceiveWindow
	}
	size := len(data)
	m.rx.add(now, size)
	m.train.record(now, size)
}

// probe matches a compact probe list from the peer against the local receive
// window and returns loss/rate/queue evidence. The bool is false if the
// payload is malformed.
func (m *autoMeter) probe(payload []byte, now time.Time) (autoEvidence, bool) {
	if len(payload) < 5 {
		return autoEvidence{}, false
	}
	count := int(payload[4])
	if count > autoProbeMaxIDs || len(payload) != 5+count*8 {
		return autoEvidence{}, false
	}
	var ev autoEvidence
	ev.stamp = binary.BigEndian.Uint32(payload[0:4])
	ev.total = count
	for i := range count {
		id := binary.BigEndian.Uint64(payload[5+i*8:])
		if _, found := m.seen[id]; !found {
			ev.missing++
		}
	}
	ev.rate = m.rx.current(now)
	ev.headroom = m.train.available(now)

	delta := clock(now) - ev.stamp
	aboveBase := int32(delta - m.minDelta)
	if !m.minDeltaSet || aboveBase < 0 {
		m.minDelta = delta
		m.minDeltaSet = true
		ev.queue = 0
	} else {
		ev.queue = time.Duration(aboveBase) * time.Millisecond
	}
	return ev, true
}

// packetIdentity returns a compact fingerprint of an eligible IPv4/TCP data
// packet. It hashes only the IP addresses, TCP ports, TCP sequence/ack numbers
// and TCP payload length; variable fields such as TTL, IP/TCP checksums and
// flags are intentionally excluded. The original packet is never copied.
func packetIdentity(p []byte) (id uint64, size int, ok bool) {
	if !eligibleReplica(p) {
		return 0, 0, false
	}
	ihl := int(p[0]&0x0f) * 4
	ipTotal := int(binary.BigEndian.Uint16(p[2:4]))
	tcpOff := int(p[ihl+12]>>4) * 4

	srcIP := binary.BigEndian.Uint32(p[12:16])
	dstIP := binary.BigEndian.Uint32(p[16:20])
	srcPort := binary.BigEndian.Uint16(p[ihl : ihl+2])
	dstPort := binary.BigEndian.Uint16(p[ihl+2 : ihl+4])
	seq := binary.BigEndian.Uint32(p[ihl+4 : ihl+8])
	ack := binary.BigEndian.Uint32(p[ihl+8 : ihl+12])
	payloadLen := ipTotal - ihl - tcpOff

	return hashIdentity(srcIP, dstIP, srcPort, dstPort, seq, ack, payloadLen), len(p), true
}

// hashIdentity is a tiny, allocation-free 64-bit fingerprint. It uses a few
// word mixes rather than a per-byte hash; it is measurement-only, not secure.
func hashIdentity(srcIP, dstIP uint32, srcPort, dstPort uint16, seq, ack uint32, payloadLen int) uint64 {
	x := uint64(srcIP)<<32 | uint64(dstIP)
	x ^= uint64(srcPort)<<16 | uint64(dstPort)
	x = x<<32 | x>>32
	x += uint64(seq)
	x ^= uint64(ack)
	x = mix64(x)
	x ^= uint64(payloadLen)
	return mix64(x)
}

func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}
