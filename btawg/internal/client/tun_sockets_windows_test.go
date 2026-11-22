package client

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
	"gvisor.dev/gvisor/pkg/tcpip/header"

	"bittorrent-tunnel/btawg/internal/config"
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
