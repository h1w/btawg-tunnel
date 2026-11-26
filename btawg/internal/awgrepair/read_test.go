package awgrepair

import (
	"bytes"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
)

// wakeOnlyTUN makes use of the copying fallback fail the test immediately.
type wakeOnlyTUN struct {
	*testTUN
	entered chan struct{}
}

func (f *wakeOnlyTUN) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	return 0, errors.New("copying fallback used for a wake-capable device")
}

func (f *wakeOnlyTUN) ReadWithWake(bufs [][]byte, sizes []int, offset int, wake <-chan struct{}) (int, error) {
	if f.entered != nil {
		close(f.entered)
		f.entered = nil
	}
	select {
	case p := <-f.input:
		sizes[0] = copy(bufs[0][offset:], p)
		return 1, nil
	case <-wake:
		return 0, nil
	case <-f.done:
		return 0, net.ErrClosed
	}
}

func TestReadWithWakeDeliversOriginalAndControlWithoutFallback(t *testing.T) {
	base := &wakeOnlyTUN{testTUN: fixtureTUN()}
	c := newDevice(base, true)
	defer c.Close()
	local := netip.MustParseAddr("10.0.0.2")
	remote := netip.MustParseAddr("10.0.0.1")
	c.replace(map[netip.Addr]*path{remote: {local: local, remote: remote, k: testKey(t)}})
	p := make([]byte, 256)
	p[0], p[2], p[3], p[8], p[9] = 0x45, 1, 0, 64, 17
	copy(p[12:16], local.AsSlice())
	copy(p[16:20], remote.AsSlice())
	base.input <- p
	if got := nextDatagram(t, c); !bytes.Equal(got, p) {
		t.Fatal("direct read changed the original packet")
	}
	probe := nextDatagram(t, c)
	if len(probe) < 20 || probe[9] != 253 || !isInnerControl(probe) {
		t.Fatalf("control did not wake the direct read: %x", probe)
	}
}

func TestReadWithWakeInterruptsBlockedQueueRead(t *testing.T) {
	base := &wakeOnlyTUN{testTUN: fixtureTUN(), entered: make(chan struct{})}
	d := newDevice(base, true)
	defer d.Close()
	local := netip.MustParseAddr("10.0.0.2")
	remote := netip.MustParseAddr("10.0.0.1")
	p := &path{local: local, remote: remote, k: testKey(t)}
	d.replace(map[netip.Addr]*path{remote: p})

	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	buf := make([]byte, 1600)
	sizes := make([]int, 1)
	entered := base.entered
	go func() {
		n, err := d.Read([][]byte{buf}, sizes, 32)
		done <- result{n: n, err: err}
	}()
	<-entered
	d.mu.Lock()
	queued := d.appendControl(p, time.Now(), func(out []byte) []byte {
		return p.session.begin(&p.k, p.local, p.remote, time.Now(), out)
	})
	d.mu.Unlock()
	if !queued {
		t.Fatal("failed to queue handshake")
	}
	select {
	case got := <-done:
		if got.n != 1 || got.err != nil {
			t.Fatalf("Read returned (%d, %v), want (1, nil)", got.n, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued control did not interrupt the underlying read")
	}
	if !isInnerControl(buf[32 : 32+sizes[0]]) {
		t.Fatal("interrupted read did not return the handshake")
	}
}

type segmentedWakeTUN struct{ *testTUN }

func (f *segmentedWakeTUN) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	return 0, errors.New("copying fallback used for a wake-capable device")
}

func (f *segmentedWakeTUN) ReadWithWake(bufs [][]byte, sizes []int, offset int, wake <-chan struct{}) (int, error) {
	packet := bytes.Repeat([]byte{0x5a}, 128)
	sizes[0] = copy(bufs[0][offset:], packet)
	return 1, tun.ErrTooManySegments
}

func TestReadWithWakePreservesTooManySegments(t *testing.T) {
	base := &segmentedWakeTUN{fixtureTUN()}
	d := newDevice(base, true)
	defer d.Close()
	buf := make([]byte, 1600)
	sizes := make([]int, 1)
	n, err := d.Read([][]byte{buf}, sizes, 32)
	if n != 1 || !errors.Is(err, tun.ErrTooManySegments) {
		t.Fatalf("Read returned (%d, %v), want (1, ErrTooManySegments)", n, err)
	}
	if sizes[0] != 128 || !bytes.Equal(buf[32:32+sizes[0]], bytes.Repeat([]byte{0x5a}, 128)) {
		t.Fatal("direct read did not preserve the segmented packet")
	}
}

func TestFallbackReadDeliversAdjustedWindowToEncryption(t *testing.T) {
	d := newDevice(fixtureTUN(), true)
	defer d.Close()
	now := time.Now()
	local, remote := netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2")
	d.profile = autoRequest
	p := &path{local: local, remote: remote, session: innerSession{established: true, last: now, gen: 1}, state: &stream{generation: 1, wideUntil: now.Add(time.Second), nextWide: now.Add(time.Second)}}
	d.replace(map[netip.Addr]*path{remote: p})
	p.state.autoState().rx = autoPolicy{mode: autoMode{clamp: true}, budget: flightBytes, last: now}
	flightNegotiate(&p.state.flight, now, 4000, 8, 7)
	ack := flightACK(16, -1, 4000, 65535)
	original := bytes.Clone(ack)
	buf := make([]byte, len(ack)+32)
	sizes := []int{0}
	n, err := d.finishRead([][]byte{buf}, sizes, 32, 1, nil, &readBatch{sizes: []int{len(ack)}, bufs: [][]byte{ack}})
	if err != nil || n != 1 || !bytes.Equal(ack, original) || bytes.Equal(buf[32:], original) || flightChecksum(buf[32:]) != 0 {
		t.Fatalf("fallback credit was not applied to encryption buffer; n=%d err=%v original=%x output=%x", n, err, original[34:38], buf[66:70])
	}
}

// netstack can hold an endpoint lock while handing its final packet to
// Read. Close needs that lock before it can close the underlying read side.
type notifyingCloseTUN struct {
	*testTUN
	abort chan struct{}
}

func (b *notifyingCloseTUN) Close() error {
	defer b.testTUN.Close()
	for range 2 {
		select {
		case b.input <- []byte{0x45}:
		case <-b.abort:
			return errors.New("fallback stopped draining before netstack closed")
		}
	}
	return nil
}

func TestFallbackDrainsNetstackDuringClose(t *testing.T) {
	for _, started := range []bool{false, true} {
		b := &notifyingCloseTUN{testTUN: fixtureTUN(), abort: make(chan struct{})}
		b.input = make(chan []byte)
		d := newDevice(b, true)
		if started {
			go func() { b.input <- []byte{1} }()
			if got := nextDatagram(t, d); !bytes.Equal(got, []byte{1}) {
				t.Fatal("missing first packet")
			}
		}
		closed := make(chan error, 1)
		go func() { closed <- d.Close() }()
		select {
		case err := <-closed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			close(b.abort)
			<-closed
			t.Fatalf("Close stranded outstanding netstack packets (reader started: %v)", started)
		}
	}
}
