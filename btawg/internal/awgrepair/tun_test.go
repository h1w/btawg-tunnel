package awgrepair

import (
	"bytes"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
)

type testTUN struct {
	tun.Device
	input, output chan []byte
	done          chan struct{}
}

func fixtureTUN() *testTUN {
	return &testTUN{input: make(chan []byte, 4), output: make(chan []byte, 4), done: make(chan struct{})}
}
func (f *testTUN) MTU() (int, error) { return 1280, nil }
func (f *testTUN) BatchSize() int    { return 1 }
func (f *testTUN) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	select {
	case p := <-f.input:
		sizes[0] = copy(bufs[0][offset:], p)
		return 1, nil
	case <-f.done:
		return 0, net.ErrClosed
	}
}
func (f *testTUN) Write(bufs [][]byte, offset int) (int, error) {
	for _, p := range bufs {
		f.output <- bytes.Clone(p[offset:])
	}
	return len(bufs), nil
}
func (f *testTUN) Close() error { close(f.done); return nil }

func nextDatagram(t *testing.T, d *Device) []byte {
	t.Helper()
	result := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 1600)
		var sizes [1]int
		n, err := d.Read([][]byte{buf}, sizes[:], 32)
		if n == 1 && err == nil {
			result <- buf[32 : 32+sizes[0]]
		} else {
			result <- nil
		}
	}()
	select {
	case p := <-result:
		return p
	case <-time.After(time.Second):
		t.Fatal("control or native datagram did not reach the AWG encryption input")
		return nil
	}
}

func TestInnerControlWakesIdleAWGAndLeavesOriginalIntact(t *testing.T) {
	base := fixtureTUN()
	k := testKey(t)
	c := newDevice(base, true)
	defer c.Close()
	local := netip.MustParseAddr("10.0.0.2")
	remote := netip.MustParseAddr("10.0.0.1")
	c.replace(map[netip.Addr]*path{remote: {local: local, remote: remote, k: k}})
	p := make([]byte, 256)
	p[0], p[2], p[3], p[8], p[9] = 0x45, 1, 0, 64, 17
	copy(p[12:16], local.AsSlice())
	copy(p[16:20], remote.AsSlice())
	base.input <- p
	got := nextDatagram(t, c)
	if !bytes.Equal(got, p) {
		t.Fatal("the original must be delivered unmodified before negotiation")
	}
	probe := nextDatagram(t, c)
	if len(probe) < 20 || probe[9] != 253 || !isInnerControl(probe) {
		t.Fatalf("not an inner IPv4 control: %x", probe)
	}
}

func testKey(t *testing.T) key {
	t.Helper()
	k, err := deriveKey("a83ee074cdbfc7e62926b34ba4ed6345d6e8ab19fc602c5d6eb50570313e4812", "8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a", true)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
