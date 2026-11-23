package server

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"
	utp "github.com/anacrolix/go-utp"

	"bittorrent-tunnel/btawg/internal/awg"
	"bittorrent-tunnel/btawg/internal/bind"
	"bittorrent-tunnel/btawg/internal/carrier"
	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/store"
)

// TestServerClientEcho drives the hub end to end over an extended µTP
// carrier: one registered client AWG device connects through the carrier
// listener, completes the AWG handshake, and exchanges a UDP echo with a
// listener inside the server's overlay netstack.
func TestServerClientEcho(t *testing.T) {

	st, err := store.Open(t.TempDir() + "/hub.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.DefaultServerConfig()
	cfg.Transport = "ext-utp"
	cfg.CarrierPort = 17777
	cfg.AWGPort, cfg.WGPort = 17801, 17802

	clientPriv, clientPub, err := awg.GenerateKeyPair()
	if err != nil {
		t.Fatalf("client key: %v", err)
	}
	rec := store.ClientRecord{
		ID:      "c1",
		Name:    "smoke",
		PubKey:  clientPub,
		PrivKey: clientPriv,
		IP:      "10.77.0.2",
		PeerID:  awg.PeerID(clientPub),
		Created: time.Unix(1, 0),
	}
	if err := st.CreateClient(rec); err != nil {
		t.Fatalf("create client: %v", err)
	}

	srv, err := New(cfg, st)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	if len(srv.InfoHash()) != 40 {
		t.Fatalf("infohash %q is not 20 bytes", srv.InfoHash())
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Run(ctx)

	// UDP echo inside the server's overlay.
	echoLn, err := srv.OverlayNet().ListenUDPAddrPort(netip.MustParseAddrPort("10.77.0.1:9000"))
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { echoLn.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := echoLn.ReadFrom(buf)
			if err != nil {
				return
			}
			if _, err := echoLn.WriteTo(buf[:n], from); err != nil {
				return
			}
		}
	}()

	// Client AWG device over its own netstack, routed through one carrier.
	clientIP := netip.MustParseAddr("10.77.0.2")
	serverIP := netip.MustParseAddr("10.77.0.1")
	cb := bind.New()
	ct, cn, err := netstack.CreateNetTUN([]netip.Addr{clientIP}, nil, 1420)
	if err != nil {
		t.Fatalf("client netstack: %v", err)
	}
	cd := device.NewDevice(ct, cb, device.NewLogger(device.LogLevelError, "client: "))
	t.Cleanup(func() { cd.Close() })
	ccfg := config.ClientConfig{
		Version:       config.ClientVersion,
		InfoHash:      srv.InfoHash(),
		ServerPubKey:  srv.PubKey(),
		ClientPrivKey: clientPriv,
		ClientPubKey:  clientPub,
		ClientIP:      "10.77.0.2",
		Subnet:        "10.77.0.0/24",
		ServerIP:      "10.77.0.1",
		Transport:     "ext-utp",
		SocksPort:     1080,
		Profile:       srv.Config().AWGProfile,
	}
	active := srv.Config().AWGProfile
	if !active.RandomTrailers || active.DisableCookies || len(active.HeaderProtectionKey) != 64 || active.JC == 0 || active.JMin >= active.JMax || active.S1 != 32 || active.S2 != 32 || active.S3 != 32 || active.S4 != 32 {
		t.Fatal("fresh hub did not provision the AWG 3.1 profile")
	}
	saved, err := st.LoadServerConfig()
	if err != nil || saved.AWGProfile != active {
		t.Fatal("fresh profile was not persisted for later imports and restart")
	}
	if err := cd.IpcSet(awg.ClientConf(ccfg)); err != nil {
		t.Fatalf("client IpcSet: %v", err)
	}
	if err := cd.Up(); err != nil {
		t.Fatalf("client up: %v", err)
	}

	// Dial the carrier: µTP over loopback to the hub's carrier port.
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("client udp: %v", err)
	}
	t.Cleanup(func() { udp.Close() })
	sock, err := utp.NewSocketFromPacketConn(udp,
		utp.WithTargetDelay(5*time.Millisecond),
		utp.WithBufferSizes(65536, 65536))
	if err != nil {
		t.Fatalf("client utp socket: %v", err)
	}
	t.Cleanup(func() { sock.Close() })
	conn, err := sock.DialTimeout("127.0.0.1:17777", 5*time.Second)
	if err != nil {
		t.Fatalf("carrier dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	infoHash, err := awg.InfoHashBytes(srv.InfoHash())
	if err != nil {
		t.Fatalf("parse infohash: %v", err)
	}
	peerID, err := awg.PeerIDBytes(rec.PeerID)
	if err != nil {
		t.Fatalf("parse peer id: %v", err)
	}
	tcar, hubID, err := carrier.NewExtended(conn, infoHash, peerID)
	if err != nil {
		t.Fatalf("carrier handshake: %v", err)
	}
	if hubID == ([20]byte{}) || strings.Contains(string(hubID[:]), "btawg") {
		t.Fatal("hub peer ID is empty or exposes the product name")
	}
	cb.Attach(bind.Endpoint(serverIP), tcar)

	// Dial the echo socket and send a first ping: outgoing traffic is what
	// makes wireguard-go initiate the AWG handshake.
	cli, err := cn.DialUDPAddrPort(netip.AddrPortFrom(clientIP, 0), netip.MustParseAddrPort("10.77.0.1:9000"))
	if err != nil {
		t.Fatalf("client dial echo: %v", err)
	}
	t.Cleanup(func() { cli.Close() })
	if _, err := cli.WriteTo([]byte("ping"), nil); err != nil {
		t.Fatalf("echo write: %v", err)
	}

	// Wait for the authenticated AWG handshake on the client device.
	ok := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		state, err := cd.IpcGet()
		if err != nil {
			t.Fatalf("client IpcGet: %v", err)
		}
		var sec int64
		for _, line := range strings.Split(state, "\n") {
			if _, err := fmt.Sscanf(line, "last_handshake_time_sec=%d", &sec); err == nil && sec > 0 {
				ok = true
				break
			}
		}
		if ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ok {
		t.Fatal("no authenticated AWG handshake within 10s")
	}
	if got := srv.ConnectedCount()["10.77.0.2"]; got < 1 {
		t.Fatalf("server reports %d carriers for the client, want >= 1", got)
	}

	// UDP echo through the tunnel (the session is up; this ping returns).
	if _, err := cli.WriteTo([]byte("ping"), nil); err != nil {
		t.Fatalf("echo write: %v", err)
	}
	cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 128)
	n, err := cli.Read(buf)
	if err != nil {
		t.Fatalf("echo read: %v", err)
	}
	if string(buf[:n]) != "ping" {
		t.Fatalf("echo = %q, want ping", buf[:n])
	}
}

// TestRegisterClientUpdatesSnapshot proves that runtime registration (panel
// create) and revocation immediately update the in-memory policy snapshot
// without requiring a server restart or policy reload.
func TestRegisterClientUpdatesSnapshot(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/snapshot.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.DefaultServerConfig()
	cfg.ServerIP = "10.77.0.1"
	cfg.CarrierPort = 17779
	cfg.AWGPort, cfg.WGPort = 17803, 17804

	aPriv, aPub, err := awg.GenerateKeyPair()
	if err != nil {
		t.Fatalf("A key: %v", err)
	}
	a := store.ClientRecord{
		ID:      "A",
		Name:    "client-a",
		IP:      "10.77.0.2",
		PubKey:  aPub,
		PrivKey: aPriv,
		PeerID:  awg.PeerID(aPub),
		Created: time.Unix(1, 0),
	}
	if err := st.CreateClient(a); err != nil {
		t.Fatalf("create A: %v", err)
	}

	srv, err := New(cfg, st)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	aIP := netip.MustParseAddr("10.77.0.2")
	bIP := netip.MustParseAddr("10.77.0.3")
	if srv.policy.clientCanReach(aIP, bIP) {
		t.Fatalf("A -> B: want false before runtime registration")
	}

	bPriv, bPub, err := awg.GenerateKeyPair()
	if err != nil {
		t.Fatalf("B key: %v", err)
	}
	b := store.ClientRecord{
		ID:      "B",
		Name:    "client-b",
		IP:      "10.77.0.3",
		PubKey:  bPub,
		PrivKey: bPriv,
		PeerID:  awg.PeerID(bPub),
		Created: time.Unix(1, 0),
	}
	if err := st.CreateClient(b); err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterClient(b); err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}
	if !srv.policy.clientCanReach(aIP, bIP) {
		t.Errorf("A -> B after register: want true, got false")
	}
	if !srv.policy.clientCanReach(bIP, aIP) {
		t.Errorf("B -> A after register: want true, got false")
	}
	public := netip.MustParseAddr("1.1.1.1")
	if !srv.policy.upload(bIP, public, true, 40, time.Now()) {
		t.Errorf("new client B cannot send routed Internet traffic")
	}

	if err := st.RevokeClient(b.ID); err != nil {
		t.Fatal(err)
	}
	if err := srv.RevokeClient(b); err != nil {
		t.Fatalf("RevokeClient: %v", err)
	}
	if srv.policy.clientCanReach(aIP, bIP) {
		t.Errorf("A -> B after revoke: want false, got true")
	}
	if srv.policy.upload(bIP, public, true, 40, time.Now()) {
		t.Errorf("revoked client B can still send routed Internet traffic")
	}
}

