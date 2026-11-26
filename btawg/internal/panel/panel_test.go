package panel

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"bittorrent-tunnel/btawg/internal/awgrepair"
	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/server"
	"bittorrent-tunnel/btawg/internal/store"
)

type fakeRegistry struct {
	*store.Store
	registered       []store.ClientRecord
	revoked          []store.ClientRecord
	reloadCalls      int
	reloadErr        error
	activeConfig     config.ServerConfig
	managed          bool
	engineQueueDrops map[string]uint64
}

func (f *fakeRegistry) RegisterClient(rec store.ClientRecord) error {
	f.registered = append(f.registered, rec)
	return nil
}

func (f *fakeRegistry) RevokeClient(rec store.ClientRecord) error {
	f.revoked = append(f.revoked, rec)
	return nil
}

func (f *fakeRegistry) ReloadPolicy() error {
	f.reloadCalls++
	return f.reloadErr
}

func (f *fakeRegistry) Config() config.ServerConfig {
	if f.activeConfig.Transport != "" {
		return f.activeConfig
	}
	return config.DefaultServerConfig()
}

func (f *fakeRegistry) Managed() bool { return f.managed }

func (f *fakeRegistry) Traffic() map[string]store.Traffic { return nil }
func (f *fakeRegistry) InfoHash() string                  { return "aabbccddeeff00112233445566778899aabbccdd" }
func (f *fakeRegistry) PubKey() string                    { return "11223344556677889900aabbccddeeff00112233" }
func (f *fakeRegistry) TLSServerKey() string              { return "" }

func (f *fakeRegistry) EngineQueueDrops() map[string]uint64 { return f.engineQueueDrops }
func (f *fakeRegistry) AWGRepairStats() awgrepair.Stats     { return awgrepair.Stats{} }

func importParse(raw string) (config.ClientConfig, error) {
	return config.ParseImportString(raw)
}

func newTestPanel(t *testing.T) (*httptest.Server, *store.Store, *fakeRegistry) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/panel.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SaveServerConfig(config.DefaultServerConfig()); err != nil {
		t.Fatalf("save cfg: %v", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	reg := &fakeRegistry{Store: st}
	p := New(st, reg, "admin", string(hash))
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	return srv, st, reg
}

// login returns a client with a session cookie.
func login(t *testing.T, base string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	c := &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse // observe redirects instead of following
	}}
	resp, err := c.PostForm(base+"/login", map[string][]string{"username": {"admin"}, "password": {"secret"}})
	if err != nil {
		t.Fatalf("login post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login status = %d, want 302", resp.StatusCode)
	}
	return c
}

func csrfOf(t *testing.T, c *http.Client, base, path string) string {
	t.Helper()
	resp, err := c.Get(base + path)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer resp.Body.Close()
	body := make([]byte, 1<<16)
	n, _ := resp.Body.Read(body)
	page := string(body[:n])
	i := strings.Index(page, `name="csrf" value="`)
	if i < 0 {
		t.Fatalf("no csrf token in %s", path)
	}
	rest := page[i+len(`name="csrf" value="`):]
	return rest[:strings.Index(rest, `"`)]
}

