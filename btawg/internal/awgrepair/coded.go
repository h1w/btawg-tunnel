package awgrepair

import (
	"bytes"
	"crypto/subtle"
	"encoding/binary"
)

const (
	codedGroupSize   = 8
	codedParityRows  = 2
	codedParityParts = 2

	codedParityHeaderLen = 4 + 1 + 1 + 1 + 1 // group, kind, row, part, total
	codedMetaLen         = codedParityHeaderLen + codedGroupSize*8 + codedGroupSize*2
	codedMaxHeadData     = innerMaxData - codedMetaLen
	codedMaxTailData     = innerMaxData - codedParityHeaderLen

	codedKindParity = byte(1)
)

// gfLog maps a non-zero GF(2^8) element to its discrete logarithm (base 3).
// gfExp maps an exponent to the corresponding field element. The table is
// extended to 512 entries so products can be looked up without a modulo.
var gfLog [255]byte
var gfExp [512]byte

func init() {
	const poly = 0x11d // x^8 + x^4 + x^3 + x^2 + 1
	x := 1
	for i := 0; i < 255; i++ {
		gfExp[i] = byte(x)
		gfExp[i+255] = byte(x)
		gfLog[x-1] = byte(i)
		x <<= 1
		if x&0x100 != 0 {
			x ^= poly
		}
	}
	gfExp[510] = gfExp[0]
}

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[int(gfLog[a-1])+int(gfLog[b-1])]
}

func gfDiv(a, b byte) byte {
	if a == 0 {
		return 0
	}
	return gfExp[int(gfLog[a-1])-int(gfLog[b-1])+255]
}

// weight[row][source] is the Reed-Solomon coefficient for the given parity row.
// Row 0 is the XOR row (weight 1). Row 1 is weighted by consecutive powers of
// the primitive element so that the two rows are linearly independent.
var weight [codedParityRows][codedGroupSize]byte

// mulTab[source][byte] precomputes gfMul(weight[1][source], byte) for the hot
// weighted encoder path.
var mulTab [codedGroupSize][256]byte

func init() {
	for r := 0; r < codedParityRows; r++ {
		for i := 0; i < codedGroupSize; i++ {
			weight[r][i] = gfExp[(r*i)%255]
		}
	}
	for i := 0; i < codedGroupSize; i++ {
		w := weight[1][i]
		for b := 0; b < 256; b++ {
			mulTab[i][b] = gfMul(w, byte(b))
		}
	}
}

// codedTX groups eligible original IP packets and emits parity rows. Each row’s
// first piece carries the full source IDs/lengths metadata, so there is no
// separate descriptor packet. The originals themselves are forwarded by the
// caller and are not emitted here.
type codedTX struct {
	nextGroup uint32
	count     int
	width     int
	ids       [codedGroupSize]uint64
	lens      [codedGroupSize]uint16
	parity    [codedParityRows][maxOriginal]byte
	emitBuf   [innerMaxData]byte
}

// add feeds one original packet into the encoder. The original is accumulated
// into the current group; when the group reaches eight members the encoder
// emits the parity row pieces.
func (t *codedTX) add(p []byte, emit func([]byte) bool) {
	idx := t.count
	t.ids[idx] = packetID(p)
	t.lens[idx] = uint16(len(p))
	if len(p) > t.width {
		t.width = len(p)
	}
	if len(p) > 0 {
		subtle.XORBytes(t.parity[0][:len(p)], t.parity[0][:len(p)], p)
	}
	row1 := t.parity[1][:]
	mt := mulTab[idx]
	for j, b := range p {
		row1[j] ^= mt[b]
	}
	t.count++

	if t.count < codedGroupSize {
		return
	}

	t.emitGroup(emit)
	t.reset()
	t.nextGroup++
}

func (t *codedTX) emitGroup(emit func([]byte) bool) {
	for r := 0; r < codedParityRows; r++ {
		data := t.parity[r][:t.width]
		total := 1
		part0Len := t.width
		if t.width > codedMaxHeadData {
			total = 2
			part0Len = codedMaxHeadData
		}
		if !emit(t.headPiece(t.emitBuf[:0], t.nextGroup, byte(r), byte(total), data[:part0Len])) {
			return
		}
		if total == 2 {
			if !emit(t.tailPiece(t.emitBuf[:0], t.nextGroup, byte(r), byte(total), data[part0Len:])) {
				return
			}
		}
	}
}