// TestNewRealTUNRequiresRoot guards the hub real-TUN constructor. Without root
// the kernel TUN creation fails; the error must name the requested interface.
// When running as root the live path is validated manually instead.
func TestNewRealTUNRequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root: live TUN creation would succeed")
	}
	st, err := store.Open(t.TempDir() + "/hub.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.DefaultServerConfig()
	cfg.Transport = "ext-tcp" // avoid carrier UDP setup on this error path
	tunName := "btawgroot0"
	_, err = NewRealTUN(cfg, st, tunName)
	if err == nil {
		t.Fatal("expected NewRealTUN to fail without root")
	}
	if !strings.Contains(err.Error(), tunName) {
		t.Fatalf("error %q does not name the TUN interface %q", err, tunName)
	}
}

// TestNewRealTUNLive runs the hub real-TUN constructor with full privileges
// and asserts the overlay address actually lands on the interface with its
// host bits intact. Regression guard for the Addr.Prefix masking bug where
// iproute2 silently assigned the network number (10.77.0.0) and the egress
// bind then failed with EADDRNOTAVAIL. Skipped without root; runnable
// rootless via `unshare -Urn go test ...`.
func TestNewRealTUNLive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root (or a user namespace): live TUN creation")
	}
	st, err := store.Open(t.TempDir() + "/hub.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.DefaultServerConfig()
	cfg.Transport = "ext-tcp"
	tunName := "btawglive0"
	s, err := NewRealTUN(cfg, st, tunName)
	if err != nil {
		t.Fatalf("NewRealTUN: %v", err)
	}
	defer s.Close()

	out, err := exec.Command("ip", "addr", "show", "dev", tunName).CombinedOutput()
	if err != nil {
		t.Fatalf("ip addr show: %v: %s", err, out)
	}
	if !strings.Contains(string(out), cfg.ServerIP+"/24") {
		t.Fatalf("tun %s missing %q, has:\n%s", tunName, cfg.ServerIP+"/24", out)
	}
}
