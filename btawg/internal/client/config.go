package client

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"slices"

	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/mesh"
	"bittorrent-tunnel/btawg/internal/service"
)

// HubRouteID names the server egress and is the default for unconfigured ports.
const HubRouteID = mesh.HubRoute

// RouteOption is one egress route currently permitted by the hub.
type RouteOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// PeerSettings controls directional peer transports independently of SOCKS5.
type PeerSettings struct {
	Incoming  []string                    `json:"incoming"`
	Outgoing  string                      `json:"outgoing"`
	Pools     map[string][]string         `json:"pools"`
	Relay     bool                        `json:"relay"`
	Overrides map[netip.Addr]PeerOverride `json:"overrides,omitempty"`
}

// PeerOverride inherits absent fields and absent pool entries from PeerSettings.
// An empty array or a non-nil false relay value is an explicit override.
type PeerOverride struct {
	Incoming []string            `json:"incoming"`
	Outgoing string              `json:"outgoing,omitempty"`
	Pools    map[string][]string `json:"pools,omitempty"`
	Relay    *bool               `json:"relay,omitempty"`
}

// ForPeer returns effective settings for one VPN address without mutating defaults.
func (p PeerSettings) ForPeer(ip netip.Addr) PeerSettings {
	o := p.Overrides[ip]
	p.Overrides = nil
	if o.Incoming != nil {
		p.Incoming = o.Incoming
	}
	if o.Outgoing != "" {
		p.Outgoing = o.Outgoing
	}
	if len(o.Pools) != 0 {
		pools := make(map[string][]string, len(p.Pools)+len(o.Pools))
		maps.Copy(pools, p.Pools)
		maps.Copy(pools, o.Pools)
		p.Pools = pools
	}
	if o.Relay != nil {
		p.Relay = *o.Relay
	}
	return p
}

// SelectedMode returns the canonical transport or pool selected for outgoing flows.
func (p PeerSettings) SelectedMode() string {
	if p.Outgoing == "" {
		return config.ModeExtUTP
	}
	return config.CanonicalMode(p.Outgoing)
}

func canonicalPeerModes(modes []string) []string {
	result := make([]string, 0, len(modes))
	for _, mode := range modes {
		result = append(result, config.CanonicalMode(mode))
	}
	return result
}

// IncomingModes returns the canonical incoming allowlist, respecting explicit empty lists.
func (p PeerSettings) IncomingModes() []string {
	if p.Incoming == nil {
		return []string{config.ModeExtUTP}
	}
	return canonicalPeerModes(p.Incoming)
}

// OutgoingModes returns the selected transport or ordered members of its pool.
func (p PeerSettings) OutgoingModes() []string {
	selected := p.SelectedMode()
	if !slices.Contains(balancerPolicies[:], selected) {
		return []string{selected}
	}
	pool, ok := p.Pools[selected]
	if !ok {
		return config.ClientModes()
	}
	return canonicalPeerModes(pool)
}

func (p PeerSettings) validate() error {
	if err := p.validateTransports(); err != nil {
		return err
	}
	if len(p.Overrides) > mesh.MaxPeerOverrides {
		return fmt.Errorf("at most %d peer overrides are supported", mesh.MaxPeerOverrides)
	}
	for ip, o := range p.Overrides {
		if !mesh.ValidPeerOverrideIP(ip) {
			return fmt.Errorf("invalid peer VPN IPv4 address %q", ip)
		}
		if err := (PeerSettings{Incoming: o.Incoming, Outgoing: o.Outgoing, Pools: o.Pools}).validateTransports(); err != nil {
			return fmt.Errorf("peer %s: %w", ip, err)
		}
	}
	return nil
}

func (p PeerSettings) validateTransports() error {
	validateModes := func(modes []string) error {
		seen := make(map[string]bool, len(modes))
		for _, mode := range modes {
			canonical := config.CanonicalMode(mode)
			if !config.ValidClientMode(canonical) {
				return fmt.Errorf("unsupported peer transport %q", mode)
			}
			if seen[canonical] {
				return fmt.Errorf("duplicate peer transport %q", mode)
			}
			seen[canonical] = true
		}
		return nil
	}
	if err := validateModes(p.Incoming); err != nil {
		return err
	}
	selected := p.SelectedMode()
	if !config.ValidClientMode(selected) && !slices.Contains(balancerPolicies[:], selected) {
		return fmt.Errorf("unknown peer transport or policy %q", p.Outgoing)
	}
	for policy, modes := range p.Pools {
		if !slices.Contains(balancerPolicies[:], policy) {
			return fmt.Errorf("unknown peer pool %q", policy)
		}
		if modes == nil {
			return fmt.Errorf("peer pool %q must be an array", policy)
		}
		if err := validateModes(modes); err != nil {
			return err
		}
	}
	return nil
}

