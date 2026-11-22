package portmap

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"unsafe"

	"bittorrent-tunnel/btawg/internal/tunsetup"

	"golang.org/x/sys/windows"
)

func defaultRouter(ctx context.Context) (router, error) {
	var table *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_INET, &table); err != nil {
		return router{}, fmt.Errorf("read Windows IPv4 routes: %w", err)
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	var routes []gatewayRoute
	for _, row := range table.Rows() {
		if row.DestinationPrefix.PrefixLength != 0 || row.NextHop.Family != windows.AF_INET || row.Loopback != 0 {
			continue
		}
		iface := windows.MibIfRow2{InterfaceLuid: row.InterfaceLuid}
		if err := windows.GetIfEntry2Ex(windows.MibIfTableNormalWithoutStatistics, &iface); err != nil {
			if errors.Is(err, windows.ERROR_NOT_FOUND) {
				continue
			}
			return router{}, fmt.Errorf("read gateway interface %d: %w", row.InterfaceIndex, err)
		}
		if iface.OperStatus != windows.IfOperStatusUp || iface.Type == windows.IF_TYPE_TUNNEL || iface.Type == windows.IF_TYPE_SOFTWARE_LOOPBACK || tunsetup.IsOwnedUnderlayAdapter(row.InterfaceLuid) {
			continue
		}
		ipif := windows.MibIpInterfaceRow{Family: windows.AF_INET, InterfaceLuid: row.InterfaceLuid}
		if err := windows.GetIpInterfaceEntry(&ipif); err != nil {
			return router{}, fmt.Errorf("read gateway interface metric: %w", err)
		}
		next := (*windows.RawSockaddrInet4)(unsafe.Pointer(&row.NextHop))
		routes = append(routes, gatewayRoute{index: int(row.InterfaceIndex), luid: row.InterfaceLuid, via: netip.AddrFrom4(next.Addr), routeMetric: row.Metric, interfaceMetric: ipif.Metric})
	}
	selected, ok := pickGateway(routes)
	if !ok {
		return router{}, errors.New("no default Windows IPv4 gateway")
	}
	r := router{gateway: selected.via, ifindex: selected.index, luid: selected.luid, natPort: 5351, ssdp: netip.MustParseAddrPort("239.255.255.250:1900")}
	var index [4]byte
	binary.BigEndian.PutUint32(index[:], uint32(r.ifindex))
	dialer := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		var sockErr error
		if err := c.Control(func(fd uintptr) {
			const ipUnicastIF = 31
			sockErr = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, ipUnicastIF, int(binary.NativeEndian.Uint32(index[:])))
		}); err != nil {
			return err
		}
		return sockErr
	}}
	conn, err := dialer.DialContext(ctx, "udp4", netip.AddrPortFrom(r.gateway, r.natPort).String())
	if err != nil {
		return router{}, fmt.Errorf("select IPv4 source for default gateway: %w", err)
	}
	r.source = conn.LocalAddr().(*net.UDPAddr).AddrPort().Addr()
	_ = conn.Close()
	iface, err := net.InterfaceByIndex(r.ifindex)
	if err != nil {
		return router{}, err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return router{}, err
	}
	for _, addr := range addrs {
		p, err := netip.ParsePrefix(addr.String())
		if err == nil && p.Addr() == r.source && r.source.IsGlobalUnicast() && r.validateSnapshot() == nil {
			return r, nil
		}
	}
	return router{}, fmt.Errorf("no IPv4 source on default route interface %s for gateway %s", iface.Name, r.gateway)
}

func gatewaySnapshotMatches(r router, luid uint64, index uint32, source netip.Addr, up bool) bool {
	return up && r.luid == luid && uint32(r.ifindex) == index && r.source == source && !tunsetup.IsOwnedUnderlayAdapter(luid)
}

func (r router) validateSnapshot() error {
	if r.luid == 0 {
		return nil
	}
	row := windows.MibIfRow2{InterfaceLuid: r.luid}
	if err := windows.GetIfEntry2Ex(windows.MibIfTableNormalWithoutStatistics, &row); err != nil {
		return err
	}
	iface, err := net.InterfaceByIndex(int(row.InterfaceIndex))
	if err != nil {
		return err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return err
	}
	for _, addr := range addrs {
		p, err := netip.ParsePrefix(addr.String())
		if err == nil && gatewaySnapshotMatches(r, row.InterfaceLuid, row.InterfaceIndex, p.Addr().Unmap(), row.OperStatus == windows.IfOperStatusUp) {
			return nil
		}
	}
	return errors.New("gateway interface or source changed")
}

func (r router) socketControl(_, _ string, raw syscall.RawConn) error {
	if err := r.validateSnapshot(); err != nil {
		return err
	}
	if r.luid == 0 {
		return nil
	}
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		sockErr = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, 31, tunsetup.UnderlaySocketIndex(uint32(r.ifindex)))
	}); err != nil {
		return err
	}
	return sockErr
}
