package server

import (
	"bytes"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"

	"bittorrent-tunnel/btawg/internal/awg"
	"bittorrent-tunnel/btawg/internal/store"
	"bittorrent-tunnel/btawg/internal/tunneldns"
)

func testRouterWithClient(t *testing.T) (*packetRouter, netip.Addr, netip.Addr) {
	t.Helper()
	clientPub, _, err := awg.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	clientIP := netip.MustParseAddr("10.77.0.2")
	gatewayIP := netip.MustParseAddr("10.77.0.1")

	inner, _, err := netstack.CreateNetTUN([]netip.Addr{gatewayIP}, nil, 1280)
	if err != nil {
		t.Fatalf("create netstack tun: %v", err)
	}
	return testRouterWithTUN(t, inner, clientPub, clientIP, gatewayIP)
}

func testRouterWithTUN(t *testing.T, inner tun.Device, clientPub string, clientIP, gatewayIP netip.Addr) (*packetRouter, netip.Addr, netip.Addr) {
	t.Helper()

	policy := newPolicySnapshot()
	policy.serverIP = gatewayIP
	rec := store.ClientRecord{
		ID:      "c1",
		Name:    "router-flow",
		PubKey:  clientPub,
		IP:      clientIP.String(),
		GroupID: "g1",
	}
	policy.byIP = map[netip.Addr]store.ClientRecord{clientIP: rec}
	policy.byAddr = map[netip.Addr]*trafficCounter{clientIP: new(trafficCounter)}
	policy.groups = map[string]store.GroupRecord{
		"g1": {ID: "g1", ServerIP: gatewayIP.String(), Subnet: "10.77.0.0/24"},
	}
	policy.gateways = map[netip.Addr]string{gatewayIP: "g1"}
	policy.prefixes = []netip.Prefix{netip.MustParsePrefix("10.77.0.0/24")}

	r := newPacketRouter(inner, false, policy)
	t.Cleanup(func() { _ = r.close() })
	return r, clientIP, gatewayIP
}

func TestRouterCountsOnlyFullEngineQueueDrops(t *testing.T) {
	r, client, gateway := testRouterWithClient(t)
	packet := makeTCPPacket(gateway.String(), client.String(), 443, 1000)
	for range cap(r.wg.packets) + 2 {
		r.inject(r.wg, packet)
	}
	r.inject(r.awg, packet)
	tls := r.newEngine(client)
	for range cap(tls.packets) + 1 {
		r.inject(tls, packet)
	}
	if err := tls.Close(); err != nil {
		t.Fatal(err)
	}
	r.inject(tls, packet)
	if err := r.wg.Close(); err != nil {
		t.Fatal(err)
	}
	r.inject(r.wg, packet)
	if got := (&Server{router: r}).EngineQueueDrops(); got["socks5-wg-udp"] != 2 || got["socks5-awg-udp"] != 0 || got["tls"] != 1 {
		t.Fatalf("engine queue drops = %v, want WG 2, AWG 0, TLS 1", got)
	}
}

// TestRouterFlowPinningPrecedence verifies that a pinned reverse flow wins
// over the per-IP primary route when choosing the destination engine.
func TestRouterFlowPinningPrecedence(t *testing.T) {
	r, clientIP, gatewayIP := testRouterWithClient(t)

	engA := r.newEngine(netip.Addr{})
	engB := r.newEngine(netip.Addr{})

	// Primary route points to B.
	r.remember(clientIP, engB, time.Now().UnixNano())

	// A packet forwarded by A pins the reverse flow to A.
	fwd := makeTCPPacket(clientIP.String(), gatewayIP.String(), 12345, 443)
	if _, err := engA.Write([][]byte{fwd}, 0); err != nil {
		t.Fatalf("engA Write: %v", err)
	}

	// The reverse packet must resolve to A, not B.
	rev := makeTCPPacket(gatewayIP.String(), clientIP.String(), 443, 12345)
	if got := r.destinationEngine(clientIP, rev, time.Now(), false); got != engA {
		t.Fatalf("lookup returned %v, want engA", got)
	}

	// Enqueue and read through A succeeds.
	r.inject(engA, rev)
	out := make([][]byte, 1)
	out[0] = make([]byte, 2048)
	sizes := make([]int, 1)
	if _, err := engA.Read(out, sizes, 0); err != nil || sizes[0] != len(rev) || !bytes.Equal(out[0][:sizes[0]], rev) {
		t.Fatalf("engA.Read: err=%v size=%d", err, sizes[0])
	}

	// The same packet is rejected by B because B is not the flow owner.
	r.inject(engB, rev)
	out[0] = make([]byte, 2048)
	sizes[0] = 0
	done := make(chan struct{})
	go func() {
		engB.Read(out, sizes, 0)
		close(done)
	}()
	select {
	case <-done:
		if sizes[0] != 0 {
			t.Fatal("engB accepted packet owned by engA")
		}
	case <-time.After(200 * time.Millisecond):
	}
}

