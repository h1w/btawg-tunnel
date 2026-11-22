package client

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/header"
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
