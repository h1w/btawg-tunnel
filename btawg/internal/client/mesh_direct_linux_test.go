package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"bittorrent-tunnel/btawg/internal/awg"
	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/server"
	"bittorrent-tunnel/btawg/internal/store"
)

func TestManagedDirectTUNWithHubRevocation(t *testing.T) {
	if os.Getenv("BTAWG_DIRECT_TEST") != "1" {
		t.Skip("requires isolated user/network namespace with netkit and TUN")
	}
	if path := os.Getenv("BTAWG_DIRECT_CHILD"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := config.ParseImportString(string(data))
		if err != nil {
			t.Fatal(err)
		}
		runDirectChild(t, cfg, path+".ready")
		return
	}
	ctx := t.Context()
	a, b := managedCfg(t), managedCfg(t)
	b.ClientIP = "10.77.0.3"
	st, err := store.Open(t.TempDir() + "/hub.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for i, c := range []config.ClientConfig{a, b} {
		id := []string{"a", "b"}[i]
		rec := store.ClientRecord{ID: id, Name: id, PubKey: c.ClientPubKey, PrivKey: c.ClientPrivKey, IP: c.ClientIP, PeerID: awg.PeerID(c.ClientPubKey), GroupID: store.DefaultGroupID, Created: time.Now()}
		if err := st.AllocateClient(rec); err != nil {
			t.Fatal(err)
		}
	}
	sc := config.DefaultServerConfig()
	sc.ExternalIP = "172.29.99.1"
	sc.CarrierPort, sc.AWGPort, sc.WGPort = freePort(t), freePort(t), freePort(t)
	hub, err := server.New(sc, st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Close() })
	go hub.Run(ctx)
	for _, cfg := range []*config.ClientConfig{&a, &b} {
		cfg.ServerPubKey = hub.PubKey()
		cfg.Transport = config.ModeWG
		cfg.WGEndpoint = netip.AddrPortFrom(netip.MustParseAddr(sc.ExternalIP), uint16(sc.WGPort)).String()
		cfg.CarrierEndpoint = netip.AddrPortFrom(netip.MustParseAddr(sc.ExternalIP), uint16(sc.CarrierPort)).String()
	}
	path := filepath.Join(t.TempDir(), "child-import")
	childConfig, err := b.ImportString()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(childConfig), 0600); err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(ctx, "unshare", "-n", "sh", "-ec", `echo ready; read _; ip link set lo up; ip addr add 172.29.99.2/24 dev mesh-b; ip link set mesh-b up; exec "$@"`, "p2p-child", os.Args[0], "-test.run=^TestManagedDirectTUNWithHubRevocation$", "-test.v")
	child.Env = append(os.Environ(), "BTAWG_DIRECT_CHILD="+path)
	child.Stderr = os.Stderr
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { child.Process.Kill(); child.Wait() })
	var ready [6]byte
	if _, err := io.ReadFull(stdout, ready[:]); err != nil || string(ready[:]) != "ready\n" {
		t.Fatalf("child startup: %q %v", ready, err)
	}
	go io.Copy(os.Stderr, stdout)
	for _, args := range [][]string{{"link", "add", "mesh-a", "type", "netkit", "peer", "name", "mesh-b"}, {"link", "set", "mesh-b", "netns", fmt.Sprint(child.Process.Pid)}, {"addr", "add", "172.29.99.1/24", "dev", "mesh-a"}, {"link", "set", "mesh-a", "up"}} {
		testIP(t, args...)
	}
	t.Cleanup(func() { _ = exec.Command("ip", "link", "delete", "mesh-a").Run() })
	if _, err := io.WriteString(stdin, "start\n"); err != nil {
		t.Fatal(err)
	}
	stdin.Close()
	waitMesh(t, func() bool { _, err := os.Stat(path + ".ready"); return err == nil }, "child TUN listener")
	mode := selectedTestPeerMode()
	client := newDirectFixture(t, a, "direct-a")

	peerIP := netip.MustParseAddr(b.ClientIP)
	dial := func() (net.Conn, error) {
		try, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
		defer cancel()
		var d net.Dialer
		return d.DialContext(try, "tcp4", netip.AddrPortFrom(peerIP, 19722).String())
	}
	if os.Getenv("BTAWG_PEER_DENY") == "1" {
		if c, err := dial(); err == nil {
			c.Close()
			t.Fatal("direct refusal leaked to the hub with relay disabled")
		}
		relay := true
		client.applyPeerSettings(PeerSettings{Overrides: map[netip.Addr]PeerOverride{peerIP: {Outgoing: mode, Relay: &relay}}})
		waitMesh(t, func() bool {
			c, err := dial()
			if c != nil {
				c.Close()
			}
			return err == nil
		}, "explicit relay")
		if client.shared.directPath(peerIP) != nil {
			t.Fatal("peer admitted forbidden direct mode")
		}
		return
	}
	waitMesh(t, func() bool {
		c, err := dial()
		if c != nil {
			c.Close()
		}
		return err == nil
	}, "peer reachable over overlay")
	waitMesh(t, func() bool { return client.shared.directPath(peerIP) != nil }, "authenticated direct path")
	if p := client.shared.directPath(peerIP); p == nil || p.peerMode != mode {
		t.Fatalf("did not select requested mode %s", mode)
	}
	before := hub.Traffic()["b"]
	c, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(2 * time.Second))
	payload := bytes.Repeat([]byte("direct-data"), 2048)
	result := make(chan error, 1)
	go func() { _, err := c.Write(payload); result <- err }()
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(c, echo); err != nil || !bytes.Equal(echo, payload) {
		t.Fatalf("direct echo mismatch: %v", err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	c.Close()
	if after := hub.Traffic()["b"]; after.Up > before.Up+800 || after.Down > before.Down+800 {
		t.Fatalf("direct flow crossed hub: before=%v after=%v", before, after)
	}
	if err := st.RevokeClient("b"); err != nil {
		t.Fatal(err)
	}
	if err := hub.ReloadPolicy(); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, func() bool { return client.shared.directPath(peerIP) == nil }, "direct path revoked")
	if conn, err := dial(); err == nil {
		conn.Close()
		t.Fatal("revoked client still reachable")
	}
}

