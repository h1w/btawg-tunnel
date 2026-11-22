package tunsetup

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var ownedUnderlayAdapters = struct {
	sync.RWMutex
	luids map[uint64]bool
}{luids: make(map[uint64]bool)}

var underlayEpoch atomic.Uint64
var underlayNotifyOnce sync.Once
var underlayNotificationsOK bool
var underlayRoutes = struct {
	sync.Mutex
	epoch   uint64
	entries map[netip.Addr]cachedUnderlayRoute
}{entries: make(map[netip.Addr]cachedUnderlayRoute)}

type cachedUnderlayRoute struct {
	index uint32
	luid  uint64
}

func underlayNotification(_, _, _ uintptr) uintptr {
	underlayEpoch.Add(1)
	return 0
}

func startUnderlayNotifications() {
	underlayNotifyOnce.Do(func() {
		callback := windows.NewCallback(underlayNotification)
		var route, iface, address windows.Handle
		if windows.NotifyRouteChange2(windows.AF_UNSPEC, callback, nil, false, &route) != nil {
			return
		}
		if windows.NotifyIpInterfaceChange(windows.AF_UNSPEC, callback, nil, false, &iface) != nil {
			_ = windows.CancelMibChangeNotify2(route)
			return
		}
		if windows.NotifyUnicastIpAddressChange(windows.AF_UNSPEC, callback, nil, false, &address) != nil {
			_ = windows.CancelMibChangeNotify2(iface)
			_ = windows.CancelMibChangeNotify2(route)
			return
		}
		underlayNotificationsOK = true
	})
}

// UnderlayRouteIdentity returns a route's index and stable adapter identity.
// Failed notification registration deliberately disables caching.
func UnderlayRouteIdentity(dst netip.Addr) (uint32, uint64, error) {
	dst = dst.Unmap()
	startUnderlayNotifications()
	for {
		epoch := underlayEpoch.Load()
		if underlayNotificationsOK {
			underlayRoutes.Lock()
			entry, ok := underlayRoutes.entries[dst]
			valid := underlayRoutes.epoch == epoch && ok
			underlayRoutes.Unlock()
			if valid && epoch == underlayEpoch.Load() {
				return entry.index, entry.luid, nil
			}
		}
		index, luid, err := lookupUnderlayInterface(dst)
		if err != nil {
			if epoch != underlayEpoch.Load() {
				continue
			}
			return 0, 0, err
		}
		if epoch != underlayEpoch.Load() {
			continue
		}
		if underlayNotificationsOK && !publishUnderlayRoute(dst, epoch, cachedUnderlayRoute{index, luid}) {
			continue
		}
		if epoch != underlayEpoch.Load() {
			continue
		}
		return index, luid, nil
	}
}

func publishUnderlayRoute(dst netip.Addr, epoch uint64, route cachedUnderlayRoute) bool {
	underlayRoutes.Lock()
	defer underlayRoutes.Unlock()
	if epoch != underlayEpoch.Load() {
		return false
	}
	if underlayRoutes.epoch != epoch || len(underlayRoutes.entries) >= 4096 {
		clear(underlayRoutes.entries)
		underlayRoutes.epoch = epoch
	}
	underlayRoutes.entries[dst] = route
	return true
}

func registerUnderlayAdapter(luid uint64, add bool) {
	ownedUnderlayAdapters.Lock()
	defer ownedUnderlayAdapters.Unlock()
	if add {
		ownedUnderlayAdapters.luids[luid] = true
	} else {
		delete(ownedUnderlayAdapters.luids, luid)
	}
	underlayEpoch.Add(1)
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
	index, luid, err := UnderlayRouteIdentity(c.destination)
	if err != nil || index != c.index || luid != c.luid {
		_ = c.Conn.Close()
		if err != nil {
			return 0, err
		}
		return 0, errors.New("underlay interface changed")
	}
	return c.Conn.Write(p)
}

// UnderlayDialGuard retains the adapter identity selected before TCP connect.
// A guard belongs to one dial attempt.
type UnderlayDialGuard struct {
	index uint32
	luid  uint64
}

func (g *UnderlayDialGuard) Control(_, address string, raw syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	index, luid, err := UnderlayRouteIdentity(ip)
	if err != nil {
		return err
	}
	if err := bindUnderlayInterface(raw, index, ip.Unmap().Is6()); err != nil {
		return err
	}
	g.index, g.luid = index, luid
	return nil
}

