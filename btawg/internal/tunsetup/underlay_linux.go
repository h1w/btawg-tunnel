package tunsetup

import (
	"net"
	"net/netip"
	"syscall"
)

func UnderlayControl(_, _ string, _ syscall.RawConn) error             { return nil }
func ProtectUnderlayPacket(conn *net.UDPConn) net.PacketConn           { return conn }
func IsUnderlayInterface(_ int) bool                                   { return true }
func ProtectUnderlayStream(c net.Conn, _ netip.Addr) (net.Conn, error) { return c, nil }
