// Package awgrepair carries recovery messages as inner IPv4 packets through
// each enrolled AWG engine. It never opens or wraps an underlay socket.
package awgrepair

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
)

const (
	blockSize   = 8
	maxOriginal = 1280
	minOriginal = 128
	cacheSize   = 128
)

func packetID(p []byte) uint64 {
	h := sha256.Sum256(p)
	return binary.BigEndian.Uint64(h[:8])
}

// originals owns copies: the AWG receive path edits and recycles its buffers.
type originals struct {
	entries [cacheSize]struct {
		id   uint64
		len  uint16
		data [maxOriginal]byte
	}
	next int
}

func (c *originals) remember(p []byte) {
	if len(p) < minOriginal || len(p) > maxOriginal {
		return
	}
	id := packetID(p)
	e := &c.entries[c.next]
	c.next = (c.next + 1) % cacheSize
	e.id, e.len = id, uint16(len(p))
	copy(e.data[:], p)
}

func (c *originals) find(id uint64) []byte {
	for i := range c.entries {
		e := &c.entries[i]
		if e.id == id && e.len != 0 {
			return e.data[:e.len]
		}
	}
	return nil
}

type block struct {
	ids       [blockSize]uint64
	count     int
	length    uint16
	width     int
	parity    [maxOriginal]byte
	protected bool
}

func (b *block) add(p []byte) {
	b.ids[b.count] = packetID(p)
	b.count++
	b.length ^= uint16(len(p))
	b.width = max(b.width, len(p))
	if b.protected {
		subtle.XORBytes(b.parity[:len(p)], b.parity[:len(p)], p)
	}
}

func (b *block) reset() {
	if b.protected {
		clear(b.parity[:b.width])
	}
	b.count, b.length, b.width, b.protected = 0, 0, 0, false
}

// parts emits the compact inner-control payloads for this block.
// The parent handles handshake/session/auth/IPv4 framing; this method only
// produces the payload bytes. When parity is false a single descriptor-only
// chunk is emitted. When parity is true the encoder packs the final (short)
// parity chunk into the descriptor whenever descriptor+tail fits in maxPiece,
// so full-MTU blocks send one leading parity piece plus one descriptor-with-tail
// piece. If all parity fits in the descriptor, only one piece is emitted. The
// emit callback receives each piece; if it returns false parts stops and returns
// false. No piece exceeds maxPiece bytes and the final parity chunk is never
// padded.
func (b *block) parts(group uint32, stamp uint32, parity bool, emit func([]byte) bool) bool {
	var buf [maxPiece]byte
	desc := buf[:0]
	desc = binary.BigEndian.AppendUint32(desc, group)
	desc = binary.BigEndian.AppendUint32(desc, stamp)
	desc = append(desc, descKind)
	flags := byte(0)
	if parity {
		flags |= descFlagParity
	}
	desc = append(desc, flags)
	desc = append(desc, byte(b.count))
	desc = binary.BigEndian.AppendUint16(desc, b.length)
	for i := range b.count {
		desc = binary.BigEndian.AppendUint64(desc, b.ids[i])
	}
	if !parity || b.width == 0 {
		return emit(desc)
	}

	data := b.parity[:b.width]
	chunks := (len(data) + parityChunk - 1) / parityChunk
	desc = append(desc, byte(chunks))

	tailStart := (chunks - 1) * parityChunk
	tailLen := len(data) - tailStart
	if len(desc)+tailLen <= maxPiece {
		// Pack the tail into the descriptor.
		desc[9] |= descFlagPacked
		desc = append(desc, data[tailStart:]...)
		if !emit(desc) {
			return false
		}
		// Emit any leading full chunks as standalone parity pieces.
		for i := range chunks - 1 {
			off := i * parityChunk
			piece := buf[:0]
			piece = binary.BigEndian.AppendUint32(piece, group)
			piece = binary.BigEndian.AppendUint32(piece, stamp)
			piece = append(piece, parityKind)
			piece = append(piece, byte(i))
			piece = append(piece, byte(chunks))
			piece = append(piece, data[off:off+parityChunk]...)
			if !emit(piece) {
				return false
			}
		}
		return true
	}

	// Fallback: descriptor carries the chunk count but no tail; emit all parity
	// pieces separately.
	if !emit(desc) {
		return false
	}
	for i := range chunks {
		off := i * parityChunk
		end := min(off+parityChunk, len(data))
		piece := buf[:0]
		piece = binary.BigEndian.AppendUint32(piece, group)
		piece = binary.BigEndian.AppendUint32(piece, stamp)
		piece = append(piece, parityKind)
		piece = append(piece, byte(i))
		piece = append(piece, byte(chunks))
		piece = append(piece, data[off:end]...)
		if !emit(piece) {
			return false
		}
	}
	return true
}

// repairBudget returns the number of inner IPv4 pieces and the sum of their
// exact emitted total lengths (authenticated inner IPv4 packet = payload +
// innerMTU-innerMaxData) that this block would produce. The caller adds its
// estimated outer framing overhead per piece.
func (b *block) repairBudget() (pieces, innerBytes int) {
	innerOverhead := innerMTU - innerMaxData
	descBase := descHeaderLen + b.count*8
	if !b.protected || b.width == 0 {
		return 1, descBase + innerOverhead
	}
	chunks := (b.width + parityChunk - 1) / parityChunk
	tailLen := b.width - (chunks-1)*parityChunk
	descParity := descBase + 1
	if descParity+tailLen <= maxPiece {
		pieces = chunks
		innerBytes = (chunks-1)*(parityHeaderLen+parityChunk) + descParity + tailLen
	} else {
		pieces = chunks + 1
		innerBytes = descParity + chunks*parityHeaderLen + b.width
	}
	innerBytes += pieces * innerOverhead
	return
}