func (g *UnderlayDialGuard) Protect(c net.Conn, destination netip.Addr) (net.Conn, error) {
	protected, err := ProtectUnderlayStream(c, destination)
	if err != nil {
		return nil, err
	}
	stream := protected.(*underlayStream)
	if g.index == 0 || stream.index != g.index || stream.luid != g.luid {
		_ = protected.Close()
		return nil, errors.New("underlay adapter changed during connect")
	}
	return protected, nil
}

func ProtectUnderlayStream(c net.Conn, destination netip.Addr) (net.Conn, error) {
	index, luid, err := UnderlayRouteIdentity(destination)
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
		var bound int
		var sockErr error
		ipv6 := destination.Unmap().Is6()
		level := windows.IPPROTO_IP
		if ipv6 {
			level = windows.IPPROTO_IPV6
		}
		err = raw.Control(func(fd uintptr) { bound, sockErr = windows.GetsockoptInt(windows.Handle(fd), level, 31) })
		if err == nil {
			err = sockErr
		}
		if err == nil && bound != 0 {
			if bound != int(index) {
				err = errors.New("underlay route changed during connect")
			}
		}
		if err == nil {
			err = bindUnderlayInterface(raw, index, ipv6)
		}
	}
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	return &underlayStream{Conn: c, destination: destination, index: index, luid: luid}, nil
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

// UnderlayInterface selects a non-owned adapter, revalidated on network changes.
func UnderlayInterface(dst netip.Addr) (uint32, error) {
	index, _, err := UnderlayRouteIdentity(dst)
	return index, err
}

func lookupUnderlayInterface(dst netip.Addr) (uint32, uint64, error) {
	dst = dst.Unmap()
	if dst.Is6() {
		var index uint32
		sa := &windows.SockaddrInet6{Addr: dst.As16()}
		if err := windows.GetBestInterfaceEx(sa, &index); err != nil {
			return 0, 0, err
		}
		row := windows.MibIfRow2{InterfaceIndex: index}
		if err := windows.GetIfEntry2Ex(windows.MibIfTableNormalWithoutStatistics, &row); err != nil {
			return 0, 0, err
		}
		if row.OperStatus != windows.IfOperStatusUp || IsOwnedUnderlayAdapter(row.InterfaceLuid) {
			return 0, 0, fmt.Errorf("no non-tunnel IPv6 route to %s", dst)
		}
		return index, row.InterfaceLuid, nil
	}
	if !dst.Is4() {
		return 0, 0, errors.New("invalid underlay address")
	}
	var bestIndex uint32
	sa := &windows.SockaddrInet4{Addr: dst.As4()}
	if err := windows.GetBestInterfaceEx(sa, &bestIndex); err == nil {
		row := windows.MibIfRow2{InterfaceIndex: bestIndex}
		if windows.GetIfEntry2Ex(windows.MibIfTableNormalWithoutStatistics, &row) == nil && row.OperStatus == windows.IfOperStatusUp && !IsOwnedUnderlayAdapter(row.InterfaceLuid) {
			return bestIndex, row.InterfaceLuid, nil
		}
	}
	var table *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_INET, &table); err != nil {
		return 0, 0, err
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
		if windows.GetIfEntry2Ex(windows.MibIfTableNormalWithoutStatistics, &iface) != nil || iface.OperStatus != windows.IfOperStatusUp || iface.InterfaceIndex != row.InterfaceIndex {
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
		return 0, 0, fmt.Errorf("no non-tunnel IPv4 route to %s", dst)
	}
	return route.index, route.luid, nil
}

// UnderlayPacketConn serializes interface selection with each datagram write.
type UnderlayPacketConn struct {
	*net.UDPConn
	mu                      sync.Mutex
	raw                     syscall.RawConn
	index4, index6          uint32
	luid4, luid6            uint64
	usoProbed, usoSupported bool
	usoBuffer               []byte
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
	if err := c.bindDestination(ip); err != nil {
		return 0, err
	}
	return c.UDPConn.WriteTo(b, addr)
}

func (c *UnderlayPacketConn) bindDestination(ip netip.Addr) error {
	index, luid, err := UnderlayRouteIdentity(ip)
	if err != nil {
		return err
	}
	indexp, luidp := &c.index4, &c.luid4
	if ip.Is6() {
		indexp, luidp = &c.index6, &c.luid6
	}
	if *indexp == index && *luidp == luid {
		return nil
	}
	if c.raw == nil {
		c.raw, err = c.SyscallConn()
		if err != nil {
			return err
		}
	}
	if err = bindUnderlayInterface(c.raw, index, ip.Is6()); err != nil {
		return err
	}
	*indexp, *luidp = index, luid
	return nil
}

