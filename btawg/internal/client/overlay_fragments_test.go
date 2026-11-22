package client

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func TestOverlayFragmentsWaitForCompleteContiguousPacket(t *testing.T) {
	first := ipv4FixtureFragment(7, 0, true, []byte("abcdefgh"))
	middle := ipv4FixtureFragment(7, 8, true, []byte("ijklmnop"))
	last := ipv4FixtureFragment(7, 16, false, []byte("qrst"))
	f := overlayReassembly{}
	now := time.Unix(10, 0)
	for _, packet := range [][]byte{last, first} {
		if got := f.push(packet, now); got != nil {
			t.Fatalf("dispatched incomplete packet: %x", got)
		}
	}
	got := f.push(middle, now)
	if len(got) != 40 || !bytes.Equal(got[20:], []byte("abcdefghijklmnopqrst")) ||
		binary.BigEndian.Uint16(got[2:4]) != 40 || binary.BigEndian.Uint16(got[6:8]) != 0 {
		t.Fatalf("unexpected reassembly: %x", got)
	}
}

func TestOverlayFragmentsRejectOverlappingData(t *testing.T) {
	f := overlayReassembly{}
	now := time.Unix(10, 0)
	f.push(ipv4FixtureFragment(7, 0, true, []byte("abcdefghijklmnop")), now)
	if got := f.push(ipv4FixtureFragment(7, 8, false, []byte("12345678")), now); got != nil {
		t.Fatalf("dispatched overlapping fragments: %x", got)
	}
}

func ipv4FixtureFragment(id uint16, offset int, more bool, data []byte) []byte {
	p := make([]byte, 20+len(data))
	p[0], p[8], p[9] = 0x45, 64, 17
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	binary.BigEndian.PutUint16(p[4:6], id)
	field := uint16(offset / 8)
	if more {
		field |= 0x2000
	}
	binary.BigEndian.PutUint16(p[6:8], field)
	copy(p[12:20], []byte{203, 0, 113, 8, 10, 77, 0, 2})
	copy(p[20:], data)
	return p
}
