package awgrepair

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func makeBlock(packets [][]byte, protected bool) block {
	b := block{protected: protected}
	for _, p := range packets {
		b.add(p)
	}
	return b
}

func captureParts(b *block, group, stamp uint32, parity bool) [][]byte {
	var parts [][]byte
	b.parts(group, stamp, parity, func(p []byte) bool {
		parts = append(parts, append([]byte(nil), p...))
		return true
	})
	return parts
}

func TestInnerCodecDescriptorOnly(t *testing.T) {
	innerOverhead := innerMTU - innerMaxData
	packets := make([][]byte, blockSize)
	for i := range packets {
		packets[i] = bytes.Repeat([]byte{byte(i + 1)}, minOriginal+i*16)
	}
	b := makeBlock(packets, false)
	parts := captureParts(&b, 7, 1234, false)
	if len(parts) != 1 {
		t.Fatalf("expected 1 descriptor-only piece, got %d", len(parts))
	}
	wantLen := descHeaderLen + blockSize*8
	if len(parts[0]) != wantLen {
		t.Fatalf("descriptor length %d, want %d", len(parts[0]), wantLen)
	}
	pieces, innerBytes := b.repairBudget()
	if pieces != 1 || innerBytes != wantLen+innerOverhead {
		t.Fatalf("budget = (%d,%d), want (1,%d)", pieces, innerBytes, wantLen+innerOverhead)
	}

	var recent originals
	for _, p := range packets {
		recent.remember(p)
	}
	var d decoder
	var out [maxOriginal]byte
	rec, missing, measured, stamp := d.receive(parts[0], &recent, out[:])
	if !measured || missing != 0 || stamp != 1234 {
		t.Fatalf("complete group: measured=%v missing=%d stamp=%d", measured, missing, stamp)
	}
	if rec != nil {
		t.Fatalf("descriptor-only must not recover a packet")
	}
	_, missing2, measured2, _ := d.receive(parts[0], &recent, out[:])
	if measured2 || missing2 != 0 {
		t.Fatalf("duplicate descriptor re-measured")
	}
}

func TestInnerCodecSinglePieceParity(t *testing.T) {
	innerOverhead := innerMTU - innerMaxData
	packets := make([][]byte, blockSize)
	for i := range packets {
		packets[i] = bytes.Repeat([]byte{byte(i + 1)}, 200+i)
	}
	b := makeBlock(packets, true)
	parts := captureParts(&b, 1, 100, true)
	if len(parts) != 1 {
		t.Fatalf("expected 1 packed piece, got %d", len(parts))
	}
	wantLen := descHeaderLen + blockSize*8 + 1 + b.width
	if len(parts[0]) != wantLen {
		t.Fatalf("packed piece length %d, want %d", len(parts[0]), wantLen)
	}
	pieces, innerBytes := b.repairBudget()
	if pieces != 1 || innerBytes != wantLen+innerOverhead {
		t.Fatalf("budget = (%d,%d), want (1,%d)", pieces, innerBytes, wantLen+innerOverhead)
	}

	var recent originals
	for i, p := range packets {
		if i != 3 {
			recent.remember(p)
		}
	}
	var d decoder
	var out [maxOriginal]byte
	rec, missing, measured, stamp := d.receive(parts[0], &recent, out[:])
	if !measured || missing != 1 || stamp != 100 {
		t.Fatalf("complete repair: measured=%v missing=%d stamp=%d", measured, missing, stamp)
	}
	if !bytes.Equal(rec, packets[3]) {
		t.Fatalf("recovered wrong packet")
	}
}

