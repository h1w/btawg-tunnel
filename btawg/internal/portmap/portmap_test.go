package portmap

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"
)

var testLocal = netip.MustParseAddr("127.0.0.1")

type gatewayPacket struct {
	data []byte
	from *net.UDPAddr
}

func testGateway(t *testing.T) (*net.UDPConn, <-chan gatewayPacket) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(testLocal, 0)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	packets := make(chan gatewayPacket, 8)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		defer close(packets)
		var buf [2048]byte
		for {
			n, from, err := conn.ReadFromUDP(buf[:])
			if err != nil {
				return
			}
			select {
			case packets <- gatewayPacket{bytes.Clone(buf[:n]), from}:
			case <-done:
				return
			}
		}
	}()
	return conn, packets
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value, ok := <-ch:
		if !ok {
			t.Fatal("gateway closed")
		}
		return value
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for gateway or lease report")
		var zero T
		return zero
	}
}

func respond(t *testing.T, conn *net.UDPConn, p gatewayPacket, data []byte) {
	t.Helper()
	if _, err := conn.WriteToUDP(data, p.from); err != nil {
		t.Fatal(err)
	}
}

func startTestMapping(t *testing.T, r router, port uint16) (context.CancelFunc, <-chan []netip.AddrPort, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	reports := make(chan []netip.AddrPort, 8)
	done := make(chan error, 1)
	go func() {
		done <- maintain(ctx, r, port, func(addrs []netip.AddrPort) { reports <- addrs })
	}()
	if got := receive(t, reports); len(got) != 0 {
		t.Fatalf("initial report = %v, want empty", got)
	}
	return cancel, reports, done
}

func checkReport(t *testing.T, reports <-chan []netip.AddrPort, want string) {
	t.Helper()
	got := receive(t, reports)
	if want == "" && len(got) == 0 || want != "" && len(got) == 1 && got[0] == netip.MustParseAddrPort(want) {
		return
	}
	t.Fatalf("report = %v, want %q", got, want)
}

