// Package server assembles the btawg hub: encrypted tunnels, IPv4 routing,
// overlay DNS, and optional BitTorrent discovery. The web panel drives the
// same store this server loads.
package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"
	utp "github.com/anacrolix/go-utp"

	"bittorrent-tunnel/btawg/internal/awg"
	"bittorrent-tunnel/btawg/internal/bind"
	"bittorrent-tunnel/btawg/internal/carrier"
	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/dht"
	"bittorrent-tunnel/btawg/internal/direct"
	"bittorrent-tunnel/btawg/internal/iptunnel"
	"bittorrent-tunnel/btawg/internal/store"
	"bittorrent-tunnel/btawg/internal/tunneldns"
	"bittorrent-tunnel/btawg/internal/tunsetup"
)

// Server is the assembled btawg hub.
type Server struct {
	cfg         config.ServerConfig
	st          *store.Store
	infoHash    [20]byte
	peerID      [20]byte
	pubKey      string
	tlsIdentity *iptunnel.Identity

	dev       *device.Device            // native AWG
	wg        *device.Device            // native WG
	modeDevs  map[string]*device.Device // AWG crypto per carrier mode
	modeBinds map[string]*bind.Bind     // stream bind per carrier mode
	ns        *netstack.Net
	router    *packetRouter
	kernel    *tunsetup.Hub
	tunName   string

	modeCarriers map[string]carrier.Transport // mode/IP -> current carrier
	managed      bool                         // true when run by the managed systemd path

	mu           sync.Mutex
	applyMu      sync.Mutex
	closeOnce    sync.Once
	closeErr     error
	trafficWG    sync.WaitGroup
	carrierWG    sync.WaitGroup
	meshWG       sync.WaitGroup
	savedTraffic map[string]store.Traffic
	hashes       map[[20]byte]string
	peers        map[string]store.ClientRecord
	sessions     map[string]map[carrier.Transport]chan struct{}
	tlsSessions  map[string]carrier.Transport // current TLS carrier by enrolled peer ID
	gateways     []netip.Prefix
	byPeerID     map[string]store.ClientRecord
	attached     map[string]int // client IP -> carrier count

	policy *policySnapshot
	panel  *panelListeners
	mesh   *meshControl

	udpPC  net.PacketConn
	sock   utp.Socket
	tcpLn  net.Listener
	anns   []*dht.Announcer
	dnsLn  net.Listener
	closed chan struct{}
}

// New builds an overlay-only userspace hub, without Internet forwarding.
func New(cfg config.ServerConfig, st *store.Store) (*Server, error) {
	return newServer(cfg, st, "")
}

// NewRealTUN shares one kernel interface across all transports.
func NewRealTUN(cfg config.ServerConfig, st *store.Store, tunName string) (*Server, error) {
	return newServer(cfg, st, tunName)
}

func newServer(cfg config.ServerConfig, st *store.Store, tunName string) (*Server, error) {
	s, err := prepareServer(cfg, st)
	if err != nil {
		return nil, err
	}
	gateways, err := groupGateways(st)
	s.tunName = tunName
	if err != nil {
		return nil, err
	}
	s.gateways = gateways
	var td tun.Device
	if tunName == "" {
		addresses := make([]netip.Addr, len(gateways))
		for i, p := range gateways {
			addresses[i] = p.Addr()
		}
		td, s.ns, err = netstack.CreateNetTUN(addresses, nil, config.DefaultMTU)
	} else {
		td, err = tun.CreateTUN(tunName, config.DefaultMTU)
	}
	if err != nil {
		return nil, fmt.Errorf("create tun %q: %w", tunName, err)
	}
	s.router = newPacketRouter(td, tunName != "", s.policy)
	if tunName != "" {
		// Set link state before installing scoped routes and NAT.
		if err = tunsetup.Setup(tunName, gateways[0], nil, config.DefaultMTU, false); err == nil {
			s.kernel, err = tunsetup.NewHub(tunName)
		}
		if err == nil {
			err = s.kernel.Apply(gateways)
		}
	}
	if err == nil {
		err = s.finish()
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	s.trafficWG.Go(s.refreshHandshakeRoutes)
	s.mesh = newMeshControl()
	return s, nil
}

func groupGateways(st *store.Store) ([]netip.Prefix, error) {
	groups, err := st.ListGroups()
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("no VPN groups")
	}
	out := make([]netip.Prefix, 0, len(groups))
	for _, g := range groups {
		out = append(out, netip.PrefixFrom(netip.MustParseAddr(g.ServerIP), netip.MustParsePrefix(g.Subnet).Bits()))
	}
	return out, nil
}

