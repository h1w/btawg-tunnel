package tunsetup

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// BindOwnedDevice preserves Linux device binding; Windows binds by adapter identity.
func BindOwnedDevice(_ interface{}, name string) func(string, string, syscall.RawConn) error {
	return BindDevice(name)
}

// OverlayTCP selects Reno only on the kernel TCP leg inside the carrier.
// Bursty ACKs and transient carrier queues can make CUBIC HyStart exit slow
// start far below the path's bandwidth-delay product. Bound the inner send
// buffer so backpressure reaches the proxy before it fills the TUN queue.
func OverlayTCP(_, _ string, conn syscall.RawConn) error {
	return overlayTCP(conn, false)
}

// NativeSOCKSTCP uses BBR on native SOCKS data sockets and preserves the
// existing per-socket memory bound even when BBR is unavailable.
func NativeSOCKSTCP(_, _ string, conn syscall.RawConn) error {
	return overlayTCP(conn, true)
}

func overlayTCP(conn syscall.RawConn, native bool) error {
	var sockErr error
	if err := conn.Control(func(fd uintptr) {
		sockErr = unix.SetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION, "reno")
		if sockErr == nil && native {
			sockErr = preferBBR(int(fd))
		}
		if sockErr == nil {
			sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF, 1<<20)
		}
	}); err != nil {
		return err
	}
	if sockErr != nil {
		return fmt.Errorf("set overlay TCP options: %w", sockErr)
	}
	return nil
}

// RelayTCP selects BBR for the VPN-facing sender without replacing TCP
// autotuning or changing a sysctl. Accepted sockets inherit the listener policy.
func RelayTCP(_, _ string, conn syscall.RawConn) error {
	var optionErr error
	if err := conn.Control(func(fd uintptr) { optionErr = preferBBR(int(fd)) }); err != nil {
		return err
	}
	return optionErr
}

func preferBBR(fd int) error {
	err := unix.SetsockoptString(fd, unix.IPPROTO_TCP, unix.TCP_CONGESTION, "bbr")
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENOPROTOOPT) {
		return nil
	}
	return err
}

// BindDevice restricts a socket to the named tunnel interface.
func BindDevice(name string) func(string, string, syscall.RawConn) error {
	return func(_, _ string, conn syscall.RawConn) error {
		var bindErr error
		if err := conn.Control(func(fd uintptr) {
			bindErr = unix.BindToDevice(int(fd), name)
		}); err != nil {
			return err
		}
		return bindErr
	}
}