// ManagedConfig is the root-owned client configuration and persists across
// upgrades and restarts.
type ManagedConfig struct {
	Import        string      `json:"import"`
	Enabled       []string    `json:"enabled"`
	ListenIP      string      `json:"listen_ip"`
	Credentials   Credentials `json:"credentials"`
	NoTUN         bool        `json:"no_tun,omitempty"`
	TUNName       string      `json:"tun_name,omitempty"`
	SOCKSBasePort int         `json:"socks_base_port,omitempty"` // zero uses ManagedPortBase
	// A missing balancer pool follows Enabled; an empty list routes nothing.
	Balancers map[string][]string `json:"balancers,omitempty"`
	// Keys are stable slot numbers (1080 + offset), independent of the listen base.
	// Missing entries use the hub; explicit choices survive catalogue changes.
	Routes             map[int]string `json:"routes,omitempty"`
	P2P                bool           `json:"p2p,omitempty"`
	AutoMap            bool           `json:"auto_map,omitempty"`
	AllowPermanentUPnP bool           `json:"allow_permanent_upnp,omitempty"`
	ServeEgress        bool           `json:"serve_egress,omitempty"`
	PeerPort           uint16         `json:"peer_port,omitempty"` // zero lets the OS allocate a port
	PeerEndpoint       string         `json:"peer_endpoint,omitempty"`
	PeerTCPEndpoint    string         `json:"peer_tcp_endpoint,omitempty"`
	Peer               PeerSettings   `json:"peer"`

	// SubscriptionURL is a secret, fetched only by an explicit client command.
	SubscriptionURL string `json:"subscription_url,omitempty"`
}

// PortBase returns the first configured SOCKS5 listen port.
func (c ManagedConfig) PortBase() int { return socksPortBase(c.SOCKSBasePort) }

func socksPortBase(base int) int {
	if base == 0 {
		return ManagedPortBase
	}
	return base
}

func socksPortCount() int { return len(config.ClientModes()) + len(balancerPolicies) }

// ValidateSOCKSBasePort checks that the entire block fits in the TCP port range.
func ValidateSOCKSBasePort(base int) error {
	maxBase := 65536 - socksPortCount()
	if base < 1 || base > maxBase {
		return fmt.Errorf("SOCKS5 base port must be between 1 and %d", maxBase)
	}
	return nil
}

func selectedRouteID(routes map[int]string, port int) string {
	if id := routes[port]; id != "" {
		return id
	}
	return HubRouteID
}

var globalIPv6 = netip.MustParsePrefix("2000::/3")

func (c ManagedConfig) validatePeerSettings(realTUN bool) error {
	if err := c.Peer.validate(); err != nil {
		return err
	}
	if c.P2P && (!realTUN || c.NoTUN) {
		return fmt.Errorf("direct peer connections require a TUN interface")
	}
	if c.AutoMap && !c.P2P {
		return fmt.Errorf("automatic peer port mapping requires P2P")
	}
	if c.AllowPermanentUPnP && !c.AutoMap {
		return fmt.Errorf("permanent UPnP mappings require automatic peer port mapping")
	}
	for i, raw := range [2]string{c.PeerEndpoint, c.PeerTCPEndpoint} {
		if raw == "" {
			continue
		}
		endpoint, err := netip.ParseAddrPort(raw)
		ip := endpoint.Addr()
		if err != nil || endpoint.Port() == 0 || ip.Zone() != "" || ip.Is4In6() || !config.PublicIPv4(ip) && !globalIPv6.Contains(ip) {
			return fmt.Errorf("public P2P %s address must be a public IPv4 or global IPv6 address with a nonzero port", [2]string{"UDP", "TCP"}[i])
		}
	}
	return nil
}

// LoadManagedConfig reads the client configuration from path.
func LoadManagedConfig(path string) (ManagedConfig, error) {
	var c ManagedConfig
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := ValidateSOCKSBasePort(c.PortBase()); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, nil
}

// Save writes the configuration atomically with root-only permissions.
func (c ManagedConfig) Save(path string) error {
	if err := ValidateSOCKSBasePort(c.PortBase()); err != nil {
		return err
	}
	if err := c.validatePeerSettings(!c.NoTUN); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return service.WriteManagedFile(path, b)
}

// GenerateCredentials returns a fresh random SOCKS5 username/password pair.
func GenerateCredentials() (Credentials, error) {
	user, err := randomToken(6)
	if err != nil {
		return Credentials{}, err
	}
	pass, err := randomToken(18)
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{Username: "btawg-" + user, Password: pass}, nil
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
