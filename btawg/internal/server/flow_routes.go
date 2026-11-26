package server

import (
	"encoding/binary"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/net/ipv4"
)

// flowKey identifies one IPv4 transport flow. For TCP/UDP the ports are the
// L4 endpoints. For ICMP echo request/reply the second port stores the ICMP
// identifier and the first port stores type<<8|code. For other protocols both
// ports are zero. Fragmented packets are handled separately via fragmentKey.
type flowKey struct {
	src   netip.Addr
	dst   netip.Addr
	proto uint8
	a, b  uint16
}

func (k flowKey) reverse() flowKey {
	r := flowKey{src: k.dst, dst: k.src, proto: k.proto}
	if k.proto == 1 {
		// ICMP echo request (type 8) and reply (type 0) share the identifier.
		typ := uint8(k.a >> 8)
		code := uint8(k.a)
		if typ == 8 && code == 0 {
			r.a = uint16(0)<<8 | uint16(code)
			r.b = k.b
			return r
		}
		if typ == 0 && code == 0 {
			r.a = uint16(8)<<8 | uint16(code)
			r.b = k.b
			return r
		}
	}
	r.a, r.b = k.b, k.a
	return r
}

// fragmentKey identifies an IPv4 datagram that may be fragmented. All
// fragments with the same ID/src/dst/proto must be delivered to the engine
// that received the first fragment.
type fragmentKey struct {
	src   netip.Addr
	dst   netip.Addr
	proto uint8
	id    uint16
}

// flowTable pins reverse IPv4 flows to the authenticated engine that admitted
// the forward flow. It is consulted before the per-IP primary route so that
// active flows are not rewired when handshakes or route preferences change.
//
// Capacity semantics: the table holds at most maxEntries flows and maxEntries
// fragment IDs. A new flow evicts the least recently used flow at capacity;
// fragments are evicted in insertion order. Flow expiry is measured from
// the last authenticated pin, not from reads of the reverse route.
type flowTable struct {
	mu sync.RWMutex

	maxEntries int
	flowTTL    time.Duration

	flows          map[flowKey]*flowEntry
	frags          map[fragmentKey]*engineTun
	oldest, newest *flowEntry
	lruFrags       []fragmentKey
}

type flowEntry struct {
	key        flowKey
	eng        *engineTun
	at         time.Time
	prev, next *flowEntry
}

const (
	defaultFlowMaxEntries = 65536
	defaultFlowTTL        = 5 * time.Minute
)

func newFlowTable() *flowTable {
	return &flowTable{
		maxEntries: defaultFlowMaxEntries,
		flowTTL:    defaultFlowTTL,
		flows:      make(map[flowKey]*flowEntry),
		frags:      make(map[fragmentKey]*engineTun),
	}
}

// pin records that packets matching key (the reverse of an authenticated
// forward flow) belong to eng unless another live engine owns the tuple.
func (t *flowTable) pin(key flowKey, eng *engineTun, now time.Time) {
	if !key.src.IsValid() || !key.dst.IsValid() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	entry := t.flows[key]
	if entry != nil {
		if entry.eng != eng && now.Sub(entry.at) <= t.flowTTL {
			return
		}
	} else {
		if len(t.flows) >= t.maxEntries {
			entry = t.oldest
			t.removeFlowLocked(entry)
		} else {
			entry = new(flowEntry)
		}
		entry.key = key
		t.flows[key] = entry
	}
	entry.eng, entry.at = eng, now
	t.touchLocked(entry)
}

// lookup returns the engine pinned for key, or nil/false for a miss.
func (t *flowTable) lookup(key flowKey, now time.Time) (*engineTun, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.flows[key]
	if entry == nil {
		return nil, false
	}
	if now.Sub(entry.at) > t.flowTTL {
		t.removeFlowLocked(entry)
		return nil, false
	}
	t.touchLocked(entry)
	return entry.eng, true
}

// pinFragment records the destination engine chosen for the first fragment.
// The sender's engine is tracked separately by the reverse flow.
func (t *flowTable) pinFragment(key fragmentKey, eng *engineTun) {
	if !key.src.IsValid() || !key.dst.IsValid() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.frags[key]; !ok && len(t.frags) >= t.maxEntries {
		t.evictOneFragmentLocked()
	}
	if _, ok := t.frags[key]; !ok {
		t.lruFrags = append(t.lruFrags, key)
	}
	t.frags[key] = eng
}

// lookupFragment returns the engine for a fragment datagram, or nil/false.
func (t *flowTable) lookupFragment(key fragmentKey, now time.Time) (*engineTun, bool) {
	t.mu.RLock()
	eng, ok := t.frags[key]
	t.mu.RUnlock()
	return eng, ok
}

// removeForIP deletes every flow and fragment entry that references ip as
// either source or destination. Called on client revocation or address change.
func (t *flowTable) removeForIP(ip netip.Addr) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, entry := range t.flows {
		if k.src == ip || k.dst == ip {
			t.removeFlowLocked(entry)
		}
	}
	for k := range t.frags {
		if k.src == ip || k.dst == ip {
			delete(t.frags, k)
			t.removeFragmentFromLRULocked(k)
		}
	}
}

// removeForEngine deletes every entry owned by eng. Called when an engine
// closes so its flows can no longer be satisfied.
func (t *flowTable) removeForEngine(eng *engineTun) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.flows {
		if e.eng == eng {
			t.removeFlowLocked(e)
		}
	}
	for k, e := range t.frags {
		if e == eng {
			delete(t.frags, k)
			t.removeFragmentFromLRULocked(k)
		}
	}
}