// prepareServer loads (or generates) the hub identity, policy snapshot, and
// peer configuration shared by New and NewRealTUN.
func prepareServer(cfg config.ServerConfig, st *store.Store) (*Server, error) {
	stored, err := st.LoadServerConfig()
	if err != nil {
		return nil, err
	}
	if cfg.LegacyTransport == "" {
		cfg.LegacyTransport = stored.LegacyTransport
		if cfg.LegacyTransport == "" {
			cfg.LegacyTransport = cfg.Transport
			if stored.InfoHash != "" {
				cfg.LegacyTransport = stored.Transport
			}
		}
	}
	cfg.Normalize()
	if stored.InfoHash == "" && stored.ServerPrivKey == "" && cfg.InfoHash == "" && cfg.ServerPrivKey == "" && cfg.AWGProfile == config.DefaultProfile() {
		cfg.AWGProfile = config.AWG31Profile("")
	}
	if err := cfg.AWGProfile.Validate(); err != nil {
		return nil, fmt.Errorf("AWG profile: %w", err)
	}
	if !config.ValidClientMode(cfg.Transport) {
		return nil, fmt.Errorf("unsupported transport %q", cfg.Transport)
	}
	for _, p := range []int{cfg.CarrierPort, cfg.AWGPort, cfg.WGPort} {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("invalid port %d", p)
		}
	}
	if cfg.WGPort == cfg.AWGPort || cfg.WGPort == cfg.CarrierPort || cfg.AWGPort == cfg.CarrierPort {
		return nil, fmt.Errorf("carrier, AWG and WG UDP ports must differ")
	}
	if cfg.ExternalIP == "" {
		if c, err := net.Dial("udp4", "1.1.1.1:53"); err == nil {
			cfg.ExternalIP = c.LocalAddr().(*net.UDPAddr).IP.String()
			_ = c.Close()
		}
	}
	if cfg.ExternalIP != "" {
		ip, err := netip.ParseAddr(cfg.ExternalIP)
		if err != nil || !ip.Is4() || ip.IsUnspecified() {
			return nil, fmt.Errorf("invalid external IPv4 address %q", cfg.ExternalIP)
		}
	}
	// The argument is the working configuration (main loads it from the
	// store, applying any CLI overrides). At first boot the deployment
	// identity is generated, persisted, and reused from then on.
	if cfg.InfoHash == "" || cfg.ServerPrivKey == "" {
		var ih [20]byte
		if _, err := rand.Read(ih[:]); err != nil {
			return nil, fmt.Errorf("generate infohash: %w", err)
		}
		cfg.InfoHash = hex.EncodeToString(ih[:])
		priv, _, err := awg.GenerateKeyPair()
		if err != nil {
			return nil, fmt.Errorf("generate server key: %w", err)
		}
		cfg.ServerPrivKey = priv
		if err := st.SaveServerConfig(cfg); err != nil {
			return nil, fmt.Errorf("save first-boot identity: %w", err)
		}
	} else if stored.LegacyTransport == "" {
		stored.LegacyTransport = cfg.LegacyTransport
		if err := st.SaveServerConfig(stored); err != nil {
			return nil, err
		}
	}

	ih, err := hex.DecodeString(cfg.InfoHash)
	if err != nil || len(ih) != 20 {
		return nil, fmt.Errorf("bad infohash in store")
	}
	serverPub, err := awg.DerivePubKey(cfg.ServerPrivKey)
	if err != nil {
		return nil, fmt.Errorf("derive server public key: %w", err)
	}
	tlsIdentity, err := iptunnel.NewIdentity(cfg.ServerPrivKey, iptunnel.Hub)
	if err != nil {
		return nil, fmt.Errorf("hub TLS identity: %w", err)
	}
	serverIP := netip.MustParseAddr(cfg.ServerIP)
	if g, err := st.GetGroup(store.DefaultGroupID); err == nil {
		cfg.ServerIP, cfg.Subnet = g.ServerIP, g.Subnet
		serverIP = netip.MustParseAddr(g.ServerIP)
	} else {
		return nil, err
	}

	s := &Server{
		cfg:          cfg,
		st:           st,
		infoHash:     [20]byte(ih),
		pubKey:       serverPub,
		tlsIdentity:  tlsIdentity,
		peers:        make(map[string]store.ClientRecord),
		sessions:     make(map[string]map[carrier.Transport]chan struct{}),
		tlsSessions:  make(map[string]carrier.Transport),
		modeDevs:     make(map[string]*device.Device),
		modeBinds:    make(map[string]*bind.Bind),
		modeCarriers: make(map[string]carrier.Transport),
		byPeerID:     make(map[string]store.ClientRecord),
		attached:     make(map[string]int),
		closed:       make(chan struct{}),
	}
	rand.Read(s.peerID[:])

	s.policy = newPolicySnapshot()
	s.savedTraffic, err = st.LoadTraffic()
	if err != nil {
		return nil, fmt.Errorf("load traffic: %w", err)
	}
	for id, v := range s.savedTraffic {
		c := new(trafficCounter)
		c.up.Store(v.Up)
		c.down.Store(v.Down)
		c.lastSeen.Store(v.LastSeen)
		s.policy.traffic[id] = c
	}
	if err := s.policy.reload(st, serverIP); err != nil {
		return nil, fmt.Errorf("load policy: %w", err)
	}

	return s, nil
}

