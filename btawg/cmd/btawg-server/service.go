package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"

	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/panel"
	"bittorrent-tunnel/btawg/internal/server"
	"bittorrent-tunnel/btawg/internal/service"
	"bittorrent-tunnel/btawg/internal/store"
)

const (
	managedStorePath = "/var/lib/btawg/server.db"
)

var managedLaunchPath = "/etc/btawg/server.json"

type serverLaunch struct {
	Store   string `json:"store"`
	NoTUN   bool   `json:"no_tun,omitempty"`
	TUNName string `json:"tun_name,omitempty"`
	subscriptionSettings
}

func installCmd(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	storePath := fs.String("store", managedStorePath, "path to existing bbolt database to use")
	noTun := fs.Bool("no-tun", false, "run overlay-only without kernel TUN or NAT")
	var subscription subscriptionSettings
	subscription.flags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	if installRequiresRoot && os.Geteuid() != 0 {
		return errors.New("install must run as root")
	}

	launch, err := loadServerLaunch()
	if err != nil {
		return err
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "store":
			launch.Store = *storePath
		case "no-tun":
			launch.NoTUN = *noTun
		}
		launch.subscriptionSettings.override(f.Name, subscription)
	})
	if err := installServer(launch); err != nil {
		return err
	}
	pending, err := os.ReadFile(launch.pendingPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := fmt.Printf("btawg-server: panel login admin / %s (shown once)\n", pending); err != nil {
		return err
	}
	return os.Remove(launch.pendingPath())
}

func loadServerLaunch() (serverLaunch, error) {
	launch := serverLaunch{Store: managedStorePath, TUNName: "btawg0"}
	data, err := os.ReadFile(managedLaunchPath)
	if errors.Is(err, os.ErrNotExist) {
		return launch, nil
	}
	if err != nil {
		return launch, err
	}
	if err := json.Unmarshal(data, &launch); err != nil {
		return launch, fmt.Errorf("read server launch settings: %w", err)
	}
	return launch, nil
}

func (s serverLaunch) pendingPath() string {
	path, err := filepath.Abs(s.Store)
	if err == nil && path == managedStorePath {
		return "/etc/btawg/.admin-password"
	}
	return s.Store + ".pending-admin"
}

