// Package config defines the btawg configuration formats: the AWG obfuscation
// profile, the versioned client import string (btawg:// + base64url JSON), and
// the server-side hub configuration. The import string is a secret: it carries
// the client's private key. It never contains the server's public endpoint;
// clients learn the server address strictly through DHT discovery.
package config

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// ImportPrefix identifies a btawg client import string.
const ImportPrefix = "btawg://"

// ClientVersion is the import-string format version accepted by the client
// daemon. Subscription refresh replaces older saved imports without running
// their legacy transports or parsing their retired format.
const ClientVersion = 2

// DefaultMTU is the default IP MTU of the hub and client tunnels.
const DefaultMTU = 1280

var nonInternetIPv4 = [...]netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// PublicIPv4 reports whether ip is eligible for routed Internet egress.
func PublicIPv4(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, p := range nonInternetIPv4 {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// Supported SOCKS5 connection chains. Native modes can also be exported
// for standalone clients that do not provide SOCKS5.
const (
	ModeExtUTP = "socks5-awg-ext-utp"
	ModeExtTCP = "socks5-awg-ext-tcp"
	ModeReqUTP = "socks5-awg-req-utp"
	ModeReqTCP = "socks5-awg-req-tcp"
	ModeAWG    = "socks5-awg-udp"
	ModeWG     = "socks5-wg-udp"

	ModeTLSExtUTP = "socks5-ext-utp"
	ModeTLSExtTCP = "socks5-ext-tcp"
	ModeTLSReqUTP = "socks5-req-utp"
	ModeTLSReqTCP = "socks5-req-tcp"
)

// ClientModes returns the canonical names in display order.
func ClientModes() []string {
	return []string{ModeExtUTP, ModeExtTCP, ModeReqUTP, ModeReqTCP, ModeAWG, ModeWG,
		ModeTLSExtUTP, ModeTLSExtTCP, ModeTLSReqUTP, ModeTLSReqTCP}
}

// CanonicalMode translates transport names from existing deployments.
func CanonicalMode(mode string) string {
	switch mode {
	case "ext-utp":
		return ModeExtUTP
	case "ext-tcp":
		return ModeExtTCP
	case "req-utp":
		return ModeReqUTP
	case "req-tcp":
		return ModeReqTCP
	case "tls-ext-utp":
		return ModeTLSExtUTP
	case "tls-ext-tcp":
		return ModeTLSExtTCP
	case "tls-req-utp":
		return ModeTLSReqUTP
	case "tls-req-tcp":
		return ModeTLSReqTCP
	case "awg":
		return ModeAWG
	case "wg":
		return ModeWG
	default:
		return mode
	}
}

// CarrierFraming reports the BitTorrent wire framing, or empty for other modes.
func CarrierFraming(mode string) string {
	switch CanonicalMode(mode) {
	case ModeExtUTP, ModeExtTCP, ModeTLSExtUTP, ModeTLSExtTCP:
		return "ext"
	case ModeReqUTP, ModeReqTCP, ModeTLSReqUTP, ModeTLSReqTCP:
		return "req"
	}
	return ""
}

// IsCarrierMode reports whether mode uses the BitTorrent carrier path.
func IsCarrierMode(mode string) bool { return CarrierFraming(mode) != "" }

// IsTLSMode reports whether the carrier uses TLS instead of AWG.
func IsTLSMode(mode string) bool {
	switch mode {
	case ModeTLSExtUTP, ModeTLSExtTCP, ModeTLSReqUTP, ModeTLSReqTCP:
		return true
	}
	return false
}

// IsNativeMode reports whether mode uses a direct AWG or WireGuard UDP path.
func IsNativeMode(mode string) bool {
	mode = CanonicalMode(mode)
	return mode == ModeAWG || mode == ModeWG
}

// ValidClientMode reports whether mode is one of the supported modes.
func ValidClientMode(mode string) bool { return IsCarrierMode(mode) || IsNativeMode(mode) }

// CarrierHelloModes returns one representative per discovery hash.
func CarrierHelloModes() [4]string {
	return [4]string{ModeExtTCP, ModeReqTCP, ModeTLSExtTCP, ModeTLSReqTCP}
}

// ClientConfig is the full client bootstrap state carried in the import string
// issued by the server panel. ServerIP is the overlay hub address, not a
// reachable endpoint: it exists only inside the tunnel.
type ClientConfig struct {
	Version         int               `json:"version"`
	InfoHash        string            `json:"infohash"`                 // legacy 20-byte hex (old framing)
	ServerPubKey    string            `json:"server_pubkey"`            // hex
	TLSServerKey    string            `json:"tls_server_key,omitempty"` // pinned Ed25519 public key
	ClientPrivKey   string            `json:"client_privkey"`           // hex (secret)
	ClientPubKey    string            `json:"client_pubkey"`            // hex
	ClientIP        string            `json:"client_ip"`                // overlay, e.g. 10.77.0.2
	Subnet          string            `json:"subnet"`                   // overlay, e.g. 10.77.0.0/24
	ServerIP        string            `json:"server_ip"`                // overlay hub, e.g. 10.77.0.1
	Transport       string            `json:"transport"`                // user-selected mode
	SocksPort       int               `json:"socks_port"`               // client's local no-auth SOCKS5
	Profile         AWGProfile        `json:"awg_profile"`
	DHTBootstrap    []string          `json:"dht_bootstrap,omitempty"`
	CarrierHashes   map[string]string `json:"carrier_hashes,omitempty"`   // mode -> 20-byte hex infohash
	CarrierEndpoint string            `json:"carrier_endpoint,omitempty"` // public IPv4 address and carrier port
	AWGEndpoint     string            `json:"awg_endpoint,omitempty"`     // host:port for native AWG
	WGEndpoint      string            `json:"wg_endpoint,omitempty"`      // host:port for native WG
	Routes          []string          `json:"routes,omitempty"`           // managed VPN CIDRs (no default route)
	SocksListenIP   netip.Addr        `json:"-"`                          // local opt-in; never accepted from an import
}

// ImportString encodes the config as btawg:// + base64url(JSON).
func (c ClientConfig) ImportString() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("marshal client config: %w", err)
	}
	return ImportPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// ParseImportString decodes and validates a btawg:// import string.
func ParseImportString(s string) (ClientConfig, error) {
	var c ClientConfig
	if !strings.HasPrefix(s, ImportPrefix) {
		return c, fmt.Errorf("import string must start with %q", ImportPrefix)
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, ImportPrefix))
	if err != nil {
		return c, fmt.Errorf("decode import string: %w", err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse import string: %w", err)
	}
	if c.Version != ClientVersion {
		return c, fmt.Errorf("unsupported import string version %d (want %d)", c.Version, ClientVersion)
	}
	if c.Profile == (AWGProfile{}) {
		c.Profile = DefaultProfile()
	}
	return c, nil
}

// CarrierHash returns a 20-byte hex discovery hash. Legacy AWG framing keeps
// its existing hashes; TLS always gets a separate namespace per framing.
func CarrierHash(base, preferred, mode string) (string, error) {
	framing := CarrierFraming(mode)
	if framing == "" {
		return "", fmt.Errorf("invalid carrier mode %q", mode)
	}
	if err := validateInfoHash(base); err != nil {
		return "", fmt.Errorf("invalid base infohash: %w", err)
	}
	if IsTLSMode(mode) {
		framing = "tls-ip-v1:" + framing
	} else if !IsTLSMode(preferred) && framing == CarrierFraming(preferred) {
		return base, nil
	}
	sum := sha256.Sum256([]byte(base + framing))
	return hex.EncodeToString(sum[:20]), nil
}

// NativeConfig renders a wg-quick / awg-quick INI configuration for direct
// native modes. Keys are exported as base64, the endpoint is the real public
// UDP endpoint, and AllowedIPs come from Routes (no default route).
func (c ClientConfig) NativeConfig(mode string) (string, error) {
	mode = CanonicalMode(mode)
	if !IsNativeMode(mode) {
		return "", fmt.Errorf("native mode must be %q or %q, got %q", ModeAWG, ModeWG, mode)
	}
	privB64, pubB64, endpoint, routes, err := c.nativeCommon(mode)
	if err != nil {
		return "", err
	}

	subnet, err := netip.ParsePrefix(c.Subnet)
	if err != nil || !subnet.Addr().Is4() || subnet.Bits() == 0 {
		return "", fmt.Errorf("invalid IPv4 subnet %q", c.Subnet)
	}

	var b strings.Builder
	b.WriteString("[Interface]\n")
	fmt.Fprintf(&b, "PrivateKey = %s\n", privB64)
	fmt.Fprintf(&b, "Address = %s/%d\n", c.ClientIP, subnet.Bits())
	fmt.Fprintf(&b, "MTU = %d\n", DefaultMTU)
	if mode == ModeAWG {
		if err := c.Profile.Validate(); err != nil {
			return "", err
		}
		c.Profile.writeDevice(&b, true)
	}
	b.WriteString("\n[Peer]\n")
	fmt.Fprintf(&b, "PublicKey = %s\n", pubB64)
	fmt.Fprintf(&b, "AllowedIPs = %s\n", strings.Join(routes, ", "))
	fmt.Fprintf(&b, "Endpoint = %s\n", endpoint)
	keepalive := "25"
	if mode == ModeAWG && c.Profile.PersistentKeepalive != "" {
		keepalive = string(c.Profile.PersistentKeepalive)
	}
	fmt.Fprintf(&b, "PersistentKeepalive = %s\n", keepalive)
	return b.String(), nil
}

// NativeDeviceConfig renders the line-oriented IPC configuration consumed by
// the AmneziaWG device for native modes. Keys are kept as hex (the engine's
// IPC format) and routes use allowed_ip stanzas.
func (c ClientConfig) NativeDeviceConfig(mode string) (string, error) {
	mode = CanonicalMode(mode)
	if !IsNativeMode(mode) {
		return "", fmt.Errorf("native mode must be %q or %q, got %q", ModeAWG, ModeWG, mode)
	}
	_, _, endpoint, _, err := c.nativeCommon(mode)
	if err != nil {
		return "", err
	}
	if c.ClientPrivKey == "" {
		return "", fmt.Errorf("client private key required")
	}
	if c.ServerPubKey == "" {
		return "", fmt.Errorf("server public key required")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\n", c.ClientPrivKey)
	if mode == ModeAWG {
		if err := c.Profile.Validate(); err != nil {
			return "", err
		}
		b.WriteString(c.Profile.DeviceIPC())
	}
	b.WriteString("replace_peers=true\n")
	fmt.Fprintf(&b, "public_key=%s\n", c.ServerPubKey)
	// The device accepts any inner source so that the hub can forward
	// overlay replies, internet unicast traffic, and user-managed routes.
	b.WriteString("allowed_ip=0.0.0.0/0\n")
	fmt.Fprintf(&b, "endpoint=%s\n", endpoint)
	keepalive := "25"
	if mode == ModeAWG && c.Profile.PersistentKeepalive != "" {
		keepalive = string(c.Profile.PersistentKeepalive)
	}
	fmt.Fprintf(&b, "persistent_keepalive_interval=%s\n", keepalive)
	return b.String(), nil
}

// nativeCommon validates keys and endpoint and returns the managed VPN routes
// shared by the export renderer and the internal IPC renderer.
func (c ClientConfig) nativeCommon(mode string) (privB64, pubB64, endpoint string, routes []string, err error) {
	if c.ClientPrivKey == "" {
		return "", "", "", nil, fmt.Errorf("client private key required")
	}
	if c.ServerPubKey == "" {
		return "", "", "", nil, fmt.Errorf("server public key required")
	}
	privB64, err = keyHexToBase64(c.ClientPrivKey)
	if err != nil {
		return "", "", "", nil, fmt.Errorf("client private key: %w", err)
	}
	pubB64, err = keyHexToBase64(c.ServerPubKey)
	if err != nil {
		return "", "", "", nil, fmt.Errorf("server public key: %w", err)
	}

	endpoint = c.AWGEndpoint
	if mode == ModeWG {
		endpoint = c.WGEndpoint
	}
	if endpoint == "" {
		return "", "", "", nil, fmt.Errorf("no %s endpoint in config", mode)
	}
	if _, _, err := net.SplitHostPort(endpoint); err != nil {
		return "", "", "", nil, fmt.Errorf("invalid %s endpoint %q: %w", mode, endpoint, err)
	}

	routes, err = c.nativeRoutes()
	if err != nil {
		return "", "", "", nil, err
	}
	return privB64, pubB64, endpoint, routes, nil
}

// nativeRoutes returns the managed VPN CIDRs to advertise to the peer.
// It falls back to the client subnet when no explicit routes are present,
// always ensures the hub overlay address is reachable, and rejects IPv6 and
// default routes.
func (c ClientConfig) nativeRoutes() ([]string, error) {
	routes := make([]string, 0, len(c.Routes)+2)
	for _, r := range c.Routes {
		p, err := netip.ParsePrefix(r)
		if err != nil {
			return nil, fmt.Errorf("invalid route %q: %w", r, err)
		}
		if !p.Addr().Is4() {
			return nil, fmt.Errorf("IPv6 routes are not supported: %q", r)
		}
		if p.Bits() == 0 {
			return nil, fmt.Errorf("default route is not allowed: %q", r)
		}
		routes = append(routes, p.Masked().String())
	}
	if len(routes) == 0 {
		p, err := netip.ParsePrefix(c.Subnet)
		if err != nil {
			return nil, fmt.Errorf("invalid subnet %q: %w", c.Subnet, err)
		}
		if !p.Addr().Is4() {
			return nil, fmt.Errorf("IPv6 subnet is not supported: %q", c.Subnet)
		}
		if p.Bits() == 0 {
			return nil, fmt.Errorf("default subnet is not allowed: %q", c.Subnet)
		}
		routes = append(routes, p.Masked().String())
	}

	serverIP, err := netip.ParseAddr(c.ServerIP)
	if err != nil {
		return nil, fmt.Errorf("invalid server_ip %q: %w", c.ServerIP, err)
	}
	if !serverIP.Is4() {
		return nil, fmt.Errorf("IPv6 server_ip is not supported: %q", c.ServerIP)
	}
	coversServer := false
	for _, r := range routes {
		p := netip.MustParsePrefix(r)
		if p.Contains(serverIP) {
			coversServer = true
			break
		}
	}
	if !coversServer {
		routes = append(routes, netip.PrefixFrom(serverIP, serverIP.BitLen()).String())
	}
	return routes, nil
}

func validateInfoHash(s string) error {
	if len(s) != 40 {
		return fmt.Errorf("infohash must be 40 hex chars, got %d", len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		return fmt.Errorf("infohash must be hex: %w", err)
	}
	return nil
}

func keyHexToBase64(s string) (string, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return "", fmt.Errorf("invalid key")
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// ServerConfig is the persistent hub configuration edited through the web
// panel. ExternalIP is the announced carrier address; empty means autodetect
// from the default-route interface.
type ServerConfig struct {
	AWGProfile      AWGProfile `json:"awg_profile"`
	Subnet          string     `json:"subnet"`
	ServerIP        string     `json:"server_ip"`
	Transport       string     `json:"transport"`
	LegacyTransport string     `json:"legacy_transport,omitempty"`
	CarrierPort     int        `json:"carrier_port"`
	AWGPort         int        `json:"awg_port"`
	WGPort          int        `json:"wg_port"`
	PanelPort       int        `json:"panel_port"`
	ExternalIP      string     `json:"external_ip"`
	InfoHash        string     `json:"infohash"`        // 20-byte hex, generated at first boot
	ServerPrivKey   string     `json:"server_privkey"`  // hex (secret), generated at first boot
	AdminPassHash   string     `json:"admin_pass_hash"` // bcrypt hash of the panel password
}

// DefaultServerConfig returns the shipped defaults used when the store has no
// saved configuration yet.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		AWGProfile:  DefaultProfile(),
		Subnet:      "10.77.0.0/24",
		ServerIP:    "10.77.0.1",
		Transport:   ModeExtUTP,
		CarrierPort: 6881,
		AWGPort:     51821,
		WGPort:      51820,
		PanelPort:   8080,
	}
}

// Normalize fills zero values with the shipped defaults. It is intended for
// persisted legacy configs that predate the native AWG/WG listener ports.
func (c *ServerConfig) Normalize() {
	def := DefaultServerConfig()
	if c.AWGProfile == (AWGProfile{}) {
		c.AWGProfile = def.AWGProfile
	}
	if c.Subnet == "" {
		c.Subnet = def.Subnet
	}
	if c.ServerIP == "" {
		c.ServerIP = def.ServerIP
	}
	if c.Transport == "" {
		c.Transport = def.Transport
	}
	c.Transport = CanonicalMode(c.Transport)
	c.LegacyTransport = CanonicalMode(c.LegacyTransport)
	if c.CarrierPort == 0 {
		c.CarrierPort = def.CarrierPort
	}
	if c.AWGPort == 0 {
		c.AWGPort = def.AWGPort
	}
	if c.WGPort == 0 {
		c.WGPort = def.WGPort
	}
	if c.PanelPort == 0 {
		c.PanelPort = def.PanelPort
	}
}
