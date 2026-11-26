package server

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"
)

func makeTCPPacket(src, dst string, sport, dport uint16) []byte {
	s := netip.MustParseAddr(src).As4()
	d := netip.MustParseAddr(dst).As4()
	p := make([]byte, 40)
	p[0] = 0x45
	p[1] = 0
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	p[4], p[5] = 0, 0
	p[6], p[7] = 0, 0
	p[8] = 64
	p[9] = 6 // TCP
	binary.BigEndian.PutUint16(p[10:12], 0)
	copy(p[12:16], s[:])
	copy(p[16:20], d[:])
	binary.BigEndian.PutUint16(p[20:22], sport)
	binary.BigEndian.PutUint16(p[22:24], dport)
	return p
}

func makeUDPPacket(src, dst string, sport, dport uint16) []byte {
	p := makeTCPPacket(src, dst, sport, dport)
	p[9] = 17 // UDP
	return p
}

func makeICMPPacket(src, dst string, typ, code uint8, id uint16) []byte {
	s := netip.MustParseAddr(src).As4()
	d := netip.MustParseAddr(dst).As4()
	p := make([]byte, 32)
	p[0] = 0x45
	p[1] = 0
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	p[8] = 64
	p[9] = 1 // ICMP
	copy(p[12:16], s[:])
	copy(p[16:20], d[:])
	off := 20
	p[off] = typ
	p[off+1] = code
	binary.BigEndian.PutUint16(p[off+4:off+6], id)
	return p
}

func TestFlowPinAndLookup(t *testing.T) {
	ft := newFlowTable()
	ft.flowTTL = time.Minute
	ft.maxEntries = 8

	e1 := &engineTun{authSource: netip.MustParseAddr("10.77.0.2")}
	now := time.Now()

	fwd := makeTCPPacket("10.77.0.2", "1.1.1.1", 12345, 443)
	key, ok := flowKeyFromPacket(fwd)
	if !ok {
		t.Fatal("flowKeyFromPacket failed for TCP")
	}
	ft.pin(key.reverse(), e1, now)

	rev := makeTCPPacket("1.1.1.1", "10.77.0.2", 443, 12345)
	revKey, ok := flowKeyFromPacket(rev)
	if !ok {
		t.Fatal("flowKeyFromPacket failed for reverse TCP")
	}
	got, ok := ft.lookup(revKey, now)
	if !ok || got != e1 {
		t.Fatalf("lookup reverse flow: got %v %v, want e1", got, ok)
	}

	// Forward direction must not be pinned.
	if _, ok := ft.lookup(key, now); ok {
		t.Fatal("forward flow should not be pinned")
	}
}

func TestFlowICMPPin(t *testing.T) {
	ft := newFlowTable()
	e1 := &engineTun{}
	now := time.Now()

	fwd := makeICMPPacket("10.77.0.2", "10.77.0.1", 8, 0, 4242)
	key, ok := flowKeyFromPacket(fwd)
	if !ok {
		t.Fatal("flowKeyFromPacket failed for ICMP")
	}
	ft.pin(key.reverse(), e1, now)

	rev := makeICMPPacket("10.77.0.1", "10.77.0.2", 0, 0, 4242)
	revKey, ok := flowKeyFromPacket(rev)
	if !ok {
		t.Fatal("flowKeyFromPacket failed for ICMP reply")
	}
	if got, ok := ft.lookup(revKey, now); !ok || got != e1 {
		t.Fatalf("lookup ICMP reply: got %v %v", got, ok)
	}

	// Different identifier misses.
	other := makeICMPPacket("10.77.0.1", "10.77.0.2", 0, 0, 9999)
	otherKey, _ := flowKeyFromPacket(other)
	if _, ok := ft.lookup(otherKey, now); ok {
		t.Fatal("different ICMP id should miss")
	}
}

