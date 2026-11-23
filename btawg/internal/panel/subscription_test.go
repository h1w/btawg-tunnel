package panel

import (
	"io"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func TestSubscriptionLifecycle(t *testing.T) {
	path := t.TempDir() + "/panel.db"
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { st.Close() }()
	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	reg := &fakeRegistry{Store: st, activeConfig: config.DefaultServerConfig()}
	reg.activeConfig.ExternalIP = "192.0.2.10"
	p := New(st, reg, "admin", string(hash))
	public := httptest.NewTLSServer(p.SubscriptionHandler("192.0.2.10", nil))
	defer public.Close()
	admin := httptest.NewServer(p.Handler())
	defer admin.Close()
	c := login(t, admin.URL)
	id := createClientForTest(t, c, admin.URL, "subscriber")
	importPage := "/clients/import?id=" + id
	csrf := csrfOf(t, c, admin.URL, importPage)

	post := func(path, id, csrf string, want int) {
		t.Helper()
		r, err := c.PostForm(admin.URL+path, url.Values{"csrf": {csrf}, "id": {id}})
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != want {
			t.Fatalf("POST %s: got %d, want %d", path, r.StatusCode, want)
		}
	}
	link := func(page string) string {
		t.Helper()
		r, err := c.Get(admin.URL + page)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, err := io.ReadAll(r.Body)
		if err != nil || r.StatusCode != 200 {
			t.Fatalf("import page: status %d, read %v", r.StatusCode, err)
		}
		_, tail, ok := strings.Cut(string(b), "https://192.0.2.10:2228/sub/")
		if !ok {
			t.Fatal("subscription URL missing from import page")
		}
		return "/sub/" + strings.SplitN(tail, "<", 2)[0]
	}
	get := func(path string, want int) string {
		t.Helper()
		r, err := public.Client().Get(public.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		if r.StatusCode != want {
			t.Fatalf("subscription response: got %d, want %d", r.StatusCode, want)
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if want == 200 && r.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("private subscription is cacheable")
		}
		return string(b)
	}
	check := func(path, clientID, addr string) {
		t.Helper()
		raw := get(path, 200)
		if !strings.HasSuffix(raw, "\n") || strings.Count(raw, "\n") != 1 {
			t.Fatal("response is not one newline-terminated import")
		}
		cfg, err := config.ParseImportString(strings.TrimSuffix(raw, "\n"))
		if err != nil {
			t.Fatal("response is not a btawg import")
		}
		rec, err := st.GetClient(clientID)
		if err != nil || !strings.HasPrefix(cfg.CarrierEndpoint, addr+":") || cfg.ClientPrivKey != rec.PrivKey || cfg.ClientIP != rec.IP {
			t.Fatal("response does not match the active hub and enrolled client")
		}
	}

	get("/login", 404)
	post("/clients/subscription", id, "wrong", 403)
	post("/clients/subscription", id, csrf, 302)
	first := link(importPage)
	other := createClientForTest(t, c, admin.URL, "deleted")
	post("/clients/subscription", other, csrf, 302)
	otherLink := link("/clients/import?id=" + other)
	if first == otherLink {
		t.Fatal("different clients received the same subscription URL")
	}
	check(otherLink, other, "192.0.2.10")
	pending := reg.activeConfig
	pending.ExternalIP = "192.0.2.99"
	if err := st.SaveServerConfig(pending); err != nil {
		t.Fatal(err)
	}
	check(first, id, "192.0.2.10")
	reg.activeConfig.ExternalIP = "192.0.2.11"
	check(first, id, "192.0.2.11")

	// Reconstruct the download handler from a reopened database.
	public.Close()
	admin.Close()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	reg.Store = st
	p = New(st, reg, "admin", string(hash))
	public = httptest.NewTLSServer(p.SubscriptionHandler("192.0.2.10", nil))
	defer public.Close()
	admin = httptest.NewServer(p.Handler())
	defer admin.Close()
	c = login(t, admin.URL)
	csrf = csrfOf(t, c, admin.URL, importPage)
	if link(importPage) != first {
		t.Fatal("subscription link changed after reopening the store")
	}
	check(first, id, "192.0.2.11")
	post("/clients/subscription", id, csrf, 302)
	second := link(importPage)
	get(first, 404)
	check(second, id, "192.0.2.11")
	post("/clients/revoke", id, csrf, 302)
	get(second, 404)

	post("/clients/delete", other, csrf, 302)
	get(otherLink, 404)
}