func (t *codedTX) headPiece(out []byte, group uint32, row, total byte, data []byte) []byte {
	out = binary.BigEndian.AppendUint32(out, group)
	out = append(out, codedKindParity, row, 0, total)
	for i := 0; i < codedGroupSize; i++ {
		out = binary.BigEndian.AppendUint64(out, t.ids[i])
	}
	for i := 0; i < codedGroupSize; i++ {
		out = binary.BigEndian.AppendUint16(out, t.lens[i])
	}
	out = append(out, data...)
	return out
}

func (t *codedTX) tailPiece(out []byte, group uint32, row, total byte, data []byte) []byte {
	out = binary.BigEndian.AppendUint32(out, group)
	out = append(out, codedKindParity, row, 1, total)
	out = append(out, data...)
	return out
}

func (t *codedTX) reset() {
	for r := 0; r < codedParityRows; r++ {
		clear(t.parity[r][:t.width])
	}
	t.count = 0
	t.width = 0
}

// codedGroupState holds recovery state for one group.
type codedGroupState struct {
	used       bool
	group      uint32
	descriptor bool
	done       bool
	width      int
	ids        [codedGroupSize]uint64
	lens       [codedGroupSize]uint16

	// pieces[r][part] stores received parity pieces before the descriptor is known.
	pieces   [codedParityRows][codedParityParts][maxOriginal]byte
	pieceLen [codedParityRows][codedParityParts]uint16
	gotMask  [codedParityRows]uint8

	delivered uint8 // bit mask of source indices already delivered
}

func (s *codedGroupState) reset() {
	s.used = false
	s.descriptor = false
	s.done = false
	s.width = 0
	s.ids = [codedGroupSize]uint64{}
	s.lens = [codedGroupSize]uint16{}
	s.pieceLen = [codedParityRows][codedParityParts]uint16{}
	s.gotMask = [codedParityRows]uint8{}
	s.delivered = 0
}

// codedPacketCache stores recently received originals. It uses fixed-size inline
// buffers so the steady-state receive path makes no per-packet allocations.
type codedPacketCache struct {
	entries [256]struct {
		id   uint64
		len  uint16
		data [maxOriginal]byte
	}
	next  int
	index map[uint64]int
}

func (c *codedPacketCache) remember(p []byte) {
	if len(p) > maxOriginal {
		return
	}
	id := packetID(p)
	if c.index == nil {
		c.index = make(map[uint64]int, len(c.entries)*2)
	}
	if idx, ok := c.index[id]; ok {
		e := &c.entries[idx]
		if e.id == id && int(e.len) == len(p) && bytes.Equal(e.data[:e.len], p) {
			// Exact duplicate: leave it in place; FIFO need not advance.
			return
		}
	}

	pos := c.next
	c.next = (c.next + 1) % len(c.entries)
	old := &c.entries[pos]
	if old.id != 0 {
		// Only delete the index entry if it still points to this slot.
		if c.index[old.id] == pos {
			delete(c.index, old.id)
		}
	}
	old.id, old.len = id, uint16(len(p))
	copy(old.data[:], p)
	c.index[id] = pos
}

func (c *codedPacketCache) find(id uint64) []byte {
	idx, ok := c.index[id]
	if !ok {
		return nil
	}
	e := &c.entries[idx]
	if e.id != id || e.len == 0 {
		delete(c.index, id)
		return nil
	}
	return e.data[:e.len]
}

const (
	codedStateWindow = 16
	codedStateCount  = 32
)

// codedRX reconstructs missing originals from parity pieces. Each row's first
// piece carries the source IDs/lengths, so recovery can proceed as soon as any
// row's head has arrived.
type codedRX struct {
	current      uint32
	states       [codedStateCount]codedGroupState
	pending      uint32 // bit mask of state indices that may still recover
	intactGroups uint32 // groups completed entirely from received originals
	cache        codedPacketCache
	scratch      [codedParityRows][maxOriginal]byte
	outScratch   [2][maxOriginal]byte
}

// original receives an unmodified original packet. It is cached for use by the
// erasure decoder; deliver is called only when a missing packet is reconstructed.
func (r *codedRX) original(p []byte, deliver func([]byte)) {
	if len(p) == 0 || len(p) > maxOriginal {
		return
	}
	r.cache.remember(p)
	if r.pending == 0 {
		return
	}
	for i := range codedStateCount {
		if r.pending&(1<<i) == 0 {
			continue
		}
		st := &r.states[i]
		if st.used && st.descriptor && !st.done {
			r.attemptRecover(st, deliver)
		}
	}
}