func TestInnerCodecPackedFullMTU(t *testing.T) {
	innerOverhead := innerMTU - innerMaxData
	packets := make([][]byte, blockSize)
	for i := range packets {
		packets[i] = bytes.Repeat([]byte{byte(i + 1)}, maxOriginal)
	}
	b := makeBlock(packets, true)
	parts := captureParts(&b, 2, 200, true)
	if len(parts) != 2 {
		t.Fatalf("expected 2 pieces, got %d", len(parts))
	}
	tailLen := maxOriginal - parityChunk
	wantDesc := descHeaderLen + blockSize*8 + 1 + tailLen
	if len(parts[0]) != wantDesc {
		t.Fatalf("descriptor-with-tail length %d, want %d", len(parts[0]), wantDesc)
	}
	if len(parts[1]) != parityHeaderLen+parityChunk {
		t.Fatalf("leading parity length %d, want %d", len(parts[1]), parityHeaderLen+parityChunk)
	}
	pieces, innerBytes := b.repairBudget()
	wantInner := (wantDesc + innerOverhead) + (parityHeaderLen + parityChunk + innerOverhead)
	if pieces != 2 || innerBytes != wantInner {
		t.Fatalf("budget = (%d,%d), want (2,%d)", pieces, innerBytes, wantInner)
	}

	var recent originals
	for i, p := range packets {
		if i != 5 {
			recent.remember(p)
		}
	}
	var d decoder
	var out [maxOriginal]byte

	// Tail-first: descriptor-with-tail before leading chunk.
	if rec, _, measured, _ := d.receive(parts[0], &recent, out[:]); rec != nil || measured {
		t.Fatalf("descriptor alone should not complete before leading chunk")
	}
	rec, missing, measured, _ := d.receive(parts[1], &recent, out[:])
	if !measured || missing != 1 {
		t.Fatalf("complete repair: measured=%v missing=%d", measured, missing)
	}
	if !bytes.Equal(rec, packets[5]) {
		t.Fatalf("recovered wrong packet")
	}
}

func TestInnerCodecPackedReverseOrder(t *testing.T) {
	packets := make([][]byte, blockSize)
	for i := range packets {
		packets[i] = bytes.Repeat([]byte{byte(i + 1)}, 1100+i*10)
	}
	b := makeBlock(packets, true)
	parts := captureParts(&b, 3, 300, true)
	if len(parts) != 2 {
		t.Fatalf("expected 2 pieces, got %d", len(parts))
	}

	var recent originals
	for i, p := range packets {
		if i != 7 {
			recent.remember(p)
		}
	}
	var d decoder
	var out [maxOriginal]byte

	// Leading chunk first, descriptor-with-tail last.
	if rec, _, measured, _ := d.receive(parts[1], &recent, out[:]); rec != nil || measured {
		t.Fatalf("leading chunk alone should not complete")
	}
	rec, missing, measured, _ := d.receive(parts[0], &recent, out[:])
	if !measured || missing != 1 || !bytes.Equal(rec, packets[7]) {
		t.Fatalf("reverse-order recovery failed")
	}
}

func TestInnerCodecUnequalLengthsPacked(t *testing.T) {
	lengths := []int{128, 200, 333, 512, 777, 900, 1100, 1150}
	packets := make([][]byte, blockSize)
	for i, n := range lengths {
		packets[i] = bytes.Repeat([]byte{byte(i + 1)}, n)
	}
	b := makeBlock(packets, true)
	parts := captureParts(&b, 4, 400, true)

	var recent originals
	for i, p := range packets {
		if i != 2 {
			recent.remember(p)
		}
	}
	var d decoder
	var out [maxOriginal]byte

	for i, p := range parts {
		rec, missing, measured, _ := d.receive(p, &recent, out[:])
		if i == len(parts)-1 {
			if !measured || missing != 1 || !bytes.Equal(rec, packets[2]) {
				t.Fatalf("unequal-length recovery failed")
			}
		}
	}
}

func TestInnerCodecMultipleLossesNoRecovery(t *testing.T) {
	packets := make([][]byte, blockSize)
	for i := range packets {
		packets[i] = bytes.Repeat([]byte{byte(i + 1)}, 256+i)
	}
	b := makeBlock(packets, true)
	parts := captureParts(&b, 5, 500, true)

	var recent originals
	for i, p := range packets {
		if i != 1 && i != 4 {
			recent.remember(p)
		}
	}
	var d decoder
	var out [maxOriginal]byte

	for _, p := range parts[:len(parts)-1] {
		d.receive(p, &recent, out[:])
	}
	rec, missing, measured, _ := d.receive(parts[len(parts)-1], &recent, out[:])
	if !measured || missing != 2 {
		t.Fatalf("two losses: measured=%v missing=%d", measured, missing)
	}
	if rec != nil {
		t.Fatalf("two losses must not recover immediately")
	}

	_, missing2, measured2, _ := d.receive(parts[0], &recent, out[:])
	if measured2 || missing2 != 0 {
		t.Fatalf("duplicate fragment re-measured")
	}
}

