package tunsetup

import (
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
	var sockErr error
	if err := conn.Control(func(fd uintptr) {
		sockErr = unix.SetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION, "reno")
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