func TestPCPAuthenticatesRenewalAndDeletesOnlyItsUDPMap(t *testing.T) {
	conn, packets := testGateway(t)
	port := uint16(40210)
	cancel, reports, done := startTestMapping(t, router{source: testLocal, gateway: testLocal, natPort: conn.LocalAddr().(*net.UDPAddr).AddrPort().Port()}, port)
	announce := receive(t, packets)
	if len(announce.data) != 24 || announce.data[0] != 2 || announce.data[1] != 0 {
		t.Fatalf("invalid PCP ANNOUNCE: %x", announce.data)
	}
	announced := make([]byte, 24)
	announced[0], announced[1] = 2, 0x80
	respond(t, conn, announce, announced)

	mapReq := receive(t, packets)
	if len(mapReq.data) != 60 || mapReq.data[0] != 2 || mapReq.data[1] != 1 || mapReq.data[36] != 17 || binary.BigEndian.Uint16(mapReq.data[40:]) != port || binary.BigEndian.Uint32(mapReq.data[4:]) == 0 {
		t.Fatalf("invalid PCP UDP MAP: %x", mapReq.data)
	}
	if !bytes.Equal(mapReq.data[8:24], netip.MustParseAddr("::ffff:127.0.0.1").AsSlice()) || bytes.Equal(mapReq.data[24:36], make([]byte, 12)) {
		t.Fatalf("invalid PCP client address or nonce: %x", mapReq.data)
	}
	response := func(req gatewayPacket, external string, seconds uint32) []byte {
		data := make([]byte, 60)
		data[0], data[1] = 2, 0x81
		binary.BigEndian.PutUint32(data[4:], seconds)
		copy(data[24:42], req.data[24:42])
		if external != "" {
			endpoint := netip.MustParseAddrPort(external)
			binary.BigEndian.PutUint16(data[42:], endpoint.Port())
			copy(data[44:], netip.AddrFrom16(endpoint.Addr().As16()).AsSlice())
		}
		return data
	}
	wrong := response(mapReq, "1.1.1.1:40211", 2)
	wrong[24] ^= 0xff
	respond(t, conn, mapReq, wrong)
	spoof, _ := testGateway(t)
	respond(t, spoof, mapReq, response(mapReq, "1.0.0.1:40212", 2))
	respond(t, conn, mapReq, response(mapReq, "8.8.4.4:40215", 2))
	checkReport(t, reports, "8.8.4.4:40215")

	renew := receive(t, packets)
	if !bytes.Equal(renew.data[24:42], mapReq.data[24:42]) || binary.BigEndian.Uint16(renew.data[42:]) != 40215 || !bytes.Equal(renew.data[44:], netip.MustParseAddr("::ffff:8.8.4.4").AsSlice()) {
		t.Fatalf("renew did not carry the same nonce and current endpoint: %x", renew.data)
	}
	respond(t, conn, renew, response(renew, "8.8.4.4:40215", 10))
	cancel()
	checkReport(t, reports, "") // The unchanged renewal must not publish a duplicate.
	remove := receive(t, packets)
	if !bytes.Equal(remove.data[24:42], mapReq.data[24:42]) || !bytes.Equal(remove.data[4:8], make([]byte, 4)) || !bytes.Equal(remove.data[42:], make([]byte, 18)) {
		t.Fatalf("delete did not use the same scoped UDP MAP and zero lifetime/suggestion: %x", remove.data)
	}
	respond(t, conn, remove, response(remove, "", 0))
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestNATPMPFallbackRenewsAndDeletesOnlyItsUDPMap(t *testing.T) {
	conn, packets := testGateway(t)
	port := uint16(40310)
	cancel, reports, done := startTestMapping(t, router{source: testLocal, gateway: testLocal, natPort: conn.LocalAddr().(*net.UDPAddr).AddrPort().Port()}, port)
	announce := receive(t, packets)
	respond(t, conn, announce, []byte{0, 128, 0, 1, 0, 0, 0, 22})
	wan := func() {
		p := receive(t, packets)
		if !bytes.Equal(p.data, []byte{0, 0}) {
			t.Fatalf("invalid NAT-PMP WAN address query: %x", p.data)
		}
		respond(t, conn, p, []byte{0, 128, 0, 0, 0, 0, 0, 22, 9, 9, 9, 9})
	}
	mapping := func(suggestion, assigned uint16, seconds uint32) {
		p := receive(t, packets)
		if len(p.data) != 12 || !bytes.Equal(p.data[:4], []byte{0, 1, 0, 0}) || binary.BigEndian.Uint16(p.data[4:]) != port || binary.BigEndian.Uint16(p.data[6:]) != suggestion || (binary.BigEndian.Uint32(p.data[8:]) == 0) != (seconds == 0) {
			t.Fatalf("invalid NAT-PMP UDP mapping request: %x", p.data)
		}
		data := []byte{0, 129, 0, 0, 0, 0, 0, 22, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint16(data[8:], port+1)
		binary.BigEndian.PutUint16(data[10:], assigned)
		binary.BigEndian.PutUint32(data[12:], seconds)
		respond(t, conn, p, data) // Wrong internal port must not be accepted.
		binary.BigEndian.PutUint16(data[8:], port)
		respond(t, conn, p, data)
	}
	wan()
	mapping(port, 40315, 2)
	checkReport(t, reports, "9.9.9.9:40315")
	wan()
	mapping(40315, 40316, 10)
	checkReport(t, reports, "9.9.9.9:40316")
	cancel()
	checkReport(t, reports, "")
	wan() // A fresh epoch check protects against deleting a mapping after a router reset.
	mapping(0, 0, 0)
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestPCPExpiryClearsReportAndDoesNotDeleteStaleLease(t *testing.T) {
	conn, packets := testGateway(t)
	cancel, reports, done := startTestMapping(t, router{source: testLocal, gateway: testLocal, natPort: conn.LocalAddr().(*net.UDPAddr).AddrPort().Port()}, 40410)
	announce := receive(t, packets)
	reply := make([]byte, 24)
	reply[0], reply[1] = 2, 0x80
	respond(t, conn, announce, reply)
	p := receive(t, packets)
	reply = make([]byte, 60)
	reply[0], reply[1] = 2, 0x81
	binary.BigEndian.PutUint32(reply[4:], 1)
	copy(reply[24:42], p.data[24:42])
	binary.BigEndian.PutUint16(reply[42:], 40411)
	copy(reply[44:], netip.MustParseAddr("::ffff:9.9.9.9").AsSlice())
	respond(t, conn, p, reply)
	checkReport(t, reports, "9.9.9.9:40411")
	p = receive(t, packets)
	if binary.BigEndian.Uint32(p.data[4:]) == 0 {
		t.Fatalf("asked to delete rather than renew: %x", p.data)
	}
	// Do not respond to the renewal; the last acknowledged lease must expire.
	checkReport(t, reports, "")
	cancel()
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-packets:
		t.Fatalf("sent a packet after expiration without verified renewal: %x", p.data)
	default:
	}
}

func TestCanceledDiscoveryDoesNotSendMapping(t *testing.T) {
	conn, packets := testGateway(t)
	cancel, _, done := startTestMapping(t, router{source: testLocal, gateway: testLocal, natPort: conn.LocalAddr().(*net.UDPAddr).AddrPort().Port()}, 40510)
	p := receive(t, packets)
	if len(p.data) != 24 || p.data[1] != 0 {
		t.Fatalf("first packet must be read-only discovery, got %x", p.data)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not interrupt discovery")
	}
	select {
	case p := <-packets:
		t.Fatalf("sent unexpected packet during cancellation: %x", p.data)
	default:
	}
}

func TestRunRejectsZeroInternalPort(t *testing.T) {
	called := false
	err := Run(context.Background(), 0, false, func(addrs []netip.AddrPort) { called = len(addrs) == 0 })
	if err == nil || !called {
		t.Fatalf("zero listener port: got err %v, cleared report %t", err, called)
	}
}

func TestProbeFailureIsNonfatal(t *testing.T) {
	conn, packets := testGateway(t)
	discovery, ssdp := testGateway(t)
	cancel, reports, done := startTestMapping(t, router{source: testLocal, gateway: testLocal, natPort: conn.LocalAddr().(*net.UDPAddr).AddrPort().Port(), ssdp: discovery.LocalAddr().(*net.UDPAddr).AddrPort()}, 40512)
	p := receive(t, packets)
	respond(t, conn, p, []byte{0, 128, 0, 1, 0, 0, 0, 22})
	p = receive(t, packets)
	if !bytes.Equal(p.data, []byte{0, 0}) {
		t.Fatalf("want read-only NAT-PMP probe, got %x", p.data)
	}
	respond(t, conn, p, []byte{0, 128, 0, 5, 0, 0, 0, 22})
	_ = receive(t, ssdp)
	cancel()
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-reports:
		t.Fatalf("published endpoint without a mapping: %v", value)
	default:
	}
}

func TestUDPTransactionsTimeOut(t *testing.T) {
	conn, _ := testGateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r := router{source: testLocal, gateway: testLocal, natPort: conn.LocalAddr().(*net.UDPAddr).AddrPort().Port()}
	err := r.udpRequest(ctx, []byte{0, 0}, false, func([]byte) (bool, error) { return false, fmt.Errorf("unexpected datagram") })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("silent gateway: got %v, want context deadline exceeded", err)
	}
}

