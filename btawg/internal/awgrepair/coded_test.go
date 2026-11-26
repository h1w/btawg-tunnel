package awgrepair

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"
)

func makeTestPacket(seed byte, size int) []byte {
	p := make([]byte, size)
	for i := range p {
		p[i] = byte(seed) ^ byte(i*7) ^ byte(i>>8)
	}
	// Ensure a plausible IPv4 header so packetID has some structure.
	p[0] = 0x45
	p[1] = 0x00
	binary.BigEndian.PutUint16(p[2:4], uint16(size))
	p[9] = 6 // TCP
	return p
}

// classify separates emitted parity pieces into heads (part 0, carrying
// metadata) and tails (part 1). For short groups there are no tails.
func classify(repairs [][]byte) (heads, tails [][]byte) {
	for _, r := range repairs {
		if len(r) >= 7 && r[4] == codedKindParity && r[6] == 0 {
			heads = append(heads, r)
		} else {
			tails = append(tails, r)
		}
	}
	return heads, tails
}

func collectGroup(t *testing.T, pkts [][]byte) (repairs [][]byte) {
	t.Helper()
	var tx codedTX
	for _, p := range pkts {
		tx.add(p, func(out []byte) bool {
			repairs = append(repairs, slices.Clone(out))
			return true
		})
	}
	if tx.count != 0 {
		t.Fatalf("encoder did not close group: got %d buffered", tx.count)
	}
	return repairs
}

func feedRepair(rx *codedRX, repairs [][]byte, drop map[int]bool, reverse bool, deliver func([]byte)) {
	if reverse {
		for i := len(repairs) - 1; i >= 0; i-- {
			if drop != nil && drop[i] {
				continue
			}
			rx.repair(repairs[i], deliver)
		}
		return
	}
	for i, r := range repairs {
		if drop != nil && drop[i] {
			continue
		}
		rx.repair(r, deliver)
	}
}

func feedOriginals(rx *codedRX, pkts [][]byte, drop map[int]bool, deliver func([]byte)) {
	for i, p := range pkts {
		if drop != nil && drop[i] {
			continue
		}
		rx.original(p, deliver)
	}
}

func TestCodedRoundTripNoLoss(t *testing.T) {
	pkts := make([][]byte, codedGroupSize)
	for i := range pkts {
		pkts[i] = makeTestPacket(byte(i), 200+i*3)
	}
	repairs := collectGroup(t, pkts)
	heads, tails := classify(repairs)
	if len(heads) != codedParityRows {
		t.Fatalf("expected %d head pieces for short packets, got %d", codedParityRows, len(heads))
	}
	if len(tails) != 0 {
		t.Fatalf("expected no tail pieces for short packets, got %d", len(tails))
	}

	rx := &codedRX{}
	var delivered int
	feedOriginals(rx, pkts, nil, func([]byte) { delivered++ })
	feedRepair(rx, repairs, nil, true, func([]byte) { delivered++ })

	if delivered != 0 {
		t.Fatalf("expected no deliveries without loss, got %d", delivered)
	}
}

func TestCodedRecoverOneMissing(t *testing.T) {
	pkts := make([][]byte, codedGroupSize)
	for i := range pkts {
		pkts[i] = makeTestPacket(byte(i), 200+i*7)
	}
	repairs := collectGroup(t, pkts)

	rx := &codedRX{}
	var got []byte
	feedOriginals(rx, pkts, map[int]bool{5: true}, func(p []byte) { got = p })
	feedRepair(rx, repairs, nil, false, func(p []byte) { got = p })

	if got == nil {
		t.Fatal("failed to recover one missing packet")
	}
	if !bytes.Equal(got, pkts[5]) {
		t.Fatal("recovered packet does not match original")
	}
}

func TestCodedRecoverTwoMissingLarge(t *testing.T) {
	pkts := make([][]byte, codedGroupSize)
	for i := range pkts {
		pkts[i] = makeTestPacket(byte(i*11), maxOriginal-i)
	}
	repairs := collectGroup(t, pkts)

	rx := &codedRX{}
	var got [][]byte
	deliver := func(p []byte) {
		got = append(got, slices.Clone(p))
	}
	feedOriginals(rx, pkts, map[int]bool{6: true, 7: true}, deliver)
	feedRepair(rx, repairs, nil, true, deliver)

	if len(got) != 2 {
		t.Fatalf("expected 2 recovered packets, got %d", len(got))
	}
	seen := make(map[uint64]bool)
	for _, p := range got {
		id := packetID(p)
		if seen[id] {
			t.Fatal("delivered same packet twice")
		}
		seen[id] = true
		var found bool
		for _, orig := range pkts[6:8] {
			if bytes.Equal(p, orig) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("recovered unknown packet %x", p)
		}
	}
}