func TestRouterForwardsPeerFragmentsOnPinnedReplyPath(t *testing.T) {
	r, a, _ := testRouterWithClient(t)
	b := netip.MustParseAddr("10.77.0.3")
	r.policy.mu.Lock()
	r.policy.byIP[b] = store.ClientRecord{ID: "c2", IP: b.String(), GroupID: "g1"}
	r.policy.byAddr[b] = new(trafficCounter)
	r.policy.mu.Unlock()

	now := time.Now()
	r.remember(a, r.wg, now.UnixNano())
	request := makeUDPPacket(b.String(), a.String(), 2200, 3300)
	if _, err := r.awg.Write([][]byte{request}, 0); err != nil {
		t.Fatal(err)
	}
	r.remember(b, r.wg, now.Add(time.Minute).UnixNano())
	first := makeUDPPacket(a.String(), b.String(), 3300, 2200)
	first[2], first[3] = 0, 36
	first[4], first[5], first[6] = 0, 7, 0x20
	first[24], first[25] = 0, 24 // UDP length across both fragments.
	first = first[:36]
	tail := append([]byte(nil), first[:28]...)
	tail[3], tail[6], tail[7] = 28, 0, 2 // Offset 16 bytes.
	if _, err := r.wg.Write([][]byte{first, tail}, 0); err != nil {
		t.Fatal(err)
	}
	got := make(chan bool, 1)
	go func() {
		bufs := [][]byte{make([]byte, 64), make([]byte, 64)}
		sizes := make([]int, len(bufs))
		n, err := r.awg.Read(bufs, sizes, 0)
		got <- err == nil && n == 2 && bytes.Equal(bufs[0][:sizes[0]], first) && bytes.Equal(bufs[1][:sizes[1]], tail)
	}()
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("recipient did not receive both original reply fragments")
		}
	case <-time.After(time.Second):
		t.Fatal("recipient did not receive reply fragments on its pinned AWG path")
	}
}

func TestNativeEngineReadsQueuedPackets(t *testing.T) {
	for _, mode := range []string{"wg", "awg", "carrier"} {
		t.Run(mode, func(t *testing.T) {
			r, client, gateway := testRouterWithClient(t)
			engine, want := r.wg, 2
			switch mode {
			case "awg":
				engine = r.awg
			case "carrier":
				engine, want = r.newEngine(netip.Addr{}), 1
				t.Cleanup(func() { _ = engine.Close() })
			}
			r.remember(client, engine, time.Now().UnixNano())
			first := makeTCPPacket(gateway.String(), client.String(), 443, 1000)
			stale := makeTCPPacket(gateway.String(), client.String(), 443, 1001)
			r.flows.pin(flowKey{src: gateway, dst: client, proto: 6, a: 443, b: 1001}, r.engineForMode("other"), time.Now())
			last := makeTCPPacket(gateway.String(), client.String(), 443, 1002)
			for _, packet := range [][]byte{first, stale, last} {
				r.inject(engine, packet)
			}
			const offset = 48
			var buffers [4][]byte
			var sizes [4]int
			for i := range buffers {
				buffers[i] = make([]byte, 256)
			}
			n, err := engine.Read(buffers[:], sizes[:], offset)
			if err != nil || n != want {
				t.Fatalf("Read = (%d, %v), want (%d, nil)", n, err, want)
			}
			for i, packet := range [][]byte{first, last}[:want] {
				if sizes[i] != len(packet) || !bytes.Equal(buffers[i][offset:offset+sizes[i]], packet) {
					t.Fatalf("packet %d: size=%d data=%x, want %x", i, sizes[i], buffers[i][offset:offset+sizes[i]], packet)
				}
			}
		})
	}
}