// cleanup removes stale entries. It is safe to call concurrently with other
// operations.
func (t *flowTable) cleanup(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.flows {
		if now.Sub(e.at) > t.flowTTL {
			t.removeFlowLocked(e)
		}
	}
	// Fragment entries do not track time; they are evicted by capacity or
	// when the flow TTL is enforced lazily through the owning flow entry.
}

func (t *flowTable) touchLocked(e *flowEntry) {
	if e == t.newest {
		return
	}
	t.unlinkLocked(e)
	e.prev = t.newest
	if t.newest != nil {
		t.newest.next = e
	} else {
		t.oldest = e
	}
	t.newest = e
}

func (t *flowTable) unlinkLocked(e *flowEntry) {
	if e.prev != nil {
		e.prev.next = e.next
	} else if t.oldest == e {
		t.oldest = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else if t.newest == e {
		t.newest = e.prev
	}
	e.prev, e.next = nil, nil
}

func (t *flowTable) removeFlowLocked(e *flowEntry) {
	t.unlinkLocked(e)
	delete(t.flows, e.key)
}

func (t *flowTable) evictOneFragmentLocked() {
	for len(t.lruFrags) > 0 {
		k := t.lruFrags[0]
		t.lruFrags = t.lruFrags[1:]
		if _, ok := t.frags[k]; ok {
			delete(t.frags, k)
			return
		}
	}
}

func (t *flowTable) removeFragmentFromLRULocked(key fragmentKey) {
	nf := t.lruFrags[:0]
	for _, k := range t.lruFrags {
		if k != key {
			nf = append(nf, k)
		}
	}
	t.lruFrags = nf
}

// flowKeyFromPacket extracts a flow key from an IPv4 packet. It returns false
// for non-IPv4 packets or packets too short to carry L4 selectors. For
// fragmented packets other than the first fragment it returns false; callers
// should use fragmentKeyFromPacket instead.
func flowKeyFromPacket(p []byte) (flowKey, bool) {
	src, dst, proto, ok := ipv4Header(p)
	if !ok {
		return flowKey{}, false
	}
	if isFragment(p) {
		// Only the first fragment carries L4 ports; later fragments use the
		// fragment table keyed by IP ID.
		if fragmentOffset(p) != 0 {
			return flowKey{}, false
		}
	}
	off := ipv4HeaderLen(p)
	if len(p) < off {
		return flowKey{}, false
	}
	switch proto {
	case 6, 17: // TCP, UDP
		if len(p) < off+4 {
			return flowKey{}, false
		}
		sport := binary.BigEndian.Uint16(p[off : off+2])
		dport := binary.BigEndian.Uint16(p[off+2 : off+4])
		return flowKey{src: src, dst: dst, proto: proto, a: sport, b: dport}, true
	case 1: // ICMP
		if len(p) < off+4 {
			return flowKey{}, false
		}
		typ := p[off]
		code := p[off+1]
		var id uint16
		if typ == 0 || typ == 8 { // echo reply / echo request
			if len(p) < off+6 {
				return flowKey{}, false
			}
			id = binary.BigEndian.Uint16(p[off+4 : off+6])
		}
		return flowKey{src: src, dst: dst, proto: proto, a: uint16(typ)<<8 | uint16(code), b: id}, true
	default:
		return flowKey{src: src, dst: dst, proto: proto}, true
	}
}

// fragmentKeyFromPacket extracts the fragment identifier from an IPv4 packet.
func fragmentKeyFromPacket(p []byte) (fragmentKey, bool) {
	src, dst, proto, ok := ipv4Header(p)
	if !ok || !isFragment(p) {
		return fragmentKey{}, false
	}
	if len(p) < 4 {
		return fragmentKey{}, false
	}
	id := binary.BigEndian.Uint16(p[4:6])
	return fragmentKey{src: src, dst: dst, proto: proto, id: id}, true
}

func ipv4Header(p []byte) (src, dst netip.Addr, proto uint8, ok bool) {
	if len(p) < ipv4.HeaderLen {
		return
	}
	if p[0]>>4 != 4 {
		return
	}
	hlen := int(p[0]&0x0f) * 4
	if hlen < ipv4.HeaderLen || len(p) < hlen {
		return
	}
	var s, d [4]byte
	copy(s[:], p[12:16])
	copy(d[:], p[16:20])
	return netip.AddrFrom4(s), netip.AddrFrom4(d), p[9], true
}

func ipv4HeaderLen(p []byte) int {
	if len(p) < 1 || p[0]>>4 != 4 {
		return 0
	}
	return int(p[0]&0x0f) * 4
}

func isFragment(p []byte) bool {
	if len(p) < 8 {
		return false
	}
	// More Fragments bit or non-zero fragment offset.
	return p[6]&0x20 != 0 || (binary.BigEndian.Uint16(p[6:8])&0x1fff) != 0
}

func fragmentOffset(p []byte) uint16 {
	if len(p) < 8 {
		return 0
	}
	return binary.BigEndian.Uint16(p[6:8]) & 0x1fff
}

// ipv4HeaderLen is also used by router.go; keep in sync with the
// ipv4.HeaderLen constant imported there.
var _ = ipv4.HeaderLen // verify import is used
