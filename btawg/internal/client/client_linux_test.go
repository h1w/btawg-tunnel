package client

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	anadht "github.com/anacrolix/dht/v2"

	"bittorrent-tunnel/btawg/internal/awg"
	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/dht"
	"bittorrent-tunnel/btawg/internal/server"
	"bittorrent-tunnel/btawg/internal/socks"
	"bittorrent-tunnel/btawg/internal/store"
)

// newRelay starts a bare DHT node standing in for the public network.
func newRelay(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("relay listen: %v", err)
	}
	srv, err := anadht.NewServer(&anadht.ServerConfig{
		Conn:       conn,
		NoSecurity: true,
		PeerStore:  dht.NewMemStore(),
		StartingNodes: func() ([]anadht.Addr, error) {
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("relay server: %v", err)
	}
	t.Cleanup(srv.Close)
	return conn.LocalAddr().String()
}

func tcpEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

// TestClientFullFlow covers DHT discovery and rejection of local destinations.
func TestClientFullFlow(t *testing.T) {
	relay := newRelay(t)

	// Hub with one registered client.
	st, err := store.Open(t.TempDir() + "/hub.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	hubCfg := config.DefaultServerConfig()
	hubCfg.Transport = "ext-utp"
	hubCfg.CarrierPort = 17783 // unique across parallel test packages (server: 17777-17779, e2e: 17781)
	hubCfg.AWGPort, hubCfg.WGPort = 17809, 17810
	hubCfg.ExternalIP = "127.0.0.1"

	clientPriv, clientPub, err := awg.GenerateKeyPair()
	if err != nil {
		t.Fatalf("client key: %v", err)
	}
	rec := store.ClientRecord{
		ID:      "c1",
		Name:    "full-flow",
		PubKey:  clientPub,
		PrivKey: clientPriv,
		IP:      "10.77.0.2",
		PeerID:  awg.PeerID(clientPub),
		Created: time.Unix(1, 0),
	}
	if err := st.CreateClient(rec); err != nil {
		t.Fatalf("create client: %v", err)
	}
	srv, err := server.New(hubCfg, st)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	srv.SetDHTBootstrap([]string{relay})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	go srv.Run(ctx)
	go srv.Announce(ctx)

	// Client: strictly-DHT discovery through the same relay.
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
		SocksPort:     0,
		Profile:       srv.Config().AWGProfile,
		DHTBootstrap:  []string{relay},
	}
	c, err := New(ccfg)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	t.Cleanup(c.Close)
	runErr := make(chan error, 1)
	go func() { runErr <- c.Run(ctx) }()

	// The local SOCKS5 appears once Run is serving.
	socksAddr := ""
	deadline := time.Now().Add(15 * time.Second)
	for socksAddr == "" && time.Now().Before(deadline) {
		socksAddr = c.SocksAddr()
		if socksAddr == "" {
			select {
			case err := <-runErr:
				t.Fatalf("client Run exited early: %v", err)
			default:
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if socksAddr == "" {
		t.Fatal("client SOCKS5 never came up")
	}

	deadline = time.Now().Add(12 * time.Second)
	for len(srv.ConnectedCount()) == 0 && time.Now().Before(deadline) {
		time.Sleep(30 * time.Millisecond)
	}
	if len(srv.ConnectedCount()) == 0 {
		t.Fatal("client never reached the hub")
	}
	conn, err := socks.NewClient(socksAddr).Dial(ctx, "tcp", tcpEcho(t))
	if err == nil {
		conn.Close()
		t.Fatal("proxy reached local loopback")
	}
}
