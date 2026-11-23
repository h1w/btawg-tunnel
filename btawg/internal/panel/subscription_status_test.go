package panel

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"bittorrent-tunnel/btawg/internal/store"
)

func TestSubscriptionPanelShowsCertificateReadiness(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/server.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	var state atomic.Int32
	p := New(st, &fakeRegistry{Store: st}, "admin", string(hash))
	p.SubscriptionHandler("192.0.2.10", func() (bool, string) {
		switch state.Load() {
		case 1:
			return false, "Port 80 is unreachable."
		case 2:
			return true, ""
		default:
			return false, "Requesting a Let's Encrypt certificate."
		}
	})
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()
	admin := login(t, srv.URL)
	id := createClientForTest(t, admin, srv.URL, "laptop")
	page := "/clients/import?id=" + id
	get := func() string {
		t.Helper()
		res, err := admin.Get(srv.URL + page)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("import returned %d: %v", res.StatusCode, err)
		}
		return string(data)
	}
	checkUnavailable := func(message string) {
		t.Helper()
		body := get()
		if !strings.Contains(body, message) || !strings.Contains(body, `id="import-string"`) || strings.Contains(body, `action="/clients/subscription"`) {
			t.Fatal("missing certificate state, hidden static import, or premature subscription form")
		}
	}
	checkUnavailable("Requesting a Let")
	csrf := csrfOf(t, admin, srv.URL, "/clients")
	res, err := admin.PostForm(srv.URL+"/clients/subscription", url.Values{"id": {id}, "csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("issue before TLS readiness returned %d", res.StatusCode)
	}
	state.Store(1)
	checkUnavailable("Port 80 is unreachable.")
	state.Store(2)
	if body := get(); !strings.Contains(body, `action="/clients/subscription"`) {
		t.Fatal("ready certificate did not enable link issuance")
	}
}
