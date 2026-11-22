package carrier

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/windows"
)

// TCPControl sizes the buffers before the TCP handshake. Congestion control
// remains the Windows host's choice.
func TCPControl(_, _ string, conn syscall.RawConn) error {
	var sockErr error
	if err := conn.Control(func(fd uintptr) {
		sock := windows.Handle(fd)
		sockErr = windows.SetsockoptInt(sock, windows.SOL_SOCKET, windows.SO_RCVBUF, 4<<20)
		if sockErr == nil {
			sockErr = windows.SetsockoptInt(sock, windows.SOL_SOCKET, windows.SO_SNDBUF, 4<<20)
		}
	}); err != nil {
		return err
	}
	if sockErr != nil {
		return fmt.Errorf("set TCP carrier buffers: %w", sockErr)
	}
	return nil
}