func TestInnerCodecZeroMissingWithRepair(t *testing.T) {
	packets := make([][]byte, blockSize)
	for i := range packets {
		packets[i] = bytes.Repeat([]byte{byte(i + 1)}, 300+i*10)
	}
	b := makeBlock(packets, true)
	parts := captureParts(&b, 6, 600, true)

	var recent originals
	for _, p := range packets {
		recent.remember(p)
	}
	var d decoder
	var out [maxOriginal]byte

	var rec []byte
	var missing int
	var measured bool
	for _, p := range parts {
		rec, missing, measured, _ = d.receive(p, &recent, out[:])
	}
	if !measured || missing != 0 || rec != nil {
		t.Fatalf("zero missing: measured=%v missing=%d rec=%v", measured, missing, rec != nil)
	}
}

func TestInnerCodecCorruptedParity(t *testing.T) {
	packets := make([][]byte, blockSize)
	for i := range packets {
		packets[i] = bytes.Repeat([]byte{byte(i + 1)}, 400+i*11)
	}
	b := makeBlock(packets, true)
	parts := captureParts(&b, 7, 700, true)

	var recent originals
	for i, p := range packets {
		if i != 6 {
			recent.remember(p)
		}
	}
	var d decoder
	var out [maxOriginal]byte

	// Corrupt a parity byte inside the missing packet's range.
	if len(parts) == 1 {
		// Single packed piece: corrupt the tail inside the descriptor.
		at := descHeaderLen + blockSize*8 + 1 + 50
		if at >= len(parts[0]) {
			at = len(parts[0]) - 1
		}
		parts[0][at] ^= 0xff
	} else {
		at := parityHeaderLen + 50
		if at >= len(parts[1]) {
			at = len(parts[1]) - 1
		}
		parts[1][at] ^= 0xff
	}

	var rec []byte
	var missing int
	var measured bool
	for _, p := range parts {
		rec, missing, measured, _ = d.receive(p, &recent, out[:])
	}
	if !measured || missing != 1 {
		t.Fatalf("corrupted parity: measured=%v missing=%d", measured, missing)
	}
	if rec != nil {
		t.Fatalf("corrupted parity must not return invalid reconstruction")
	}
}

func TestInnerCodecMalformedAndTruncated(t *testing.T) {
	packets := make([][]byte, blockSize)
	for i := range packets {
		packets[i] = bytes.Repeat([]byte{byte(i + 1)}, 200+i*5)
	}
	b := makeBlock(packets, true)
	good := captureParts(&b, 8, 800, true)

	cases := [][]byte{
		// too short
		{0, 0, 0, 5, 0, 0, 0, 0, descKind},
		// unknown kind
		append([]byte{0, 0, 0, 5, 0, 0, 0, 0, 7}, good[0][9:]...),
		// truncated descriptor
		good[0][:descHeaderLen+4],
		// count too large
		func() []byte {
			p := append([]byte(nil), good[0]...)
			p[10] = blockSize + 1
			return p
		}(),
		// packed flag without parity flag
		func() []byte {
			p := append([]byte(nil), good[0]...)
			p[9] = descFlagPacked
			return p
		}(),
		// packed descriptor with no tail bytes
		func() []byte {
			p := append([]byte(nil), good[0]...)
			p[9] |= descFlagPacked
			return p[:descHeaderLen+blockSize*8+1]
		}(),
		// oversized parity piece
		func() []byte {
			if len(good) < 2 {
				return nil
			}
			p := append([]byte(nil), good[1]...)
			for len(p)-parityHeaderLen <= parityChunk {
				p = append(p, 0)
			}
			return p
		}(),
		// parity piece with index >= total
		func() []byte {
			if len(good) < 2 {
				return nil
			}
			p := append([]byte(nil), good[1]...)
			p[9] = p[10]
			return p
		}(),
		// inconsistent parity totals
		func() []byte {
			if len(good) < 2 {
				return nil
			}
			p := append([]byte(nil), good[1]...)
			p[10] = 99
			return p
		}(),
	}

	var recent originals
	var d decoder
	var out [maxOriginal]byte
	for _, c := range cases {
		if c == nil {
			continue
		}
		rec, missing, measured, _ := d.receive(c, &recent, out[:])
		if rec != nil || missing != 0 || measured {
			t.Fatalf("malformed payload accepted: rec=%v missing=%d measured=%v", rec != nil, missing, measured)
		}
	}
}