func (s *Server) finish() error {
	// Native AWG and WG use dedicated UDP binds; each BitTorrent carrier mode
	// gets its own AWG crypto engine on a stream-only bind so no native UDP
	// socket is allocated per carrier mode.
	s.dev = device.NewDevice(s.router.awg, bind.NewHybrid(), device.NewLogger(device.LogLevelError, "btawg-awg: "))
	s.wg = device.NewDevice(s.router.wg, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "btawg-wg: "))
	awgProfile := s.cfg.AWGProfile.DeviceIPC()
	for _, mode := range []string{config.ModeExtUTP, config.ModeExtTCP, config.ModeReqUTP, config.ModeReqTCP} {
		eng := s.router.engineForMode(mode)
		b := bind.New()
		d := device.NewDevice(eng, b, device.NewLogger(device.LogLevelError, "btawg-"+mode+": "))
		if err := d.IpcSet(fmt.Sprintf("private_key=%s\nlisten_port=%d\n%s", s.cfg.ServerPrivKey, s.cfg.AWGPort, awgProfile)); err != nil {
			return fmt.Errorf("configure %s device: %w", mode, err)
		}
		s.modeDevs[mode] = d
		s.modeBinds[mode] = b
	}
	if err := s.dev.IpcSet(fmt.Sprintf("private_key=%s\nlisten_port=%d\n%s", s.cfg.ServerPrivKey, s.cfg.AWGPort, awgProfile)); err != nil {
		return err
	}
	if err := s.wg.IpcSet(fmt.Sprintf("private_key=%s\nlisten_port=%d\n", s.cfg.ServerPrivKey, s.cfg.WGPort)); err != nil {
		return err
	}
	if err := s.reloadPeers(); err != nil {
		return err
	}
	if err := s.dev.Up(); err != nil {
		return fmt.Errorf("AWG: %w", err)
	}
	if err := s.wg.Up(); err != nil {
		return fmt.Errorf("WG: %w", err)
	}
	for mode, d := range s.modeDevs {
		if err := d.Up(); err != nil {
			return fmt.Errorf("%s: %w", mode, err)
		}
	}
	listen := net.ListenConfig{Control: carrier.TCPControl}
	var err error
	s.tcpLn, err = listen.Listen(context.Background(), "tcp4", fmt.Sprintf(":%d", s.cfg.CarrierPort))
	if err != nil {
		return err
	}
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{Port: s.cfg.CarrierPort})
	if err != nil {
		return err
	}
	s.udpPC = pc
	s.sock, err = carrier.NewUTPSocket(pc)
	if err != nil {
		return err
	}
	if s.kernel != nil {
		lc := net.ListenConfig{Control: tunsetup.BindDevice(s.tunName)}
		s.dnsLn, err = lc.Listen(context.Background(), "tcp4", fmt.Sprintf(":%d", tunneldns.Port))
		if err != nil {
			return fmt.Errorf("overlay DNS: %w", err)
		}
	}
	s.hashes = make(map[[20]byte]string, len(config.CarrierHelloModes()))
	for _, mode := range config.CarrierHelloModes() {
		hash, err := config.CarrierHash(s.InfoHash(), s.cfg.LegacyTransport, mode)
		if err != nil {
			return err
		}
		key, err := awg.InfoHashBytes(hash)
		if err != nil {
			return err
		}
		family := config.CarrierFraming(mode)
		if config.IsTLSMode(mode) {
			family += "-tls"
		}
		s.hashes[key] = family
		ann, err := dht.NewAnnouncer(key, netip.AddrPortFrom(netip.IPv4Unspecified(), uint16(s.cfg.CarrierPort)), nil)
		if err != nil {
			return err
		}
		s.anns = append(s.anns, ann)
	}
	return nil
}