func TestLoginRequired(t *testing.T) {
	srv, _, _ := newTestPanel(t)
	resp, err := (&http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}).Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("unauthenticated GET / = %d, want 302 to /login", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Fatalf("redirect = %q, want /login", loc)
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	srv, _, _ := newTestPanel(t)
	resp, err := http.PostForm(srv.URL+"/login", map[string][]string{"username": {"admin"}, "password": {"wrong"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bad login status = %d, want 200 (form re-render)", resp.StatusCode)
	}
}

func TestServerConfigFormRoundTrip(t *testing.T) {
	srv, st, _ := newTestPanel(t)
	c := login(t, srv.URL)
	csrf := csrfOf(t, c, srv.URL, "/server")
	form := map[string][]string{
		"csrf": {csrf},
		"jc":   {"4"}, "jmin": {"40"}, "jmax": {"100"},
		"s1": {"32"}, "s2": {"32"}, "s3": {"32"}, "s4": {"32"},
		"h1": {"101"}, "h2": {"102"}, "h3": {"103"}, "h4": {"104"},
		"transport":    {"ext-tcp"},
		"carrier_port": {"17777"}, "awg_port": {"51821"}, "wg_port": {"51820"},
		"panel_port":  {"8080"},
		"external_ip": {"203.0.113.7"},
	}
	resp, err := c.PostForm(srv.URL+"/server", form)
	if err != nil {
		t.Fatalf("post server: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("post server = %d, want 302", resp.StatusCode)
	}
	cfg, err := st.LoadServerConfig()
	if err != nil {
		t.Fatalf("load cfg: %v", err)
	}
	if cfg.Transport != config.ModeExtTCP || cfg.CarrierPort != 17777 || cfg.ExternalIP != "203.0.113.7" {
		t.Fatalf("cfg not saved: %+v", cfg)
	}
	if cfg.InfoHash != "" {
		// The loaded config preserves identity fields only if they were set;
		// the fake store never set them. Nothing to assert here.
		_ = cfg.InfoHash
	}
}

func TestServerAWGProfileRejectsInvalidAndKeepsSecret(t *testing.T) {
	srv, st, reg := newTestPanel(t)
	c := login(t, srv.URL)
	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	form := map[string][]string{
		"csrf": {csrfOf(t, c, srv.URL, "/server")},
		"jc":   {"4"}, "jmin": {"40"}, "jmax": {"100"},
		"s1": {"32"}, "s2": {"32"}, "s3": {"32"}, "s4": {"32"},
		"h1": {"1000-1099"}, "h2": {"1100-1199"}, "h3": {"1200-1299"}, "h4": {"1300-1399"},
		"header_protection_key": {key}, "random_trailers": {"1"}, "i1": {"<r 32>"},
		"transport": {"ext-tcp"}, "carrier_port": {"6881"}, "awg_port": {"51821"},
		"wg_port": {"51820"}, "panel_port": {"8080"}, "external_ip": {"203.0.113.7"},
	}
	post := func(want int) {
		t.Helper()
		resp, err := c.PostForm(srv.URL+"/server", form)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("POST status %d, want %d: %s", resp.StatusCode, want, body)
		}
		if strings.Contains(string(body), key) {
			t.Fatal("settings response disclosed the header protection key")
		}
	}
	post(http.StatusFound)
	get := func() config.ServerConfig {
		t.Helper()
		saved, err := st.LoadServerConfig()
		if err != nil {
			t.Fatal(err)
		}
		return saved
	}
	reg.activeConfig = get()
	delete(form, "header_protection_key")
	form["h2"] = []string{"1050-1150"}
	post(http.StatusBadRequest)
	form["h2"] = []string{"1100-1199"}
	post(http.StatusFound)
	ipc := get().AWGProfile.DeviceIPC()
	for _, line := range []string{"h1=1000-1099\n", "header_protection_key=" + key + "\n", "random_trailers=1\n", "i1=<r 32>\n"} {
		if !strings.Contains(ipc, line) {
			t.Fatal("saved settings did not reach the runtime profile")
		}
	}
	resp, err := c.Get(srv.URL + "/server")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), key) || !strings.Contains(string(body), "1000-1099") {
		t.Fatal("settings page failed to render the range without disclosing the key")
	}
}