func TestCodedRecoverOneMissingPartialRowLarge(t *testing.T) {
	pkts := make([][]byte, codedGroupSize)
	for i := range pkts {
		pkts[i] = makeTestPacket(byte(i*13), maxOriginal-i*2)
	}
	repairs := collectGroup(t, pkts)

	// Drop a real tail piece from row 0. Row 0 is now incomplete; row 1 alone
	// must recover the single missing original.
	tailIdx := -1
	for i, r := range repairs {
		if len(r) >= 8 && r[4] == codedKindParity && r[5] == 0 && r[6] == 1 {
			tailIdx = i
			break
		}
	}
	if tailIdx < 0 {
		t.Fatal("test did not generate a real tail piece")
	}

	rx := &codedRX{}
	var got []byte
	deliver := func(p []byte) { got = p }
	feedOriginals(rx, pkts, map[int]bool{3: true}, deliver)
	feedRepair(rx, repairs, map[int]bool{tailIdx: true}, true, deliver)

	if got == nil {
		t.Fatal("failed to recover one missing packet when one parity row is partial")
	}
	if !bytes.Equal(got, pkts[3]) {
		t.Fatal("recovered packet does not match original")
	}
}

func TestCodedNoDeliveryTwoMissingPartialRowLarge(t *testing.T) {
	pkts := make([][]byte, codedGroupSize)
	for i := range pkts {
		pkts[i] = makeTestPacket(byte(i*17), maxOriginal-i)
	}
	repairs := collectGroup(t, pkts)

	// Drop two originals and one tail from row 0. Only row 1 is complete, so
	// two missing packets cannot be recovered.
	tailIdx := -1
	for i, r := range repairs {
		if len(r) >= 8 && r[4] == codedKindParity && r[5] == 0 && r[6] == 1 {
			tailIdx = i
			break
		}
	}
	if tailIdx < 0 {
		t.Fatal("test did not generate a real tail piece")
	}

	rx := &codedRX{}
	var delivered int
	deliver := func([]byte) { delivered++ }
	feedOriginals(rx, pkts, map[int]bool{2: true, 5: true}, deliver)
	feedRepair(rx, repairs, map[int]bool{tailIdx: true}, true, deliver)

	if delivered != 0 {
		t.Fatalf("expected no delivery with 2 missing and partial parity row, got %d", delivered)
	}
}

func TestCodedHeadMetadataReplicated(t *testing.T) {
	pkts := make([][]byte, codedGroupSize)
	for i := range pkts {
		pkts[i] = makeTestPacket(byte(i*19), maxOriginal-i)
	}
	repairs := collectGroup(t, pkts)
	heads, _ := classify(repairs)
	if len(heads) != codedParityRows {
		t.Fatalf("expected %d heads, got %d", codedParityRows, len(heads))
	}

	// Drop the head of row 0 (metadata). The head of row 1 still carries the
	// IDs/lengths needed to recover one missing original using row 1.
	head0 := -1
	for i, r := range heads {
		if r[5] == 0 {
			head0 = i
			break
		}
	}
	if head0 < 0 {
		t.Fatal("row 0 head not found")
	}

	rx := &codedRX{}
	var got []byte
	deliver := func(p []byte) { got = p }
	feedOriginals(rx, pkts, map[int]bool{4: true}, deliver)
	for i, r := range repairs {
		if i == head0 {
			continue
		}
		rx.repair(r, deliver)
	}

	if got == nil || !bytes.Equal(got, pkts[4]) {
		t.Fatal("failed to recover using replicated metadata from row 1")
	}
}