// InfoHash returns the deployment's hex infohash (shown by the panel, carried
// in every client import string).
func (s *Server) InfoHash() string { return hex.EncodeToString(s.infoHash[:]) }

// PubKey returns the hub's AWG public key.
func (s *Server) PubKey() string { return s.pubKey }

// TLSServerKey is the TLS public key pinned by new client imports.
func (s *Server) TLSServerKey() string { return s.tlsIdentity.Pin() }

// Config returns the active server configuration.
func (s *Server) Config() config.ServerConfig { return s.cfg }

// Managed reports whether the hub was started by the managed systemd path.
func (s *Server) Managed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.managed
}

// SetManaged marks the hub as managed by the systemd service path. It is not
// safe to call after listeners have been started.
func (s *Server) SetManaged(managed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.managed = managed
}

// ReloadPolicy applies stored group addresses, peer addresses and access rules.
func (s *Server) ReloadPolicy() error {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	return s.reloadPolicy()
}

// reloadPolicy requires applyMu.
func (s *Server) reloadPolicy() (err error) {
	defer func() {
		if err == nil {
			return
		}
		s.policy.disableClients()
		s.mu.Lock()
		for _, t := range s.tlsSessions {
			t.Close()
		}
		s.mu.Unlock()
	}()
	gs, err := groupGateways(s.st)
	if err != nil {
		return err
	}
	if s.ns != nil {
		if len(gs) != len(s.gateways) {
			return fmt.Errorf("changing group networks in -no-tun mode requires a restart or a real TUN")
		}
		for i, g := range gs {
			if g != s.gateways[i] {
				return fmt.Errorf("changing group networks in -no-tun mode requires a restart or a real TUN")
			}
		}
	}
	if s.kernel != nil && !slices.Equal(gs, s.gateways) {
		if err = s.kernel.Apply(gs); err != nil {
			return err
		}
		s.gateways = gs
	}
	if err = s.reloadPeers(); err != nil {
		return err
	}
	group, err := s.st.GetGroup(store.DefaultGroupID)
	if err != nil {
		return err
	}
	gateway := netip.MustParseAddr(group.ServerIP)
	if err = s.policy.reload(s.st, gateway); err != nil {
		return err
	}
	s.gateways = gs
	if err = s.rebindPanel(gateway); err != nil {
		return err
	}
	return nil
}

// OverlayNet exposes the hub's userspace network stack for the panel and diagnostics.
func (s *Server) OverlayNet() *netstack.Net { return s.ns }

