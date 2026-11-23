// Package client assembles the btawg VPN node with a TUN or netstack and
// a local SOCKS5 proxy. The outer connection uses the selected AWG, WG or
// TLS carrier chain; the client never installs an OS default route.
package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"

	"bittorrent-tunnel/btawg/internal/awg"
	carrierbind "bittorrent-tunnel/btawg/internal/bind"
	"bittorrent-tunnel/btawg/internal/carrier"
	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/dht"
	"bittorrent-tunnel/btawg/internal/iptunnel"
	"bittorrent-tunnel/btawg/internal/tunsetup"
)

// retryDelay spaces carrier reconnect attempts.
const retryDelay = 3 * time.Second

// Client is the assembled btawg client node.
type Client struct {
	cfg  config.ClientConfig
	bind conn.Bind
	dev  *device.Device
	ns   *netstack.Net // non-nil in netstack mode
	ip   *tlsTunnel    // non-nil for TLS carrier modes

	proxyTCP func(ctx context.Context, addr netip.AddrPort) (net.Conn, error)
	proxyUDP func(ctx context.Context) (net.PacketConn, error)
	closeTUN func() error
	path     *tunPath

	disc func(ctx context.Context) ([]netip.AddrPort, error)

	socksAddr atomic.Value // string, set once the SOCKS5 listener is up
}

// New builds an unprivileged client whose SOCKS sockets use the overlay netstack.
func New(cfg config.ClientConfig) (*Client, error) {
	cfg.Transport = config.CanonicalMode(cfg.Transport)
	c := &Client{cfg: cfg}
	clientIP := netip.MustParseAddr(cfg.ClientIP)
	tunDev, ns, err := netstack.CreateNetTUN([]netip.Addr{clientIP}, nil, config.DefaultMTU)
	if err != nil {
		return nil, fmt.Errorf("client netstack: %w", err)
	}
	c.ns = ns
	if err := c.start(tunDev); err != nil {
		return nil, err
	}
	c.proxyTCP = func(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
		return ns.DialContextTCPAddrPort(ctx, addr)
	}
	c.proxyUDP = func(ctx context.Context) (net.PacketConn, error) {
		// Bind the client's overlay address: an unspecified-source socket
		// gets "no route to host" in the netstack on WriteTo.
		return ns.ListenUDPAddrPort(netip.AddrPortFrom(clientIP, 0))
	}
	return c, nil
}

// NewRealTUN builds a foreground client on its own kernel TUN.
func NewRealTUN(cfg config.ClientConfig, tunName string) (*Client, error) {
	cfg.Transport = config.CanonicalMode(cfg.Transport)
	c := &Client{cfg: cfg}
	dev, err := openClientTUN(cfg, tunName)
	if err != nil {
		return nil, err
	}
	path, err := c.configureForegroundTUN(dev, tunName)
	if err != nil {
		_ = dev.Close()
		return nil, err
	}
	if err := c.start(path); err != nil {
		_ = path.Close()
		if c.closeTUN != nil {
			_ = c.closeTUN()
		}
		return nil, err
	}
	return c, nil
}

func openClientTUN(cfg config.ClientConfig, name string) (tun.Device, error) {
	subnet, err := netip.ParsePrefix(cfg.Subnet)
	if err != nil || !subnet.Addr().Is4() || subnet.Bits() == 0 {
		return nil, fmt.Errorf("invalid IPv4 subnet %q", cfg.Subnet)
	}
	ip, err := netip.ParseAddr(cfg.ClientIP)
	if err != nil || !ip.Is4() || !subnet.Contains(ip) {
		return nil, fmt.Errorf("invalid client IPv4 address %q for %s", cfg.ClientIP, subnet)
	}
	dev, err := tunsetup.OpenClientTUN(name, netip.PrefixFrom(ip, subnet.Bits()), cfg.Routes, config.DefaultMTU)
	if err != nil {
		return nil, fmt.Errorf("client TUN %s: %w", name, err)
	}
	return dev, nil
}

