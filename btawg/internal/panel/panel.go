// Package panel is the btawg web interface (3x-ui style): single-admin login
// with CSRF and login rate limiting, the hub configuration form, and client
// management whose create action yields the btawg:// import string. The panel
// binds loopback in production; SSH port-forwarding reaches it.
package panel

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"bittorrent-tunnel/btawg/internal/awg"
	"bittorrent-tunnel/btawg/internal/awgrepair"
	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/store"
)

// Registry is what the panel drives; the assembled hub implements it.
type Registry interface {
	RegisterClient(store.ClientRecord) error
	RevokeClient(store.ClientRecord) error
	DeleteClient(string) error
	ReloadPolicy() error
	Config() config.ServerConfig
	Managed() bool
	Traffic() map[string]store.Traffic
	EngineQueueDrops() map[string]uint64
	AWGRepairStats() awgrepair.Stats
	InfoHash() string
	PubKey() string
	TLSServerKey() string
}

const (
	sessionTTL    = 24 * time.Hour
	loginWindow   = 15 * time.Minute
	maxLoginFails = 5
)

type session struct {
	csrf    string
	expires time.Time
}

type failState struct {
	count int
	until time.Time
}

// Panel is the web interface.
type Panel struct {
	st   *store.Store
	reg  Registry
	user string
	pass []byte // bcrypt hash

	subscriptionBase   string
	subscriptionStatus func() (ready bool, message string)

	mu        sync.Mutex
	sessions  map[string]*session
	fails     map[string]*failState
	mutations sync.Mutex
}

// New builds the panel for the given store, hub registry and admin
// credentials (pass is a bcrypt hash).
func New(st *store.Store, reg Registry, user, passHash string) *Panel {
	return &Panel{
		st:       st,
		reg:      reg,
		user:     user,
		pass:     []byte(passHash),
		sessions: make(map[string]*session),
		fails:    make(map[string]*failState),
	}
}

// Handler returns the routed panel.
func (p *Panel) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", p.loginPage)
	mux.HandleFunc("GET /assets/panel.css", panelAsset("text/css; charset=utf-8", panelCSS))
	mux.HandleFunc("GET /assets/panel.js", panelAsset("text/javascript; charset=utf-8", panelJS))
	mux.HandleFunc("POST /login", p.loginSubmit)
	mux.HandleFunc("POST /logout", p.auth(p.logout))
	mux.HandleFunc("GET /{$}", p.auth(p.dashboard))
	mux.HandleFunc("GET /api/traffic", p.auth(p.liveStats))
	mux.HandleFunc("GET /server", p.auth(p.serverPage))
	mux.HandleFunc("POST /server", p.auth(p.serverSubmit))
	mux.HandleFunc("POST /server/awg31-preset", p.auth(p.serverAWG31Preset))
	mux.HandleFunc("GET /clients", p.auth(p.clientsPage))
	mux.HandleFunc("POST /clients/create", p.auth(p.clientCreate))
	mux.HandleFunc("POST /clients/revoke", p.auth(p.clientRevoke))
	mux.HandleFunc("POST /clients/delete", p.auth(p.clientDelete))
	mux.HandleFunc("POST /clients/move", p.auth(p.clientMove))
	mux.HandleFunc("POST /clients/policy", p.auth(p.clientPolicy))
	mux.HandleFunc("GET /clients/import", p.auth(p.clientImport))
	mux.HandleFunc("POST /clients/subscription", p.auth(p.rotateSubscription))
	mux.HandleFunc("GET /clients/native", p.auth(p.clientNative))
	mux.HandleFunc("GET /groups", p.auth(p.groupsPage))
	mux.HandleFunc("GET /groups/edit", p.auth(p.groupEdit))
	mux.HandleFunc("POST /groups/save", p.auth(p.groupSave))
	mux.HandleFunc("POST /groups/delete", p.auth(p.groupDelete))
	mux.HandleFunc("GET /routes", p.auth(p.routesPage))
	mux.HandleFunc("GET /routes/edit", p.auth(p.routeEdit))
	mux.HandleFunc("POST /routes/save", p.auth(p.routeSave))
	mux.HandleFunc("POST /routes/delete", p.auth(p.routeDelete))
	mux.HandleFunc("POST /routes/role", p.auth(p.routeRole))
	return mux
}