func TestNATPMPDoesNotDeleteAfterEpochReset(t *testing.T) {
	conn, packets := testGateway(t)
	cancel, reports, done := startTestMapping(t, router{source: testLocal, gateway: testLocal, natPort: conn.LocalAddr().(*net.UDPAddr).AddrPort().Port()}, 40610)
	p := receive(t, packets)
	respond(t, conn, p, []byte{0, 128, 0, 1, 0, 0, 1, 0})
	p = receive(t, packets)
	respond(t, conn, p, []byte{0, 128, 0, 0, 0, 0, 1, 0, 8, 8, 4, 4})
	p = receive(t, packets)
	if len(p.data) != 12 || p.data[1] != 1 || binary.BigEndian.Uint16(p.data[4:]) != 40610 {
		t.Fatalf("expected scoped UDP mapping, got %x", p.data)
	}
	respond(t, conn, p, []byte{0, 129, 0, 0, 0, 0, 1, 0, 0x9e, 0xa2, 0x9e, 0xa3, 0, 0, 0, 120})
	checkReport(t, reports, "8.8.4.4:40611")
	cancel()
	checkReport(t, reports, "")
	p = receive(t, packets)
	if !bytes.Equal(p.data, []byte{0, 0}) {
		t.Fatalf("cleanup did not first check gateway epoch: %x", p.data)
	}
	respond(t, conn, p, []byte{0, 128, 0, 0, 0, 0, 0, 0, 8, 8, 4, 4})
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-packets:
		t.Fatalf("deleted mapping after gateway reset: %x", p.data)
	default:
	}
}

func TestExternalEndpointsRejectInvalidWanAddresses(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:123", "0.0.0.4:123", "127.0.0.1:123", "192.168.2.1:123", "169.254.2.1:123", "100.127.255.255:123", "192.0.2.2:123", "198.19.2.1:123", "198.51.100.1:123", "203.0.113.2:123", "224.1.2.3:123", "240.1.2.3:123", "255.255.255.255:123", "8.8.8.8:0", "[2001:4860::1]:123"} {
		if usableEndpoint(netip.MustParseAddrPort(addr)) {
			t.Errorf("accepted invalid/unusable WAN endpoint %s", addr)
		}
	}
	if !usableEndpoint(netip.MustParseAddrPort("8.8.8.8:123")) {
		t.Error("rejected globally usable IPv4 endpoint")
	}
}