func newDirectFixture(t *testing.T, cfg config.ClientConfig, name string) *Managed {
	t.Helper()
	mode := selectedTestPeerMode()
	remote := netip.MustParseAddr("10.77.0.3")
	if name == "direct-b" {
		remote = netip.MustParseAddr("10.77.0.2")
	}
	override := PeerOverride{Incoming: []string{mode}, Outgoing: mode}
	if name == "direct-b" && os.Getenv("BTAWG_PEER_DENY") == "1" {
		relay := true
		override.Incoming, override.Relay = []string{}, &relay
	}
	peer := PeerSettings{Incoming: []string{}, Outgoing: failoverPolicy, Pools: map[string][]string{failoverPolicy: {}}, Overrides: map[netip.Addr]PeerOverride{remote: override}}
	m, err := newManagedWithPeer(t.Context(), cfg, []string{config.ModeWG}, netip.MustParseAddr("127.0.0.1"), Credentials{Username: "direct", Password: "test"}, true, name, 0, nil, nil, true, peer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	m.startNetwork(true, false, false, false, 0, "", "")
	return m
}

func selectedTestPeerMode() string {
	if mode := os.Getenv("BTAWG_PEER_MODE"); mode != "" {
		return mode
	}
	return config.ModeExtUTP
}

func runDirectChild(t *testing.T, cfg config.ClientConfig, ready string) {
	newDirectFixture(t, cfg, "direct-b")
	ln, err := net.Listen("tcp4", net.JoinHostPort(cfg.ClientIP, "19722"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := os.WriteFile(ready, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
	}
}

func testIP(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
		t.Fatalf("ip %v: %v: %s", args, err, out)
	}
}

func waitMesh(t *testing.T, ready func() bool, what string) {
	t.Helper()
	for until := time.Now().Add(18 * time.Second); time.Now().Before(until); time.Sleep(30 * time.Millisecond) {
		if ready() {
			return
		}
	}
	t.Fatalf("timed out: %s", what)
}