// Run serves carrier connections and overlay DNS until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
		case <-s.closed:
		}
		cancel()
	}()

	type listener struct {
		accept func() (net.Conn, error)
		proto  string
	}
	listeners := []listener{
		{accept: s.tcpLn.Accept, proto: "tcp"},
		{accept: s.sock.Accept, proto: "utp"},
	}
	errs := make(chan error, len(listeners)+4)
	if s.dnsLn != nil {
		go func() { errs <- tunneldns.Serve(runCtx, s.dnsLn, s.policy.allowDNS) }()
	}
	s.mu.Lock()
	select {
	case <-s.closed:
		s.mu.Unlock()
		return nil
	default:
	}
	s.meshWG.Go(func() { errs <- s.serveMesh(runCtx) })
	s.meshWG.Go(func() { errs <- s.serveRouteRelay(runCtx) })
	s.meshWG.Go(func() { errs <- direct.ServeObserver(runCtx, s.sock) })
	s.mu.Unlock()
	defer func() { cancel(); s.meshWG.Wait() }()
	for _, ln := range listeners {
		go func(ln listener) {
			for {
				c, err := ln.accept()
				if err != nil {
					errs <- err
					return
				}
				s.mu.Lock()
				select {
				case <-s.closed:
					s.mu.Unlock()
					_ = c.Close()
					return
				default:
				}
				s.carrierWG.Go(func() { s.acceptConn(runCtx, c, ln.proto) })
				s.mu.Unlock()
			}
		}(ln)
	}
	select {
	case <-runCtx.Done():
		return nil
	case err := <-errs:
		select {
		case <-s.closed:
			return nil
		default:
			return fmt.Errorf("hub listener: %w", err)
		}
	}
}

// Announce publishes the AWG and TLS framing hashes through DHT.
func (s *Server) Announce(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, len(s.anns))
	for _, a := range s.anns {
		go func() { errs <- a.Run(ctx) }()
	}
	select {
	case <-ctx.Done():
		return nil
	case <-s.closed:
		return nil
	case err := <-errs:
		return err
	}
}

// carrierMode maps the carrier framing family and listener protocol to the
// canonical client mode used for per-mode crypto engines and session keys.
func carrierMode(family, proto string) string {
	switch family {
	case "ext":
		if proto == "tcp" {
			return config.ModeExtTCP
		}
		return config.ModeExtUTP
	case "req":
		if proto == "tcp" {
			return config.ModeReqTCP
		}
		return config.ModeReqUTP
	case "ext-tls":
		if proto == "tcp" {
			return config.ModeTLSExtTCP
		}
		return config.ModeTLSExtUTP
	case "req-tls":
		if proto == "tcp" {
			return config.ModeTLSReqTCP
		}
		return config.ModeTLSReqUTP
	}
	return ""
}

