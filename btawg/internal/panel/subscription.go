package panel

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"bittorrent-tunnel/btawg/internal/store"
)

// SubscriptionPort is the public TLS port for profile downloads.
const SubscriptionPort = 2228

// SubscriptionHandler enables the subscription controls and returns the handler
// for the separate HTTPS listener. Call it before serving the admin panel.
func (p *Panel) SubscriptionHandler(host string) http.Handler {
	p.subscriptionBase = "https://" + net.JoinHostPort(host, strconv.Itoa(SubscriptionPort)) + "/sub/"
	return http.HandlerFunc(p.downloadSubscription)
}

func (p *Panel) clientProfile(rec store.ClientRecord) (string, error) {
	cfg, err := p.buildClientConfig(rec, p.reg.Config())
	if err != nil {
		return "", err
	}
	return cfg.ImportString()
}

func (p *Panel) rotateSubscription(w http.ResponseWriter, r *http.Request) {
	if p.subscriptionBase == "" {
		http.NotFound(w, r)
		return
	}
	id := r.PostFormValue("id")
	if err := p.st.RotateSubscription(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "subscription unavailable", http.StatusInternalServerError)
		}
		return
	}
	http.Redirect(w, r, "/clients/import?id="+url.QueryEscape(id), http.StatusFound)
}

func (p *Panel) downloadSubscription(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	token, ok := strings.CutPrefix(r.URL.Path, "/sub/")
	if !ok || r.Method != http.MethodGet || r.TLS == nil {
		http.NotFound(w, r)
		return
	}
	rec, err := p.st.SubscriptionClient(token)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err == nil {
		var profile string
		profile, err = p.clientProfile(rec)
		if err == nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			io.WriteString(w, profile)
			io.WriteString(w, "\n")
			return
		}
	}
	http.Error(w, "subscription unavailable", http.StatusInternalServerError)
}
