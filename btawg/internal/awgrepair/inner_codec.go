package awgrepair

import (
	"crypto/subtle"
	"encoding/binary"
)

// Inner-control recovery payload layout.
//
// Every piece carries the 32-bit group serial and stamp so out-of-order
// fragments can be reassembled without session state per fragment.
//
// Descriptor piece (kind=0):
//
//	group[4] || stamp[4] || kind[1] || flags[1] || count[1] || lengthXOR[2] || ids[8*count] [|| total[1] [|| packed tail]]
//	flags: bit 0 = parity expected; bit 1 = parity tail packed into descriptor.
//	If parity is expected, a total-parity-chunks byte follows the IDs.
//	If the packed flag is set, the final parity chunk follows that byte.
//
// Parity piece (kind=1):
//
//	group[4] || stamp[4] || kind[1] || index[1] || total[1] || parity bytes
//	Non-final pieces are exactly parityChunk bytes; the final piece is unpadded.
//	When the descriptor carries the packed tail, separate pieces are only the
//	leading full chunks (indices 0..total-2).
const (
	maxPiece        = 1180
	descKind        = byte(0)
	parityKind      = byte(1)
	descHeaderLen   = 4 + 4 + 1 + 1 + 1 + 2 // 13
	parityHeaderLen = 4 + 4 + 1 + 1 + 1     // 11
	parityChunk     = maxPiece - parityHeaderLen

	descFlagParity = 1 << 0
	descFlagPacked = 1 << 1

	stateWindow = 16
	stateCount  = 32
)

// decoder reassembles inner-control recovery payloads, reports loss once per
// complete group, and reconstructs a single missing original when parity data
// is available. Memory is bounded to a fixed ring of group states and recent
// originals copies.
type decoder struct {
	current uint32
	states  [stateCount]groupState
	pending map[uint64]pendingRef
}

type pendingRef struct {
	st    *groupState
	group uint32
}

type groupState struct {
	group      uint32
	used       bool
	stamp      uint32
	descriptor bool
	hasParity  bool
	pending    bool
	count      int
	lengthXOR  uint16
	ids        [blockSize]uint64
	parity     [maxOriginal]byte
	pieceLen   [maxParityChunks]int
	got        uint8
	total      int
	measured   bool
	recovered  bool
}

const maxParityChunks = (maxOriginal + parityChunk - 1) / parityChunk

// state returns the ring slot for group, advancing or resetting the session
// window as needed. It returns nil for groups too far behind the current one.
func (d *decoder) state(group uint32) *groupState {
	delta := int32(group - d.current)
	if delta > stateWindow {
		d.reset(group)
		return &d.states[group%stateCount]
	}
	if delta < -stateWindow {
		return nil
	}
	if delta > 0 {
		target := d.current + uint32(delta)
		for d.current != target {
			d.current++
			slot := &d.states[d.current%stateCount]
			if slot.pending {
				d.removePending(slot)
			}
			d.states[d.current%stateCount] = groupState{}
		}
	}
	s := &d.states[group%stateCount]
	if !s.used {
		*s = groupState{used: true, group: group}
	}
	return s
}

func (d *decoder) reset(group uint32) {
	for i := range d.states {
		d.states[i] = groupState{}
	}
	d.current = group
	d.states[group%stateCount] = groupState{used: true, group: group}
	d.pending = nil
}

// receive processes one inner-control payload. It returns a recovered original
// (when exactly one member of the group is missing and parity is complete),
// the number of missing members for the group at measurement time, a flag that
// is true the first time the group is observed complete, and the remote stamp
// from the payload. A group is complete when its descriptor is present and,
// if parity was requested, all parity pieces have arrived. Out-of-order and
// duplicate fragments are accepted; malformed or truncated payloads are
// ignored.
//
// out may alias payload; the decoder copies what it needs from payload before
// writing to out, so a wrapper may reuse the incoming fragment buffer as the
// repair scratch if it wishes.
func (d *decoder) receive(payload []byte, recent *originals, out []byte) (recovered []byte, missing int, measured bool, stamp uint32) {
	if len(payload) < 9 {
		return nil, 0, false, 0
	}
	group := binary.BigEndian.Uint32(payload[0:4])
	stamp = binary.BigEndian.Uint32(payload[4:8])
	kind := payload[8]

	st := d.state(group)
	if st == nil {
		return nil, 0, false, 0
	}
	if st.recovered || st.measured || (kind == descKind && st.descriptor) {
		return nil, 0, false, 0
	}

	ok := false
	switch kind {
	case descKind:
		ok = d.applyDescriptor(st, payload)
	case parityKind:
		ok = d.applyParity(st, payload)
	default:
		return nil, 0, false, 0
	}
	if !ok {
		return nil, 0, false, 0
	}

	complete := st.descriptor && (!st.hasParity || (st.total > 0 && st.got == (uint8(1)<<st.total)-1))
	if complete && !st.measured {
		st.measured = true
		measured = true
		missing = d.countMissing(st, recent)
	}

	if complete && st.hasParity {
		recovered, _ = d.finishGroup(st, recent, out)
	}
	return
}

// late is called after the caller has inserted a just-arrived original into
// recent. If that original was the second-to-last missing member of a pending
// group, the now-single missing original is reconstructed and returned.
// Returns nil when there is no pending group or the original is not one of the
// missing IDs. One recovered original is delivered at most once.
func (d *decoder) late(original []byte, recent *originals, out []byte) []byte {
	if len(d.pending) == 0 {
		return nil
	}
	id := packetID(original)
	ref, ok := d.pending[id]
	if !ok {
		return nil
	}
	st := ref.st
	if st == nil || !st.used || st.group != ref.group || st.recovered || !st.pending {
		delete(d.pending, id)
		return nil
	}
	rec, _ := d.finishGroup(st, recent, out)
	return rec
}

