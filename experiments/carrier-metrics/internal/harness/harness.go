// Package harness sets up a real btawg hub and two registered clients for
// isolated carrier measurements. It reuses the btawg/internal/e2e helper
// pattern but lives outside _test.go files so it can be driven from a
// standalone command.
package harness

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	anadht "github.com/anacrolix/dht/v2"

	"bittorrent-tunnel/btawg/internal/awg"
	"bittorrent-tunnel/btawg/internal/client"
	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/dht"
	"bittorrent-tunnel/btawg/internal/server"
	"bittorrent-tunnel/btawg/internal/store"
)

// ClientInfo describes one side of the measured pair.
type ClientInfo struct {
	ID   string
	IP   string
	Rec  *store.ClientRecord
	Cl   *client.Client
	Sock string // SOCKS5 listen address, once available
}

// Hub holds the running hub and its registered clients.
type Hub struct {
	Srv   *server.Server
	St    *store.Store
	DBDir string
	Relay string
	A, B  ClientInfo
}

// SetupHub builds a hub with two pre-registered clients and starts it.
// transport is one of the config Mode* constants.
func SetupHub(ctx context.Context, transport string) (*Hub, error) {
	relay, err := startRelay()
	if err != nil {
		return nil, fmt.Errorf("relay: %w", err)
	}

	dbDir, err := os.MkdirTemp("", "carrier-metrics-")
	if err != nil {
		relayClose()
		return nil, fmt.Errorf("temp dir: %w", err)
	}
	st, err := store.Open(dbDir + "/hub.db")
	if err != nil {
		relayClose()
		return nil, fmt.Errorf("store: %w", err)
	}

	specs := []struct{ id, ip string }{
		{id: "metrics-a", ip: "10.77.0.2"},
		{id: "metrics-b", ip: "10.77.0.3"},
	}
	recs := make([]*store.ClientRecord, len(specs))
	for i, s := range specs {
		priv, pub, err := awg.GenerateKeyPair()
		if err != nil {
			st.Close()
			relayClose()
			return nil, fmt.Errorf("keys for %s: %w", s.id, err)
		}
		rec := &store.ClientRecord{
			ID:      s.id,
			Name:    s.id,
			PubKey:  pub,
			PrivKey: priv,
			IP:      s.ip,
			PeerID:  awg.PeerID(pub),
			Created: time.Unix(1, 0),
		}
		if err := st.CreateClient(*rec); err != nil {
			st.Close()
			relayClose()
			return nil, fmt.Errorf("create %s: %w", s.id, err)
		}
		recs[i] = rec
	}

	hubCfg := config.DefaultServerConfig()
	hubCfg.Transport = transport
	// Normalize() turns a zero port back into the well-known default (6881),
	// which collides with any locally running torrent hub. Reserve real free
	// ports instead; clients discover them through the DHT relay.
	used := map[int]bool{}
	alloc := func() int {
		for {
			p := freePort()
			if p != 0 && !used[p] {
				used[p] = true
				return p
			}
		}
	}
	hubCfg.CarrierPort = alloc()
	hubCfg.AWGPort = alloc()
	hubCfg.WGPort = alloc()
	hubCfg.ExternalIP = "127.0.0.1"

	srv, err := server.New(hubCfg, st)
	if err != nil {
		st.Close()
		relayClose()
		return nil, fmt.Errorf("server.New: %w", err)
	}
	srv.SetDHTBootstrap([]string{relay})
	go srv.Run(ctx)
	go srv.Announce(ctx)

	return &Hub{
		Srv:   srv,
		St:    st,
		DBDir: dbDir,
		Relay: relay,
		A:     ClientInfo{ID: specs[0].id, IP: specs[0].ip, Rec: recs[0]},
		B:     ClientInfo{ID: specs[1].id, IP: specs[1].ip, Rec: recs[1]},
	}, nil
}

// StartClient boots one client and waits for its SOCKS5 listener.
func (h *Hub) StartClient(ctx context.Context, ci *ClientInfo) error {
	cfg := clientConfigFor(h.Srv, ci.Rec, h.Relay, ci.IP)
	c, err := client.New(cfg)
	if err != nil {
		return fmt.Errorf("client %s: %w", ci.ID, err)
	}
	ci.Cl = c
	go c.Run(ctx)

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if a := c.SocksAddr(); a != "" {
			ci.Sock = a
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("client %s SOCKS5 never came up", ci.ID)
}

// WaitConnected blocks until the hub reports at least one carrier for ip.
func (h *Hub) WaitConnected(ctx context.Context, ip string) error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if h.Srv.ConnectedCount()[ip] > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("client %s never attached a carrier", ip)
}

// Close shuts down the hub, clients, relay, and removes the temporary store directory.
func (h *Hub) Close() {
	if h.A.Cl != nil {
		h.A.Cl.Close()
	}
	if h.B.Cl != nil {
		h.B.Cl.Close()
	}
	if h.Srv != nil {
		h.Srv.Close()
	}
	if h.St != nil {
		h.St.Close()
	}
	relayClose()
	if h.DBDir != "" {
		_ = os.RemoveAll(h.DBDir)
	}
}

func clientConfigFor(srv *server.Server, rec *store.ClientRecord, relay, ip string) config.ClientConfig {
	cfg := srv.Config()
	cc := config.ClientConfig{
		Version:       config.ClientVersion,
		InfoHash:      srv.InfoHash(),
		ServerPubKey:  srv.PubKey(),
		TLSServerKey:  srv.TLSServerKey(),
		ClientPrivKey: rec.PrivKey,
		ClientPubKey:  rec.PubKey,
		ClientIP:      ip,
		Subnet:        "10.77.0.0/24",
		ServerIP:      "10.77.0.1",
		Transport:     cfg.Transport,
		SocksPort:     0,
		Profile:       cfg.AWGProfile,
		DHTBootstrap:  []string{relay},
	}
	if config.IsTLSMode(cfg.Transport) {
		hash, err := config.CarrierHash(srv.InfoHash(), cfg.LegacyTransport, cfg.Transport)
		if err != nil {
			panic(err)
		}
		cc.CarrierHashes = map[string]string{cfg.Transport: hash}
	}
	return cc
}

var (
	relayConn net.PacketConn
	relaySrv  *anadht.Server
)

// freePort returns a port available for both TCP and UDP on loopback, or 0.
func freePort() int {
	for range 200 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			continue
		}
		p := l.Addr().(*net.TCPAddr).Port
		u, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: p})
		if err != nil {
			l.Close()
			continue
		}
		l.Close()
		u.Close()
		return p
	}
	return 0
}

func startRelay() (string, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		return "", err
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
		conn.Close()
		return "", err
	}
	relayConn = conn
	relaySrv = srv
	return conn.LocalAddr().String(), nil
}

func relayClose() {
	if relaySrv != nil {
		relaySrv.Close()
	}
	if relayConn != nil {
		relayConn.Close()
	}
}
