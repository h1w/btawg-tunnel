package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"

	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/hubrelay"
	"bittorrent-tunnel/btawg/internal/mesh"
)

func TestWindowsOverlayPortExcludesKernelWildcard(t *testing.T) {
	for _, tc := range []struct {
		name  string
		proto byte
	}{{"tcp", 6}, {"udp", 17}} {
		t.Run(tc.name, func(t *testing.T) {
			port, release, err := reserveOverlayPort(netip.MustParseAddr("127.0.0.1"), tc.proto)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			addr := net.JoinHostPort("0.0.0.0", strconv.Itoa(int(port)))
			if tc.proto == 6 {
				if ln, err := net.Listen("tcp4", addr); err == nil {
					ln.Close()
					t.Fatal("kernel listener acquired the overlay TCP source port")
				}
			} else if pc, err := net.ListenPacket("udp4", addr); err == nil {
				pc.Close()
				t.Fatal("kernel listener acquired the overlay UDP source port")
			}
		})
	}
}

func TestWindowsOverlayUDPReplySurvivesHubPathChange(t *testing.T) {
	local := netip.MustParseAddr("10.77.0.2")
	remote := netip.MustParseAddrPort("203.0.113.8:1053")
	s := &sharedTUN{mtu: 1280, ports: make(map[localPort]*socketPath), done: make(chan struct{})}
	s.pool.New = func() any { return newPacketBuffer(s.mtu) }
	a, b := s.newPath(), s.newPath()
	t.Cleanup(func() {
		a.Close()
		b.Close()
		for _, owner := range s.ports {
			owner.release()
		}
	})
	var ca, cb Client
	if err := a.proxySockets(&ca, local); err != nil {
		t.Fatal(err)
	}
	if err := b.proxySockets(&cb, local); err != nil {
		t.Fatal(err)
	}
	for i, c := range []*Client{&ca, &cb} {
		pc, err := c.proxyUDP(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer pc.Close()
		path, ingress := a, b
		if i == 1 {
			path, ingress = b, a
		}
		if _, err := pc.WriteTo([]byte{byte(i)}, net.UDPAddrFromAddrPort(remote)); err != nil {
			t.Fatal(err)
		}
		var port uint16
		select {
		case wire := <-path.packets:
			p := (*wire)[packetHeadroom:]
			port = binary.BigEndian.Uint16(p[20:22])
			s.pool.Put(wire)
		case <-time.After(time.Second):
			t.Fatal("UDP request went to the wrong transport")
		}
		p := testOverlayPacket(17, remote.Addr().As4(), local.As4(), remote.Port(), port)
		payload := []byte{byte(i), 0x55}
		p = append(p, payload...)
		p[8] = 64
		binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
		binary.BigEndian.PutUint16(p[24:26], uint16(len(p)-20))
		binary.BigEndian.PutUint16(p[10:12], ^header.IPv4(p).CalculateChecksum())
		if !ingress.deliverOverlay(p) {
			t.Fatal("proxy reply fell through to Wintun")
		}
		_ = pc.SetReadDeadline(time.Now().Add(time.Second))
		var got [8]byte
		n, from, err := pc.ReadFrom(got[:])
		if err != nil || !bytes.Equal(got[:n], payload) || from.String() != remote.String() {
			t.Fatalf("reply to owned overlay socket = %x from %v, %v", got[:n], from, err)
		}
	}
}

func TestWindowsProxySocketsUseHubRelayForTCP(t *testing.T) {
	local := netip.MustParseAddr("10.77.0.2")
	gateway := netip.MustParseAddr("10.77.0.1")
	s := &sharedTUN{mtu: 1280, ports: make(map[localPort]*socketPath), done: make(chan struct{})}
	s.pool.New = func() any { return newPacketBuffer(s.mtu) }
	p := s.newPath()
	t.Cleanup(func() {
		p.Close()
		for _, owner := range s.ports {
			owner.release()
		}
	})
	var c Client
	if err := p.proxySockets(&c, local); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	dialed := make(chan error, 1)
	go func() {
		conn, err := c.dialSOCKSData(ctx, gateway, netip.MustParseAddrPort("93.184.216.77:443"))
		if err == nil {
			_ = conn.Close()
		}
		dialed <- err
	}()
	select {
	case wire := <-p.packets:
		packet := (*wire)[packetHeadroom:]
		defer s.pool.Put(wire)
		if len(packet) < 24 || packet[9] != 6 || !bytes.Equal(packet[12:16], local.AsSlice()) || !bytes.Equal(packet[16:20], gateway.AsSlice()) {
			t.Fatalf("relay SYN = %x, want %s -> %s TCP", packet, local, gateway)
		}
		if port := binary.BigEndian.Uint16(packet[22:24]); port != hubrelay.Port {
			t.Fatalf("relay destination port = %d, want %d", port, hubrelay.Port)
		}
	case <-ctx.Done():
		t.Fatal("native SOCKS dial did not use the overlay TCP path")
	}
	cancel()
	select {
	case <-dialed:
	case <-time.After(time.Second):
		t.Fatal("overlay TCP dial survived cancellation")
	}
}

const (
	overlayTCPSyn = byte(0x02)
	overlayTCPRst = byte(0x04)
	overlayTCPPsh = byte(0x08)
	overlayTCPAck = byte(0x10)
)

func readOverlayPacket(t *testing.T, p *tunPath, s *sharedTUN, timeout time.Duration) []byte {
	t.Helper()
	select {
	case wire := <-p.packets:
		packet := bytes.Clone((*wire)[packetHeadroom:])
		s.pool.Put(wire)
		return packet
	case <-time.After(timeout):
		t.Fatal("timeout waiting for overlay TCP packet")
		return nil
	}
}

func overlayTCPPacket(src, dst netip.Addr, sport, dport uint16, seq, ack uint32, flags byte, payload []byte) []byte {
	p := make([]byte, 40+len(payload))
	p[0], p[9] = 0x45, 6
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	p[8] = 64
	copy(p[12:16], src.AsSlice())
	copy(p[16:20], dst.AsSlice())
	tcp := p[20:]
	binary.BigEndian.PutUint16(tcp[0:2], sport)
	binary.BigEndian.PutUint16(tcp[2:4], dport)
	binary.BigEndian.PutUint32(tcp[4:8], seq)
	binary.BigEndian.PutUint32(tcp[8:12], ack)
	tcp[12] = 5 << 4
	tcp[13] = flags
	binary.BigEndian.PutUint16(tcp[14:16], 65535)
	copy(tcp[20:], payload)
	partial := header.PseudoHeaderChecksum(header.TCPProtocolNumber, tcpip.AddrFrom4(src.As4()), tcpip.AddrFrom4(dst.As4()), uint16(20+len(payload)))
	partial = checksum.Combine(partial, checksum.Checksum(payload, 0))
	sum := header.TCP(tcp).CalculateChecksum(partial)
	header.TCP(tcp).SetChecksum(^sum)
	binary.BigEndian.PutUint16(p[10:12], ^header.IPv4(p).CalculateChecksum())
	return p
}

func TestWindowsHubRelayRefusedFallsBackToDestination(t *testing.T) {
	local := netip.MustParseAddr("10.77.0.2")
	gateway := netip.MustParseAddr("10.77.0.1")
	target := netip.MustParseAddrPort("93.184.216.77:443")
	s := &sharedTUN{mtu: 1280, ports: make(map[localPort]*socketPath), done: make(chan struct{})}
	s.pool.New = func() any { return newPacketBuffer(s.mtu) }
	p := s.newPath()
	t.Cleanup(func() {
		p.Close()
		for _, owner := range s.ports {
			owner.release()
		}
	})
	var c Client
	if err := p.proxySockets(&c, local); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	dialed := make(chan error, 1)
	go func() {
		conn, err := c.dialSOCKSData(ctx, gateway, target)
		if err == nil {
			_ = conn.Close()
		}
		dialed <- err
	}()
	syn := readOverlayPacket(t, p, s, time.Second)
	if len(syn) < 28 || syn[9] != 6 || !bytes.Equal(syn[12:16], local.AsSlice()) || !bytes.Equal(syn[16:20], gateway.AsSlice()) {
		t.Fatalf("relay SYN = %x, want %s -> %s TCP", syn, local, gateway)
	}
	if binary.BigEndian.Uint16(syn[22:24]) != hubrelay.Port {
		t.Fatalf("first destination port = %d, want relay %d", binary.BigEndian.Uint16(syn[22:24]), hubrelay.Port)
	}
	clientPort := binary.BigEndian.Uint16(syn[20:22])
	clientSeq := binary.BigEndian.Uint32(syn[24:28])
	rst := overlayTCPPacket(gateway, local, hubrelay.Port, clientPort, clientSeq+1, clientSeq+1, overlayTCPRst|overlayTCPAck, nil)
	if !p.deliverOverlay(rst) {
		t.Fatal("relay RST had no overlay socket")
	}
	var fallback []byte
	for range 4 {
		packet := readOverlayPacket(t, p, s, time.Second)
		if len(packet) >= 24 && packet[9] == 6 && bytes.Equal(packet[16:20], target.Addr().AsSlice()) && binary.BigEndian.Uint16(packet[22:24]) == target.Port() {
			fallback = packet
			break
		}
	}
	if fallback == nil {
		t.Fatal("connection refused did not fall back to a destination SYN")
	}
	cancel()
	select {
	case <-dialed:
	case <-time.After(time.Second):
		t.Fatal("fallback overlay TCP dial survived cancellation")
	}
}

func TestWindowsHubRelayDenyDoesNotRetryDestination(t *testing.T) {
	local := netip.MustParseAddr("10.77.0.2")
	gateway := netip.MustParseAddr("10.77.0.1")
	target := netip.MustParseAddrPort("93.184.216.77:443")
	s := &sharedTUN{mtu: 1280, ports: make(map[localPort]*socketPath), done: make(chan struct{})}
	s.pool.New = func() any { return newPacketBuffer(s.mtu) }
	p := s.newPath()
	t.Cleanup(func() {
		p.Close()
		for _, owner := range s.ports {
			owner.release()
		}
	})
	var c Client
	if err := p.proxySockets(&c, local); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	dialed := make(chan error, 1)
	go func() {
		conn, err := c.dialSOCKSData(ctx, gateway, target)
		if err == nil {
			_ = conn.Close()
		}
		dialed <- err
	}()
	syn := readOverlayPacket(t, p, s, time.Second)
	if len(syn) < 28 || binary.BigEndian.Uint16(syn[22:24]) != hubrelay.Port {
		t.Fatalf("first SYN = %x, want relay port %d", syn, hubrelay.Port)
	}
	clientPort := binary.BigEndian.Uint16(syn[20:22])
	clientSeq := binary.BigEndian.Uint32(syn[24:28])
	serverSeq := uint32(1000)
	synack := overlayTCPPacket(gateway, local, hubrelay.Port, clientPort, serverSeq, clientSeq+1, overlayTCPSyn|overlayTCPAck, nil)
	if !p.deliverOverlay(synack) {
		t.Fatal("relay SYN-ACK had no overlay socket")
	}
	var request []byte
	for range 4 {
		packet := readOverlayPacket(t, p, s, time.Second)
		if len(packet) < 40 {
			continue
		}
		offset := 20 + int(packet[32]>>4)*4
		if len(packet) > offset {
			request = packet
			break
		}
	}
	if request == nil {
		t.Fatal("relay SOCKS request was not sent")
	}
	offset := 20 + int(request[32]>>4)*4
	requestSeq := binary.BigEndian.Uint32(request[24:28])
	denied := overlayTCPPacket(gateway, local, hubrelay.Port, clientPort, serverSeq+1, requestSeq+uint32(len(request)-offset), overlayTCPAck|overlayTCPPsh, []byte{byte(hubrelay.Denied)})
	if !p.deliverOverlay(denied) {
		t.Fatal("relay denial had no overlay socket")
	}
	select {
	case err := <-dialed:
		if err == nil {
			t.Fatal("relay denial was treated as an established SOCKS session")
		}
	case <-time.After(time.Second):
		t.Fatal("relay denial did not finish the SOCKS dial")
	}
	// The dial has finished; ACK/FIN to the relay are normal TCP teardown.
	for {
		select {
		case wire := <-p.packets:
			packet := (*wire)[packetHeadroom:]
			direct := len(packet) >= 24 && bytes.Equal(packet[16:20], target.Addr().AsSlice()) && binary.BigEndian.Uint16(packet[22:24]) == target.Port()
			s.pool.Put(wire)
			if direct {
				t.Fatal("relay denial retried the destination")
			}
		default:
			return
		}
	}
}

type probeTestTUN struct {
	tun.Device
	writes int
}

func (d *probeTestTUN) Write(bufs [][]byte, _ int) (int, error) {
	d.writes += len(bufs)
	return len(bufs), nil
}

func TestWindowsDirectProbeNeverNeedsWintun(t *testing.T) {
	cfg := config.ClientConfig{ClientIP: "10.77.0.2", ServerIP: "10.77.0.1", Subnet: "10.77.0.0/24"}
	local, peer := netip.MustParseAddr(cfg.ClientIP), netip.MustParseAddr("10.77.0.3")
	dev := &probeTestTUN{}
	s := &sharedTUN{dev: dev, mtu: 1280, done: make(chan struct{})}
	s.pool.New = func() any { return newPacketBuffer(s.mtu) }
	s.enableDirect(cfg)
	s.applyPeerPolicies(compilePeerPolicies(PeerSettings{Incoming: []string{config.ModeExtUTP}}))
	p := s.newPath()
	fixture := directFlowPath(t, config.ModeExtUTP, false)
	p.directLife, p.directUntil, p.peerMode = fixture.directLife, fixture.directUntil, fixture.peerMode
	t.Cleanup(func() { p.Close() })
	link := &directTun{tunPath: p, local: local, peer: peer}
	probe := peerTCP(peer, local, 19600, mesh.PeerProbePort, 0x02)
	probe[8] = 64
	binary.BigEndian.PutUint16(probe[10:12], ^header.IPv4(probe).CalculateChecksum())
	peerIP, localIP := header.IPv4(probe).SourceAddress(), header.IPv4(probe).DestinationAddress()
	binary.BigEndian.PutUint16(probe[36:38], ^header.TCP(probe[20:]).CalculateChecksum(header.PseudoHeaderChecksum(header.TCPProtocolNumber, peerIP, localIP, 20)))
	if _, err := link.Write([][]byte{probe}, 0); err != nil {
		t.Fatal(err)
	}
	if dev.writes != 0 {
		t.Fatal("authenticated probe was sent to Windows Firewall through Wintun")
	}
	select {
	case buf := <-p.packets:
		reply := (*buf)[packetHeadroom:]
		defer s.pool.Put(buf)
		ip := header.IPv4(reply)
		tcp := header.TCP(reply[ip.HeaderLength():])
		if ip.SourceAddress() != localIP || ip.DestinationAddress() != peerIP || tcp.SourcePort() != mesh.PeerProbePort || tcp.DestinationPort() != 19600 || tcp.Flags() != header.TCPFlagSyn|header.TCPFlagAck {
			t.Fatalf("invalid direct probe reply: %x", reply)
		}
	case <-time.After(time.Second):
		t.Fatal("probe did not receive a SYN-ACK on its authenticated path")
	}
	if _, err := link.Write([][]byte{peerTCP(peer, local, 19601, 19722, 0x02)}, 0); err != nil || dev.writes != 1 {
		t.Fatalf("ordinary traffic did not reach Wintun: writes=%d err=%v", dev.writes, err)
	}
}
