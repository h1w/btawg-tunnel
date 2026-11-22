package client

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"bittorrent-tunnel/btawg/internal/awg"
	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/server"
	"bittorrent-tunnel/btawg/internal/store"
)

func TestManagedEgressCascadeAndLiveRevocation(t *testing.T) {
	if os.Getenv("BTAWG_EGRESS_TEST") != "1" {
		t.Skip("requires isolated Linux network namespace with netkit")
	}
	const address = "11.249.9.2"
	const echoPort = "19440"
	if os.Getenv("BTAWG_EGRESS_CHILD") == "1" {
		ln, err := net.Listen("tcp4", net.JoinHostPort(address, echoPort))
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		pc, err := net.ListenPacket("udp4", net.JoinHostPort(address, echoPort))
		if err != nil {
			t.Fatal(err)
		}
		defer pc.Close()
		go func() {
			var b [512]byte
			for {
				n, from, err := pc.ReadFrom(b[:])
				if err != nil {
					return
				}
				_, _ = pc.WriteTo(b[:n], from)
			}
		}()
		fmt.Println("echo ready")
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}
	child := exec.CommandContext(t.Context(), "unshare", "-n", "sh", "-ec", `echo ready; read _; ip link set lo up; ip addr add 172.20.42.2/30 dev exit-host; ip addr add 11.249.9.2/32 dev lo; ip link set exit-host up; exec "$@"`, "egress-child", os.Args[0], "-test.run=^TestManagedEgressCascadeAndLiveRevocation$")
	child.Env = append(os.Environ(), "BTAWG_EGRESS_CHILD=1")
	child.Stderr = os.Stderr
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	in, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { child.Process.Kill(); child.Wait() })
	var ready [6]byte
	if _, err := io.ReadFull(out, ready[:]); err != nil || string(ready[:]) != "ready\n" {
		t.Fatalf("child startup %q %v", ready, err)
	}
	for _, args := range [][]string{{"link", "add", "exit-origin", "type", "netkit", "peer", "name", "exit-host"}, {"link", "set", "exit-host", "netns", fmt.Sprint(child.Process.Pid)}, {"addr", "add", "172.20.42.1/30", "dev", "exit-origin"}, {"link", "set", "exit-origin", "up"}, {"route", "add", address + "/32", "via", "172.20.42.2", "dev", "exit-origin"}} {
		testIP(t, args...)
	}
	t.Cleanup(func() { _ = exec.Command("ip", "link", "delete", "exit-origin").Run() })
	if _, err := io.WriteString(in, "start\n"); err != nil {
		t.Fatal(err)
	}
	in.Close()
	var echoReady [11]byte
	if _, err := io.ReadFull(out, echoReady[:]); err != nil || string(echoReady[:]) != "echo ready\n" {
		t.Fatalf("echo startup %q %v", echoReady, err)
	}
	destination := netip.MustParseAddr(address)
	st, err := store.Open(t.TempDir() + "/hub.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfgs := make([]config.ClientConfig, 3)
	ids := []string{"origin", "transit", "exit"}
	for i, id := range ids {
		cfg := managedCfg(t)
		cfg.ClientIP = "10.77.0." + strconv.Itoa(i+2)
		cfgs[i] = cfg
		rec := store.ClientRecord{ID: id, Name: id, PubKey: cfg.ClientPubKey, PrivKey: cfg.ClientPrivKey, IP: cfg.ClientIP, PeerID: awg.PeerID(cfg.ClientPubKey), GroupID: store.DefaultGroupID, VisiblePeers: []string{}, Internet: new(false), Created: time.Now()}
		if err := st.AllocateClient(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetEgressRole("transit", store.EgressRole{Transit: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEgressRole("exit", store.EgressRole{Exit: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveEgressRoute(store.EgressRoute{ID: "cascade", Name: "Transit → Exit", Hops: []string{"transit", "exit"}, Clients: []string{"origin"}}); err != nil {
		t.Fatal(err)
	}
	hc := config.DefaultServerConfig()
	hc.ExternalIP = "127.0.0.1"
	hc.CarrierPort, hc.AWGPort, hc.WGPort = freePort(t), freePort(t), freePort(t)
	hub, err := server.New(hc, st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Close() })
	go hub.Run(t.Context())
	auth := Credentials{Username: "test", Password: "route"}
	var nodes []*Managed
	for i, cfg := range cfgs {
		cfg.ServerPubKey = hub.PubKey()
		cfg.WGEndpoint = net.JoinHostPort("127.0.0.1", strconv.Itoa(hc.WGPort))
		var selection map[int]string
		if i == 0 {
			// The WG fixed inbound selects the hub (which has no egress here).
			// The same WG transport must still serve all three named routes.
			selection = map[int]string{1090: "cascade", 1091: "cascade", 1092: "cascade"}
		}
		m, err := newManagedAt(t.Context(), cfg, []string{config.ModeWG}, netip.MustParseAddr("127.0.0.1"), auth, false, "", 0, nil, selection)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { m.Close() })
		m.startNetwork(false, false, false, i != 0, 0, "", "")
		nodes = append(nodes, m)
	}
	origin := nodes[0]
	until := time.Now().Add(20 * time.Second)
	for {
		cat := origin.network.catalogue.Load()
		if cat != nil && len(cat.routes) == 1 && cat.routes[0].Grant != "" && origin.tunnels[5].score.Load() > 0 {
			break
		}
		if time.Now().After(until) {
			t.Fatalf("no live cascade/working WG: %#v", cat)
		}
		time.Sleep(30 * time.Millisecond)
	}
	connect := func(slot int) (net.Conn, byte) {
		t.Helper()
		c, err := proxyAuth(t, origin.inbound.slots[slot].ln.Addr().String(), auth)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		c.SetDeadline(time.Now().Add(12 * time.Second))
		var req [10]byte
		copy(req[:], []byte{5, 1, 0, 1})
		addr := destination.As4()
		copy(req[4:8], addr[:])
		binary.BigEndian.PutUint16(req[8:], 19440)
		if _, err := c.Write(req[:]); err != nil {
			t.Fatal(err)
		}
		var status [10]byte
		if _, err := io.ReadFull(c, status[:]); err != nil {
			t.Fatal(err)
		}
		return c, status[1]
	}
	if c, status := connect(5); status == 0 {
		c.Close()
		t.Fatal("fixed WG port unexpectedly inherited the balancer route")
	} else {
		c.Close()
	}
	var held net.Conn
	for _, slot := range []int{10, 11, 12} {
		c, status := connect(slot)
		if status != 0 {
			t.Fatalf("slot %d CONNECT failed: %d", slot, status)
		}
		if _, err := c.Write([]byte("cascade")); err != nil {
			t.Fatal(err)
		}
		var got [7]byte
		if _, err := io.ReadFull(c, got[:]); err != nil || string(got[:]) != "cascade" {
			t.Fatalf("slot %d echo = %q, %v", slot, got, err)
		}
		if held == nil {
			held = c
		} else {
			c.Close()
		}
	}
	assoc, err := proxyAuth(t, origin.inbound.slots[12].ln.Addr().String(), auth)
	if err != nil {
		t.Fatal(err)
	}
	defer assoc.Close()
	assoc.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := assoc.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	var header [10]byte
	if _, err := io.ReadFull(assoc, header[:]); err != nil || header[1] != 0 {
		t.Fatalf("Adaptive UDP association: %v %v", header, err)
	}
	udp, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IP(header[4:8]), Port: int(binary.BigEndian.Uint16(header[8:]))})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	udp.SetDeadline(time.Now().Add(4 * time.Second))
	msg := []byte{0, 0, 0, 1, 11, 249, 9, 2, 0, 0, 'u', 'd', 'p'}
	binary.BigEndian.PutUint16(msg[8:], 19440)
	if _, err := udp.Write(msg); err != nil {
		t.Fatal(err)
	}
	var buf [64]byte
	if n, err := udp.Read(buf[:]); err != nil || n != len(msg) || string(buf[:n]) != string(msg) {
		t.Fatalf("UDP reply %x %v", buf[:n], err)
	}
	if err := st.SetEgressRole("transit", store.EgressRole{}); err != nil {
		t.Fatal(err)
	}
	held.SetReadDeadline(time.Now().Add(7 * time.Second))
	var data [1]byte
	if _, err := held.Read(data[:]); err == nil {
		t.Fatal("revoked stream was not closed")
	} else if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("revoked stream stayed open")
	}
	if c, status := connect(12); status == 0 {
		c.Close()
		t.Fatal("Adaptive bypassed revoked selected cascade")
	}
}