func TestInnerCodecSessionResetAndWindow(t *testing.T) {
	packets := make([][]byte, blockSize)
	for i := range packets {
		packets[i] = bytes.Repeat([]byte{byte(i + 1)}, 250+i*3)
	}
	b := makeBlock(packets, true)
	parts := captureParts(&b, 10, 1000, true)

	var recent originals
	for i, p := range packets {
		if i != 3 {
			recent.remember(p)
		}
	}
	var d decoder
	var out [maxOriginal]byte

	d.receive(parts[0], &recent, out[:])

	far := make([]byte, 8)
	binary.BigEndian.PutUint32(far[0:4], 1000)
	binary.BigEndian.PutUint32(far[4:8], 2000)
	far = append(far, descKind, 0, byte(blockSize), 0, 0)
	for range blockSize {
		far = binary.BigEndian.AppendUint64(far, 0)
	}
	if _, _, measured, _ := d.receive(far, &recent, out[:]); !measured {
		t.Fatalf("far-ahead group should reset and be measured")
	}

	for i := 1; i < len(parts); i++ {
		if rec, _, measured, _ := d.receive(parts[i], &recent, out[:]); rec != nil || measured {
			t.Fatalf("old group fragment after reset should be ignored")
		}
	}
}

func TestInnerCodecOutAliasing(t *testing.T) {
	packets := make([][]byte, blockSize)
	for i := range packets {
		packets[i] = bytes.Repeat([]byte{byte(i + 1)}, 300+i*7)
	}
	b := makeBlock(packets, true)
	parts := captureParts(&b, 9, 900, true)

	var recent originals
	for i, p := range packets {
		if i != 0 {
			recent.remember(p)
		}
	}
	var d decoder
	scratch := make([]byte, maxOriginal)
	copy(scratch, parts[len(parts)-1])
	payload := scratch[:len(parts[len(parts)-1])]
	for i, p := range parts {
		if i == len(parts)-1 {
			d.receive(payload, &recent, scratch[:maxOriginal])
		} else {
			d.receive(p, &recent, scratch[:maxOriginal])
		}
	}
	if !bytes.Equal(scratch[:len(packets[0])], packets[0]) {
		t.Fatalf("repair into aliasing scratch produced wrong data")
	}
}

func TestInnerCodecLateReorderedOriginals(t *testing.T) {
	packets := make([][]byte, blockSize)
	for i := range packets {
		packets[i] = bytes.Repeat([]byte{byte(i + 1)}, 256+i*13)
	}
	b := makeBlock(packets, true)
	parts := captureParts(&b, 11, 1100, true)

	var recent originals
	for i, p := range packets {
		if i != 2 && i != 5 {
			recent.remember(p)
		}
	}
	var d decoder
	var out [maxOriginal]byte

	for _, p := range parts {
		d.receive(p, &recent, out[:])
	}

	recent.remember(packets[5])
	rec := d.late(packets[5], &recent, out[:])
	if rec == nil {
		t.Fatalf("first missing original should trigger recovery of the other")
	}
	if !bytes.Equal(rec, packets[2]) {
		t.Fatalf("late recovery mismatch")
	}

	if rec2 := d.late(packets[5], &recent, out[:]); rec2 != nil {
		t.Fatalf("duplicate late should not deliver again")
	}
}

func TestInnerCodecLateAlias(t *testing.T) {
	packets := make([][]byte, blockSize)
	for i := range packets {
		packets[i] = bytes.Repeat([]byte{byte(i + 1)}, maxOriginal)
	}
	b := makeBlock(packets, true)
	parts := captureParts(&b, 12, 1200, true)

	var recent originals
	for i, p := range packets {
		if i != 1 && i != 6 {
			recent.remember(p)
		}
	}
	var d decoder

	for _, p := range parts {
		var out [maxOriginal]byte
		d.receive(p, &recent, out[:])
	}

	recent.remember(packets[1])
	scratch := make([]byte, maxOriginal)
	copy(scratch, packets[1])
	rec := d.late(scratch[:len(packets[1])], &recent, scratch[:maxOriginal])
	if rec == nil {
		t.Fatalf("first missing original should recover the other")
	}
	if !bytes.Equal(rec, packets[6]) {
		t.Fatalf("aliased late recovery produced wrong packet")
	}

	recent.remember(packets[6])
	copy(scratch, packets[6])
	if rec2 := d.late(scratch[:len(packets[6])], &recent, scratch[:maxOriginal]); rec2 != nil {
		t.Fatalf("second late original should be a duplicate")
	}
}