func TestHubDownlinkReusesPacketStorage(t *testing.T) {
	r, client, gateway := testRouterWithClient(t)
	r.remember(client, r.wg, time.Now().UnixNano())
	packet := makeTCPPacket(gateway.String(), client.String(), 443, 1000)
	var storage [128]byte
	bufs := [][]byte{storage[:]}
	var sizes [1]int
	allocs := testing.AllocsPerRun(100, func() {
		r.inject(r.wg, packet)
		n, err := r.wg.Read(bufs, sizes[:], 48)
		if err != nil || n != 1 || !bytes.Equal(storage[48:48+sizes[0]], packet) {
			t.Fatalf("downlink lost or changed a packet: n=%d size=%d err=%v", n, sizes[0], err)
		}
	})
	if allocs != 0 {
		t.Fatalf("reused hub buffers allocate %.2f objects per packet, want 0", allocs)
	}
}

type countRouterTUNWrites struct {
	tun.Device
	packets int
}

func (d *countRouterTUNWrites) Write(bufs [][]byte, _ int) (int, error) {
	d.packets += len(bufs)
	// Linux GRO can mutate this buffer list without retaining it.
	bufs[0] = nil
	return len(bufs), nil
}

func TestHubUploadDoesNotAllocateForUnfilteredBatch(t *testing.T) {
	gw := netip.MustParseAddr("10.77.0.1")
	ip := netip.MustParseAddr("10.77.0.2")
	inner, _, err := netstack.CreateNetTUN([]netip.Addr{gw}, nil, 1280)
	if err != nil {
		t.Fatal(err)
	}
	d := &countRouterTUNWrites{Device: inner}
	r, _, _ := testRouterWithTUN(t, d, "", ip, gw)
	packet := makeTCPPacket(ip.String(), gw.String(), 1000, 443)
	batch := [][]byte{packet, packet, packet, packet}
	allocs := testing.AllocsPerRun(100, func() {
		before := d.packets
		n, err := r.wg.Write(batch, 0)
		if err != nil || n != len(batch) || d.packets-before != len(batch) || len(batch[0]) != len(packet) {
			t.Fatalf("Write = (%d, %v), forwarded %d packets, want %d", n, err, d.packets-before, len(batch))
		}
	})
	if allocs != 0 {
		t.Fatalf("unfiltered upload allocated %.2f objects per batch, want 0", allocs)
	}
}

type segmentedRouterTUN struct {
	tun.Device
	packets chan []byte
}

func (d *segmentedRouterTUN) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	p, ok := <-d.packets
	if !ok {
		return 0, net.ErrClosed
	}
	sizes[0] = copy(bufs[0][offset:], p)
	return 1, tun.ErrTooManySegments
}

func (d *segmentedRouterTUN) Close() error {
	close(d.packets)
	return d.Device.Close()
}

func TestRouterForwardsSegmentsAfterOverflow(t *testing.T) {
	client := netip.MustParseAddr("10.77.0.2")
	gateway := netip.MustParseAddr("10.77.0.1")
	inner, _, err := netstack.CreateNetTUN([]netip.Addr{gateway}, nil, 1280)
	if err != nil {
		t.Fatal(err)
	}
	d := &segmentedRouterTUN{Device: inner, packets: make(chan []byte, 2)}
	r, _, _ := testRouterWithTUN(t, d, "", client, gateway)
	r.remember(client, r.wg, time.Now().UnixNano())
	for _, port := range []uint16{1000, 1001} {
		packet := makeTCPPacket(gateway.String(), client.String(), 443, port)
		d.packets <- packet
		got := make(chan []byte, 1)
		go func() {
			buf := make([]byte, 128)
			sizes := make([]int, 1)
			if n, err := r.wg.Read([][]byte{buf}, sizes, 0); err != nil || n != 1 {
				got <- nil
				return
			}
			got <- buf[:sizes[0]]
		}()
		select {
		case b := <-got:
			if !bytes.Equal(b, packet) {
				t.Fatalf("received %x, want %x", b, packet)
			}
		case <-time.After(time.Second):
			t.Fatal("recoverable segmentation overflow stopped forwarding")
		}
	}
}