func (c *Client) start(tunDev tun.Device) error {
	if config.IsCarrierMode(c.cfg.Transport) {
		if err := c.startDiscovery(); err != nil {
			tunDev.Close()
			return err
		}
	}
	if config.IsTLSMode(c.cfg.Transport) {
		var err error
		c.ip, err = newTLSTunnel(tunDev, c.cfg)
		if err != nil {
			tunDev.Close()
		}
		return err
	}
	level := device.LogLevelError
	if os.Getenv("BTAWG_AWG_VERBOSE") != "" {
		// Field debugging: shows handshake initiation/response exchange on
		// the wire when the tunnel does not come up.
		level = device.LogLevelVerbose
	}

	mode := c.cfg.Transport
	if config.IsNativeMode(mode) {
		c.bind = newUnderlayBind()
	} else {
		c.bind = carrierbind.New()
	}

	c.dev = device.NewDevice(tunDev, c.bind, device.NewLogger(level, "btawg-client: "))

	var ipc string
	var err error
	if config.IsNativeMode(mode) {
		ipc, err = c.cfg.NativeDeviceConfig(mode)
	} else {
		ipc = awg.ClientConf(c.cfg)
		err = c.cfg.Profile.Validate()
	}
	if err != nil {
		c.dev.Close()
		return fmt.Errorf("client config (%s): %w", mode, err)
	}
	if err := c.dev.IpcSet(ipc); err != nil {
		c.dev.Close()
		return fmt.Errorf("client IpcSet: %w", err)
	}
	if err := c.dev.Up(); err != nil {
		c.dev.Close()
		return fmt.Errorf("client up: %w", err)
	}
	return nil
}

func (c *Client) startDiscovery() error {
	hash, err := c.carrierInfoHash()
	if err != nil {
		return fmt.Errorf("carrier infohash: %w", err)
	}
	infoHash, err := awg.InfoHashBytes(hash)
	if err != nil {
		return fmt.Errorf("config infohash: %w", err)
	}
	if c.cfg.CarrierEndpoint != "" {
		ap, err := netip.ParseAddrPort(c.cfg.CarrierEndpoint)
		if err != nil || !ap.Addr().Is4() || ap.Addr().IsUnspecified() || ap.Port() == 0 {
			return fmt.Errorf("invalid carrier endpoint %q", c.cfg.CarrierEndpoint)
		}
		c.disc = func(context.Context) ([]netip.AddrPort, error) { return []netip.AddrPort{ap}, nil }
		return nil
	}
	c.disc = func(ctx context.Context) ([]netip.AddrPort, error) {
		return dht.Discover(ctx, infoHash, c.cfg.DHTBootstrap)
	}
	return nil
}

// carrierInfoHash returns the infohash to use for the selected carrier mode.
// It prefers an explicit entry in CarrierHashes and falls back to the legacy
// base InfoHash for backward compatibility.
func (c *Client) carrierInfoHash() (string, error) {
	mode := c.cfg.Transport
	if h := c.cfg.CarrierHashes[mode]; h != "" {
		if _, err := awg.InfoHashBytes(h); err != nil {
			return "", fmt.Errorf("invalid carrier hash for %s: %w", mode, err)
		}
		return h, nil
	}
	// Existing imports index their discovery entries by the old names.
	for name, hash := range c.cfg.CarrierHashes {
		if config.CanonicalMode(name) == mode {
			if _, err := awg.InfoHashBytes(hash); err != nil {
				return "", fmt.Errorf("invalid carrier hash for %s: %w", mode, err)
			}
			return hash, nil
		}
	}
	if config.IsTLSMode(mode) {
		return "", fmt.Errorf("TLS discovery hash missing for %s; obtain a fresh import", mode)
	}
	return c.cfg.InfoHash, nil
}

// SetDiscovery replaces endpoint selection for an integration test.
func (c *Client) SetDiscovery(fn func(ctx context.Context) ([]netip.AddrPort, error)) {
	if fn != nil {
		c.disc = fn
	}
}

// SocksAddr returns the local no-auth SOCKS5 address once it is listening.
func (c *Client) SocksAddr() string {
	s, _ := c.socksAddr.Load().(string)
	return s
}

// OverlayDialTCP dials a TCP address inside the overlay — another client's
// address or the hub. This is the client-to-client path: packets route
// through the tunnel and the hub forwards them between carriers; SOCKS5 and
// the egress are never involved.
func (c *Client) OverlayDialTCP(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
	if c.ns != nil {
		return c.ns.DialContextTCPAddrPort(ctx, addr)
	}
	// Real-TUN mode: the kernel routes overlay addresses via the interface.
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr.String())
}