func TestInnerCodecLateDuplicateThenMissing(t *testing.T) {
	packets := make([][]byte, blockSize)
	for i := range packets {
		packets[i] = bytes.Repeat([]byte{byte(i + 1)}, 300+i*7)
	}
	b := makeBlock(packets, true)
	parts := captureParts(&b, 13, 1300, true)

	var recent originals
	for i, p := range packets {
		if i != 0 && i != 7 {
			recent.remember(p)
		}
	}
	var d decoder
	var out [maxOriginal]byte

	for _, p := range parts {
		d.receive(p, &recent, out[:])
	}

	recent.remember(packets[3])
	if rec := d.late(packets[3], &recent, out[:]); rec != nil {
		t.Fatalf("non-missing late original should not trigger recovery")
	}

	recent.remember(packets[0])
	rec := d.late(packets[0], &recent, out[:])
	if rec == nil {
		t.Fatalf("missing original should trigger recovery")
	}
	if !bytes.Equal(rec, packets[7]) {
		t.Fatalf("late recovery produced wrong packet")
	}
}

func TestInnerCodecLateNoPending(t *testing.T) {
	packets := make([][]byte, blockSize)
	for i := range packets {
		packets[i] = bytes.Repeat([]byte{byte(i + 1)}, 300+i*5)
	}
	var recent originals
	for _, p := range packets {
		recent.remember(p)
	}
	var d decoder
	var out [maxOriginal]byte
	if rec := d.late(packets[0], &recent, out[:]); rec != nil {
		t.Fatalf("late with no pending groups must return nil")
	}
}

func TestInnerCodecSessionResetClearsPending(t *testing.T) {
	packets := make([][]byte, blockSize)
	for i := range packets {
		packets[i] = bytes.Repeat([]byte{byte(i + 1)}, 350+i*4)
	}
	b := makeBlock(packets, true)
	parts := captureParts(&b, 14, 1400, true)

	var recent originals
	for i, p := range packets {
		if i != 1 && i != 4 {
			recent.remember(p)
		}
	}
	var d decoder
	var out [maxOriginal]byte

	for _, p := range parts {
		d.receive(p, &recent, out[:])
	}

	// Reset the session with a far-ahead group.
	far := make([]byte, 8)
	binary.BigEndian.PutUint32(far[0:4], 2000)
	binary.BigEndian.PutUint32(far[4:8], 3000)
	far = append(far, descKind, 0, byte(blockSize), 0, 0)
	for range blockSize {
		far = binary.BigEndian.AppendUint64(far, 0)
	}
	d.receive(far, &recent, out[:])

	// A late original from the discarded group must not be hashed into a stale
	// pending entry.
	recent.remember(packets[1])
	if rec := d.late(packets[1], &recent, out[:]); rec != nil {
		t.Fatalf("late after session reset should return nil")
	}
}

func TestInnerCodecRepairBudget(t *testing.T) {
	innerOverhead := innerMTU - innerMaxData
	if innerOverhead != 20+innerHeaderLen+innerMacLen {
		t.Fatalf("inner overhead %d != 20+21+16", innerOverhead)
	}

	// Descriptor-only block.
	packets := make([][]byte, blockSize)
	for i := range packets {
		packets[i] = bytes.Repeat([]byte{byte(i + 1)}, minOriginal+i)
	}
	b := makeBlock(packets, false)
	pieces, innerBytes := b.repairBudget()
	want := descHeaderLen + blockSize*8 + innerOverhead
	if pieces != 1 || innerBytes != want {
		t.Fatalf("descriptor budget = (%d,%d), want (1,%d)", pieces, innerBytes, want)
	}

	// Single-piece parity.
	b = makeBlock(packets, true)
	pieces, innerBytes = b.repairBudget()
	want = descHeaderLen + blockSize*8 + 1 + b.width + innerOverhead
	if pieces != 1 || innerBytes != want {
		t.Fatalf("single-parity budget = (%d,%d), want (1,%d)", pieces, innerBytes, want)
	}

	// Full-MTU two-piece parity.
	for i := range packets {
		packets[i] = bytes.Repeat([]byte{byte(i + 1)}, maxOriginal)
	}
	b = makeBlock(packets, true)
	pieces, innerBytes = b.repairBudget()
	tailLen := maxOriginal - parityChunk
	want = (descHeaderLen + blockSize*8 + 1 + tailLen + innerOverhead) +
		(parityHeaderLen + parityChunk + innerOverhead)
	if pieces != 2 || innerBytes != want {
		t.Fatalf("full-MTU budget = (%d,%d), want (2,%d)", pieces, innerBytes, want)
	}
}