// ---- auth plumbing ----

func (p *Panel) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("btawg_session")
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		p.mu.Lock()
		s := p.sessions[c.Value]
		if s != nil && time.Now().After(s.expires) {
			delete(p.sessions, c.Value)
			s = nil
		}
		p.mu.Unlock()
		if s == nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil || r.PostFormValue("csrf") != s.csrf {
				http.Error(w, "bad csrf", http.StatusForbidden)
				return
			}
			p.mutations.Lock()
			defer p.mutations.Unlock()
		}
		next(w, r)
	}
}

func (p *Panel) newSession(w http.ResponseWriter) {
	tok := randHex(24)
	p.mu.Lock()
	p.sessions[tok] = &session{csrf: randHex(16), expires: time.Now().Add(sessionTTL)}
	p.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     "btawg_session",
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

func (p *Panel) session(r *http.Request) *session {
	c, err := r.Cookie("btawg_session")
	if err != nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessions[c.Value]
}

func (p *Panel) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("btawg_session"); err == nil {
		p.mu.Lock()
		delete(p.sessions, c.Value)
		p.mu.Unlock()
	}
	http.Redirect(w, r, "/login", http.StatusFound)
}

// ---- login ----

func (p *Panel) loginPage(w http.ResponseWriter, r *http.Request) {
	p.render(w, "login", map[string]any{"Error": ""})
}