// OverlayListenTCP starts a TCP listener inside the client's overlay
// netstack (netstack mode only): other clients reach it at ClientIP:port
// through the hub's forwarding — the "local network among clients".
func (c *Client) OverlayListenTCP(port uint16) (net.Listener, error) {
	if c.ns == nil {
		return nil, fmt.Errorf("overlay listener requires netstack mode")
	}
	return c.ns.ListenTCPAddrPort(netip.AddrPortFrom(netip.MustParseAddr(c.cfg.ClientIP), port))
}

// Run serves the local SOCKS5 and keeps the data path alive. In carrier modes
// it discovers the hub via DHT and reconnects on failure; in native modes it
// relies on the direct UDP endpoint configured in the import string.
func (c *Client) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if c.ip != nil {
		stop := context.AfterFunc(c.ip.life, cancel)
		defer stop()
	}
	listenIP := c.cfg.SocksListenIP
	if !listenIP.IsValid() {
		listenIP = netip.AddrFrom4([4]byte{127, 0, 0, 1})
	}
	ln, err := net.Listen("tcp4", net.JoinHostPort(listenIP.String(), strconv.Itoa(c.cfg.SocksPort)))
	if err != nil {
		return fmt.Errorf("socks listen: %w", err)
	}
	c.socksAddr.Store(ln.Addr().String())
	local := c.localProxy()
	go local.Serve(ctx, ln)

	return c.runTunnel(ctx, nil)
}

// runTunnel keeps only the selected network path alive; managed clients
// separately own all their SOCKS listeners.
func (c *Client) runTunnel(ctx context.Context, active *atomic.Pointer[carrierHealth]) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if c.ip != nil {
		stop := context.AfterFunc(c.ip.life, cancel)
		defer stop()
	}

	if config.IsNativeMode(c.cfg.Transport) {
		<-ctx.Done()
		return nil
	}

	serverEp := carrierbind.Endpoint(netip.MustParseAddr(c.cfg.ServerIP))
	hash, _ := c.carrierInfoHash()
	for {
		// DHT announces may not have landed yet when we look: retry the
		// discovery for a bounded window, then fail explicitly.
		var addrs []netip.AddrPort
		var discErr error
		for range 15 {
			var a []netip.AddrPort
			a, discErr = c.disc(ctx)
			if discErr == nil && len(a) > 0 {
				addrs = a
				break
			}
			if discErr == nil {
				discErr = fmt.Errorf("no peers found")
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
			}
		}
		if len(addrs) == 0 {
			return fmt.Errorf("dht discovery: %v (infohash %s)", discErr, hash)
		}
		t, raw, err := c.connectCarrier(ctx, addrs[0])
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Field debugging: silent retry loops made "no carrier"
			// errors undiagnosable (dial failures vs discovery gaps).
			fmt.Printf("btawg-client: carrier dial %s: %v; retrying\n", addrs[0], err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(retryDelay):
			}
			continue
		}
		if c.ip != nil {
			if err := c.ip.run(ctx, raw, t, active); err != nil && ctx.Err() == nil {
				fmt.Printf("btawg-client: TLS carrier %s: %v; retrying\n", addrs[0], err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(retryDelay):
				continue
			}
		}
		fmt.Printf("btawg-client: carrier connected to %s\n", addrs[0])
		c.bind.(*carrierbind.Bind).Attach(serverEp, t)
		if active != nil {
			active.Store(&carrierHealth{done: t.Done()})
		}
		var key device.NoisePublicKey
		if key.FromHex(c.cfg.ServerPubKey) == nil {
			if peer := c.dev.LookupPeer(key); peer != nil {
				peer.SendKeepalive()
			}
		}
		select {
		case <-t.Done():
			if active != nil {
				active.Store(nil)
			}
			c.bind.(*carrierbind.Bind).Detach(serverEp)
			t.Wait()
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(retryDelay):
			}
		case <-ctx.Done():
			if active != nil {
				active.Store(nil)
			}
			c.bind.(*carrierbind.Bind).Detach(serverEp)
			t.Wait()
			return nil
		}
	}
}