// finishGroup tries to reconstruct a single missing original for a complete
// parity group. If two or more originals are still missing, the group is kept
// pending until one of them arrives via late(). It returns the recovered
// packet (if any) and the current missing count.
func (d *decoder) finishGroup(st *groupState, recent *originals, out []byte) ([]byte, int) {
	if st.recovered || !st.hasParity {
		return nil, -1
	}
	rec, missing := d.tryRecover(st, recent, out)
	if rec != nil {
		st.recovered = true
		st.pending = false
		d.removePending(st)
		return rec, missing
	}
	if missing == 0 {
		st.recovered = true
		st.pending = false
		d.removePending(st)
		return nil, 0
	}
	if missing > 1 {
		st.pending = true
		d.addPending(st, recent)
		return nil, missing
	}
	// Exactly one missing but reconstruction failed (corrupted parity); give up.
	st.recovered = true
	st.pending = false
	d.removePending(st)
	return nil, missing
}

func (d *decoder) addPending(st *groupState, recent *originals) {
	if d.pending == nil {
		d.pending = make(map[uint64]pendingRef)
	}
	for i := range st.count {
		if recent.find(st.ids[i]) == nil {
			d.pending[st.ids[i]] = pendingRef{st: st, group: st.group}
		}
	}
}

func (d *decoder) removePending(st *groupState) {
	for id, ref := range d.pending {
		if ref.st == st {
			delete(d.pending, id)
		}
	}
}

func (d *decoder) applyDescriptor(st *groupState, payload []byte) bool {
	if len(payload) < descHeaderLen+8 {
		return false
	}
	count := int(payload[10])
	flags := payload[9]
	hasParity := flags&descFlagParity != 0
	packed := flags&descFlagPacked != 0
	if count != blockSize || flags&^byte(descFlagParity|descFlagPacked) != 0 || (packed && !hasParity) {
		return false
	}

	baseLen := descHeaderLen + count*8
	var wantLen int
	switch {
	case !hasParity:
		wantLen = baseLen
	case packed:
		wantLen = baseLen + 1 + 1
	default:
		wantLen = baseLen + 1
	}
	if len(payload) < wantLen {
		return false
	}
	if !hasParity && len(payload) != baseLen {
		return false
	}
	if !packed && hasParity && len(payload) != baseLen+1 {
		return false
	}

	// Decode everything before mutating state.
	stamp := binary.BigEndian.Uint32(payload[4:8])
	lengthXOR := binary.BigEndian.Uint16(payload[11:13])
	var ids [blockSize]uint64
	off := descHeaderLen
	for i := range count {
		ids[i] = binary.BigEndian.Uint64(payload[off : off+8])
		off += 8
	}

	var total int
	var tail []byte
	if hasParity {
		total = int(payload[off])
		off++
		if total < 1 || total > maxParityChunks {
			return false
		}
		if packed {
			tail = payload[off:]
			if len(tail) == 0 || len(tail) > parityChunk {
				return false
			}
			idx := total - 1
			if idx*parityChunk+len(tail) > maxOriginal {
				return false
			}
		}
	}

	if st.total != 0 && st.total != total {
		return false
	}
	st.descriptor = true
	st.stamp = stamp
	st.hasParity = hasParity
	st.count = count
	st.lengthXOR = lengthXOR
	st.ids = ids
	if !hasParity {
		return true
	}
	st.total = total
	if !packed {
		return true
	}
	idx := total - 1
	off = idx * parityChunk
	copy(st.parity[off:], tail)
	st.pieceLen[idx] = len(tail)
	st.got |= 1 << idx
	return true
}

func (d *decoder) applyParity(st *groupState, payload []byte) bool {
	if len(payload) < parityHeaderLen+1 {
		return false
	}
	index := int(payload[9])
	total := int(payload[10])
	if total < 1 || total > maxParityChunks || index >= total {
		return false
	}
	data := payload[parityHeaderLen:]
	if len(data) > parityChunk {
		return false
	}
	if index < total-1 && len(data) != parityChunk {
		return false
	}
	if st.total != 0 && st.total != total {
		return false
	}
	if st.got&(1<<index) != 0 {
		return false
	}
	off := index * parityChunk
	if off+len(data) > maxOriginal {
		return false
	}
	st.total = total
	st.hasParity = true
	copy(st.parity[off:], data)
	st.pieceLen[index] = len(data)
	st.got |= 1 << index
	return true
}

func (d *decoder) countMissing(st *groupState, recent *originals) int {
	missing := 0
	for i := range st.count {
		if recent.find(st.ids[i]) == nil {
			missing++
		}
	}
	return missing
}

func (d *decoder) tryRecover(st *groupState, recent *originals, out []byte) ([]byte, int) {
	count := st.count
	if count == 0 {
		return nil, 0
	}

	var have [blockSize][]byte
	missing := 0
	var missingID uint64
	size := st.lengthXOR
	for i := range count {
		p := recent.find(st.ids[i])
		if p == nil {
			missing++
			missingID = st.ids[i]
			continue
		}
		have[i] = p
		size ^= uint16(len(p))
	}

	if missing != 1 || int(size) > maxOriginal || int(size) < minOriginal || int(size) > len(out) {
		return nil, missing
	}

	width := 0
	for i := range st.total {
		width += st.pieceLen[i]
	}
	if width < int(size) {
		return nil, missing
	}

	out = out[:size]
	copy(out, st.parity[:size])
	for _, p := range have {
		if p == nil {
			continue
		}
		n := min(len(p), len(out))
		subtle.XORBytes(out[:n], out[:n], p[:n])
	}
	if packetID(out) != missingID {
		return nil, missing
	}
	return out, missing
}