func (p *Panel) loginSubmit(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	p.mu.Lock()
	f := p.fails[ip]
	if f == nil {
		f = &failState{}
		p.fails[ip] = f
	}
	locked := time.Now().Before(f.until)
	p.mu.Unlock()
	if locked {
		p.render(w, "login", map[string]any{"Error": "too many attempts, wait 15 minutes"})
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	user := r.PostFormValue("username")
	pass := r.PostFormValue("password")
	if user != p.user || bcrypt.CompareHashAndPassword(p.pass, []byte(pass)) != nil {
		p.mu.Lock()
		f.count++
		if f.count >= maxLoginFails {
			f.until = time.Now().Add(loginWindow)
			f.count = 0
		}
		p.mu.Unlock()
		p.render(w, "login", map[string]any{"Error": "wrong username or password"})
		return
	}
	p.mu.Lock()
	f.count = 0
	p.mu.Unlock()
	p.newSession(w)
	http.Redirect(w, r, "/", http.StatusFound)
}

// ---- dashboard ----

func (p *Panel) dashboard(w http.ResponseWriter, r *http.Request) {
	p.userPage(w, r, "dashboard")
}

// ---- server settings ----

func (p *Panel) serverPage(w http.ResponseWriter, r *http.Request) {
	pending, err := p.st.LoadServerConfig()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pending.Normalize()
	active := p.reg.Config()
	profileChanged := pending.AWGProfile != active.AWGProfile
	keyConfigured := pending.AWGProfile.HeaderProtectionKey != ""
	pending.AWGProfile.HeaderProtectionKey = ""
	active.AWGProfile.HeaderProtectionKey = ""
	p.render(w, "server", map[string]any{
		"CSRF":              p.session(r).csrf,
		"Cfg":               pending,
		"ActiveCfg":         active,
		"AWGProfileChanged": profileChanged,
		"AWGKeyConfigured":  keyConfigured,
		"Managed":           p.reg.Managed(),
		"Modes":             config.ClientModes(),
		"Saved":             r.URL.Query().Get("saved") == "1",
		"Active":            "server",
	})
}

func (p *Panel) serverSubmit(w http.ResponseWriter, r *http.Request) {
	cfg, err := p.st.LoadServerConfig()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	get := func(name string) int {
		var v int
		fmt.Sscanf(r.PostFormValue(name), "%d", &v)
		return v
	}
	value := func(field string) string { return strings.TrimSpace(r.PostFormValue(field)) }
	rangeValue := func(field string) config.AWGRange { return config.AWGRange(value(field)) }
	checked := func(field string) bool { return value(field) == "1" }
	profile := config.AWGProfile{
		H1: rangeValue("h1"), H2: rangeValue("h2"), H3: rangeValue("h3"), H4: rangeValue("h4"),
		I1: value("i1"), I2: value("i2"), I3: value("i3"), I4: value("i4"), I5: value("i5"),
		HeaderProtectionKey:    cfg.AWGProfile.HeaderProtectionKey,
		ContentPaddingAddition: rangeValue("content_padding_addition"),
		RekeyAfterTime:         rangeValue("rekey_after_time"), RekeyTimeout: rangeValue("rekey_timeout"),
		RejectAfterTime: rangeValue("reject_after_time"), KeepaliveTimeout: rangeValue("keepalive_timeout"),
		MaxHandshakeAttempts: rangeValue("max_handshake_attempts"), PersistentKeepalive: rangeValue("persistent_keepalive"),
		RandomTrailers: checked("random_trailers"), DisableCookies: checked("disable_cookies"),
	}
	for name, dst := range map[string]*int{
		"jc": &profile.JC, "jmin": &profile.JMin, "jmax": &profile.JMax,
		"s1": &profile.S1, "s2": &profile.S2, "s3": &profile.S3, "s4": &profile.S4,
	} {
		n, err := strconv.ParseUint(value(name), 10, 16)
		if err != nil {
			http.Error(w, "invalid AWG setting: "+name, http.StatusBadRequest)
			return
		}
		*dst = int(n)
	}
	key := value("header_protection_key")
	if checked("clear_header_protection_key") {
		if key != "" {
			http.Error(w, "choose either replacing or clearing the header protection key", http.StatusBadRequest)
			return
		}
		profile.HeaderProtectionKey = ""
	} else if key != "" {
		profile.HeaderProtectionKey = strings.ToLower(key)
	}
	if profile.H1 == "" || profile.H2 == "" || profile.H3 == "" || profile.H4 == "" {
		http.Error(w, "H1–H4 are required", http.StatusBadRequest)
		return
	}
	if err := profile.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cfg.AWGProfile = profile
	cfg.Transport = config.CanonicalMode(r.PostFormValue("transport"))
	if cfg.LegacyTransport == "" {
		cfg.LegacyTransport = p.reg.Config().LegacyTransport
	}
	if !config.ValidClientMode(cfg.Transport) {
		http.Error(w, "bad transport", http.StatusBadRequest)
		return
	}
	if p.reg.Managed() {
		cfg.CarrierPort = config.DefaultServerConfig().CarrierPort
		cfg.AWGPort = config.DefaultServerConfig().AWGPort
		cfg.WGPort = config.DefaultServerConfig().WGPort
	} else {
		cfg.CarrierPort = get("carrier_port")
		cfg.AWGPort = get("awg_port")
		cfg.WGPort = get("wg_port")
	}
	cfg.PanelPort = get("panel_port")
	cfg.ExternalIP = strings.TrimSpace(r.PostFormValue("external_ip"))
	if cfg.CarrierPort <= 0 || cfg.CarrierPort > 65535 ||
		cfg.AWGPort <= 0 || cfg.AWGPort > 65535 ||
		cfg.WGPort <= 0 || cfg.WGPort > 65535 ||
		cfg.PanelPort <= 0 || cfg.PanelPort > 65535 {
		http.Error(w, "bad port", http.StatusBadRequest)
		return
	}
	if cfg.CarrierPort == cfg.AWGPort || cfg.CarrierPort == cfg.WGPort || cfg.AWGPort == cfg.WGPort {
		http.Error(w, "UDP listener ports must be distinct", http.StatusBadRequest)
		return
	}
	if cfg.ExternalIP != "" {
		if ip, err := netip.ParseAddr(cfg.ExternalIP); err != nil || !ip.Is4() || ip.IsUnspecified() {
			http.Error(w, "bad external ip", http.StatusBadRequest)
			return
		}
	}
	if err := p.st.SaveServerConfig(cfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/server?saved=1", http.StatusFound)
}

func (p *Panel) serverAWG31Preset(w http.ResponseWriter, r *http.Request) {
	cfg, err := p.st.LoadServerConfig()
	if err == nil {
		cfg.AWGProfile = config.AWG31Profile(cfg.AWGProfile.HeaderProtectionKey)
		err = p.st.SaveServerConfig(cfg)
	}
	if err != nil {
		http.Error(w, "could not save AWG 3.1 preset", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/server?saved=1", http.StatusFound)
}

// ---- clients ----

func (p *Panel) clientsPage(w http.ResponseWriter, r *http.Request) {
	p.userPage(w, r, "clients")
}

func (p *Panel) userPage(w http.ResponseWriter, r *http.Request, page string) {
	clients, err := p.st.ListClients()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	groups, err := p.st.ListGroups()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	traffic := p.reg.Traffic()
	now := time.Now()
	live := make(map[string]liveClient, len(clients))
	for _, c := range clients {
		live[c.ID] = clientLive(c.Revoked, traffic[c.ID], now)
	}
	p.render(w, page, map[string]any{
		"CSRF":       p.session(r).csrf,
		"Clients":    clients,
		"Groups":     groups,
		"GroupNames": groupNames(groups),
		"Live":       live,
		"Active":     page,
	})
}

func (p *Panel) clientCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	groupID := strings.TrimSpace(r.PostFormValue("group_id"))
	if groupID == "" {
		groupID = store.DefaultGroupID
	}
	priv, pub, err := awg.GenerateKeyPair()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rec := store.ClientRecord{
		ID:      randHex(8),
		Name:    name,
		PubKey:  pub,
		PrivKey: priv,
		GroupID: groupID,
		PeerID:  awg.PeerID(pub),
		Created: time.Now(),
	}
	if err := p.st.AllocateClient(rec); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Reload the allocated record so the import page has the assigned IP.
	rec, err = p.st.GetClient(rec.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := p.reg.RegisterClient(rec); err != nil {
		p.rollback(w, err, p.st.DeleteClient(rec.ID))
		return
	}
	http.Redirect(w, r, "/clients/import?id="+rec.ID, http.StatusFound)
}

func (p *Panel) clientRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	rec, err := p.st.GetClient(id)
	if err != nil {
		http.Error(w, "unknown client", http.StatusNotFound)
		return
	}
	if err := p.st.RevokeClient(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := p.reg.RevokeClient(rec); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/clients", http.StatusFound)
}

func (p *Panel) clientDelete(w http.ResponseWriter, r *http.Request) {
	if err := p.reg.DeleteClient(r.PostFormValue("id")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown client", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	http.Redirect(w, r, "/clients", http.StatusFound)
}

func (p *Panel) clientPolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	rec, err := p.st.GetClient(id)
	if err != nil {
		http.Error(w, "unknown client", http.StatusNotFound)
		return
	}
	old := rec

	// internet: "inherit" => nil, "on" => &true, "off" => &false.
	switch r.PostFormValue("internet") {
	case "inherit":
		rec.Internet = nil
	case "on":
		v := true
		rec.Internet = &v
	case "off":
		v := false
		rec.Internet = &v
	}

	// visible_peers: "inherit" => nil; "*" => all; explicit list => override.
	peers := r.PostForm["peers"]
	if hasValue(peers, "inherit") {
		rec.VisiblePeers = nil
	} else if hasValue(peers, "*") {
		rec.VisiblePeers = []string{"*"}
	} else if hasValue(peers, "none") {
		rec.VisiblePeers = []string{}
	} else if len(peers) > 0 {
		rec.VisiblePeers = peers
	} else {
		rec.VisiblePeers = []string{}
	}

	if err := p.st.SaveClient(rec); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := p.reg.ReloadPolicy(); err != nil {
		p.rollback(w, err, p.st.SaveClient(old))
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (p *Panel) clientImport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	rec, err := p.st.GetClient(r.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, "unknown client", http.StatusNotFound)
		return
	}
	if rec.Revoked {
		http.Error(w, "client revoked", http.StatusForbidden)
		return
	}
	s, err := p.clientProfile(rec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var subscription string
	ready, status := p.subscriptionReadiness()
	if ready {
		token, err := p.st.SubscriptionToken(rec.ID)
		if err != nil {
			http.Error(w, "subscription unavailable", http.StatusInternalServerError)
			return
		}
		if token != "" {
			subscription = p.subscriptionBase + token
		}
	}
	p.render(w, "import", map[string]any{
		"CSRF":               p.session(r).csrf,
		"Client":             rec,
		"Import":             s,
		"Active":             "clients",
		"SubscriptionReady":  ready,
		"SubscriptionStatus": status,
		"SubscriptionURL":    subscription,
	})
}

// buildClientConfig assembles the full client bootstrap from the active hub
// configuration and a client record. It uses the client's group for subnet and
// gateway, fills carrier hashes for all modes, native endpoints when a public
// IPv4 is configured, and managed routes from visible group subnets.
func (p *Panel) buildClientConfig(rec store.ClientRecord, cfg config.ServerConfig) (config.ClientConfig, error) {
	base := p.reg.InfoHash()
	preferred := cfg.Transport
	legacy := cfg.LegacyTransport
	if legacy == "" {
		legacy = preferred
	}
	hashes := make(map[string]string)
	for _, mode := range config.ClientModes() {
		if !config.IsCarrierMode(mode) {
			continue
		}
		h, err := config.CarrierHash(base, legacy, mode)
		if err != nil {
			return config.ClientConfig{}, err
		}
		hashes[mode] = h
	}

	group, err := p.st.GetGroup(rec.GroupID)
	if err != nil {
		return config.ClientConfig{}, fmt.Errorf("load client group: %w", err)
	}

	routes, err := p.groupRoutes()
	if err != nil {
		return config.ClientConfig{}, err
	}

	ccfg := config.ClientConfig{
		Version:       config.ClientVersion,
		InfoHash:      hashes[preferred],
		ServerPubKey:  p.reg.PubKey(),
		ClientPrivKey: rec.PrivKey,
		TLSServerKey:  p.reg.TLSServerKey(),
		ClientPubKey:  rec.PubKey,
		ClientIP:      rec.IP,
		Subnet:        group.Subnet,
		ServerIP:      group.ServerIP,
		Transport:     preferred,
		SocksPort:     1080,
		Profile:       cfg.AWGProfile,
		CarrierHashes: hashes,
		Routes:        routes,
	}

	if cfg.ExternalIP != "" {
		ccfg.CarrierEndpoint = net.JoinHostPort(cfg.ExternalIP, strconv.Itoa(cfg.CarrierPort))
		ccfg.AWGEndpoint = net.JoinHostPort(cfg.ExternalIP, strconv.Itoa(cfg.AWGPort))
		ccfg.WGEndpoint = net.JoinHostPort(cfg.ExternalIP, strconv.Itoa(cfg.WGPort))
	}

	return ccfg, nil
}

// Route all managed subnets; hub ACLs, not client routing, grant access.
func (p *Panel) groupRoutes() ([]string, error) {
	groups, err := p.st.ListGroups()
	if err != nil {
		return nil, err
	}
	routes := make([]string, 0, len(groups))
	for _, g := range groups {
		routes = append(routes, g.Subnet)
	}
	return routes, nil
}

// clientNative renders a standard WireGuard/AmneziaWG config for the selected
// native mode. It requires a public ExternalIP and refuses revoked clients.
func (p *Panel) clientNative(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	rec, err := p.st.GetClient(r.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, "unknown client", http.StatusNotFound)
		return
	}
	if rec.Revoked {
		http.Error(w, "client revoked", http.StatusForbidden)
		return
	}
	cfg := p.reg.Config()
	if cfg.ExternalIP == "" {
		http.Error(w, "native config requires a public ExternalIP", http.StatusPreconditionFailed)
		return
	}
	mode := config.CanonicalMode(r.URL.Query().Get("mode"))
	if !config.IsNativeMode(mode) {
		http.Error(w, "bad native mode", http.StatusBadRequest)
		return
	}
	ccfg, err := p.buildClientConfig(rec, cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	native, err := ccfg.NativeConfig(mode)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p.render(w, "native", map[string]any{
		"CSRF":   p.session(r).csrf,
		"Client": rec,
		"Mode":   mode,
		"Config": native,
		"Active": "clients",
	})
}

// ---- groups ----

func (p *Panel) groupsPage(w http.ResponseWriter, r *http.Request) {
	groups, err := p.st.ListGroups()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p.render(w, "groups", map[string]any{
		"CSRF":   p.session(r).csrf,
		"Groups": groups,
		"Active": "groups",
	})
}

func (p *Panel) groupEdit(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	var rec store.GroupRecord
	if id != "" {
		var err error
		rec, err = p.st.GetGroup(id)
		if err != nil {
			http.Error(w, "unknown group", http.StatusNotFound)
			return
		}
	}
	p.render(w, "group-edit", map[string]any{
		"CSRF":   p.session(r).csrf,
		"Group":  rec,
		"Active": "groups",
	})
}

func (p *Panel) groupSave(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PostFormValue("id"))
	if id == "" {
		id = randHex(8)
	}
	rec := store.GroupRecord{
		ID:         id,
		Name:       strings.TrimSpace(r.PostFormValue("name")),
		Subnet:     strings.TrimSpace(r.PostFormValue("subnet")),
		ServerIP:   strings.TrimSpace(r.PostFormValue("server_ip")),
		NoInternet: len(r.PostForm["internet"]) == 0,
	}
	if rec.Name == "" || rec.Subnet == "" || rec.ServerIP == "" {
		http.Error(w, "name, subnet and server IP required", http.StatusBadRequest)
		return
	}
	// VisibleGroups: nil = same group only, [] = none, ["*"] = all, explicit list.
	switch r.PostFormValue("visible_mode") {
	case "all":
		rec.VisibleGroups = []string{"*"}
	case "list":
		rec.VisibleGroups = splitIDs(r.PostFormValue("visible_groups"))
	case "none":
		rec.VisibleGroups = []string{}
	case "same":
		fallthrough
	default:
		rec.VisibleGroups = nil
	}

	old, oldErr := p.st.GetGroup(rec.ID)
	if err := p.st.SaveGroup(rec); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := p.reg.ReloadPolicy(); err != nil {
		if oldErr == nil {
			p.rollback(w, err, p.st.SaveGroup(old))
		} else {
			p.rollback(w, err, p.st.DeleteGroup(rec.ID))
		}
		return
	}
	http.Redirect(w, r, "/groups", http.StatusFound)
}

func (p *Panel) groupDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	if id == "" || id == store.DefaultGroupID {
		http.Error(w, "cannot delete default group", http.StatusBadRequest)
		return
	}
	old, err := p.st.GetGroup(id)
	if err != nil {
		http.Error(w, "unknown group", http.StatusNotFound)
		return
	}
	if err := p.st.DeleteGroup(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := p.reg.ReloadPolicy(); err != nil {
		p.rollback(w, err, p.st.SaveGroup(old))
		return
	}
	http.Redirect(w, r, "/groups", http.StatusFound)
}

func (p *Panel) clientMove(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	groupID := strings.TrimSpace(r.PostFormValue("group_id"))
	if groupID == "" {
		groupID = store.DefaultGroupID
	}
	rec, err := p.st.GetClient(id)
	if err != nil {
		http.Error(w, "unknown client", http.StatusNotFound)
		return
	}
	old := rec
	if err := p.st.MoveClient(id, groupID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := p.reg.ReloadPolicy(); err != nil {
		p.rollback(w, err, p.st.SaveClient(old))
		return
	}

	http.Redirect(w, r, "/clients", http.StatusFound)
}

// ---- helpers ----

func (p *Panel) rollback(w http.ResponseWriter, applyErr, restoreErr error) {
	if restoreErr != nil {
		http.Error(w, fmt.Sprintf("apply failed: %v; restore failed: %v", applyErr, restoreErr), http.StatusInternalServerError)
		return
	}
	if err := p.reg.ReloadPolicy(); err != nil {
		http.Error(w, fmt.Sprintf("apply failed: %v; reconcile failed: %v", applyErr, err), http.StatusInternalServerError)
		return
	}
	http.Error(w, applyErr.Error(), http.StatusInternalServerError)
}

func hasValue(vals []string, want string) bool {
	for _, v := range vals {
		if v == want {
			return true
		}
	}
	return false
}

func splitIDs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