func TestRouterDNSPreservesUnsolicitedReturnPath(t *testing.T) {
	r, clientIP, gatewayIP := testRouterWithClient(t)
	primary := r.newEngine(netip.Addr{})
	probe := r.newEngine(netip.Addr{})
	now := time.Now()
	r.remember(clientIP, primary, now.UnixNano())
	dns := makeTCPPacket(clientIP.String(), gatewayIP.String(), 12345, tunneldns.Port)
	if _, err := probe.Write([][]byte{dns}, 0); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		srcPort, dstPort uint16
		want             *engineTun
	}{{tunneldns.Port, 12345, probe}, {443, 23456, primary}} {
		packet := makeTCPPacket(gatewayIP.String(), clientIP.String(), tc.srcPort, tc.dstPort)
		if got := r.destinationEngine(clientIP, packet, now, false); got != tc.want {
			t.Fatalf("flow %d -> %d returned via the wrong path", tc.srcPort, tc.dstPort)
		}
	}
}

func TestRouterPrimaryDoesNotMoveBackInTime(t *testing.T) {
	r, client, gateway := testRouterWithClient(t)
	now := time.Now()
	r.remember(client, r.awg, now.UnixNano())
	// Another engine can finish processing an earlier batch after AWG
	// updated its primary route, even if both batches were authenticated.
	r.remember(client, r.wg, now.Add(-time.Millisecond).UnixNano())
	packet := makeTCPPacket(gateway.String(), client.String(), 443, 4321)
	if got := r.destinationEngine(client, packet, now, false); got != r.awg {
		t.Fatal("an earlier batch replaced the newest unsolicited return path")
	}
}

// TestRouterStaleEngineFallback verifies that closing an engine removes its
// pinned flows and lets subsequent reverse traffic fall back to the primary
// route.
func TestRouterStaleEngineFallback(t *testing.T) {
	r, clientIP, gatewayIP := testRouterWithClient(t)

	engA := r.newEngine(netip.Addr{})
	engB := r.newEngine(netip.Addr{})

	fwd := makeTCPPacket(clientIP.String(), gatewayIP.String(), 12345, 443)
	if _, err := engA.Write([][]byte{fwd}, 0); err != nil {
		t.Fatalf("engA Write: %v", err)
	}

	if err := engA.Close(); err != nil {
		t.Fatalf("engA Close: %v", err)
	}

	r.remember(clientIP, engB, time.Now().UnixNano())

	rev := makeTCPPacket(gatewayIP.String(), clientIP.String(), 443, 12345)
	if got := r.destinationEngine(clientIP, rev, time.Now(), false); got != engB {
		t.Fatalf("lookup returned %v after close, want engB", got)
	}
}

// TestRouterUnauthenticatedSourceCannotSpoof verifies that a TLS engine whose
// authSource is pinned to one client rejects packets with a different source
// address and does not create flow pins for the spoofed source.
func TestRouterUnauthenticatedSourceCannotSpoof(t *testing.T) {
	r, clientIP, gatewayIP := testRouterWithClient(t)
	otherIP := netip.MustParseAddr("10.77.0.3")

	// Add a second client to the policy so the spoofed source is a known IP.
	otherPub, _, err := awg.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}
	r.policy.byIP[otherIP] = store.ClientRecord{
		ID:      "c2",
		Name:    "other",
		PubKey:  otherPub,
		IP:      otherIP.String(),
		GroupID: "g1",
	}
	r.policy.byAddr[otherIP] = new(trafficCounter)

	eng := r.newEngine(clientIP)
	defer eng.Close()

	spoof := makeTCPPacket(otherIP.String(), gatewayIP.String(), 12345, 443)
	if _, err := eng.Write([][]byte{spoof}, 0); err != nil {
		t.Fatalf("Write: %v", err)
	}

	key, _ := flowKeyFromPacket(spoof)
	if _, ok := r.flows.lookup(key.reverse(), time.Now()); ok {
		t.Fatal("unauthenticated engine pinned a spoofed flow")
	}
}

func TestTLSModesBothReturnTrafficForOneIP(t *testing.T) {
	r, clientIP, gatewayIP := testRouterWithClient(t)
	first, second := r.newEngine(clientIP), r.newEngine(clientIP)
	defer first.Close()
	defer second.Close()
	r.remember(clientIP, second, time.Now().UnixNano())
	for i, e := range []*engineTun{first, second} {
		port := uint16(32000 + i)
		if _, err := e.Write([][]byte{makeTCPPacket(clientIP.String(), gatewayIP.String(), port, 443)}, 0); err != nil {
			t.Fatal(err)
		}
		reply := makeTCPPacket(gatewayIP.String(), clientIP.String(), 443, port)
		if r.destinationEngine(clientIP, reply, time.Now(), false) != e {
			t.Fatalf("reply to port %d left on the wrong TLS tunnel", port)
		}
	}
}