// repair receives a parity payload (head or tail). deliver is called once for
// each successfully reconstructed original.
func (r *codedRX) repair(payload []byte, deliver func([]byte)) {
	if len(payload) < codedParityHeaderLen+1 {
		return
	}
	group := binary.BigEndian.Uint32(payload[0:4])
	st := r.state(group)
	if st == nil || st.done {
		return
	}
	if r.applyParity(st, payload) {
		r.attemptRecover(st, deliver)
	}
}

func (r *codedRX) state(group uint32) *codedGroupState {
	delta := int32(group - r.current)
	if delta > codedStateWindow {
		r.reset(group)
		return &r.states[group%codedStateCount]
	}
	if delta < -codedStateWindow {
		return nil
	}
	if delta > 0 {
		target := r.current + uint32(delta)
		for r.current != target {
			r.current++
			idx := r.current % codedStateCount
			r.pending &^= 1 << idx
			r.states[idx].reset()
		}
	}
	s := &r.states[group%codedStateCount]
	if !s.used {
		s.used = true
		s.group = group
	}
	return s
}

func (r *codedRX) reset(group uint32) {
	for i := range r.states {
		r.states[i].reset()
	}
	r.current = group
	r.pending = 0
	s := &r.states[group%codedStateCount]
	s.used = true
	s.group = group
}

func (r *codedRX) setPending(st *codedGroupState) {
	r.pending |= 1 << (st.group % codedStateCount)
}

func (r *codedRX) clearPending(st *codedGroupState) {
	r.pending &^= 1 << (st.group % codedStateCount)
}

func (r *codedRX) applyParity(st *codedGroupState, payload []byte) bool {
	if len(payload) < codedParityHeaderLen+1 {
		return false
	}
	if payload[4] != codedKindParity {
		return false
	}
	row := int(payload[5])
	part := int(payload[6])
	total := int(payload[7])
	if row < 0 || row >= codedParityRows {
		return false
	}
	if total < 1 || total > codedParityParts || part < 0 || part >= total {
		return false
	}

	if part == 0 {
		return r.applyHead(st, payload)
	}
	return r.applyTail(st, payload)
}

func (r *codedRX) applyHead(st *codedGroupState, payload []byte) bool {
	if len(payload) < codedMetaLen+1 {
		return false
	}
	row := int(payload[5])
	data := payload[codedMetaLen:]
	if len(data) > codedMaxHeadData {
		return false
	}

	off := codedParityHeaderLen
	var ids [codedGroupSize]uint64
	for i := 0; i < codedGroupSize; i++ {
		ids[i] = binary.BigEndian.Uint64(payload[off : off+8])
		off += 8
	}
	var lens [codedGroupSize]uint16
	width := 0
	for i := 0; i < codedGroupSize; i++ {
		l := binary.BigEndian.Uint16(payload[off : off+2])
		if l == 0 || int(l) > maxOriginal {
			return false
		}
		lens[i] = l
		if int(l) > width {
			width = int(l)
		}
		off += 2
	}
	total := int(payload[7])
	wantTotal := 1
	part0Len := width
	if width > codedMaxHeadData {
		wantTotal = 2
		part0Len = codedMaxHeadData
	}
	if total != wantTotal || len(data) != part0Len {
		return false
	}

	if st.descriptor {
		if st.width != width {
			return false
		}
		for i := 0; i < codedGroupSize; i++ {
			if st.ids[i] != ids[i] || st.lens[i] != lens[i] {
				return false
			}
		}
	} else {
		st.descriptor = true
		st.width = width
		st.ids = ids
		st.lens = lens
	}

	if st.gotMask[row]&1 != 0 {
		return false
	}
	st.gotMask[row] |= 1
	copy(st.pieces[row][0][:], data)
	st.pieceLen[row][0] = uint16(len(data))
	return true
}

func (r *codedRX) applyTail(st *codedGroupState, payload []byte) bool {
	row := int(payload[5])
	data := payload[codedParityHeaderLen:]
	if len(data) > codedMaxTailData {
		return false
	}

	if st.descriptor {
		want := r.expectedTailLen(st.width)
		if want < 0 || len(data) != want {
			return false
		}
	}

	if st.gotMask[row]&(1<<1) != 0 {
		return false
	}
	st.gotMask[row] |= 1 << 1
	copy(st.pieces[row][1][:], data)
	st.pieceLen[row][1] = uint16(len(data))
	return true
}

