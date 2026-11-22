package tunsetup

import (
	"net"
	"net/netip"
	"syscall"
)

func ProtectUnderlayPacket(conn *net.UDPConn) net.PacketConn           { return conn }
func IsUnderlayInterface(_ int) bool                                   { return true }
func ProtectUnderlayStream(c net.Conn, _ netip.Addr) (net.Conn, error) { return c, nil }

type UnderlayDialGuard struct{}

func (*UnderlayDialGuard) Control(_, _ string, _ syscall.RawConn) error       { return nil }
func (*UnderlayDialGuard) Protect(c net.Conn, _ netip.Addr) (net.Conn, error) { return c, nil }