func TestFlowFragmentPin(t *testing.T) {
	ft := newFlowTable()
	e1 := &engineTun{}
	now := time.Now()

	// First fragment: offset 0, MF set.
	first := makeTCPPacket("10.77.0.2", "1.1.1.1", 12345, 443)
	first[6] = 0x20 // MF
	first[7] = 0
	binary.BigEndian.PutUint16(first[4:6], 1234) // IP ID

	key, ok := flowKeyFromPacket(first)
	if !ok {
		t.Fatal("first fragment should yield flow key")
	}
	fragKey, ok := fragmentKeyFromPacket(first)
	if !ok {
		t.Fatal("first fragment should yield fragment key")
	}
	ft.pin(key.reverse(), e1, now)
	ft.pinFragment(fragKey, e1)

	// Later fragment: offset > 0, no ports.
	later := make([]byte, 28)
	later[0] = 0x45
	binary.BigEndian.PutUint16(later[2:4], uint16(len(later)))
	binary.BigEndian.PutUint16(later[4:6], 1234) // same ID
	later[6] = 0x00                              // MF clear
	later[7] = 0x01                              // offset 8 bytes (unit is 8 bytes)
	later[8] = 64
	later[9] = 6 // TCP
	s := netip.MustParseAddr("10.77.0.2").As4()
	d := netip.MustParseAddr("1.1.1.1").As4()
	copy(later[12:16], s[:])
	copy(later[16:20], d[:])

	fragKey2, ok := fragmentKeyFromPacket(later)
	if !ok {
		t.Fatal("later fragment should yield fragment key")
	}
	if fragKey2 != fragKey {
		t.Fatalf("fragment keys differ: %v vs %v", fragKey, fragKey2)
	}
	if got, ok := ft.lookupFragment(fragKey2, now); !ok || got != e1 {
		t.Fatalf("lookup later fragment: got %v %v", got, ok)
	}
	if _, ok := flowKeyFromPacket(later); ok {
		t.Fatal("later fragment must not yield L4 flow key")
	}
}

func TestActiveFlowCannotBeMovedToAnotherEngine(t *testing.T) {
	ft := newFlowTable()
	first, second := &engineTun{}, &engineTun{}
	key, _ := flowKeyFromPacket(makeTCPPacket("1.1.1.1", "10.77.0.2", 443, 12345))
	now := time.Now()
	ft.pin(key, first, now)
	ft.pin(key, second, now.Add(time.Second))
	if got, _ := ft.lookup(key, now.Add(time.Second)); got != first {
		t.Fatal("active reply path was stolen")
	}
	ft.pin(key, second, now.Add(2*ft.flowTTL))
	if got, _ := ft.lookup(key, now.Add(2*ft.flowTTL)); got != second {
		t.Fatal("expired reply path could not be replaced")
	}
}

func TestFlowCapacityEviction(t *testing.T) {
	ft := newFlowTable()
	ft.flowTTL = time.Hour
	ft.maxEntries = 3

	now := time.Now()
	engines := []*engineTun{{}, {}, {}, {}}
	ips := []string{"10.77.0.2", "10.77.0.3", "10.77.0.4", "10.77.0.5"}
	for i, ip := range ips {
		fwd := makeTCPPacket(ip, "1.1.1.1", uint16(1000+i), 443)
		key, _ := flowKeyFromPacket(fwd)
		ft.pin(key.reverse(), engines[i], now)
		time.Sleep(time.Millisecond) // ensure distinct insertion order for LRU
	}

	// Oldest entry should have been evicted.
	oldest := makeTCPPacket("10.77.0.2", "1.1.1.1", 1000, 443)
	oldestKey, _ := flowKeyFromPacket(oldest)
	if _, ok := ft.lookup(oldestKey.reverse(), now); ok {
		t.Fatal("oldest flow should have been evicted")
	}

	// Remaining entries should be present.
	for i := 1; i < len(ips); i++ {
		rev := makeTCPPacket("1.1.1.1", ips[i], 443, uint16(1000+i))
		key, _ := flowKeyFromPacket(rev)
		if _, ok := ft.lookup(key, now); !ok {
			t.Fatalf("flow %d should be present", i)
		}
	}
}