func (r *codedRX) expectedTailLen(width int) int {
	if width <= codedMaxHeadData {
		return -1
	}
	return width - codedMaxHeadData
}

func (r *codedRX) attemptRecover(st *codedGroupState, deliver func([]byte)) {
	if !st.descriptor || st.done {
		return
	}

	var missingIdx [codedGroupSize]int
	var haveIdx [codedGroupSize]int
	var havePtr [codedGroupSize][]byte
	missing := 0
	have := 0
	for i := 0; i < codedGroupSize; i++ {
		if st.delivered&(1<<i) != 0 {
			continue
		}
		if p := r.cache.find(st.ids[i]); p == nil {
			missingIdx[missing] = i
			missing++
		} else {
			haveIdx[have] = i
			havePtr[have] = p
			have++
		}
	}
	if missing == 0 {
		r.intactGroups++
		st.done = true
		r.clearPending(st)
		return
	}

	completeRows := 0
	var rowIdx [codedParityRows]int
	for row := 0; row < codedParityRows; row++ {
		total := 1
		if st.width > codedMaxHeadData {
			total = 2
		}
		if st.gotMask[row] == (1<<total)-1 {
			rowIdx[completeRows] = row
			completeRows++
		}
	}
	if missing > completeRows {
		r.setPending(st)
		return
	}

	width := st.width

	// Assemble every complete parity row and subtract known originals once.
	for _, ri := range rowIdx[:completeRows] {
		copy(r.scratch[ri][:width], r.assembledRow(st, ri)[:width])
	}
	for h := 0; h < have; h++ {
		p := havePtr[h]
		si := haveIdx[h]
		for _, ri := range rowIdx[:completeRows] {
			w := weight[ri][si]
			row := r.scratch[ri][:]
			for j, b := range p {
				row[j] ^= gfMul(w, b)
			}
		}
	}

	switch missing {
	case 1:
		si := missingIdx[0]
		wantID := st.ids[si]
		wantLen := int(st.lens[si])
		for _, ri := range rowIdx[:completeRows] {
			out := r.outScratch[0][:wantLen]
			w := weight[ri][si]
			b := r.scratch[ri][:]
			if w == 1 {
				copy(out, b[:wantLen])
			} else {
				for j := range out {
					out[j] = gfDiv(b[j], w)
				}
			}
			if packetID(out) == wantID {
				st.delivered |= 1 << si
				st.done = true
				deliver(out)
				r.clearPending(st)
				return
			}
		}
		// No complete row verified; stay pending.
		r.setPending(st)

	case 2:
		m0 := missingIdx[0]
		m1 := missingIdx[1]
		wm := weight[1][m0]
		wn := weight[1][m1]
		det := wm ^ wn
		if det == 0 {
			r.setPending(st)
			return
		}
		b0 := r.scratch[0][:width]
		b1 := r.scratch[1][:width]
		out0 := r.outScratch[0][:st.lens[m0]]
		out1 := r.outScratch[1][:st.lens[m1]]
		for j := 0; j < width; j++ {
			v0 := gfMul(wn, b0[j]) ^ b1[j]
			v1 := gfMul(wm, b0[j]) ^ b1[j]
			rec0 := gfDiv(v0, det)
			rec1 := gfDiv(v1, det)
			if j < len(out0) {
				out0[j] = rec0
			}
			if j < len(out1) {
				out1[j] = rec1
			}
		}
		if packetID(out0) != st.ids[m0] || packetID(out1) != st.ids[m1] {
			r.setPending(st)
			return
		}
		st.delivered |= 1 << m0
		st.delivered |= 1 << m1
		st.done = true
		deliver(out0)
		deliver(out1)
		r.clearPending(st)
	}
}

func (r *codedRX) assembledRow(st *codedGroupState, row int) []byte {
	if st.width <= codedMaxHeadData {
		return st.pieces[row][0][:st.pieceLen[row][0]]
	}
	out := r.scratch[row][:st.width]
	copy(out[:codedMaxHeadData], st.pieces[row][0][:st.pieceLen[row][0]])
	copy(out[codedMaxHeadData:], st.pieces[row][1][:st.pieceLen[row][1]])
	return out
}
