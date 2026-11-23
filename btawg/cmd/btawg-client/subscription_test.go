package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"bittorrent-tunnel/btawg/internal/client"
	"bittorrent-tunnel/btawg/internal/config"
)

func TestRefreshSubscriptionPreservesLocalSettings(t *testing.T) {
	profile := config.ClientConfig{
		Version: config.ClientVersion, ClientIP: "10.77.0.2", ServerIP: "10.77.0.1", Subnet: "10.77.0.0/24",
		ServerPubKey: strings.Repeat("01", 32), ClientPrivKey: strings.Repeat("02", 32), ClientPubKey: strings.Repeat("03", 32),
		InfoHash: strings.Repeat("04", 20), Transport: config.ModeExtTCP, CarrierEndpoint: "192.0.2.1:6881",
	}
	old, err := profile.ImportString()
	if err != nil {
		t.Fatal(err)
	}
	profile.CarrierEndpoint = "192.0.2.2:6881"
	newImport, err := profile.ImportString()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		status     int
		wantChange bool
		wantError  bool
	}{
		{"changed", newImport + "\n", 200, true, false},
		{"unchanged", old + "\n", 200, false, false},
		{"malformed", "btawg://bogus", 200, false, true},
		{"oversized", strings.Repeat("x", (64<<10)+1), 200, false, true},
		{"revoked", "not found", 404, false, true},
		{"redirect", "", 302, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/elsewhere" {
					t.Error("subscription refresh followed a redirect")
				}
				w.Header().Set("Location", "/elsewhere")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer s.Close()
			path := filepath.Join(t.TempDir(), "client.json")
			saved := client.ManagedConfig{
				Import: old, SubscriptionURL: s.URL + "/sub/example", Enabled: []string{config.ModeReqTCP},
				ListenIP: "127.0.0.4", TUNName: "my-tun", SOCKSBasePort: 12080,
				Credentials: client.Credentials{Username: "local-user", Password: "local-pass"},
				Balancers:   map[string][]string{"adaptive": {config.ModeReqTCP}},
				Routes:      map[int]string{1080: "remote"},
			}
			if err := saved.Save(path); err != nil {
				t.Fatal(err)
			}
			applied := 0
			changed, err := refreshSubscription(context.Background(), s.Client(), path, func(save func() error) error {
				applied++
				return save()
			})
			if changed != tc.wantChange || (err != nil) != tc.wantError || (applied != 0) != tc.wantChange {
				t.Fatalf("changed=%t, apply count=%d, error=%v", changed, applied, err)
			}
			got, err := client.LoadManagedConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantChange {
				saved.Import = newImport
			}
			if !reflect.DeepEqual(saved, got) {
				t.Fatal("refresh did not preserve local settings and expected import")
			}
		})
	}
}