// SendUDPSegments sends equal-length datagrams as one USO message. False with
// no error means the caller must send each original datagram separately.
func (c *UnderlayPacketConn) SendUDPSegments(parts [][]byte, addr *net.UDPAddr) (bool, error) {
	if addr == nil || len(parts) < 2 || len(parts) > 32 {
		return false, nil
	}
	ip := addr.AddrPort().Addr().Unmap()
	if !ip.Is4() || len(parts[0]) == 0 || len(parts[0])*len(parts) > 65507 {
		return false, nil
	}
	for _, p := range parts {
		if len(p) != len(parts[0]) {
			return false, nil
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.bindDestination(ip); err != nil {
		return false, err
	}
	raw := c.raw
	if !c.usoProbed {
		var probeErr error
		if err := raw.Control(func(fd uintptr) {
			_, probeErr = windows.GetsockoptInt(windows.Handle(fd), windows.IPPROTO_UDP, windows.UDP_SEND_MSG_SIZE)
			if probeErr == nil && !sameSendMsgProvider(windows.Handle(fd)) {
				probeErr = errors.New("socket has a different Winsock provider")
			}
		}); err != nil {
			return false, err
		}
		c.usoProbed, c.usoSupported = true, probeErr == nil
	}
	if !c.usoSupported {
		return false, nil
	}
	total := len(parts) * len(parts[0])
	if cap(c.usoBuffer) < total {
		c.usoBuffer = make([]byte, total)
	}
	data := c.usoBuffer[:total]
	for i, p := range parts {
		copy(data[i*len(p):], p)
	}
	control := struct {
		hdr     windows.WSACMSGHDR
		value   uint32
		padding uint32
	}{}
	control.hdr.Len = unsafe.Sizeof(control.hdr) + 4
	control.hdr.Level = windows.IPPROTO_UDP
	control.hdr.Type = windows.UDP_SEND_MSG_SIZE
	control.value = uint32(len(parts[0]))
	controlLen := (control.hdr.Len + unsafe.Sizeof(uintptr(0)) - 1) &^ (unsafe.Sizeof(uintptr(0)) - 1)
	controlBytes := unsafe.Slice((*byte)(unsafe.Pointer(&control)), int(controlLen))
	sent, _, sendErr := c.UDPConn.WriteMsgUDPAddrPort(data, controlBytes, addr.AddrPort())
	if errors.Is(sendErr, windows.WSAEINVAL) || errors.Is(sendErr, windows.WSAEOPNOTSUPP) || errors.Is(sendErr, windows.WSAENOPROTOOPT) {
		c.usoSupported = false
		return false, nil
	}
	if errors.Is(sendErr, windows.WSAEMSGSIZE) || errors.Is(sendErr, windows.WSAEWOULDBLOCK) || errors.Is(sendErr, windows.WSAENOBUFS) {
		return false, nil
	}
	if sendErr != nil {
		return false, sendErr
	}
	// Winsock sends a datagram message atomically. Go's Windows WriteMsg
	// reports zero bytes on synchronous overlapped success when completion
	// notifications on success are disabled. Do not resend that message.
	if sent != 0 && sent != total {
		return false, errors.New("partial UDP segmentation send")
	}
	return true, nil
}

// The Go Windows net implementation resolves WSASendMsg using a default
// socket. A different provider may require a different extension pointer.
func sameSendMsgProvider(socket windows.Handle) bool {
	defaultSendMsgOnce.Do(func() {
		s, err := windows.Socket(windows.AF_INET, windows.SOCK_DGRAM, windows.IPPROTO_UDP)
		if err != nil {
			return
		}
		defer windows.Closesocket(s)
		defaultSendMsg = sendMsgAddress(s)
	})
	return defaultSendMsg != 0 && sendMsgAddress(socket) == defaultSendMsg
}

var defaultSendMsgOnce sync.Once
var defaultSendMsg uintptr

func sendMsgAddress(socket windows.Handle) uintptr {
	var address uintptr
	var size uint32
	err := windows.WSAIoctl(socket, windows.SIO_GET_EXTENSION_FUNCTION_POINTER,
		(*byte)(unsafe.Pointer(&windows.WSAID_WSASENDMSG)), uint32(unsafe.Sizeof(windows.WSAID_WSASENDMSG)),
		(*byte)(unsafe.Pointer(&address)), uint32(unsafe.Sizeof(address)), &size, nil, 0)
	if err != nil || size != uint32(unsafe.Sizeof(address)) {
		return 0
	}
	return address
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
