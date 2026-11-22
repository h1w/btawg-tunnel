package tunsetup

import (
	"bytes"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsClientTUNLifecycle(t *testing.T) {
	windowsTUNFixture(t)
	name := fmt.Sprintf("btawg-test-%d", os.Getpid())
	addr := netip.MustParsePrefix("10.77.0.2/24")
	extra := netip.MustParsePrefix("10.78.0.0/24")
	defaults := defaultRoutes(t)
	dev, err := OpenClientTUN(name, addr, []string{extra.String()}, 1280)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dev.Close() })
	iface, err := net.InterfaceByName(name)
	if err != nil {
		t.Fatal(err)
	}
	ipif := windows.MibIpInterfaceRow{Family: windows.AF_INET, InterfaceIndex: uint32(iface.Index)}
	if err := windows.GetIpInterfaceEntry(&ipif); err != nil {
		t.Fatal(err)
	}
	if ipif.NlMtu != 1280 {
		t.Fatalf("IPv4 MTU = %d, want 1280", ipif.NlMtu)
	}
	if routes := routesOn(t, iface.Index); !slices.Contains(routes, addr.Masked()) || !slices.Contains(routes, extra) {
		t.Fatalf("missing configured routes on %s: %v", name, routes)
	}
	if got := defaultRoutes(t); !slices.Equal(got, defaults) {
		t.Fatalf("default routes changed: %v -> %v", defaults, got)
	}

	// A normal kernel UDP socket, not a tun.Device.Write, emits this packet.
	target := netip.MustParseAddrPort("10.78.0.9:15337")
	c, err := net.DialUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(addr.Addr(), 0)), net.UDPAddrFromAddrPort(target))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	payload := []byte("btawg wintun outbound")
	if _, err := c.Write(payload); err != nil {
		t.Fatal(err)
	}
	seen := make(chan error, 1)
	go func() {
		bufs := [][]byte{make([]byte, 1500)}
		sizes := []int{0}
		for {
			if _, err := dev.Read(bufs, sizes, 0); err != nil {
				seen <- err
				return
			}
			b := bufs[0][:sizes[0]]
			if len(b) >= 28 && b[0] == 0x45 && b[9] == 17 &&
				netip.AddrFrom4([4]byte(b[12:16])) == addr.Addr() &&
				netip.AddrFrom4([4]byte(b[16:20])) == target.Addr() && bytes.Equal(b[28:], payload) {
				seen <- nil
				return
			}
		}
	}()
	select {
	case err := <-seen:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("kernel UDP packet did not reach Wintun")
	}
	if err := dev.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := net.InterfaceByName(name); err == nil {
		t.Fatalf("adapter %s still exists after close", name)
	}
	if routes := routesOn(t, iface.Index); len(routes) != 0 {
		t.Fatalf("routes from closed adapter remain: %v", routes)
	}
	if got := defaultRoutes(t); !slices.Equal(got, defaults) {
		t.Fatalf("default routes changed after close: %v -> %v", defaults, got)
	}
}

func TestOwnedAdapterIndexSurvivesRenameAndRejectsRemoval(t *testing.T) {
	windowsTUNFixture(t)
	name := fmt.Sprintf("btawg-id-%d", os.Getpid())
	dev, err := OpenClientTUN(name, netip.MustParsePrefix("10.179.0.2/24"), nil, 1280)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dev.Close() })
	owned := dev.(*windowsTUN)
	index, err := owned.interfaceIndex()
	if err != nil {
		t.Fatal(err)
	}
	if err := renameWindowsAdapter(owned.adapter.LUID(), name+"-renamed"); err != nil {
		t.Fatal(err)
	}
	if got, err := owned.interfaceIndex(); err != nil || got != index {
		t.Fatalf("renamed adapter index = %d, %v; want %d", got, err, index)
	}
	if err := dev.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := owned.interfaceIndex(); err == nil {
		t.Fatal("removed adapter still accepted")
	}
}

func TestWindowsClientTUNRejectsForeignState(t *testing.T) {
	windowsTUNFixture(t)
	name := fmt.Sprintf("btawg-foreign-%d", os.Getpid())
	other := name + "-other"
	foreign, err := OpenClientTUN(name, netip.MustParsePrefix("10.76.0.2/14"), nil, 1280)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = foreign.Close() })
	iface, err := net.InterfaceByName(name)
	if err != nil {
		t.Fatal(err)
	}
	before := routesOn(t, iface.Index)
	addr := netip.MustParsePrefix("10.77.0.2/24")
	for _, attempt := range []struct{ name, detail string }{{name, name}, {other, "10.76.0.0/14"}} {
		dev, err := OpenClientTUN(attempt.name, addr, nil, 1280)
		if err == nil {
			dev.Close()
			t.Fatalf("OpenClientTUN(%s) accepted conflicting state", attempt.name)
		}
		if !strings.Contains(err.Error(), attempt.detail) || !strings.Contains(err.Error(), name) {
			t.Fatalf("conflict does not identify existing route/interface: %v", err)
		}
	}
	if got := routesOn(t, iface.Index); !slices.Equal(got, before) {
		t.Fatalf("foreign routes changed: %v -> %v", before, got)
	}
	if _, err := net.InterfaceByName(other); err == nil {
		t.Fatalf("conflicting setup left adapter %s", other)
	}
}

func windowsTUNFixture(t *testing.T) {
	t.Helper()
	if os.Getenv("BTAWG_WINDOWS_NETWORK_TESTS") != "1" {
		t.Skip("requires an elevated isolated Windows fixture with signed wintun.dll and unused test networks")
	}
}

func routesOn(t *testing.T, index int) []netip.Prefix {
	t.Helper()
	var table *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_INET, &table); err != nil {
		t.Fatal(err)
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	var routes []netip.Prefix
	for _, row := range table.Rows() {
		if int(row.InterfaceIndex) != index {
			continue
		}
		dst := (*windows.RawSockaddrInet4)(unsafe.Pointer(&row.DestinationPrefix.Prefix))
		routes = append(routes, netip.PrefixFrom(netip.AddrFrom4(dst.Addr), int(row.DestinationPrefix.PrefixLength)))
	}
	slices.SortFunc(routes, func(a, b netip.Prefix) int { return a.Compare(b) })
	return routes
}

type defaultRouteSnapshot struct {
	index  uint32
	via    [4]byte
	metric uint32
}

func defaultRoutes(t *testing.T) []defaultRouteSnapshot {
	t.Helper()
	var table *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_INET, &table); err != nil {
		t.Fatal(err)
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	var routes []defaultRouteSnapshot
	for _, row := range table.Rows() {
		if row.DestinationPrefix.PrefixLength == 0 {
			next := (*windows.RawSockaddrInet4)(unsafe.Pointer(&row.NextHop))
			routes = append(routes, defaultRouteSnapshot{row.InterfaceIndex, next.Addr, row.Metric})
		}
	}
	slices.SortFunc(routes, func(a, b defaultRouteSnapshot) int {
		if a.index != b.index {
			if a.index < b.index {
				return -1
			}
			return 1
		}
		if a.metric != b.metric {
			if a.metric < b.metric {
				return -1
			}
			return 1
		}
		return bytes.Compare(a.via[:], b.via[:])
	})
	return routes
}
