package tunsetup

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var ownedUnderlayAdapters = struct {
	sync.RWMutex
	luids map[uint64]bool
}{luids: make(map[uint64]bool)}

func registerUnderlayAdapter(luid uint64, add bool) {
	ownedUnderlayAdapters.Lock()
	defer ownedUnderlayAdapters.Unlock()
	if add {
		ownedUnderlayAdapters.luids[luid] = true
	} else {
		delete(ownedUnderlayAdapters.luids, luid)
	}
}

func IsOwnedUnderlayAdapter(luid uint64) bool {
	ownedUnderlayAdapters.RLock()
	defer ownedUnderlayAdapters.RUnlock()
	return ownedUnderlayAdapters.luids[luid]
}

func IsUnderlayInterface(index int) bool {
	row := windows.MibIfRow2{InterfaceIndex: uint32(index)}
	return windows.GetIfEntry2Ex(windows.MibIfTableNormalWithoutStatistics, &row) == nil && !IsOwnedUnderlayAdapter(row.InterfaceLuid)
}

type underlayStream struct {
	net.Conn
	destination netip.Addr
	index       uint32
	luid        uint64
}

func (c *underlayStream) Write(p []byte) (int, error) {
	index, err := UnderlayInterface(c.destination)
	if err != nil {
		return 0, err
	}
	if index != c.index {
		return 0, errors.New("underlay route changed")
	}
	row := windows.MibIfRow2{InterfaceIndex: index}
	if err := windows.GetIfEntry2Ex(windows.MibIfTableNormalWithoutStatistics, &row); err != nil {
		return 0, err
	}
	if row.InterfaceLuid != c.luid || IsOwnedUnderlayAdapter(row.InterfaceLuid) {
		return 0, errors.New("underlay interface changed")
	}
	return c.Conn.Write(p)
}

func ProtectUnderlayStream(c net.Conn, destination netip.Addr) (net.Conn, error) {
	index, err := UnderlayInterface(destination)
	row := windows.MibIfRow2{InterfaceIndex: index}
	if err := windows.GetIfEntry2Ex(windows.MibIfTableNormalWithoutStatistics, &row); err != nil || IsOwnedUnderlayAdapter(row.InterfaceLuid) {
		_ = c.Close()
		return nil, errors.New("underlay interface unavailable")
	}
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	socket, ok := c.(syscall.Conn)
	if !ok {
		_ = c.Close()
		return nil, errors.New("underlay stream has no socket")
	}
	raw, err := socket.SyscallConn()
	if err == nil {
		err = bindUnderlayInterface(raw, index, destination.Unmap().Is6())
	}
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	return &underlayStream{Conn: c, destination: destination, index: index, luid: row.InterfaceLuid}, nil
}

type underlayRoute struct {
	prefix netip.Prefix
	index  uint32
	luid   uint64
	metric uint64
}

func betterUnderlayRoute(route, best underlayRoute, dst netip.Addr, excluded map[uint64]bool, found bool) bool {
	if excluded[route.luid] || route.index == 0 || !route.prefix.Contains(dst) {
		return false
	}
	return !found || route.prefix.Bits() > best.prefix.Bits() || route.prefix.Bits() == best.prefix.Bits() && route.metric < best.metric
}

func selectUnderlayRoute(routes []underlayRoute, dst netip.Addr, excluded map[uint64]bool) (underlayRoute, bool) {
	var best underlayRoute
	found := false
	for _, route := range routes {
		if betterUnderlayRoute(route, best, dst, excluded, found) {
			best, found = route, true
		}
	}
	return best, found
}

