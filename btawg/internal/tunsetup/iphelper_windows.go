package tunsetup

import (
	"fmt"
	"net/netip"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ipHelper                  = windows.NewLazySystemDLL("iphlpapi.dll")
	setIPInterfaceEntry       = ipHelper.NewProc("SetIpInterfaceEntry")
	initUnicastAddressEntry   = ipHelper.NewProc("InitializeUnicastIpAddressEntry")
	createUnicastAddressEntry = ipHelper.NewProc("CreateUnicastIpAddressEntry")
	initIPForwardEntry        = ipHelper.NewProc("InitializeIpForwardEntry")
	createIPForwardEntry      = ipHelper.NewProc("CreateIpForwardEntry2")
)

func findWindowsAdapter(name string) (windows.MibIfRow2, error) {
	var table *windows.MibIfTable2
	if err := windows.GetIfTable2Ex(windows.MibIfTableNormalWithoutStatistics, &table); err != nil {
		return windows.MibIfRow2{}, err
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	rows := unsafe.Slice(&table.Table[0], table.NumEntries)
	for i := range rows {
		row := &rows[i]
		if strings.EqualFold(windows.UTF16ToString(row.Alias[:]), name) {
			return *row, nil
		}
	}
	return windows.MibIfRow2{}, nil
}

func rejectWindowsRouteOverlap(owned uint64, addr netip.Prefix, routes []netip.Prefix) error {
	var table *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_INET, &table); err != nil {
		return fmt.Errorf("inspect IPv4 routes: %w", err)
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	for _, row := range table.Rows() {
		dst := &row.DestinationPrefix
		if dst.Prefix.Family != windows.AF_INET || dst.PrefixLength == 0 || (owned != 0 && row.InterfaceLuid == owned) {
			continue
		}
		ip := (*windows.RawSockaddrInet4)(unsafe.Pointer(&dst.Prefix))
		prefix := netip.PrefixFrom(netip.AddrFrom4(ip.Addr), int(dst.PrefixLength)).Masked()
		conflict := addr.Masked()
		if !prefix.Overlaps(conflict) {
			conflict = netip.Prefix{}
			for _, route := range routes {
				if prefix.Overlaps(route) {
					conflict = route
					break
				}
			}
		}
		if conflict.IsValid() {
			iface := windows.MibIfRow2{InterfaceLuid: row.InterfaceLuid}
			name := fmt.Sprintf("interface %d", row.InterfaceIndex)
			if err := windows.GetIfEntry2Ex(windows.MibIfTableNormalWithoutStatistics, &iface); err == nil {
				name = windows.UTF16ToString(iface.Alias[:])
			}
			return fmt.Errorf("VPN prefix %s overlaps route %s on %s", conflict, prefix, name)
		}
	}
	return nil
}

func configureWindowsTUN(luid uint64, addr netip.Prefix, routes []netip.Prefix, mtu int) error {
	iface := windows.MibIpInterfaceRow{Family: windows.AF_INET, InterfaceLuid: luid}
	if err := windows.GetIpInterfaceEntry(&iface); err != nil {
		return fmt.Errorf("read IPv4 interface: %w", err)
	}
	iface.NlMtu = uint32(mtu)
	iface.RouterDiscoveryBehavior = 0 // Disabled. The overlay has no router advertisements.
	iface.DadTransmits = 0            // Address ownership is assigned by the hub.
	// IPv4 requires zero even when GetIpInterfaceEntry returned a value.
	iface.SitePrefixLength = 0
	if ret, _, _ := setIPInterfaceEntry.Call(uintptr(unsafe.Pointer(&iface))); ret != 0 {
		return fmt.Errorf("configure IPv4 interface: %w", syscall.Errno(ret))
	}

	var unicast windows.MibUnicastIpAddressRow
	initUnicastAddressEntry.Call(uintptr(unsafe.Pointer(&unicast)))
	*(*windows.RawSockaddrInet4)(unsafe.Pointer(&unicast.Address)) = windows.RawSockaddrInet4{Family: windows.AF_INET, Addr: addr.Addr().As4()}
	unicast.InterfaceLuid = luid
	unicast.OnLinkPrefixLength = uint8(addr.Bits())
	if ret, _, _ := createUnicastAddressEntry.Call(uintptr(unsafe.Pointer(&unicast))); ret != 0 {
		return fmt.Errorf("assign VPN address %s: %w", addr, syscall.Errno(ret))
	}

	for _, prefix := range routes {
		var row windows.MibIpForwardRow2
		initIPForwardEntry.Call(uintptr(unsafe.Pointer(&row)))
		row.InterfaceLuid = luid
		row.SitePrefixLength = 0
		row.Metric = 0
		row.Protocol = windows.MIB_IPPROTO_NETMGMT
		row.Loopback, row.AutoconfigureAddress, row.Publish = 0, 0, 0
		row.DestinationPrefix.PrefixLength = uint8(prefix.Bits())
		*(*windows.RawSockaddrInet4)(unsafe.Pointer(&row.DestinationPrefix.Prefix)) = windows.RawSockaddrInet4{Family: windows.AF_INET, Addr: prefix.Addr().As4()}
		row.NextHop.Family = windows.AF_INET // On-link, not a gateway.
		if ret, _, _ := createIPForwardEntry.Call(uintptr(unsafe.Pointer(&row))); ret != 0 {
			return fmt.Errorf("add VPN route %s: %w", prefix, syscall.Errno(ret))
		}
	}
	return nil
}