func TestCodedBadRowGoodRowFallback(t *testing.T) {
	pkts := make([][]byte, codedGroupSize)
	for i := range pkts {
		pkts[i] = makeTestPacket(byte(i*21), 240+i*5)
	}
	repairs := collectGroup(t, pkts)

	// Corrupt row 0 parity data so that the first row tried fails packetID
	// verification; row 1 must be used instead.
	for i, r := range repairs {
		if len(r) >= 6 && r[4] == codedKindParity && r[5] == 0 {
			repairs[i] = slices.Clone(r)
			repairs[i][codedMetaLen+5] ^= 0xff
			break
		}
	}

	rx := &codedRX{}
	var got []byte
	deliver := func(p []byte) { got = p }
	feedOriginals(rx, pkts, map[int]bool{2: true}, deliver)
	feedRepair(rx, repairs, nil, false, deliver)

	if got == nil || !bytes.Equal(got, pkts[2]) {
		t.Fatal("failed to fall back to good parity row")
	}
}

func TestCodedNoFalseRepairWithInsufficientParity(t *testing.T) {
	pkts := make([][]byte, codedGroupSize)
	for i := range pkts {
		pkts[i] = makeTestPacket(byte(i*13), 250+i*5)
	}
	repairs := collectGroup(t, pkts)

	// Provide only row 0 pieces while two originals are missing.
	row0Only := map[int]bool{}
	for i, r := range repairs {
		if len(r) >= 6 && r[4] == codedKindParity && r[5] == 1 {
			row0Only[i] = true
		}
	}

	rx := &codedRX{}
	var delivered int
	feedOriginals(rx, pkts, map[int]bool{2: true, 5: true}, func([]byte) { delivered++ })
	feedRepair(rx, repairs, row0Only, false, func([]byte) { delivered++ })

	if delivered != 0 {
		t.Fatalf("expected no delivery with insufficient parity, got %d", delivered)
	}
}

func TestCodedNoFalseRepairCorruptedParity(t *testing.T) {
	pkts := make([][]byte, codedGroupSize)
	for i := range pkts {
		pkts[i] = makeTestPacket(byte(i*17), 260+i*9)
	}
	repairs := collectGroup(t, pkts)

	// Corrupt parity data in both rows so that neither verifies.
	for i, r := range repairs {
		if len(r) >= 6 && r[4] == codedKindParity {
			repairs[i] = slices.Clone(r)
			repairs[i][codedMetaLen+10] ^= 0xff
		}
	}

	rx := &codedRX{}
	var delivered int
	feedOriginals(rx, pkts, map[int]bool{3: true}, func([]byte) { delivered++ })
	feedRepair(rx, repairs, nil, false, func([]byte) { delivered++ })

	if delivered != 0 {
		t.Fatalf("expected no delivery with corrupted parity, got %d", delivered)
	}
}

func TestCodedLateOriginalsSuppressed(t *testing.T) {
	pkts := make([][]byte, codedGroupSize)
	for i := range pkts {
		pkts[i] = makeTestPacket(byte(i*19), 240+i*13)
	}
	repairs := collectGroup(t, pkts)

	rx := &codedRX{}
	var got [][]byte
	deliver := func(p []byte) {
		got = append(got, slices.Clone(p))
	}
	feedOriginals(rx, pkts, map[int]bool{1: true, 4: true}, deliver)
	feedRepair(rx, repairs, nil, false, deliver)
	if len(got) != 2 {
		t.Fatalf("expected 2 recovered before late originals, got %d", len(got))
	}
	// Late originals must not trigger additional deliveries.
	nBefore := len(got)
	feedOriginals(rx, pkts, nil, deliver)
	if len(got) != nBefore {
		t.Fatalf("late originals caused extra deliveries: %d vs %d", len(got), nBefore)
	}
}

func TestCodedParityBeforeOriginals(t *testing.T) {
	pkts := make([][]byte, codedGroupSize)
	for i := range pkts {
		pkts[i] = makeTestPacket(byte(i*23), 220+i*17)
	}
	repairs := collectGroup(t, pkts)

	rx := &codedRX{}
	var got [][]byte
	deliver := func(p []byte) { got = append(got, slices.Clone(p)) }
	feedRepair(rx, repairs, nil, true, deliver)
	feedOriginals(rx, pkts, map[int]bool{0: true, 7: true}, deliver)

	if len(got) != 2 {
		t.Fatalf("expected 2 recovered packets, got %d", len(got))
	}
}

