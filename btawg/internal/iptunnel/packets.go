package iptunnel

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
)

// Linux's TUN with VNET_HDR requires headroom on writes.
const tunHeadroom = 16

// SendPackets owns the device read loop for its lifetime. The callback uses
// the packet synchronously; its storage is reused for the next TUN read.
func SendPackets(dev tun.Device, send func([]byte) error) error {
	batch := max(1, dev.BatchSize())
	bufs := make([][]byte, batch)
	sizes := make([]int, batch)
	storage := make([]byte, batch*(tunHeadroom+MTU))
	for i := range bufs {
		bufs[i] = storage[i*(tunHeadroom+MTU) : (i+1)*(tunHeadroom+MTU) : (i+1)*(tunHeadroom+MTU)]
	}
	for {
		n, err := dev.Read(bufs, sizes, tunHeadroom)
		if err != nil && !errors.Is(err, tun.ErrTooManySegments) {
			return err
		}
		for i := range n {
			p := bufs[i][tunHeadroom : tunHeadroom+sizes[i]]
			if len(p) < 20 || p[0]>>4 != 4 || int(binary.BigEndian.Uint16(p[2:4])) != len(p) {
				continue
			}
			if err := send(p); err != nil {
				return err
			}
		}
	}
}

// ReceivePackets reads authenticated IPv4 from a stream and writes it to TUN.
func ReceivePackets(dev interface {
	Write([][]byte, int) (int, error)
}, r io.Reader) error {
	var buf [tunHeadroom + MTU]byte
	var batch [1][]byte
	for {
		p, err := ReadPacket(r, buf[tunHeadroom:])
		if err != nil {
			return err
		}
		batch[0] = buf[:tunHeadroom+len(p)]
		if _, err := dev.Write(batch[:], tunHeadroom); err != nil {
			return err
		}
	}
}
