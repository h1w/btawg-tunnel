package panel

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestLiveStatsRequireSessionAndExcludeSecrets(t *testing.T) {
	srv, _, registry := newTestPanel(t)
	registry.engineQueueDrops = map[string]uint64{"socks5-wg-udp": 7}
	admin := login(t, srv.URL)
	const endpoint = "/api/traffic"
	visitor := &http.Client{CheckRedirect: admin.CheckRedirect}
	r, err := visitor.Get(srv.URL + endpoint)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusFound || r.Header.Get("Location") != "/login" {
		t.Fatalf("anonymous stats: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	r, err = admin.PostForm(srv.URL+"/clients/create", map[string][]string{
		"csrf": {csrfOf(t, admin, srv.URL, "/clients")}, "name": {"Studio laptop"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusFound {
		t.Fatalf("client create: %d", r.StatusCode)
	}
	r, err = admin.Get(srv.URL + endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK || !strings.Contains(r.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("stats: %d, cache %q", r.StatusCode, r.Header.Get("Cache-Control"))
	}
	var data struct {
		Clients          map[string]map[string]json.RawMessage
		EngineQueueDrops map[string]uint64 `json:"engine_queue_drops"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	if data.EngineQueueDrops["socks5-wg-udp"] != 7 {
		t.Fatalf("unexpected engine drops: %v", data.EngineQueueDrops)
	}
	if len(data.Clients) != 1 {
		t.Fatalf("stats users = %v", data.Clients)
	}
	for _, row := range data.Clients {
		if string(row["status"]) != `"never"` {
			t.Fatalf("fresh client status = %s", row["status"])
		}
		for _, key := range []string{"privkey", "pubkey", "peer_id"} {
			if _, ok := row[key]; ok {
				t.Fatalf("stats expose %s", key)
			}
		}
	}
}