// connectCarrier dials the hub's carrier address over
// the host network and performs the BitTorrent handshake. This dial never
// goes through the tunnel: the carrier IS the tunnel's transport.
func (c *Client) connectCarrier(ctx context.Context, addr netip.AddrPort) (carrier.Transport, net.Conn, error) {
	hash, err := c.carrierInfoHash()
	if err != nil {
		return nil, nil, err
	}
	infoHash, err := awg.InfoHashBytes(hash)
	if err != nil {
		return nil, nil, err
	}
	peerID, err := awg.PeerIDBytes(awg.PeerID(c.cfg.ClientPubKey))
	if err != nil {
		return nil, nil, err
	}
	var conn net.Conn
	var closeSocket func() error
	switch c.cfg.Transport {
	case config.ModeExtUTP, config.ModeReqUTP, config.ModeTLSExtUTP, config.ModeTLSReqUTP:
		pc, err := net.ListenUDP("udp4", nil)
		if err != nil {
			return nil, nil, fmt.Errorf("carrier udp: %w", err)
		}
		sock, err := carrier.NewUTPSocket(pc)
		if err != nil {
			pc.Close()
			return nil, nil, fmt.Errorf("carrier utp socket: %w", err)
		}
		dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		conn, err = sock.DialContext(dialCtx, "", addr.String())
		cancel()
		if err != nil {
			sock.Close()
			return nil, nil, fmt.Errorf("carrier dial %s: %w", addr, err)
		}
		closeSocket = sock.Close
	case config.ModeExtTCP, config.ModeReqTCP, config.ModeTLSExtTCP, config.ModeTLSReqTCP:
		var guard tunsetup.UnderlayDialGuard
		d := net.Dialer{Control: func(network, address string, raw syscall.RawConn) error {
			if err := guard.Control(network, address, raw); err != nil {
				return err
			}
			return carrier.TCPControl(network, address, raw)
		}}
		conn, err = d.DialContext(ctx, "tcp", addr.String())
		if err != nil {
			return nil, nil, fmt.Errorf("carrier dial %s: %w", addr, err)
		}
		conn, err = guard.Protect(conn, addr.Addr())
		if err != nil {
			return nil, nil, fmt.Errorf("carrier underlay: %w", err)
		}
	default:
		return nil, nil, fmt.Errorf("unknown transport %q", c.cfg.Transport)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		conn.Close()
		if closeSocket != nil {
			closeSocket()
		}
		return nil, nil, err
	}
	var t carrier.Transport
	switch config.CarrierFraming(c.cfg.Transport) {
	case "ext":
		if config.IsTLSMode(c.cfg.Transport) {
			t, _, err = carrier.NewExtended(conn, infoHash, peerID)
		} else {
			t, _, err = carrier.NewHubExtended(conn, infoHash, peerID, c.cfg.TLSServerKey != "")
		}
	case "req":
		if config.IsTLSMode(c.cfg.Transport) {
			t, _, err = carrier.NewRequestPiece(conn, infoHash, peerID, false)
		} else {
			t, _, err = carrier.NewHubRequestPiece(conn, infoHash, peerID, false)
		}
	}
	base := t
	if err == nil && carrier.ExtendedExperiment(t) != "" {
		t, err = c.protectCarrier(ctx, conn, base, carrier.ExtendedExperiment(base))
		if err != nil {
			base.Wait()
		}
	}
	if err == nil && !config.IsTLSMode(c.cfg.Transport) {
		var timed carrier.Transport
		timed, err = carrier.WithTiming(t, base)
		if err == nil {
			t = timed
		} else {
			t.Wait()
		}
	}
	if err != nil {
		conn.Close()
		if closeSocket != nil {
			closeSocket()
		}
		return nil, nil, fmt.Errorf("carrier handshake: %w", err)
	}
	if closeSocket != nil {
		go func() {
			<-t.Done()
			closeSocket()
		}()
	}
	if !config.IsTLSMode(c.cfg.Transport) {
		if err := conn.SetDeadline(time.Time{}); err != nil {
			t.Wait()
			return nil, nil, err
		}
	}
	return t, conn, nil
}

func (c *Client) protectCarrier(ctx context.Context, raw net.Conn, base carrier.Transport, mode string) (carrier.Transport, error) {
	id, err := iptunnel.NewIdentity(c.cfg.ClientPrivKey, iptunnel.Client)
	if err != nil {
		return nil, err
	}
	cfg, err := id.TLS(c.cfg.TLSServerKey)
	if err != nil {
		return nil, err
	}
	secure := tls.Client(iptunnel.Stream(raw, base), cfg)
	if err := secure.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("protected carrier authentication: %w", err)
	}
	return iptunnel.SecureDatagramRecords(secure, base, mode)
}

// Close tears the client down.
func (c *Client) Close() {
	if c.closeTUN != nil {
		defer c.closeTUN()
	}
	if c.ip != nil {
		c.ip.close()
		return
	}
	c.bind.Close()
	c.dev.Close()
}