func TestPCPDoesNotDeleteStaticMapping(t *testing.T) {
	conn, packets := testGateway(t)
	cancel, reports, done := startTestMapping(t, router{source: testLocal, gateway: testLocal, natPort: conn.LocalAddr().(*net.UDPAddr).AddrPort().Port()}, 40710)
	p := receive(t, packets)
	announce := make([]byte, 24)
	announce[0], announce[1] = 2, 0x80
	respond(t, conn, p, announce)
	p = receive(t, packets)
	reply := make([]byte, 60)
	reply[0], reply[1] = 2, 0x81
	binary.BigEndian.PutUint32(reply[4:], ^uint32(0))
	copy(reply[24:42], p.data[24:42])
	binary.BigEndian.PutUint16(reply[42:], 40711)
	copy(reply[44:], netip.MustParseAddr("::ffff:9.9.9.9").AsSlice())
	respond(t, conn, p, reply)
	checkReport(t, reports, "9.9.9.9:40711")
	cancel()
	checkReport(t, reports, "")
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-packets:
		t.Fatalf("attempted to delete static mapping: %x", p.data)
	default:
	}
}

func TestUnacknowledgedPCPWriteDoesNotSwitchProtocolsOrDelete(t *testing.T) {
	conn, packets := testGateway(t)
	cancel, reports, done := startTestMapping(t, router{source: testLocal, gateway: testLocal, natPort: conn.LocalAddr().(*net.UDPAddr).AddrPort().Port()}, 40810)
	p := receive(t, packets)
	announce := make([]byte, 24)
	announce[0], announce[1] = 2, 0x80
	respond(t, conn, p, announce)
	p = receive(t, packets)
	if len(p.data) != 60 || p.data[1] != 1 || binary.BigEndian.Uint32(p.data[4:]) == 0 {
		t.Fatalf("want positive-lifetime PCP MAP, got %x", p.data)
	}
	select {
	case p := <-packets:
		t.Fatalf("sent an unrelated command after an ambiguous write: %x", p.data)
	case <-time.After(2200 * time.Millisecond):
	}
	cancel()
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-reports:
		t.Fatalf("reported unacknowledged mapping: %v", value)
	default:
	}
	select {
	case p := <-packets:
		t.Fatalf("sent unexpected cleanup for unacknowledged mapping: %x", p.data)
	default:
	}
}

func TestPCPMapRejectionAllowsNATPMPFallback(t *testing.T) {
	conn, packets := testGateway(t)
	cancel, _, done := startTestMapping(t, router{source: testLocal, gateway: testLocal, natPort: conn.LocalAddr().(*net.UDPAddr).AddrPort().Port()}, 40811)
	p := receive(t, packets)
	announce := make([]byte, 24)
	announce[0], announce[1] = 2, 0x80
	respond(t, conn, p, announce)
	p = receive(t, packets)
	if len(p.data) != 60 || p.data[1] != 1 || binary.BigEndian.Uint32(p.data[4:]) == 0 {
		t.Fatalf("want positive-lifetime PCP MAP, got %x", p.data)
	}
	reply := make([]byte, 60)
	reply[0], reply[1], reply[3] = 2, 0x81, 2
	copy(reply[24:], p.data[24:])
	respond(t, conn, p, reply)
	p = receive(t, packets)
	if !bytes.Equal(p.data, []byte{0, 0}) {
		t.Fatalf("expected read-only NAT-PMP fallback after rejected PCP MAP, got %x", p.data)
	}
	cancel()
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyNATPMPVersionReplyTriggersPromptFallback(t *testing.T) {
	conn, packets := testGateway(t)
	cancel, _, done := startTestMapping(t, router{source: testLocal, gateway: testLocal, natPort: conn.LocalAddr().(*net.UDPAddr).AddrPort().Port()}, 40910)
	p := receive(t, packets)
	respond(t, conn, p, []byte{0, 0, 0, 1, 0, 0, 0, 22}) // RFC 6886 Figure: Unsupported Version.
	p = receive(t, packets)
	if !bytes.Equal(p.data, []byte{0, 0}) {
		t.Fatalf("expected NAT-PMP address request without retransmitting PCP, got %x", p.data)
	}
	cancel()
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
}
