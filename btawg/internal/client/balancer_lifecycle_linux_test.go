package client

import (
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"bittorrent-tunnel/btawg/internal/awg"
	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/server"
	"bittorrent-tunnel/btawg/internal/store"
	"bittorrent-tunnel/btawg/internal/tunneldns"
)

func TestManagedBalancerPoolEditsDoNotMoveLiveSessions(t *testing.T) {
	cfg := managedCfg(t)
	st, err := store.Open(t.TempDir() + "/hub.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// An IPv4 documentation address serves as a SOCKS-eligible destination
	// entirely inside the test hub. No Internet or host routes are needed.
	if err := st.SaveGroup(store.GroupRecord{ID: "echo", Name: "echo", Subnet: "192.0.2.0/24", ServerIP: "192.0.2.1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateClient(store.ClientRecord{ID: "c1", Name: "c1", PubKey: cfg.ClientPubKey, PrivKey: cfg.ClientPrivKey, IP: cfg.ClientIP, PeerID: awg.PeerID(cfg.ClientPubKey), Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	hubCfg := config.DefaultServerConfig()
	hubCfg.ExternalIP = "127.0.0.1"
	hubCfg.CarrierPort, hubCfg.AWGPort, hubCfg.WGPort = 17787, 17817, 17818
	hub, err := server.New(hubCfg, st)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	go hub.Run(t.Context())
	cfg.ServerPubKey, cfg.WGEndpoint = hub.PubKey(), "127.0.0.1:17818"
	cfg.AWGEndpoint = "127.0.0.1:17817"
	cfg.Profile = hub.Config().AWGProfile

	listen := func(addr netip.AddrPort, serve func(net.Conn)) {
		t.Helper()
		ln, err := hub.OverlayNet().ListenTCPAddrPort(addr)
		if err != nil {
			t.Fatal(err)
		}
		// hub.Close closes the overlay listeners.
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() { defer c.Close(); serve(c) }()
			}
		}()
	}
	listen(netip.AddrPortFrom(netip.MustParseAddr("10.77.0.1"), tunneldns.Port), func(net.Conn) {})
	listen(netip.MustParseAddrPort("192.0.2.1:19000"), func(c net.Conn) { _, _ = io.Copy(c, c) })
	udp, err := hub.OverlayNet().ListenUDPAddrPort(netip.MustParseAddrPort("192.0.2.1:19001"))
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	go func() {
		var b [128]byte
		for {
			n, addr, err := udp.ReadFrom(b[:])
			if err != nil {
				return
			}
			_, _ = udp.WriteTo(b[:n], addr)
		}
	}()

	auth := Credentials{Username: "operator", Password: "test-only"}
	m, err := newManagedAt(t.Context(), cfg, nil, netip.MustParseAddr("127.0.0.1"), auth, false, "btawg", 0,
		map[string][]string{"failover": {config.ModeWG, config.ModeAWG}, "adaptive": {config.ModeWG}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for deadline := time.Now().Add(15 * time.Second); m.tunnels[4].score.Load() <= 0 || m.tunnels[5].score.Load() <= 0; {
		if time.Now().After(deadline) {
			t.Fatal("both native paths did not answer the hub probe")
		}
		time.Sleep(25 * time.Millisecond)
	}
	preferred := func(want string) {
		t.Helper()
		if path := m.balance[0].pick(); path == nil || path.c.cfg.Transport != want {
			t.Fatalf("healthy Failover did not pick the configured first tunnel %s", want)
		}
	}
	preferred(config.ModeWG)
	if err := m.apply(nil, auth, map[string][]string{"failover": {config.ModeAWG, config.ModeWG}, "adaptive": {config.ModeWG}}, nil); err != nil {
		t.Fatal(err)
	}
	preferred(config.ModeAWG)
	if c, err := proxyAuth(t, m.inbound.slots[5].ln.Addr().String(), auth); err == nil {
		c.Close()
		t.Fatal("balancer-only tunnel enabled its fixed inbound")
	}
	request := func(slot int, payload []byte) (net.Conn, [10]byte) {
		t.Helper()
		c, err := proxyAuth(t, m.inbound.slots[slot].ln.Addr().String(), auth)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		if _, err := c.Write(payload); err != nil {
			t.Fatal(err)
		}
		var reply [10]byte
		if _, err := io.ReadFull(c, reply[:]); err != nil {
			t.Fatal(err)
		}
		return c, reply
	}
	connect := []byte{5, 1, 0, 1, 192, 0, 2, 1, 0x4a, 0x38}
	stream, reply := request(12, connect)
	if reply[1] != 0 {
		t.Fatalf("Adaptive TCP CONNECT: %v", reply)
	}
	assoc, reply := request(12, []byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0})
	if reply[1] != 0 {
		t.Fatalf("Adaptive UDP ASSOCIATE: %v", reply)
	}
	defer assoc.Close()
	pc, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IP(reply[4:8]), Port: int(binary.BigEndian.Uint16(reply[8:]))})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	pc.SetDeadline(time.Now().Add(3 * time.Second))

	if err := m.apply(nil, auth, map[string][]string{"failover": {config.ModeWG}, "adaptive": {}}, nil); err != nil {
		t.Fatal(err)
	}
	if c, reply := request(12, connect); reply[1] == 0 {
		c.Close()
		t.Fatal("new CONNECT used removed pool member")
	}
	if c, reply := request(10, connect); reply[1] != 0 {
		c.Close()
		t.Fatal("editing Adaptive changed Failover")
	} else {
		c.Close()
	}
	if _, err := stream.Write([]byte("tcp")); err != nil {
		t.Fatal(err)
	}
	var tcp [3]byte
	if _, err := io.ReadFull(stream, tcp[:]); err != nil || string(tcp[:]) != "tcp" {
		t.Fatalf("existing TCP flow stopped after pool edit: %q, %v", tcp, err)
	}
	if _, err := pc.Write([]byte{0, 0, 0, 1, 192, 0, 2, 1, 0x4a, 0x39, 'u', 'd', 'p'}); err != nil {
		t.Fatal(err)
	}
	var datagram [128]byte
	if n, err := pc.Read(datagram[:]); err != nil || n != 13 || string(datagram[10:n]) != "udp" {
		t.Fatalf("existing UDP association stopped after pool edit: %x, %v", datagram[:n], err)
	}
	if err := m.apply(nil, auth, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range []net.Conn{stream, assoc} {
		c.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := c.Read(tcp[:]); err == nil {
			t.Fatal("session remained open after its tunnel was retired")
		} else if err, ok := err.(net.Error); ok && err.Timeout() {
			t.Fatal("session did not close promptly on tunnel retirement")
		}
	}
}