func (s serverLaunch) save() error {
	if s.Store == "" {
		return errors.New("server store path must not be empty")
	}
	path, err := filepath.Abs(s.Store)
	if err != nil {
		return err
	}
	s.Store = path
	if !s.NoTUN {
		if err := service.ValidateInterface(s.TUNName, 15); err != nil {
			return err
		}
	}
	if _, err := s.subscriptionSettings.loadTLS(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return service.WriteFile(managedLaunchPath, data, 0600)
}

func installServer(launch serverLaunch) error {
	state, _, err := service.Server.State()
	if err != nil {
		return err
	}
	switch state {
	case service.NotInstalled, "inactive", "failed":
		if launch.Store == "" {
			return errors.New("server store path must not be empty")
		}
		st, _, _, err := openOrCreateServerStore(launch.Store, launch.pendingPath())
		if err != nil {
			return err
		}
		if err := st.Close(); err != nil {
			return err
		}
	}
	if err := launch.save(); err != nil {
		return err
	}
	return service.Server.Install()
}

func runCmd(parent context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	managed := fs.Bool("managed", false, "run as a managed systemd service with fixed ports and durable paths")
	storePath := fs.String("store", "", "path to the bbolt database (managed default: /var/lib/btawg/server.db)")
	transport := fs.String("transport", "", "preferred connection chain for new imports ("+strings.Join(config.ClientModes(), "|")+")")
	carrierPort := fs.Int("carrier-port", 0, "override carrier listen port")
	externalIP := fs.String("external-ip", "", "public IPv4 for native imports (default: outbound interface address)")
	dhtBootstrap := fs.String("dht-bootstrap", "", "override DHT bootstrap nodes, comma-separated host:port (default: public)")
	noTun := fs.Bool("no-tun", false, "userspace overlay only: no OS interface or Internet forwarding")
	tunName := fs.String("tun", "btawg0", "TUN interface name in real-TUN mode")
	var subscription subscriptionSettings
	subscription.flags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	var launch serverLaunch

	if *managed {
		var err error
		launch, err = loadServerLaunch()
		if err != nil {
			return err
		}
		if *storePath == "" {
			*storePath = launch.Store
		}
		if *tunName == "btawg0" && launch.TUNName != "" {
			*tunName = launch.TUNName
		}
		*noTun = *noTun || launch.NoTUN
	}
	launch.subscriptionSettings.overrideFlags(fs, subscription)
	subscription = launch.subscriptionSettings
	subscriptionTLS, err := subscription.loadTLS()
	if err != nil {
		return err
	}
	if *storePath == "" {
		*storePath = "btawg.db"
	}

	pendingPath := ""
	if *managed {
		pendingPath = (serverLaunch{Store: *storePath}).pendingPath()
	}
	st, _, pw, err := openOrCreateServerStore(*storePath, pendingPath)
	if err != nil {
		return err
	}
	defer st.Close()
	if pw != "" && !*managed {
		fmt.Printf("btawg-server: panel login admin / %s (shown once)\n", pw)
	}

	cfg, err := st.LoadServerConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if *transport != "" {
		cfg.Transport = *transport
	}
	if *managed {
		cfg.CarrierPort = config.DefaultServerConfig().CarrierPort
		cfg.AWGPort = config.DefaultServerConfig().AWGPort
		cfg.WGPort = config.DefaultServerConfig().WGPort
	} else if *carrierPort != 0 {
		cfg.CarrierPort = *carrierPort
	}
	if *externalIP != "" {
		cfg.ExternalIP = *externalIP
	}

	var srv *server.Server
	if *noTun {
		srv, err = server.New(cfg, st)
	} else {
		srv, err = server.NewRealTUN(cfg, st, *tunName)
	}
	if err != nil {
		return err
	}
	if *managed {
		srv.SetManaged(true)
	}
	defer func() {
		_ = srv.Close()
	}()

	if *dhtBootstrap != "" {
		srv.SetDHTBootstrap(strings.Split(*dhtBootstrap, ","))
	}

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err = st.LoadServerConfig()
	if err != nil {
		return fmt.Errorf("reload config: %w", err)
	}
	if *transport != "" {
		cfg.Transport = *transport
	}
	if *managed {
		cfg.CarrierPort = config.DefaultServerConfig().CarrierPort
		cfg.AWGPort = config.DefaultServerConfig().AWGPort
		cfg.WGPort = config.DefaultServerConfig().WGPort
	} else if *carrierPort != 0 {
		cfg.CarrierPort = *carrierPort
	}
	if *externalIP != "" {
		cfg.ExternalIP = *externalIP
	}

	p := panel.New(st, srv, "admin", cfg.AdminPassHash)
	var subscriptionServer *http.Server
	var subscriptionErrors <-chan error
	if subscriptionTLS != nil {
		subscriptionServer, subscriptionErrors, err = startSubscription(ctx, p, subscription.SubscriptionHost, subscriptionTLS)
		if err != nil {
			return err
		}
		defer subscriptionServer.Close()
		fmt.Printf("btawg-server: HTTPS subscriptions on TCP :%d\n", panel.SubscriptionPort)
	}
	web := &http.Server{Handler: p.Handler(), ReadHeaderTimeout: 5 * time.Second}
	defer web.Close()
	webErrors, err := srv.StartPanel(ctx, cfg.PanelPort, web)
	if err != nil {
		return fmt.Errorf("panel listeners: %w", err)
	}

	if *managed {
		if err := service.NotifyReady(); err != nil {
			return fmt.Errorf("systemd readiness: %w", err)
		}
	}

	errCh := make(chan error, 2)
	go func() { errCh <- srv.Run(ctx) }()
	go func() { errCh <- srv.Announce(ctx) }()

	active := srv.Config()
	fmt.Printf("btawg-server: modes=%s carrier=:%d awg=:%d/udp wg=:%d/udp infohash=%s\n",
		strings.Join(config.ClientModes(), ","), active.CarrierPort, active.AWGPort, active.WGPort, srv.InfoHash())
	fmt.Println("btawg-server: optional DHT discovery announce loop started")

	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var subscriptionErr error
		if subscriptionServer != nil {
			subscriptionErr = subscriptionServer.Shutdown(shutdown)
		}
		return errors.Join(subscriptionErr, web.Shutdown(shutdown))
	case err := <-errCh:
		return err
	case err := <-webErrors:
		return err
	case err := <-subscriptionErrors:
		return err
	}
}

// openOrCreateServerStore opens the database, generating server identity and
// an admin password when the store is new. It returns the store, whether a
// password was generated, and the generated plaintext password.
func openOrCreateServerStore(path string, pendingPath ...string) (*store.Store, bool, string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, "", fmt.Errorf("create db dir: %w", err)
	}
	st, err := store.Open(path)
	if err != nil {
		return nil, false, "", fmt.Errorf("open store: %w", err)
	}
	cfg, err := st.LoadServerConfig()
	if err != nil {
		st.Close()
		return nil, false, "", fmt.Errorf("load config: %w", err)
	}
	if cfg.AdminPassHash != "" {
		return st, false, "", nil
	}
	pw := randomPassword()
	if len(pendingPath) > 0 && pendingPath[0] != "" {
		if err := service.WriteFile(pendingPath[0], []byte(pw), 0600); err != nil {
			st.Close()
			return nil, false, "", err
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		st.Close()
		return nil, false, "", fmt.Errorf("hash password: %w", err)
	}
	cfg.AdminPassHash = string(hash)
	if err := st.SaveServerConfig(cfg); err != nil {
		st.Close()
		return nil, false, "", fmt.Errorf("save admin hash: %w", err)
	}
	return st, true, pw, nil
}

var installRequiresRoot = true

// setInstallRequiresRoot is used by tests to bypass the root check.
func setInstallRequiresRoot(v bool) { installRequiresRoot = v }

// randomPassword returns a 16-char alphanumeric password.
func randomPassword() string {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 16)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
