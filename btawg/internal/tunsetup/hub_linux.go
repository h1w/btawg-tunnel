package tunsetup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// Hub configures a server-side TUN interface for IPv4 overlay routing.
//
// It assumes the caller has already created the TUN device and brought it up
// with an appropriate MTU and txqueuelen.  Hub.Apply assigns the requested
// gateway addresses, enables IPv4 forwarding, and installs a scoped nftables
// table that:
//   - accepts established/related traffic only on the allowed interface
//     pairs (WAN<->TUN and TUN<->TUN),
//   - accepts new forwarded traffic from this TUN, sourced from the approved
//     subnets, and destined either to the WAN interface or back to the same
//     TUN interface for overlay routing,
//   - masquerades approved tunnel traffic that leaves through the WAN,
//   - drops other forwarded traffic to/from the TUN so that tunnel clients
//     cannot reach arbitrary local networks and vice-versa.
//
// It deliberately does not touch INPUT/OUTPUT chains and does not delete
// unrelated nftables rules.
//
// Operator firewall interaction:
//
// The rules live in a dedicated per-interface nftables table and base chains
// hooked into forward and postrouting.  An explicit accept verdict in one base
// chain terminates evaluation of that chain, but it does not prevent later
// base chains at the same hook from seeing the packet; only drop/stop verdicts
// end hook traversal.  Therefore an operator-owned forward chain with policy
// drop can still drop packets after they pass through this package's accept
// rules.  The drop rules added by this package do terminate hook traversal,
// so an operator chain at a later priority will not see packets that match
// them.  Close removes the whole scoped table and the addresses it installed;
// it does not touch any operator-owned tables.
type Hub struct {
	name     string
	table    string
	gateways []netip.Prefix
	wan      string
}

// NewHub returns a Hub for the named TUN interface.
func NewHub(name string) (*Hub, error) {
	if name == "" {
		return nil, errors.New("tunsetup: hub interface name is empty")
	}
	return &Hub{
		name:  name,
		table: "btawg_" + sanitizeID(name),
	}, nil
}

// WAN returns the uplink used by this hub's forwarding policy.
// Call under the same lock that protects Apply.
func (h *Hub) WAN() string { return h.wan }

// Apply assigns gateway addresses on the TUN, enables IPv4 forwarding, and
// installs scoped NAT/forwarding rules.  Each prefix must be an IPv4 prefix
// whose host bits identify the gateway address to assign.
//
// Apply is idempotent: it can be called repeatedly with the same or different
// gateways.  Addresses that are no longer in the gateway list are removed, new
// addresses are added, and the scoped nftables table is replaced atomically.
// If address assignment succeeds but the nftables transaction fails, the newly
// added addresses are rolled back.  If an unrelated table with the same name
// already exists, Apply returns an error instead of clobbering it.
func (h *Hub) Apply(gateways []netip.Prefix) error {
	for i, g := range gateways {
		if !g.IsValid() || !g.Addr().Is4() {
			return fmt.Errorf("tunsetup: gateway %d (%s) is not a valid IPv4 prefix", i, g)
		}
	}

	wan, err := defaultWAN(h.name)
	if err != nil {
		return fmt.Errorf("hub %s: detect WAN interface: %w", h.name, err)
	}

	if err := enableIPv4Forwarding(); err != nil {
		return fmt.Errorf("hub %s: enable IPv4 forwarding: %w", h.name, err)
	}

	toAdd := prefixDiff(gateways, h.gateways)
	toRemove := prefixDiff(h.gateways, gateways)

	var added, removed []netip.Prefix
	rollback := func(err error) error {
		for _, a := range added {
			err = errors.Join(err, run("ip", "addr", "del", a.String(), "dev", h.name))
		}
		for _, a := range removed {
			err = errors.Join(err, run("ip", "addr", "add", a.String(), "dev", h.name))
		}
		return err
	}
	for _, g := range toAdd {
		if hasAddr(h.name, g) {
			continue
		}
		if err := run("ip", "addr", "add", g.String(), "dev", h.name); err != nil {
			return rollback(fmt.Errorf("hub %s: add address %s: %w", h.name, g, err))
		}
		added = append(added, g)
	}

	for _, g := range toRemove {
		if !hasAddr(h.name, g) {
			continue
		}
		if err := run("ip", "addr", "del", g.String(), "dev", h.name); err != nil {
			return rollback(fmt.Errorf("hub %s: remove obsolete address %s: %w", h.name, g, err))
		}
		removed = append(removed, g)
	}
	if err := h.applyNFT(gateways, wan); err != nil {
		return rollback(fmt.Errorf("hub %s: apply nftables: %w", h.name, err))
	}

	h.gateways = append([]netip.Prefix(nil), gateways...)
	h.wan = wan
	return nil
}