func TestFlowLookupRefreshesEvictionOrderNotPinLifetime(t *testing.T) {
	ft := newFlowTable()
	ft.maxEntries = 2
	owner := &engineTun{}
	now := time.Now()
	key := func(port uint16) flowKey {
		k, _ := flowKeyFromPacket(makeTCPPacket("1.1.1.1", "10.77.0.2", 443, port))
		return k
	}
	a, b, c := key(1000), key(1001), key(1002)
	ft.pin(a, owner, now)
	ft.pin(b, owner, now)
	if got, ok := ft.lookup(a, now.Add(ft.flowTTL/2)); !ok || got != owner {
		t.Fatal("live flow did not resolve to its owner")
	}
	ft.pin(c, owner, now.Add(ft.flowTTL/2))
	if _, ok := ft.lookup(b, now.Add(ft.flowTTL/2)); ok {
		t.Fatal("least recently used flow survived eviction")
	}
	if got, ok := ft.lookup(a, now.Add(ft.flowTTL/2)); !ok || got != owner {
		t.Fatal("recently used flow was evicted")
	}
	if _, ok := ft.lookup(a, now.Add(ft.flowTTL+time.Nanosecond)); ok {
		t.Fatal("lookup extended the authenticated pin lifetime")
	}
}

func TestFlowStaleCleanup(t *testing.T) {
	ft := newFlowTable()
	ft.flowTTL = time.Millisecond
	ft.maxEntries = 8

	e1 := &engineTun{}
	now := time.Now()
	fwd := makeTCPPacket("10.77.0.2", "1.1.1.1", 12345, 443)
	key, _ := flowKeyFromPacket(fwd)
	ft.pin(key.reverse(), e1, now)

	time.Sleep(5 * ft.flowTTL)
	if _, ok := ft.lookup(key.reverse(), time.Now()); ok {
		t.Fatal("stale flow should be cleaned up")
	}
}

func TestFlowRemoveForIP(t *testing.T) {
	ft := newFlowTable()
	e1, e2 := &engineTun{}, &engineTun{}
	now := time.Now()

	fwd1 := makeTCPPacket("10.77.0.2", "1.1.1.1", 12345, 443)
	key1, _ := flowKeyFromPacket(fwd1)
	ft.pin(key1.reverse(), e1, now)

	fwd2 := makeTCPPacket("10.77.0.3", "1.1.1.1", 12345, 443)
	key2, _ := flowKeyFromPacket(fwd2)
	ft.pin(key2.reverse(), e2, now)

	ft.removeForIP(netip.MustParseAddr("10.77.0.2"))

	if _, ok := ft.lookup(key1.reverse(), now); ok {
		t.Fatal("flow for removed IP should be gone")
	}
	if _, ok := ft.lookup(key2.reverse(), now); !ok {
		t.Fatal("flow for unrelated IP should remain")
	}
}

func TestFlowRemoveForEngine(t *testing.T) {
	ft := newFlowTable()
	e1, e2 := &engineTun{}, &engineTun{}
	now := time.Now()

	fwd1 := makeTCPPacket("10.77.0.2", "1.1.1.1", 12345, 443)
	key1, _ := flowKeyFromPacket(fwd1)
	ft.pin(key1.reverse(), e1, now)

	fwd2 := makeTCPPacket("10.77.0.3", "1.1.1.1", 12345, 443)
	key2, _ := flowKeyFromPacket(fwd2)
	ft.pin(key2.reverse(), e2, now)

	ft.removeForEngine(e1)

	if _, ok := ft.lookup(key1.reverse(), now); ok {
		t.Fatal("flow for removed engine should be gone")
	}
	if _, ok := ft.lookup(key2.reverse(), now); !ok {
		t.Fatal("flow for unrelated engine should remain")
	}
}

func TestFlowCleanupDoesNotRace(t *testing.T) {
	ft := newFlowTable()
	ft.flowTTL = time.Hour
	ft.maxEntries = 1024
	e1 := &engineTun{}
	now := time.Now()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			fwd := makeTCPPacket("10.77.0.2", "1.1.1.1", uint16(i), 443)
			key, _ := flowKeyFromPacket(fwd)
			ft.pin(key.reverse(), e1, now)
		}
		close(done)
	}()
	for i := 0; i < 100; i++ {
		ft.cleanup(now)
		ft.removeForIP(netip.MustParseAddr("10.77.0.2"))
	}
	<-done
}
