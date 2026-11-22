package tunsetup

import (
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"net/netip"
)

// TestSetupNonRootFails asserts that, without root, Setup cannot configure an
// interface. When CAP_NET_ADMIN is absent the underlying ip(8) calls fail and
// the error names the requested interface.
func TestSetupNonRootFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("non-root test: root would succeed")
	}
	addr := netip.MustParsePrefix("10.99.0.1/24")
	route := netip.MustParsePrefix("10.99.0.0/24")
	err := Setup("btawgroot0", addr, &route, 1420, false)
	if err == nil {
		t.Fatal("expected Setup to fail without root")
	}
	if !strings.Contains(err.Error(), "btawgroot0") {
		t.Fatalf("error %q does not name the interface", err)
	}
}

// TestSetupRequiresRoot performs live verification of interface setup when
// running as root. It creates a throw-away TUN, assigns the address and route,
// then tears it down.
func TestSetupRequiresRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root required for live TUN setup")
	}
	name := "tunsetuptest0"
	addr := netip.MustParsePrefix("10.99.0.1/24")
	route := netip.MustParsePrefix("10.99.0.0/24")
	defer func() {
		_ = exec.Command("ip", "link", "set", "dev", name, "down").Run()
		_ = exec.Command("ip", "tuntap", "del", "dev", name, "mode", "tun").Run()
	}()

	ipFwdPath := "/proc/sys/net/ipv4/ip_forward"
	old, _ := os.ReadFile(ipFwdPath)
	defer func() {
		if old != nil {
			_ = os.WriteFile(ipFwdPath, old, 0o644)
		}
	}()

	if err := Setup(name, addr, &route, 1420, true); err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	// Re-running with the device, address, and connected route already in
	// place must succeed (leftover state after a crashed run).
	if err := Setup(name, addr, &route, 1420, true); err != nil {
		t.Fatalf("second Setup failed: %v", err)
	}

	out, err := exec.Command("ip", "addr", "show", "dev", name).Output()
	if err != nil {
		t.Fatalf("ip addr show: %v", err)
	}
	if !strings.Contains(string(out), "10.99.0.1/24") {
		t.Fatalf("interface address missing from ip output:\n%s", out)
	}

	link, err := exec.Command("ip", "-j", "link", "show", "dev", name).Output()
	if err != nil {
		t.Fatalf("ip link show: %v", err)
	}
	var links []struct {
		QueueLength int `json:"txqlen"`
	}
	if err := json.Unmarshal(link, &links); err != nil {
		t.Fatalf("decode ip link output: %v", err)
	}
	if len(links) != 1 || links[0].QueueLength < 2048 {
		t.Fatalf("TUN queue too small to absorb bursts: %s", link)
	}

	routes, err := exec.Command("ip", "route", "show", "dev", name).Output()
	if err != nil {
		t.Fatalf("ip route show: %v", err)
	}
	if !strings.Contains(string(routes), "10.99.0.0/24") {
		t.Fatalf("subnet route missing from ip output:\n%s", routes)
	}

	fwd, err := os.ReadFile(ipFwdPath)
	if err != nil {
		t.Fatalf("read ip_forward: %v", err)
	}
	v := strings.TrimSpace(string(fwd))
	if i, _ := strconv.Atoi(v); i != 1 {
		t.Fatalf("ip_forward = %s, want 1", v)
	}
}