// Close removes the scoped nftables table and the addresses that Apply added.
// It is safe to call on a TUN device that the caller has already deleted, and
// it will not delete an identically-named table that was not created by this
// Hub.
func (h *Hub) Close() error {
	var errs []error

	managed, err := nftTableManaged(h.table, h.name)
	if err != nil && !notExists(err) {
		errs = append(errs, fmt.Errorf("check nft table %s: %w", h.table, err))
	} else if managed {
		if err := run("nft", "delete", "table", "ip", h.table); err != nil && !notExists(err) {
			errs = append(errs, fmt.Errorf("delete nft table %s: %w", h.table, err))
		}
	}

	for _, g := range h.gateways {
		if err := run("ip", "addr", "del", g.String(), "dev", h.name); err != nil && !notExists(err) {
			errs = append(errs, fmt.Errorf("remove address %s from %s: %w", g, h.name, err))
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// defaultWAN returns the host's default outbound interface, validating that it
// is not the tunnel itself and is not loopback.  It first asks the routing
// table for a path to a public address; if that fails it falls back to the
// default route.  This makes it less likely to pick another local interface.
func defaultWAN(tun string) (string, error) {
	dev := routeDev("1.1.1.1")
	if dev == "" {
		dev = routeDev("default")
	}
	if dev == "" {
		return "", errors.New("no default route or public route found")
	}
	if dev == tun {
		return "", fmt.Errorf("default outbound interface is the tunnel %q", dev)
	}
	if dev == "lo" {
		return "", fmt.Errorf("default outbound interface is loopback")
	}
	if err := exec.Command("ip", "link", "show", "dev", dev).Run(); err != nil {
		return "", fmt.Errorf("WAN interface %q is not present: %w", dev, err)
	}
	return dev, nil
}

type routeEntry struct {
	Dev string `json:"dev"`
}

// routeDev runs `ip route get` or `ip route show` for the given target and
// returns the first interface name from the JSON output.
func routeDev(target string) string {
	var out []byte
	var err error
	if target == "default" {
		out, err = exec.Command("ip", "-j", "route", "show", "default").Output()
	} else {
		out, err = exec.Command("ip", "-j", "route", "get", target).Output()
	}
	if err != nil {
		return ""
	}

	var list []routeEntry
	if json.Unmarshal(out, &list) == nil && len(list) > 0 {
		return list[0].Dev
	}
	var single routeEntry
	if json.Unmarshal(out, &single) == nil {
		return single.Dev
	}
	return ""
}

func enableIPv4Forwarding() error {
	return run("sysctl", "-w", "net.ipv4.ip_forward=1")
}

func (h *Hub) applyNFT(gateways []netip.Prefix, wan string) error {
	exists, err := nftTableExists(h.table)
	if err != nil {
		return err
	}
	if exists {
		managed, err := nftTableManaged(h.table, h.name)
		if err != nil {
			return err
		}
		if !managed {
			return fmt.Errorf("nft table %q already exists and is not managed by this hub", h.table)
		}
	}

	script := h.nftScript(gateways, wan, exists)
	return nftApply(script)
}

// nftScript renders the atomic nftables ruleset.  It is exported to tests so
// the generated syntax can be checked with `nft -c -f -` without privileges.
func (h *Hub) nftScript(gateways []netip.Prefix, wan string, exists bool) string {
	var b bytes.Buffer
	if exists {
		fmt.Fprintf(&b, "delete table ip %s\n", h.table)
	}
	fmt.Fprintf(&b, "table ip %s {\n", h.table)
	fmt.Fprintf(&b, "\tset __btawg_marker { type ipv4_addr; comment \"btawg hub %s\"; }\n", h.name)
	fmt.Fprintln(&b, "\tchain forward {")
	fmt.Fprintln(&b, "\t\ttype filter hook forward priority 0; policy accept;")
	fmt.Fprintf(&b, "\t\tiifname %q oifname %q ct state established,related counter accept\n", wan, h.name)
	for _, g := range gateways {
		fmt.Fprintf(&b, "\t\tiifname %q ip saddr %s oifname %q counter accept\n", h.name, g, wan)
	}
	fmt.Fprintf(&b, "\t\tiifname %q counter drop\n", h.name)
	fmt.Fprintf(&b, "\t\toifname %q counter drop\n", h.name)
	fmt.Fprintln(&b, "\t}")
	fmt.Fprintln(&b, "\tchain input {")
	fmt.Fprintln(&b, "\t\ttype filter hook input priority 0; policy accept;")
	for _, g := range gateways {
		fmt.Fprintf(&b, "\t\tiifname %q ip daddr %s counter accept\n", h.name, g.Addr())
	}
	fmt.Fprintf(&b, "\t\tiifname %q counter drop\n", h.name)
	fmt.Fprintln(&b, "\t}")
	if len(gateways) > 0 {
		fmt.Fprintln(&b, "\tchain postrouting {")
		fmt.Fprintln(&b, "\t\ttype nat hook postrouting priority 100; policy accept;")
		for _, g := range gateways {
			fmt.Fprintf(&b, "\t\tiifname %q ip saddr %s oifname %q counter masquerade\n",
				h.name, g, wan)
		}
		fmt.Fprintln(&b, "\t}")
	}
	fmt.Fprintln(&b, "}")
	return b.String()
}

func nftTableExists(table string) (bool, error) {
	err := exec.Command("nft", "list", "table", "ip", table).Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("nft list table %s: %w", table, err)
}

// nftTableManaged reports whether the table exists and was created by this
// package for the named interface.  It looks for the private marker set and
// its comment.
func nftTableManaged(table, ifname string) (bool, error) {
	out, err := exec.Command("nft", "list", "table", "ip", table).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("nft list table %s: %w", table, err)
	}
	s := string(out)
	return strings.Contains(s, "__btawg_marker") && strings.Contains(s, fmt.Sprintf("btawg hub %s", ifname)), nil
}

func nftApply(script string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stdout = os.Stderr
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nft apply: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func sanitizeID(s string) string {
	return regexp.MustCompile(`[^a-zA-Z0-9_]+`).ReplaceAllString(s, "_")
}

// prefixDiff returns all prefixes in a that are not in b.
func prefixDiff(a, b []netip.Prefix) []netip.Prefix {
	present := make(map[string]struct{}, len(b))
	for _, p := range b {
		present[p.String()] = struct{}{}
	}
	var out []netip.Prefix
	for _, p := range a {
		if _, ok := present[p.String()]; !ok {
			out = append(out, p)
		}
	}
	return out
}

// notExists reports whether an ip(8) or nft(8) error is clearly a "does not
// exist" condition, so Close can ignore it when the caller already removed
// the interface or table.
func notExists(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "cannot find device") ||
		strings.Contains(msg, "no such device") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "table does not exist") ||
		strings.Contains(msg, "cannot assign requested address")
}
