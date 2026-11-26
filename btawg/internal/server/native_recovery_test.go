package server

import (
	"context"
	"io"
	"net/netip"
	"testing"
	"time"

	"bittorrent-tunnel/btawg/internal/awg"
	"bittorrent-tunnel/btawg/internal/client"
	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/store"
)

// Native AWG and unmodified WG share group routing, never recovery state.
func TestNativeRecoveryRoutesAcrossEngines(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/hub.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	group := store.GroupRecord{ID: "team", Name: "Team", Subnet: "10.84.0.0/24", ServerIP: "10.84.0.1"}
	if err := st.SaveGroup(group); err != nil {
		t.Fatal(err)
	}
	hubCfg := config.DefaultServerConfig()
	hubCfg.ExternalIP = "127.0.0.1"
	hubCfg.CarrierPort, hubCfg.AWGPort, hubCfg.WGPort = 17841, 17842, 17843
	hub, err := New(hubCfg, st)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	newPeer := func(mode, ip string) (*client.Client, store.ClientRecord) {
		t.Helper()
		priv, pub, err := awg.GenerateKeyPair()
		if err != nil {
			t.Fatal(err)
		}
		r := store.ClientRecord{ID: mode, Name: mode, GroupID: group.ID, IP: ip, PrivKey: priv, PubKey: pub, PeerID: awg.PeerID(pub)}
		if err := st.CreateClient(r); err != nil {
			t.Fatal(err)
		}
		if err := hub.RegisterClient(r); err != nil {
			t.Fatal(err)
		}
		c, err := client.New(config.ClientConfig{Transport: mode, ClientIP: ip, Subnet: group.Subnet, ServerIP: group.ServerIP,
			ClientPrivKey: priv, ServerPubKey: hub.PubKey(), Profile: hub.Config().AWGProfile,
			AWGEndpoint: "127.0.0.1:17842", WGEndpoint: "127.0.0.1:17843"})
		if err != nil {
			t.Fatal(err)
		}
		return c, r
	}
	a, _ := newPeer(config.ModeAWG, "10.84.0.2")
	defer a.Close()
	b, rec := newPeer(config.ModeWG, "10.84.0.3")
	defer b.Close()
	listener, err := b.OverlayListenTCP(19001)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		if c, err := listener.Accept(); err == nil {
			defer c.Close()
			_, _ = c.Write([]byte("team"))
		}
	}()
	dst := netip.AddrPortFrom(netip.MustParseAddr(rec.IP), 19001)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := a.OverlayDialTCP(ctx, dst)
	if err != nil {
		t.Fatalf("AWG to WG group route: %v", err)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	var got [4]byte
	if _, err := io.ReadFull(conn, got[:]); err != nil || string(got[:]) != "team" {
		t.Fatalf("WG return path: %q, %v", got, err)
	}
	if hub.router.wg.recovery.Load() != nil {
		t.Fatal("plain WG wrapped in AWG recovery")
	}
	if hub.router.awg.recovery.Load().Stats().ControlBytes == 0 {
		t.Fatal("AWG did not negotiate using its group gateway")
	}
	for _, e := range hub.router.awgEngines() {
		if e == nil || e.recovery.Load() == nil {
			t.Fatal("missing AWG recovery device")
		}
	}
	if err := st.RevokeClient(rec.ID); err != nil {
		t.Fatal(err)
	}
	if err := hub.RevokeClient(rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := hub.router.lookup(dst.Addr()); ok {
		t.Fatal("revoked WG peer still owns its route")
	}
	denied, stop := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer stop()
	if c, err := a.OverlayDialTCP(denied, dst); err == nil {
		c.Close()
		t.Fatal("AWG peer reached a revoked WG peer")
	}
}
