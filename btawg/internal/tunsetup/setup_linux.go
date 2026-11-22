// Package tunsetup configures a kernel TUN interface for btawg nodes. It is
// shared between the client (address + subnet route, no ip_forward) and the
// hub (address only, ip_forward enabled so the kernel forwards peer traffic).
package tunsetup

import (
	"bytes"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"strings"
)

// Setup brings up the named TUN interface with the overlay address, optional
// subnet route, link MTU, and (for the hub) IPv4 forwarding. The address is
// given as a prefix so the interface mask is set correctly; route is nil for
// the hub because it owns the prefix directly. A route whose prefix equals
// the address prefix is skipped: assigning the address already installs the
// connected route, and an explicit "ip route add" for it fails with
// "File exists". Repeated calls are idempotent — a leftover device or
// address from a previous run must not turn into an error.
func Setup(name string, addr netip.Prefix, route *netip.Prefix, mtu int, ipForward bool) error {
	if !deviceExists(name) {
		if err := run("ip", "tuntap", "add", "dev", name, "mode", "tun"); err != nil {
			return err
		}
	}
	if err := setupLink(name, addr, mtu); err != nil {
		return err
	}
	if route != nil && route.Masked() != addr.Masked() {
		if err := run("ip", "route", "add", route.String(), "dev", name); err != nil {
			return err
		}
	}
	if ipForward {
		if err := run("sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
			return err
		}
	}
	return nil
}

func setupLink(name string, addr netip.Prefix, mtu int) error {
	if !hasAddr(name, addr) {
		if err := run("ip", "addr", "add", addr.String(), "dev", name); err != nil {
			return err
		}
	}
	// A larger queue absorbs bursts while the carrier is backpressured.
	return run("ip", "link", "set", "dev", name, "mtu", fmt.Sprintf("%d", mtu), "txqueuelen", "8192", "up")
}

// deviceExists reports whether the named link is present. Best-effort and
// quiet: only used to decide whether creation is needed.
func deviceExists(name string) bool {
	return exec.Command("ip", "link", "show", "dev", name).Run() == nil
}

// hasAddr reports whether the interface already carries the exact prefix.
// Best-effort and quiet: only used to decide whether assignment is needed.
func hasAddr(name string, addr netip.Prefix) bool {
	out, err := exec.Command("ip", "addr", "show", "dev", name).Output()
	return err == nil && strings.Contains(string(out), addr.String())
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stdout = os.Stderr
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
