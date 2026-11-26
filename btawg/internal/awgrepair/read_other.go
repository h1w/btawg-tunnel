//go:build !linux

package awgrepair

import (
	"net"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
)

type nativeReader struct{}

func newNativeReader(tun.Device) *nativeReader { return nil }
func (*nativeReader) signal()                  {}
func (*nativeReader) interrupt()               {}
func (*nativeReader) close()                   {}
func (*nativeReader) read([][]byte, []int, int) (int, error) {
	return 0, net.ErrClosed
}
