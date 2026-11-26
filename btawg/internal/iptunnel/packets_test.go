package iptunnel

import (
	"errors"
	"slices"
	"testing"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
)

type segmentedPacketReader struct {
	tun.Device
	reads int
}

var errPacketReaderStopped = errors.New("TUN reader stopped")

func (*segmentedPacketReader) BatchSize() int { return 2 }

func (d *segmentedPacketReader) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	d.reads++
	var count int
	var err error
	switch d.reads {
	case 1:
		count, err = 2, tun.ErrTooManySegments
	case 2:
		count = 1
	default:
		return 0, errPacketReaderStopped
	}
	for i := range count {
		p := bufs[i][offset:][:20]
		clear(p)
		p[0], p[3], p[19] = 0x45, 20, byte(2*(d.reads-1)+i+1)
		sizes[i] = len(p)
	}
	return count, err
}

func TestSendPacketsKeepsForwardingAfterSegmentationOverflow(t *testing.T) {
	var got []byte
	err := SendPackets(new(segmentedPacketReader), func(p []byte) error {
		got = append(got, p[19])
		return nil
	})
	if !slices.Equal(got, []byte{1, 2, 3}) || !errors.Is(err, errPacketReaderStopped) {
		t.Fatalf("forwarded packets %v with error %v, want [1 2 3] followed by TUN failure", got, err)
	}
}
