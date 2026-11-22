package client

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"bittorrent-tunnel/btawg/internal/config"
)

func TestDaemonControlToggleRotatePersist(t *testing.T) {
	base := managedCfg(t)
	raw, err := base.ImportString()
	if err != nil {
		t.Fatal(err)
	}
	cfg := ManagedConfig{Import: raw, Enabled: []string{config.ModeWG}, ListenIP: "127.0.0.1", Credentials: Credentials{Username: "first", Password: "secret"}}
	cfg.Routes = map[int]string{1080: "unavailable-route"}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "state", "client.json")
	socket := testControlEndpoint(t, filepath.Join(dir, "run", "ctl.sock"))
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runManagedAt(ctx, cfg, cfgPath, socket, false, "btawg", 0, nil) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	send := func(r ControlRequest) ControlResponse {
		t.Helper()
		res, err := request(socket, r)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		r, err := request(socket, ControlRequest{Op: "status"})
		if err == nil {
			if !slices.Equal(r.Enabled, []string{config.ModeWG}) || r.Credentials != nil {
				t.Fatalf("unexpected status: %+v", r)
			}
			break
		}
		if !errors.Is(err, ErrNotRunning) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := send(ControlRequest{Op: "status"}).RouteID(1080); got != "unavailable-route" {
		t.Fatalf("daemon changed stale selection to %q", got)
	}
	if _, err := request(socket, ControlRequest{Op: "set-route", Port: 1092, RouteID: "not-in-live-catalogue"}); err == nil {
		t.Fatal("daemon accepted an egress route missing from the catalogue")
	}
	send(ControlRequest{Op: "apply", Enabled: []string{config.ModeAWG}})
	// The three inbounds have independent settings; [] is not the default pool.
	send(ControlRequest{Op: "set-balancer", Balancer: "failover", Enabled: []string{config.ModeWG, config.ModeAWG, config.ModeWG}})
	send(ControlRequest{Op: "set-balancer", Balancer: "round-robin"})
	send(ControlRequest{Op: "set-balancer", Balancer: "adaptive", Enabled: []string{config.ModeWG, config.ModeAWG, config.ModeWG}})
	if _, err := request(socket, ControlRequest{Op: "set-balancer", Balancer: "adaptive", Enabled: []string{"invalid"}}); err == nil {
		t.Fatal("unsupported mode accepted into a balancer pool")
	}
	checkPools := func(pools map[string][]string) {
		t.Helper()
		if !slices.Equal(pools["failover"], []string{config.ModeWG, config.ModeAWG}) || !slices.Equal(pools["adaptive"], []string{config.ModeWG, config.ModeAWG}) || pools["round-robin"] == nil || len(pools["round-robin"]) != 0 || len(pools) != 3 {
			t.Fatalf("wrong balancer pools: %#v", pools)
		}
	}
	checkPools(send(ControlRequest{Op: "status"}).Balancers)
	if err := os.Rename(filepath.Dir(cfgPath), filepath.Join(dir, "backup")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Dir(cfgPath), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := request(socket, ControlRequest{Op: "apply", Enabled: []string{config.ModeWG}}); err == nil {
		t.Fatal("update accepted without persistence")
	}
	if r := send(ControlRequest{Op: "status"}); !slices.Equal(r.Enabled, []string{config.ModeAWG}) {
		t.Fatal(r.Enabled)
	}
	if err := os.Remove(filepath.Dir(cfgPath)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "backup"), filepath.Dir(cfgPath)); err != nil {
		t.Fatal(err)
	}
	r := send(ControlRequest{Op: "rotate"})
	if r.Credentials == nil || *r.Credentials == cfg.Credentials {
		t.Fatal("credentials were not rotated")
	}
	loaded, err := LoadManagedConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Credentials != *r.Credentials || !slices.Equal(loaded.Enabled, []string{config.ModeAWG}) {
		t.Fatal("rotation or toggle was not persisted")
	}
	checkPools(loaded.Balancers)
	if len(loaded.Routes) != 1 || loaded.Routes[1080] != "unavailable-route" {
		t.Fatalf("other settings updates changed saved routes: %v", loaded.Routes)
	}
	shown := send(ControlRequest{Op: "show"}).Credentials
	if shown == nil || *shown != loaded.Credentials {
		t.Fatal("show did not return current credentials")
	}
}

func TestControlStatusReportsAdaptiveSelection(t *testing.T) {
	var tunnel managedTunnel
	tunnel.c = &Client{cfg: config.ClientConfig{Transport: config.ModeWG}}
	tunnel.score.Store(int64(20 * time.Millisecond))
	m := &Managed{}
	m.applyRoutes(nil)
	b := &m.balance[2]
	b.policy = adaptivePolicy
	b.replace([]*managedTunnel{&tunnel})
	b.pick()
	d := clientControl{m: m}
	if got := d.handle(ControlRequest{Op: "status"}).AdaptiveMode; got != config.ModeWG {
		t.Fatalf("adaptive selection = %q", got)
	}
}