func TestServerAWG31PresetKeepsKeyAndOtherSettings(t *testing.T) {
	srv, st, _ := newTestPanel(t)
	c := login(t, srv.URL)
	csrf := csrfOf(t, c, srv.URL, "/server")
	apply := func() config.ServerConfig {
		t.Helper()
		resp, err := c.PostForm(srv.URL+"/server/awg31-preset", map[string][]string{"csrf": {csrf}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("preset status = %d, want 302", resp.StatusCode)
		}
		cfg, err := st.LoadServerConfig()
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	cfg := apply()
	p := cfg.AWGProfile
	if !p.RandomTrailers || p.DisableCookies || p.JC != 4 || p.JMin != 40 || p.JMax != 100 || p.S1 != 32 || p.S2 != 32 || p.S3 != 32 || p.S4 != 32 || len(p.HeaderProtectionKey) != 64 {
		t.Fatal("selected preset was not saved")
	}
	if apply().AWGProfile != p {
		t.Fatal("reapplying the preset changed the shared key")
	}
	if cfg.PanelPort != 8080 || cfg.Transport != config.ModeExtUTP {
		t.Fatal("applying the AWG profile changed unrelated settings")
	}
}

func TestCreateClientProducesImportString(t *testing.T) {
	srv, st, reg := newTestPanel(t)
	c := login(t, srv.URL)
	csrf := csrfOf(t, c, srv.URL, "/clients")
	resp, err := c.PostForm(srv.URL+"/clients/create", map[string][]string{"csrf": {csrf}, "name": {"laptop"}})
	if err != nil {
		t.Fatalf("post create: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("create = %d, want 302", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/clients/import?id=") {
		t.Fatalf("redirect = %q, want import page", loc)
	}
	if len(reg.registered) != 1 {
		t.Fatalf("registry saw %d registrations, want 1", len(reg.registered))
	}

	// Fetch the import page and extract the btawg:// string.
	resp, err = c.Get(srv.URL + loc)
	if err != nil {
		t.Fatalf("get import: %v", err)
	}
	defer resp.Body.Close()
	pageBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read import page: %v", err)
	}
	page := string(pageBytes)
	i := -1
	for from := 0; ; {
		j := strings.Index(page[from:], "btawg://")
		if j < 0 {
			break
		}
		j += from
		if j+len("btawg://") < len(page) && isB64Byte(page[j+len("btawg://")]) {
			i = j
			break
		}
		from = j + 1
	}
	if i < 0 {
		t.Fatal("no btawg:// import string on the import page")
	}
	// The template HTML-escapes quotes; cut at the first byte outside the
	// base64url alphabet.
	end := i + len("btawg://")
	for end < len(page) && isB64Byte(page[end]) {
		end++
	}
	raw := page[i:end]
	cfg, err := parseImportForTest(t, st, raw)
	_ = cfg
	if err != nil {
		t.Fatalf("import string invalid: %v", err)
	}
}

func parseImportForTest(t *testing.T, st *store.Store, raw string) (any, error) {
	t.Helper()
	cfg, err := importParse(raw)
	if err != nil {
		return nil, err
	}
	clients, err := st.ListClients()
	if err != nil {
		return nil, err
	}
	if len(clients) != 1 {
		t.Fatalf("store has %d clients, want 1", len(clients))
	}
	if cfg.ClientIP != clients[0].IP {
		t.Fatalf("import IP %q != store IP %q", cfg.ClientIP, clients[0].IP)
	}
	if cfg.InfoHash != fakeInfoHash || cfg.ServerPubKey != fakePubKey {
		t.Fatal("import does not carry the hub identity")
	}
	if cfg.Version != config.ClientVersion {
		t.Fatal("import version")
	}
	return cfg, nil
}

func isB64Byte(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-' || b == '_'
}

const (
	fakeInfoHash = "aabbccddeeff00112233445566778899aabbccdd"
	fakePubKey   = "11223344556677889900aabbccddeeff00112233"
)

func TestRevokeClient(t *testing.T) {
	srv, st, reg := newTestPanel(t)
	c := login(t, srv.URL)
	csrf := csrfOf(t, c, srv.URL, "/clients")
	resp, err := c.PostForm(srv.URL+"/clients/create", map[string][]string{"csrf": {csrf}, "name": {"revoke-me"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	resp.Body.Close()
	clients, err := st.ListClients()
	if err != nil || len(clients) != 1 {
		t.Fatalf("clients after create: %v %d", err, len(clients))
	}
	id := clients[0].ID

	csrf = csrfOf(t, c, srv.URL, "/clients")
	resp, err = c.PostForm(srv.URL+"/clients/revoke", map[string][]string{"csrf": {csrf}, "id": {id}})
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("revoke = %d, want 302", resp.StatusCode)
	}
	got, err := st.GetClient(id)
	if err != nil {
		t.Fatalf("get revoked: %v", err)
	}
	if !got.Revoked {
		t.Fatal("store did not mark the client revoked")
	}
	if len(reg.revoked) != 1 {
		t.Fatalf("registry saw %d revocations, want 1", len(reg.revoked))
	}
}

func TestDeleteClientReleasesAddress(t *testing.T) {
	for _, state := range []string{"active", "revoked"} {
		t.Run(state, func(t *testing.T) {
			st, err := store.Open(t.TempDir() + "/panel.db")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			cfg := config.DefaultServerConfig()
			cfg.ExternalIP = "127.0.0.1"
			// Keep these ports distinct while obtaining all three from the OS.
			var reservations []*net.UDPConn
			for _, port := range []*int{&cfg.CarrierPort, &cfg.AWGPort, &cfg.WGPort} {
				pc, err := net.ListenUDP("udp4", &net.UDPAddr{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { pc.Close() })
				*port = pc.LocalAddr().(*net.UDPAddr).Port
				reservations = append(reservations, pc)
			}
			for _, pc := range reservations {
				pc.Close()
			}
			hub, err := server.New(cfg, st)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { hub.Close() })
			hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(New(st, hub, "admin", string(hash)).Handler())
			t.Cleanup(srv.Close)
			c := login(t, srv.URL)
			id := createClientForTest(t, c, srv.URL, "old-client")
			old, err := st.GetClient(id)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SaveTraffic(map[string]store.Traffic{id: {Up: 1}}); err != nil {
				t.Fatal(err)
			}
			form := map[string][]string{"csrf": {csrfOf(t, c, srv.URL, "/clients")}, "id": {id}}
			if state == "revoked" {
				resp, err := c.PostForm(srv.URL+"/clients/revoke", form)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusFound {
					t.Fatalf("revoke HTTP status = %d", resp.StatusCode)
				}
			}

			resp, err := c.PostForm(srv.URL+"/clients/delete", form)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusFound {
				t.Fatalf("delete HTTP status = %d, want redirect", resp.StatusCode)
			}
			resp, err = c.Get(srv.URL + "/clients/import?id=" + id)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("deleted import HTTP status = %d, want 404", resp.StatusCode)
			}
			traffic, err := st.LoadTraffic()
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := traffic[id]; exists {
				t.Fatal("deleted client's traffic is still persisted")
			}
			replacement := createClientForTest(t, c, srv.URL, "replacement")
			got, err := st.GetClient(replacement)
			if err != nil || got.IP != old.IP || got.PubKey == old.PubKey {
				t.Fatalf("replacement IP = %q, want %q with a new key; err = %v", got.IP, old.IP, err)
			}
		})
	}
}

func TestCSRFRequired(t *testing.T) {
	srv, _, _ := newTestPanel(t)
	c := login(t, srv.URL)
	resp, err := c.PostForm(srv.URL+"/clients/create", map[string][]string{"name": {"no-csrf"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("post without csrf = %d, want 403", resp.StatusCode)
	}
}

func createClientForTest(t *testing.T, c *http.Client, base, name string) string {
	t.Helper()
	csrf := csrfOf(t, c, base, "/clients")
	resp, err := c.PostForm(base+"/clients/create", map[string][]string{"csrf": {csrf}, "name": {name}})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("create %s = %d, want 302", name, resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	const prefix = "/clients/import?id="
	if !strings.HasPrefix(loc, prefix) {
		t.Fatalf("create %s redirect = %q, want import page", name, loc)
	}
	return loc[len(prefix):]
}

func TestUpdateClientPolicy(t *testing.T) {
	srv, st, reg := newTestPanel(t)
	c := login(t, srv.URL)
	idA := createClientForTest(t, c, srv.URL, "A")
	idB := createClientForTest(t, c, srv.URL, "B")

	csrf := csrfOf(t, c, srv.URL, "/clients")
	resp, err := c.PostForm(srv.URL+"/clients/policy", map[string][]string{
		"csrf":     {csrf},
		"id":       {idA},
		"internet": {"off"},
		"peers":    {idB},
	})
	if err != nil {
		t.Fatalf("post policy: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("post policy = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Fatalf("redirect = %q, want /", loc)
	}

	rec, err := st.GetClient(idA)
	if err != nil {
		t.Fatalf("get client: %v", err)
	}
	if rec.Internet == nil || *rec.Internet {
		t.Fatalf("Internet = %v, want explicit false", rec.Internet)
	}
	if len(rec.VisiblePeers) != 1 || rec.VisiblePeers[0] != idB {
		t.Fatalf("VisiblePeers = %v, want [%s]", rec.VisiblePeers, idB)
	}
	if reg.reloadCalls != 1 {
		t.Fatalf("ReloadPolicy calls = %d, want 1", reg.reloadCalls)
	}
}

func TestUpdateClientPolicyInternetPresence(t *testing.T) {
	srv, st, reg := newTestPanel(t)
	c := login(t, srv.URL)
	id := createClientForTest(t, c, srv.URL, "A")

	csrf := csrfOf(t, c, srv.URL, "/clients")
	resp, err := c.PostForm(srv.URL+"/clients/policy", map[string][]string{
		"csrf":     {csrf},
		"id":       {id},
		"internet": {"on"},
		"peers":    {"*"},
	})
	if err != nil {
		t.Fatalf("post policy: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("post policy = %d, want 302", resp.StatusCode)
	}

	rec, err := st.GetClient(id)
	if err != nil {
		t.Fatalf("get client: %v", err)
	}
	if rec.Internet == nil || !*rec.Internet {
		t.Fatalf("Internet = %v, want explicit true when internet field is on", rec.Internet)
	}
	if reg.reloadCalls != 1 {
		t.Fatalf("ReloadPolicy calls = %d, want 1", reg.reloadCalls)
	}
}

func TestUpdateClientPolicyEmptyPeersIsNobody(t *testing.T) {
	srv, st, _ := newTestPanel(t)
	c := login(t, srv.URL)
	id := createClientForTest(t, c, srv.URL, "A")

	csrf := csrfOf(t, c, srv.URL, "/clients")
	resp, err := c.PostForm(srv.URL+"/clients/policy", map[string][]string{
		"csrf": {csrf},
		"id":   {id},
		// peers intentionally omitted -> [] (nobody)
	})
	if err != nil {
		t.Fatalf("post policy: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("post policy = %d, want 302", resp.StatusCode)
	}

	rec, err := st.GetClient(id)
	if err != nil {
		t.Fatalf("get client: %v", err)
	}
	if len(rec.VisiblePeers) != 0 {
		t.Fatalf("VisiblePeers = %v, want []", rec.VisiblePeers)
	}

	// Saving the same record again with no peer selection must preserve isolation.
	csrf = csrfOf(t, c, srv.URL, "/clients")
	resp, err = c.PostForm(srv.URL+"/clients/policy", map[string][]string{
		"csrf": {csrf},
		"id":   {id},
	})
	if err != nil {
		t.Fatalf("post policy again: %v", err)
	}
	resp.Body.Close()
	rec, err = st.GetClient(id)
	if err != nil {
		t.Fatalf("get client: %v", err)
	}
	if len(rec.VisiblePeers) != 0 {
		t.Fatalf("VisiblePeers after re-save = %v, want []", rec.VisiblePeers)
	}
}

func TestUpdateClientPolicyAllOptionIsAll(t *testing.T) {
	srv, st, _ := newTestPanel(t)
	c := login(t, srv.URL)
	id := createClientForTest(t, c, srv.URL, "A")

	csrf := csrfOf(t, c, srv.URL, "/clients")
	resp, err := c.PostForm(srv.URL+"/clients/policy", map[string][]string{
		"csrf":  {csrf},
		"id":    {id},
		"peers": {"*"},
	})
	if err != nil {
		t.Fatalf("post policy: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("post policy = %d, want 302", resp.StatusCode)
	}

	rec, err := st.GetClient(id)
	if err != nil {
		t.Fatalf("get client: %v", err)
	}
	if len(rec.VisiblePeers) != 1 || rec.VisiblePeers[0] != "*" {
		t.Fatalf("VisiblePeers = %v, want [*]", rec.VisiblePeers)
	}
}

func TestUpdateClientPolicyEditInternetKeepsIsolation(t *testing.T) {
	srv, st, _ := newTestPanel(t)
	c := login(t, srv.URL)
	id := createClientForTest(t, c, srv.URL, "A")

	// First isolate the client.
	csrf := csrfOf(t, c, srv.URL, "/clients")
	resp, err := c.PostForm(srv.URL+"/clients/policy", map[string][]string{
		"csrf": {csrf},
		"id":   {id},
	})
	if err != nil {
		t.Fatalf("isolate: %v", err)
	}
	resp.Body.Close()

	// Now edit only the internet checkbox; peers stays unselected.
	csrf = csrfOf(t, c, srv.URL, "/clients")
	resp, err = c.PostForm(srv.URL+"/clients/policy", map[string][]string{
		"csrf":     {csrf},
		"id":       {id},
		"internet": {"on"},
	})
	if err != nil {
		t.Fatalf("enable internet: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("post policy = %d, want 302", resp.StatusCode)
	}

	rec, err := st.GetClient(id)
	if err != nil {
		t.Fatalf("get client: %v", err)
	}
	if rec.Internet == nil || !*rec.Internet {
		t.Fatalf("Internet = %v, want explicit true", rec.Internet)
	}
	if len(rec.VisiblePeers) != 0 {
		t.Fatalf("VisiblePeers = %v, want []", rec.VisiblePeers)
	}
}

func TestUpdateClientPolicyUnknownID(t *testing.T) {
	srv, _, _ := newTestPanel(t)
	c := login(t, srv.URL)
	csrf := csrfOf(t, c, srv.URL, "/clients")
	resp, err := c.PostForm(srv.URL+"/clients/policy", map[string][]string{
		"csrf":  {csrf},
		"id":    {"no-such-client"},
		"peers": {"*"},
	})
	if err != nil {
		t.Fatalf("post policy: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("post policy unknown id = %d, want 404", resp.StatusCode)
	}
}

func TestCreateClientInGroup(t *testing.T) {
	srv, st, reg := newTestPanel(t)
	c := login(t, srv.URL)

	// Create a custom group first.
	csrf := csrfOf(t, c, srv.URL, "/groups")
	resp, err := c.PostForm(srv.URL+"/groups/save", map[string][]string{
		"csrf":         {csrf},
		"name":         {"work"},
		"subnet":       {"10.77.5.0/24"},
		"server_ip":    {"10.77.5.1"},
		"internet":     {"on"},
		"visible_mode": {"same"},
	})
	if err != nil {
		t.Fatalf("save group: %v", err)
	}
	resp.Body.Close()
	groups, _ := st.ListGroups()
	var groupID string
	for _, g := range groups {
		if g.Name == "work" {
			groupID = g.ID
		}
	}
	if groupID == "" {
		t.Fatalf("work group not found in %d groups", len(groups))
	}

	// Create a client assigned to that group.
	csrf = csrfOf(t, c, srv.URL, "/clients")
	resp, err = c.PostForm(srv.URL+"/clients/create", map[string][]string{
		"csrf":     {csrf},
		"name":     {"laptop"},
		"group_id": {groupID},
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	resp.Body.Close()
	if len(reg.registered) != 1 {
		t.Fatalf("registered = %d, want 1", len(reg.registered))
	}
	rec := reg.registered[0]
	if rec.GroupID != groupID {
		t.Fatalf("GroupID = %q, want %q", rec.GroupID, groupID)
	}
	if !strings.HasPrefix(rec.IP, "10.77.5.") {
		t.Fatalf("IP %q not in group subnet", rec.IP)
	}
}

func TestMoveClientChangesGroup(t *testing.T) {
	srv, st, _ := newTestPanel(t)
	c := login(t, srv.URL)
	id := createClientForTest(t, c, srv.URL, "A")

	csrf := csrfOf(t, c, srv.URL, "/groups")
	resp, err := c.PostForm(srv.URL+"/groups/save", map[string][]string{
		"csrf":         {csrf},
		"name":         {"work"},
		"subnet":       {"10.77.5.0/24"},
		"server_ip":    {"10.77.5.1"},
		"internet":     {"on"},
		"visible_mode": {"same"},
	})
	if err != nil {
		t.Fatalf("save group: %v", err)
	}
	resp.Body.Close()
	groups, _ := st.ListGroups()
	var groupID string
	for _, g := range groups {
		if g.Name == "work" {
			groupID = g.ID
		}
	}
	if groupID == "" {
		t.Fatalf("work group not found")
	}

	before, _ := st.GetClient(id)
	csrf = csrfOf(t, c, srv.URL, "/clients")
	resp, err = c.PostForm(srv.URL+"/clients/move", map[string][]string{
		"csrf":     {csrf},
		"id":       {id},
		"group_id": {groupID},
	})
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("move = %d, want 302", resp.StatusCode)
	}

	after, _ := st.GetClient(id)
	if after.GroupID != groupID {
		t.Fatalf("GroupID = %q, want %q", after.GroupID, groupID)
	}
	if after.IP == before.IP {
		t.Fatalf("IP unchanged after move: %s", after.IP)
	}
	if !strings.HasPrefix(after.IP, "10.77.5.") {
		t.Fatalf("IP %q not in new group subnet", after.IP)
	}
}

func TestGroupCRUD(t *testing.T) {
	srv, st, _ := newTestPanel(t)
	c := login(t, srv.URL)

	csrf := csrfOf(t, c, srv.URL, "/groups")
	resp, err := c.PostForm(srv.URL+"/groups/save", map[string][]string{
		"csrf":         {csrf},
		"name":         {"work"},
		"subnet":       {"10.77.5.0/24"},
		"server_ip":    {"10.77.5.1"},
		"internet":     {"on"},
		"visible_mode": {"all"},
	})
	if err != nil {
		t.Fatalf("save group: %v", err)
	}
	resp.Body.Close()

	groups, _ := st.ListGroups()
	var g store.GroupRecord
	for _, gr := range groups {
		if gr.Name == "work" {
			g = gr
		}
	}
	if g.ID == "" {
		t.Fatalf("work group not found after save in %d groups", len(groups))
	}
	if g.Name != "work" || len(g.VisibleGroups) != 1 || g.VisibleGroups[0] != "*" {
		t.Fatalf("group = %+v", g)
	}

	// Edit to listed visibility.
	csrf = csrfOf(t, c, srv.URL, "/groups")
	resp, err = c.PostForm(srv.URL+"/groups/save", map[string][]string{
		"csrf":           {csrf},
		"id":             {g.ID},
		"name":           {"work"},
		"subnet":         {"10.77.5.0/24"},
		"server_ip":      {"10.77.5.1"},
		"internet":       {"on"},
		"visible_mode":   {"list"},
		"visible_groups": {"other"},
	})
	if err != nil {
		t.Fatalf("edit group: %v", err)
	}
	resp.Body.Close()
	g, _ = st.GetGroup(g.ID)
	if len(g.VisibleGroups) != 1 || g.VisibleGroups[0] != "other" {
		t.Fatalf("VisibleGroups after edit = %v", g.VisibleGroups)
	}

	// Delete.
	csrf = csrfOf(t, c, srv.URL, "/groups")
	resp, err = c.PostForm(srv.URL+"/groups/delete", map[string][]string{
		"csrf": {csrf},
		"id":   {g.ID},
	})
	if err != nil {
		t.Fatalf("delete group: %v", err)
	}
	resp.Body.Close()
	groups, _ = st.ListGroups()
	for _, gr := range groups {
		if gr.Name == "work" {
			t.Fatalf("work group still present after delete")
		}
	}
}

func TestNativeExportRequiresExternalIP(t *testing.T) {
	srv, _, _ := newTestPanel(t)
	c := login(t, srv.URL)
	id := createClientForTest(t, c, srv.URL, "A")

	resp, err := c.Get(srv.URL + "/clients/native?id=" + id + "&mode=awg")
	if err != nil {
		t.Fatalf("get native: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("native without ExternalIP = %d, want 412", resp.StatusCode)
	}
}

func TestRevokedClientRefusedExport(t *testing.T) {
	srv, _, _ := newTestPanel(t)
	c := login(t, srv.URL)
	id := createClientForTest(t, c, srv.URL, "A")

	csrf := csrfOf(t, c, srv.URL, "/clients")
	resp, err := c.PostForm(srv.URL+"/clients/revoke", map[string][]string{
		"csrf": {csrf},
		"id":   {id},
	})
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	resp.Body.Close()

	resp, err = c.Get(srv.URL + "/clients/import?id=" + id)
	if err != nil {
		t.Fatalf("get import: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("import revoked = %d, want 403", resp.StatusCode)
	}

	resp, err = c.Get(srv.URL + "/clients/native?id=" + id + "&mode=awg")
	if err != nil {
		t.Fatalf("get native: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("native revoked = %d, want 403", resp.StatusCode)
	}
}

func TestGroupSaveReloadRevert(t *testing.T) {
	srv, st, reg := newTestPanel(t)
	reg.reloadErr = fmt.Errorf("runtime reload failed")
	c := login(t, srv.URL)

	csrf := csrfOf(t, c, srv.URL, "/groups")
	resp, err := c.PostForm(srv.URL+"/groups/save", map[string][]string{
		"csrf":         {csrf},
		"name":         {"work"},
		"subnet":       {"10.77.5.0/24"},
		"server_ip":    {"10.77.5.1"},
		"internet":     {"on"},
		"visible_mode": {"same"},
	})
	if err != nil {
		t.Fatalf("save group: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("save group with failed reload = %d, want 500", resp.StatusCode)
	}
	groups, _ := st.ListGroups()
	for _, gr := range groups {
		if gr.Name == "work" {
			t.Fatalf("work group was not reverted")
		}
	}
}