// UnderlayInterface chooses an eligible route on every use, excluding all owned
// Wintun adapters by LUID even if an adapter was renamed or routing changed.
func UnderlayInterface(dst netip.Addr) (uint32, error) {
	dst = dst.Unmap()
	if dst.Is6() {
		var index uint32
		sa := &windows.SockaddrInet6{Addr: dst.As16()}
		if err := windows.GetBestInterfaceEx(sa, &index); err != nil {
			return 0, err
		}
		row := windows.MibIfRow2{InterfaceIndex: index}
		if err := windows.GetIfEntry2Ex(windows.MibIfTableNormalWithoutStatistics, &row); err != nil {
			return 0, err
		}
		if row.OperStatus != windows.IfOperStatusUp || IsOwnedUnderlayAdapter(row.InterfaceLuid) {
			return 0, fmt.Errorf("no non-tunnel IPv6 route to %s", dst)
		}
		return index, nil
	}
	if !dst.Is4() {
		return 0, errors.New("invalid underlay address")
	}
	var bestIndex uint32
	sa := &windows.SockaddrInet4{Addr: dst.As4()}
	if err := windows.GetBestInterfaceEx(sa, &bestIndex); err == nil {
		row := windows.MibIfRow2{InterfaceIndex: bestIndex}
		if windows.GetIfEntry2Ex(windows.MibIfTableNormalWithoutStatistics, &row) == nil && row.OperStatus == windows.IfOperStatusUp && !IsOwnedUnderlayAdapter(row.InterfaceLuid) {
			return bestIndex, nil
		}
	}
	var table *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_INET, &table); err != nil {
		return 0, err
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	ownedUnderlayAdapters.RLock()
	defer ownedUnderlayAdapters.RUnlock()
	var route underlayRoute
	found := false
	for _, row := range table.Rows() {
		if row.DestinationPrefix.Prefix.Family != windows.AF_INET || ownedUnderlayAdapters.luids[row.InterfaceLuid] {
			continue
		}
		iface := windows.MibIfRow2{InterfaceLuid: row.InterfaceLuid}
		if windows.GetIfEntry2Ex(windows.MibIfTableNormalWithoutStatistics, &iface) != nil || iface.OperStatus != windows.IfOperStatusUp {
			continue
		}
		ipif := windows.MibIpInterfaceRow{Family: windows.AF_INET, InterfaceLuid: row.InterfaceLuid}
		if windows.GetIpInterfaceEntry(&ipif) != nil {
			continue
		}
		raw := (*windows.RawSockaddrInet4)(unsafe.Pointer(&row.DestinationPrefix.Prefix))
		p := netip.PrefixFrom(netip.AddrFrom4(raw.Addr), int(row.DestinationPrefix.PrefixLength)).Masked()
		candidate := underlayRoute{prefix: p, index: row.InterfaceIndex, luid: row.InterfaceLuid, metric: uint64(row.Metric) + uint64(ipif.Metric)}
		if betterUnderlayRoute(candidate, route, dst, ownedUnderlayAdapters.luids, found) {
			route, found = candidate, true
		}
	}
	if !found {
		return 0, fmt.Errorf("no non-tunnel IPv4 route to %s", dst)
	}
	return route.index, nil
}

// UnderlayControl confines a TCP connection to the route selected for its peer.
func UnderlayControl(_, address string, raw syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	index, err := UnderlayInterface(ip)
	if err != nil {
		return err
	}
	return bindUnderlayInterface(raw, index, ip.Is6())
}

// UnderlayPacketConn serializes interface selection with each datagram write.
type UnderlayPacketConn struct {
	*net.UDPConn
	mu sync.Mutex
}

func ProtectUnderlayPacket(conn *net.UDPConn) net.PacketConn {
	return &UnderlayPacketConn{UDPConn: conn}
}
func (c *UnderlayPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	u, ok := addr.(*net.UDPAddr)
	if !ok {
		return 0, errors.New("invalid UDP underlay destination")
	}
	ip := u.AddrPort().Addr().Unmap()
	c.mu.Lock()
	defer c.mu.Unlock()
	index, err := UnderlayInterface(ip)
	if err != nil {
		return 0, err
	}
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	if err := bindUnderlayInterface(raw, index, ip.Is6()); err != nil {
		return 0, err
	}
	return c.UDPConn.WriteTo(b, addr)
}

func bindUnderlayInterface(raw syscall.RawConn, index uint32, ipv6 bool) error {
	if !ipv6 {
		return bindWindowsInterface(raw, index)
	}
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		sockErr = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IPV6, 31, int(index))
	}); err != nil {
		return err
	}
	return sockErr
}

// UnderlaySocketIndex encodes an IPv4 interface index in Winsock byte order.
func UnderlaySocketIndex(index uint32) int {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], index)
	return int(binary.NativeEndian.Uint32(b[:]))
}