func (s *Server) acceptConn(ctx context.Context, c net.Conn, proto string) {
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return
	}
	t, theirs, family, err := carrier.AcceptHub(c, s.peerID, s.hashes)
	if err != nil {
		return
	}
	defer t.Wait()
	base := t
	id := hex.EncodeToString(theirs[:])
	s.mu.Lock()
	rec, ok := s.byPeerID[id]
	s.mu.Unlock()
	if !ok {
		return
	}
	mode := carrierMode(family, proto)
	if mode == "" {
		return
	}
	var secure *tls.Conn
	recordMode := carrier.ExtendedExperiment(t)
	if strings.HasSuffix(family, "-tls") || recordMode != "" {
		pin, err := iptunnel.PublicKey(rec.PrivKey, iptunnel.Client)
		if err != nil {
			return
		}
		cfg, err := s.tlsIdentity.TLS(pin)
		if err != nil {
			return
		}
		secure = tls.Server(iptunnel.Stream(c, t), cfg)
		if err := secure.HandshakeContext(ctx); err != nil {
			return
		}
		if recordMode != "" {
			records, err := iptunnel.SecureDatagramRecords(secure, base, recordMode)
			if err != nil {
				return
			}
			defer records.Wait()
			t = records
			secure = nil // AWG still owns the IP layer.
		}
	}
	if err := c.SetDeadline(time.Time{}); err != nil {
		return
	}
	if secure == nil {
		timed, err := carrier.WithTiming(t, base)
		if err != nil {
			return
		}
		defer timed.Wait()
		t = timed
	}
	ip := netip.MustParseAddr(rec.IP)
	s.applyMu.Lock()
	s.mu.Lock()
	current, ok := s.byPeerID[id]
	if !ok || current.ID != rec.ID || current.IP != rec.IP || current.PubKey != rec.PubKey || current.PrivKey != rec.PrivKey || ctx.Err() != nil {
		s.mu.Unlock()
		s.applyMu.Unlock()
		return
	}
	select {
	case <-s.closed:
		s.mu.Unlock()
		s.applyMu.Unlock()
		return
	default:
	}
	var eng *engineTun
	modeKey := mode + "/" + rec.IP
	if secure != nil {
		now := time.Now()
		if !s.policy.handshake(ip, rec.PubKey, now) {
			s.mu.Unlock()
			s.applyMu.Unlock()
			return
		}
		eng = s.router.newEngine(ip)
		s.router.remember(ip, eng, now.UnixNano())
		if old := s.tlsSessions[modeKey]; old != nil {
			old.Close()
		}
		s.tlsSessions[modeKey] = t
	} else {
		b := s.modeBinds[mode]
		if b == nil {
			s.mu.Unlock()
			s.applyMu.Unlock()
			return
		}
		s.modeCarriers[modeKey] = t
		b.Attach(bind.Endpoint(ip), t)
	}
	if s.sessions[id] == nil {
		s.sessions[id] = make(map[carrier.Transport]chan struct{})
	}
	retired := make(chan struct{})
	s.sessions[id][t] = retired
	s.attached[rec.IP]++
	s.mu.Unlock()
	s.applyMu.Unlock()
	if secure != nil {
		s.runTLS(eng, secure, t)
	} else {
		select {
		case <-t.Done():
		case <-ctx.Done():
		case <-s.closed:
		}
		s.mu.Lock()
		if s.modeCarriers[modeKey] == t {
			delete(s.modeCarriers, modeKey)
			if b := s.modeBinds[mode]; b != nil {
				b.Detach(bind.Endpoint(ip))
			}
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	if s.tlsSessions[modeKey] == t {
		delete(s.tlsSessions, modeKey)
	}
	delete(s.sessions[id], t)
	if len(s.sessions[id]) == 0 {
		delete(s.sessions, id)
	}
	if s.attached[rec.IP] > 1 {
		s.attached[rec.IP]--
	} else {
		delete(s.attached, rec.IP)
	}
	close(retired)
	s.mu.Unlock()
}

func (s *Server) runTLS(eng *engineTun, secure *tls.Conn, t carrier.Transport) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer t.Close()
		_ = iptunnel.SendPackets(eng, func(p []byte) error {
			_, err := secure.Write(p)
			return err
		})
	}()
	_ = iptunnel.ReceivePackets(eng, secure)
	t.Close()
	_ = eng.Close()
	<-done
	s.router.removeEngine(eng)
}

func (s *Server) reloadPeers() error {
	recs, err := s.st.ListClients()
	if err != nil {
		return err
	}
	next := make(map[string]store.ClientRecord, len(recs))
	byPeerID := make(map[string]store.ClientRecord, len(recs))
	for _, r := range recs {
		if r.Revoked {
			continue
		}
		if _, err := netip.ParseAddr(r.IP); err != nil {
			return err
		}
		if _, err := awg.InfoHashBytes(r.PeerID); r.PeerID != "" && err != nil {
			return fmt.Errorf("invalid carrier identity for user %q", r.ID)
		}
		if decoded, err := hex.DecodeString(r.PubKey); err != nil || len(decoded) != 32 || r.PubKey == s.pubKey {
			return fmt.Errorf("invalid public key for user %q", r.ID)
		}
		if r.PeerID == "" {
			r.PeerID = awg.PeerID(r.PubKey)
		}
		next[r.ID] = r
		byPeerID[r.PeerID] = r
	}
	type retiringSession struct {
		transport carrier.Transport
		done      <-chan struct{}
	}
	var closing []retiringSession
	s.mu.Lock()
	s.byPeerID = byPeerID
	for id, old := range s.peers {
		if r, ok := next[id]; ok && r.IP == old.IP && r.PubKey == old.PubKey && r.PrivKey == old.PrivKey && r.PeerID == old.PeerID {
			continue
		}
		for t, done := range s.sessions[old.PeerID] {
			closing = append(closing, retiringSession{t, done})
		}
	}
	s.mu.Unlock()
	for _, session := range closing {
		session.transport.Close()
	}
	// TLS engines pin only the source IP. Drain their readers before any
	// later enrollment can reuse the address and accept their queued packets.
	for _, session := range closing {
		<-session.done
	}
	for id, old := range s.peers {
		if r, ok := next[id]; ok && r.IP == old.IP && r.PubKey == old.PubKey {
			continue
		}
		ip := netip.MustParseAddr(old.IP)
		s.policy.remove(ip)
		s.router.forget(ip)
		line := fmt.Sprintf("public_key=%s\nremove=true\n", old.PubKey)
		if err := s.ipcSetAll(line); err != nil {
			return err
		}
	}
	for id, r := range next {
		if old, ok := s.peers[id]; ok && old.IP == r.IP && old.PubKey == r.PubKey {
			continue
		}
		line := fmt.Sprintf("public_key=%s\nreplace_allowed_ips=true\nallowed_ip=%s/32\n", r.PubKey, r.IP)
		if err := s.ipcSetAll(line); err != nil {
			return err
		}
	}
	s.peers = next
	return nil
}

