package client

import (
	"maps"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bittorrent-tunnel/btawg/internal/mesh"
)

func TestManagedRoutesPersistIndependently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.json")
	if err := os.WriteFile(path, []byte(`{"listen_ip":"127.0.0.1","enabled":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadManagedConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.P2P || cfg.AutoMap || cfg.ServeEgress || cfg.PeerEndpoint != "" || cfg.PeerPort != 0 {
		t.Fatal("old config opted in to network features")
	}
	for port := 1080; port <= 1092; port++ {
		if got := cfg.response("status").RouteID(port); got != "hub" {
			t.Fatalf("default route for port %d = %q", port, got)
		}
	}
	options := []RouteOption{{ID: "hub", Name: "Hub"}, {ID: "exit", Name: "Exit"}, {ID: "cascade", Name: "Via transit"}}
	want := map[int]string{
		1080: "exit", 1081: "cascade", 1082: "exit", 1083: "cascade", 1084: "exit",
		1085: "cascade", 1086: "exit", 1087: "cascade", 1088: "exit", 1089: "cascade",
		1090: "exit", 1091: "cascade", 1092: "exit",
	}
	for port, id := range want {
		if err := cfg.update(ControlRequest{Op: "set-route", Port: port, RouteID: id}, options, true); err != nil {
			t.Fatal(err)
		}
	}
	cfg.P2P, cfg.AutoMap, cfg.ServeEgress = true, true, true
	cfg.PeerEndpoint, cfg.PeerPort = "8.8.4.4:51840", 51841
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadManagedConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	status := loaded.response("status")
	if !status.P2P || !status.AutoMap || !status.ServeEgress {
		t.Fatal("saved network opts were lost")
	}
	if status.PeerEndpoint != "8.8.4.4:51840" || status.PeerPort != 51841 {
		t.Fatalf("manual peer endpoint and listen port not restored: %+v", status)
	}
	if !maps.Equal(status.SelectedRoutes, want) {
		t.Fatalf("routes after reload = %v; want %v", status.SelectedRoutes, want)
	}
}

func TestManagedRoutesFollowSOCKSSlotsWhenBaseMoves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.json")
	if err := os.WriteFile(path, []byte(`{"routes":{"1080":"exit","1092":"cascade"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadManagedConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.response("status"); got.RouteID(1080) != "exit" || got.RouteID(1092) != "cascade" {
		t.Fatal("legacy routes did not load")
	}
	cfg.SOCKSBasePort = 1081 // Overlap the old block: preserve offsets, not absolute ports.
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadManagedConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.response("status"); !maps.Equal(got.SelectedRoutes, map[int]string{1081: "exit", 1093: "cascade"}) {
		t.Fatalf("routes after moving base: %v", got.SelectedRoutes)
	}
	if err := cfg.update(ControlRequest{Op: "set-route", Port: 1080, RouteID: HubRouteID}, nil, false); err == nil {
		t.Fatal("accepted a port outside the new block")
	}
	if err := cfg.update(ControlRequest{Op: "set-route", Port: 1093, RouteID: HubRouteID}, nil, false); err != nil {
		t.Fatal(err)
	}
	cfg.SOCKSBasePort = 0
	if got := cfg.response("status"); !maps.Equal(got.SelectedRoutes, map[int]string{1080: "exit"}) {
		t.Fatalf("routes after resetting base: %v", got.SelectedRoutes)
	}
}

func TestManagedSOCKSBaseFitsAllPorts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.json")
	for _, tc := range []struct {
		base int
		ok   bool
	}{
		{0, true}, {1, true}, {65523, true}, {65524, false}, {-1, false},
	} {
		cfg := ManagedConfig{SOCKSBasePort: tc.base}
		if err := cfg.Save(path); (err == nil) != tc.ok {
			t.Errorf("save base %d: %v; want success=%t", tc.base, err, tc.ok)
		}
	}
}

func TestSetRouteUsesCurrentCatalogueAndPreservesStaleSelection(t *testing.T) {
	cfg := ManagedConfig{Routes: map[int]string{1080: "retired", 1092: "also-retired"}}
	options := []RouteOption{{ID: "exit", Name: "Renamed exit"}}
	for _, tc := range []struct {
		port    int
		id      string
		running bool
	}{
		{1079, "exit", true}, {1093, "exit", true}, {1080, "", true},
		{1080, "unknown", true}, {1080, "retired", true}, {1080, "hub", true},
		{1080, "exit", false},
	} {
		candidate := cfg
		req := ControlRequest{Op: "set-route", Port: tc.port, RouteID: tc.id}
		if err := candidate.update(req, options, tc.running); err == nil {
			t.Errorf("accepted invalid request %+v, running=%v", req, tc.running)
		}
		if !maps.Equal(candidate.Routes, map[int]string{1080: "retired", 1092: "also-retired"}) {
			t.Errorf("rejected request changed routes: %v", candidate.Routes)
		}
	}
	if err := cfg.update(ControlRequest{Op: "set-route", Port: 1080, RouteID: "retired"}, nil, false); err != nil {
		t.Fatalf("stopped service cannot keep existing selection: %v", err)
	}
	// The daemon shallow-copies the current settings before it attempts to save.
	// Rejecting a save must not mutate the original map.
	candidate := cfg
	if err := candidate.update(ControlRequest{Op: "set-route", Port: 1080, RouteID: "hub"}, nil, false); err != nil {
		t.Fatalf("stopped service cannot reset to hub: %v", err)
	}
	if candidate.response("status").RouteID(1080) != "hub" || candidate.Routes[1092] != "also-retired" || cfg.Routes[1080] != "retired" {
		t.Fatalf("reset routes = %v; original routes = %v", candidate.Routes, cfg.Routes)
	}
}

func TestControlAppliesRouteOnlyAfterSave(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, nil, 0600); err != nil {
		t.Fatal(err)
	}
	m := &Managed{}
	m.applyRoutes(map[int]string{1080: "retired", 1092: "cascade"})
	m.network.catalogue.Store(&catalogue{
		routes: []mesh.Route{{ID: "exit", Name: "Live exit"}},
		until:  time.Now().Add(time.Hour),
	})
	d := &clientControl{m: m, path: filepath.Join(blocked, "client.json"), cfg: ManagedConfig{
		Routes: map[int]string{1080: "retired", 1092: "cascade"},
	}}
	req := ControlRequest{Op: "set-route", Port: 1080, RouteID: "exit"}
	if got := d.handle(req); got.OK || got.Error == "" {
		t.Fatalf("route change with unwritable config: %+v", got)
	}
	if got := (*m.network.selected.Load())[1080]; got != "retired" {
		t.Fatalf("failed save changed live route to %q", got)
	}
	if got := d.handle(ControlRequest{Op: "status"}).RouteID(1080); got != "retired" {
		t.Fatalf("failed save changed control state to %q", got)
	}

	d.path = filepath.Join(dir, "client.json")
	sock := filepath.Join(dir, "control.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	defer func() { ln.Close(); <-done }()
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err == nil {
			d.serve(t.Context(), conn)
		}
	}()
	res, err := request(sock, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.RouteID(1080) != "exit" || res.RouteID(1092) != "cascade" || len(res.Routes) != 1 || res.Routes[0].Name != "Live exit" {
		t.Fatalf("successful route change response: %+v", res)
	}
	loaded, err := LoadManagedConfig(d.path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{1080: "exit", 1092: "cascade"}
	if !maps.Equal(loaded.Routes, want) || !maps.Equal(*m.network.selected.Load(), want) {
		t.Fatalf("saved routes %v; live routes %v", loaded.Routes, *m.network.selected.Load())
	}
}

func TestManagedConfigRejectsInvalidP2PSettings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   ManagedConfig
		valid bool
	}{
		{"defaults", ManagedConfig{}, true},
		{"public IPv4", ManagedConfig{P2P: true, PeerEndpoint: "8.8.8.8:51840"}, true},
		{"global IPv6", ManagedConfig{P2P: true, PeerEndpoint: "[2606:4700::1111]:65535", PeerPort: 65535}, true},
		{"private IPv4", ManagedConfig{P2P: true, PeerEndpoint: "192.168.1.2:51840"}, false},
		{"carrier NAT IPv4", ManagedConfig{P2P: true, PeerEndpoint: "100.64.2.3:51840"}, false},
		{"unique-local IPv6", ManagedConfig{P2P: true, PeerEndpoint: "[fd00::1]:51840"}, false},
		{"IPv6 link-local", ManagedConfig{P2P: true, PeerEndpoint: "[fe80::1%eth0]:51840"}, false},
		{"loopback", ManagedConfig{P2P: true, PeerEndpoint: "127.0.0.1:51840"}, false},
		{"missing port", ManagedConfig{P2P: true, PeerEndpoint: "8.8.8.8"}, false},
		{"zero port", ManagedConfig{P2P: true, PeerEndpoint: "8.8.8.8:0"}, false},
		{"hostname", ManagedConfig{P2P: true, PeerEndpoint: "example.org:51840"}, false},
		{"P2P without TUN", ManagedConfig{P2P: true, NoTUN: true}, false},
		{"mapping without P2P", ManagedConfig{AutoMap: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "client.json")
			err := tc.cfg.Save(path)
			if (err == nil) != tc.valid {
				t.Fatalf("Save error = %v; want valid=%v", err, tc.valid)
			}
			if !tc.valid {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("invalid settings left a config file: %v", err)
				}
			}
		})
	}
}
