//go:build linux

package awgrepair

import (
	"errors"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
)

// NativeTun already blocks via Go's network poller. Interrupt only a control
// notification, rather than doing an additional poll syscall for every ACK.
// No File.Fd(): it can turn a runtime-managed descriptor back into blocking IO.
// Only the TUN reader resets the deadline; a wake arriving before Read stays
// pending. The control queue is checked again before the next blocking read.
type nativeReader struct {
	dev     tun.Device
	file    *os.File
	stopped atomic.Bool
}

func newNativeReader(base tun.Device) *nativeReader {
	dev, ok := base.(*tun.NativeTun)
	if !ok || dev.File() == nil {
		return nil
	}
	f := dev.File()
	if err := f.SetReadDeadline(time.Time{}); err != nil {
		return nil
	}
	return &nativeReader{dev: dev, file: f}
}

var wakeDeadline = time.Unix(1, 0)

func (r *nativeReader) signal() {
	if r != nil {
		_ = r.file.SetReadDeadline(wakeDeadline)
	}
}
func (r *nativeReader) interrupt() {
	if r != nil {
		r.stopped.Store(true)
		r.signal()
	}
}
func (r *nativeReader) close() {
	if r != nil {
		r.stopped.Store(true)
	}
}
func (r *nativeReader) read(bufs [][]byte, sizes []int, offset int) (int, error) {
	if r.stopped.Load() {
		return 0, net.ErrClosed
	}
	n, err := r.dev.Read(bufs, sizes, offset)
	if n == 0 && errors.Is(err, os.ErrDeadlineExceeded) {
		if r.stopped.Load() {
			return 0, net.ErrClosed
		}
		return 0, r.file.SetReadDeadline(time.Time{})
	}
	return n, err
}