// ipcSetAll applies an IPC command to every device that shares the peer key
// namespace: native AWG, native WG, and one AWG device per carrier mode.
func (s *Server) ipcSetAll(line string) error {
	if s.dev != nil {
		if err := s.dev.IpcSet(line); err != nil {
			return err
		}
	}
	if s.wg != nil {
		if err := s.wg.IpcSet(line); err != nil {
			return err
		}
	}
	for mode, d := range s.modeDevs {
		if err := d.IpcSet(line); err != nil {
			return fmt.Errorf("%s: %w", mode, err)
		}
	}
	return nil
}

// allDevices returns the native AWG/WG devices plus every per-mode carrier
// device. The caller must not modify the returned slice.
func (s *Server) allDevices() []*device.Device {
	out := make([]*device.Device, 0, 2+len(s.modeDevs))
	if s.dev != nil {
		out = append(out, s.dev)
	}
	if s.wg != nil {
		out = append(out, s.wg)
	}
	for _, d := range s.modeDevs {
		out = append(out, d)
	}
	return out
}

// RegisterClient applies an enrollment already persisted by the panel.
func (s *Server) RegisterClient(rec store.ClientRecord) error { return s.ReloadPolicy() }

// RevokeClient applies a revocation already persisted by the panel.
func (s *Server) RevokeClient(rec store.ClientRecord) error { return s.ReloadPolicy() }

// DeleteClient retires the old key before making the VPN address reusable.
// A failed reload leaves the record revoked with its address still reserved.
func (s *Server) DeleteClient(id string) error {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	if err := s.st.RevokeClient(id); err != nil {
		return err
	}
	if err := s.reloadPolicy(); err != nil {
		return err
	}
	if err := s.st.DeleteClient(id); err != nil {
		return err
	}
	s.policy.mu.Lock()
	delete(s.policy.traffic, id)
	s.policy.mu.Unlock()
	delete(s.savedTraffic, id)
	return nil
}

// ConnectedCount reports how many carriers are attached per overlay IP.
func (s *Server) ConnectedCount() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.attached))
	for ip, n := range s.attached {
		out[ip] = n
	}
	return out
}

// Close tears the hub down.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		close(s.closed)
		for _, sessions := range s.sessions {
			for t := range sessions {
				t.Close()
			}
		}
		s.mu.Unlock()
		s.closePanel()
		s.trafficWG.Wait()
		s.meshWG.Wait()
		for _, a := range s.anns {
			a.Close()
		}
		if s.sock != nil {
			_ = s.sock.Close()
		}
		if s.tcpLn != nil {
			_ = s.tcpLn.Close()
		}
		if s.udpPC != nil {
			_ = s.udpPC.Close()
		}
		s.carrierWG.Wait()
		if s.dnsLn != nil {
			_ = s.dnsLn.Close()
		}
		for _, d := range s.allDevices() {
			d.Close()
		}
		if s.kernel != nil {
			s.closeErr = s.kernel.Close()
		}
		if s.router != nil {
			s.closeErr = errors.Join(s.closeErr, s.router.close(), s.flushTraffic())
		}
	})
	return s.closeErr
}

// SetDHTBootstrap updates discovery bootstrap for all carrier families.
func (s *Server) SetDHTBootstrap(hosts []string) {
	for _, a := range s.anns {
		a.SetBootstrap(hosts)
	}
}