func TestCodedRecoverLargePackets(t *testing.T) {
	pkts := make([][]byte, codedGroupSize)
	for i := range pkts {
		pkts[i] = makeTestPacket(byte(i*29), maxOriginal-i)
	}
	repairs := collectGroup(t, pkts)

	// Large width forces each parity row into a head and a tail.
	heads, tails := classify(repairs)
	wantPieces := codedParityRows * 2
	if len(heads)+len(tails) != wantPieces {
		t.Fatalf("expected %d parity pieces, got %d", wantPieces, len(heads)+len(tails))
	}
	if len(heads) != codedParityRows || len(tails) != codedParityRows {
		t.Fatalf("expected %d heads and %d tails, got %d/%d", codedParityRows, codedParityRows, len(heads), len(tails))
	}

	rx := &codedRX{}
	var got [][]byte
	feedOriginals(rx, pkts, map[int]bool{5: true, 6: true}, func(p []byte) { got = append(got, slices.Clone(p)) })
	feedRepair(rx, repairs, nil, true, func(p []byte) { got = append(got, slices.Clone(p)) })
	if len(got) != 2 {
		t.Fatalf("expected 2 recovered large packets, got %d", len(got))
	}
}

func TestCodedRecoverAfterThreeMissingThenLate(t *testing.T) {
	pkts := make([][]byte, codedGroupSize)
	for i := range pkts {
		pkts[i] = makeTestPacket(byte(i*31), 210+i*11)
	}
	repairs := collectGroup(t, pkts)

	// Start with three missing and all parity. Don't give up.
	rx := &codedRX{}
	var got [][]byte
	deliver := func(p []byte) { got = append(got, slices.Clone(p)) }
	feedOriginals(rx, pkts, map[int]bool{2: true, 5: true, 7: true}, deliver)
	feedRepair(rx, repairs, nil, false, deliver)
	if len(got) != 0 {
		t.Fatalf("expected no recovery with 3 missing and 2 parity rows, got %d", len(got))
	}

	// A late original reduces the missing count to 2; recovery should now succeed.
	rx.original(pkts[5], deliver)
	if len(got) != 2 {
		t.Fatalf("expected recovery after late original, got %d", len(got))
	}
}

func TestCodedMalformedIgnored(t *testing.T) {
	pkts := make([][]byte, codedGroupSize)
	for i := range pkts {
		pkts[i] = makeTestPacket(byte(i*37), 230+i*7)
	}
	repairs := collectGroup(t, pkts)

	rx := &codedRX{}
	var delivered int
	deliver := func([]byte) { delivered++ }

	// Head too short to contain metadata.
	rx.repair(repairs[0][:codedMetaLen-1], deliver)
	// Oversized head data.
	bad := make([]byte, codedMetaLen+codedMaxHeadData+1)
	copy(bad, repairs[1])
	rx.repair(bad, deliver)
	// Bad row index.
	bad2 := slices.Clone(repairs[1])
	bad2[5] = 99
	rx.repair(bad2, deliver)
	// Bad part index.
	bad3 := slices.Clone(repairs[1])
	bad3[6] = 99
	rx.repair(bad3, deliver)

	feedOriginals(rx, pkts, map[int]bool{4: true}, deliver)
	feedRepair(rx, repairs, nil, false, deliver)

	if delivered != 1 {
		t.Fatalf("expected exactly one valid delivery, got %d", delivered)
	}
}

func TestCodedPendingNotScannedWhenEmpty(t *testing.T) {
	pkts := make([][]byte, codedGroupSize)
	for i := range pkts {
		pkts[i] = makeTestPacket(byte(i*43), 205+i*5)
	}

	rx := &codedRX{}
	var scanned bool
	// Before any repair payload there are no pending groups, so a stray original
	// should not trigger any recovery attempts.
	feedOriginals(rx, pkts, nil, func([]byte) { scanned = true })
	if scanned {
		t.Fatal("original callback should not fire when no group is pending")
	}
}

func TestCodedMemoryBound(t *testing.T) {
	pkts := make([][]byte, codedGroupSize)
	for i := range pkts {
		pkts[i] = makeTestPacket(byte(i), 250+i)
	}
	repairs := collectGroup(t, pkts)

	rx := &codedRX{}
	var delivered int
	deliver := func([]byte) { delivered++ }
	// Feed many groups to ensure window eviction does not panic.
	for g := range 200 {
		for _, r := range repairs {
			mut := slices.Clone(r)
			binary.BigEndian.PutUint32(mut[0:4], uint32(g))
			rx.repair(mut, deliver)
		}
	}
	if delivered != 0 {
		t.Fatalf("unexpected delivery from empty groups: %d", delivered)
	}
}
