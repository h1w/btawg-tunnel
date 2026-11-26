package client

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"

	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/tunsetup"
)

func TestSharedTUNRoutesProxyPackets(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires network administration; run with unshare -Urn")
	}
	const name = "btawg-sockets"
	addr := netip.MustParsePrefix("10.78.0.2/24")
	dev, err := tunsetup.OpenClientTUN(name, addr, nil, config.DefaultMTU)
	if err != nil {
		t.Fatal(err)
	}
	shared := newSharedTUN(dev, name, config.DefaultMTU)
	defer shared.close()
	route, err := tunsetup.NewProxyRoute(name)
	if err != nil {
		t.Fatal(err)
	}
	defer route.Close()
	primary, proxy := shared.newPath(), shared.newPath()
	defer primary.Close()
	defer proxy.Close()
	shared.primary.Store(primary)
	var c Client
	proxy.proxySockets(&c, addr.Addr())

	// Use real sockets and /dev/net/tun: the wrong path is observable on the
	// primary queue even for the first SYN, before DialContext can return.
	packet := func(on *tunPath, proto byte) []byte {
		t.Helper()
		other := primary
		if on == primary {
			other = proxy
		}
		select {
		case b := <-on.packets:
			defer shared.pool.Put(b)
			p := (*b)[packetHeadroom:]
			if p[9] != proto {
				t.Fatalf("got IPv4 protocol %d, want %d", p[9], proto)
			}
			return append([]byte(nil), p...)
		case b := <-other.packets:
			shared.pool.Put(b)
			t.Fatal("packet went through the wrong transport")
		case <-time.After(2 * time.Second):
			t.Fatal("no packet on the shared TUN")
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := c.proxyTCP(ctx, netip.MustParseAddrPort("1.1.1.1:443"))
		if err == nil {
			conn.Close()
		}
	}()
	defer func() { cancel(); <-done }()
	if syn := packet(proxy, 6); syn[33]&2 == 0 {
		t.Fatalf("first TCP packet is not a SYN: flags 0x%x", syn[33])
	}
	cancel()
	<-done

	udp, err := c.proxyUDP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	if _, err := udp.WriteTo(make([]byte, 2200), net.UDPAddrFromAddrPort(netip.MustParseAddrPort("1.1.1.1:53"))); err != nil {
		t.Fatal(err)
	}
	first, last := packet(proxy, 17), packet(proxy, 17)
	if first[6]&32 == 0 || last[6]&32 != 0 || binary.BigEndian.Uint16(last[6:8])&0x1fff == 0 || string(first[4:6]) != string(last[4:6]) {
		t.Fatalf("UDP fragments not delivered together: %x, %x", first[:8], last[:8])
	}

	lan, err := net.ListenPacket("udp4", "10.78.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lan.Close()
	if _, err := lan.WriteTo([]byte("LAN"), &net.UDPAddr{IP: net.IPv4(10, 78, 0, 1), Port: 9999}); err != nil {
		t.Fatal(err)
	}
	packet(primary, 17)
}

type faultTUN struct {
	tun.Device // Only the dispatcher methods below are used.
	fault      chan error
	events     chan tun.Event
}

func (d *faultTUN) BatchSize() int           { return 1 }
func (d *faultTUN) Events() <-chan tun.Event { return d.events }
func (d *faultTUN) Close() error {
	close(d.events)
	close(d.fault)
	return nil
}

func (d *faultTUN) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	err, ok := <-d.fault
	if !ok {
		return 0, os.ErrClosed
	}
	if !errors.Is(err, tun.ErrTooManySegments) {
		return 0, err
	}
	p := bufs[0][offset:][:20]
	clear(p)
	p[0] = 0x45
	sizes[0] = len(p)
	return 1, err
}

func TestSharedTUNReadErrors(t *testing.T) {
	d := &faultTUN{fault: make(chan error, 3), events: make(chan tun.Event)}
	shared := newSharedTUN(d, "test", config.DefaultMTU)
	defer shared.close()
	path := shared.newPath()
	defer path.Close()
	shared.primary.Store(path)
	for range 2 {
		d.fault <- tun.ErrTooManySegments
		select {
		case b := <-path.packets:
			shared.pool.Put(b)
		case <-time.After(time.Second):
			t.Fatal("segmentation overflow discarded valid packets or stopped forwarding")
		}
	}
	fatal := errors.New("TUN read failed")
	d.fault <- fatal
	got := make(chan error, 1)
	go func() {
		b := make([]byte, config.DefaultMTU)
		_, err := path.Read([][]byte{b}, []int{0}, 0)
		got <- err
	}()
	select {
	case err := <-got:
		if !errors.Is(err, fatal) {
			t.Fatalf("path read error = %v, want %v", err, fatal)
		}
	case <-time.After(time.Second):
		t.Fatal("path reader blocked after fatal shared device error")
	}
}

func TestSharedTUNCountsOnlyFullPathDrops(t *testing.T) {
	d := &faultTUN{fault: make(chan error, pathQueueSize+3), events: make(chan tun.Event)}
	s := newSharedTUN(d, "test", config.DefaultMTU)
	defer s.close()
	p := s.newPath()
	defer p.Close()
	s.primary.Store(p)
	for range pathQueueSize + 2 {
		d.fault <- tun.ErrTooManySegments
	}
	d.fault <- os.ErrClosed
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("a full path blocked the shared TUN dispatcher")
	}
	if got := s.tunQueueDrops.Load(); got != 2 {
		t.Fatalf("full-path drops = %d, want 2", got)
	}
}

type fragmentInputTUN struct {
	faultTUN
	input <-chan []byte
}

func (d *fragmentInputTUN) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	p := <-d.input
	if p == nil {
		return 0, os.ErrClosed
	}
	sizes[0] = copy(bufs[0][offset:], p)
	return 1, nil
}

func TestSharedTUNRejectsFragmentsAfterQueueDrop(t *testing.T) {
	for _, dropFirst := range []bool{true, false} {
		name := "middle"
		if dropFirst {
			name = "first"
		}
		t.Run(name, func(t *testing.T) {
			input := make(chan []byte)
			d := &fragmentInputTUN{faultTUN{fault: make(chan error), events: make(chan tun.Event)}, input}
			s := newSharedTUN(d, "test", config.DefaultMTU)
			defer s.close()
			defer close(input)
			p := s.newPath()
			defer p.Close()
			s.primary.Store(p)

			fill := cap(p.packets)
			if !dropFirst {
				fill--
			}
			ordinary := ipv4FixtureFragment(0, 0, false, []byte("ordinary"))
			for range fill {
				input <- ordinary
			}
			input <- ipv4FixtureFragment(7, 0, true, []byte("abcdefgh"))
			if !dropFirst {
				input <- ipv4FixtureFragment(7, 8, true, []byte("ijklmnop"))
			}
			// Drop one queued packet to free a slot, after the fragment drop.
			ready := ipv4FixtureFragment(42, 8, false, []byte("unknown"))
			input <- ready
			s.pool.Put(<-p.packets)
			input <- ipv4FixtureFragment(7, 16, false, []byte("qrst"))
			probe := ipv4FixtureFragment(8, 0, false, []byte("survives"))
			input <- probe
			input <- nil
			<-s.done
			if got := s.tunQueueDrops.Load(); got != 1 {
				t.Errorf("queue drops = %d, want only the rejected fragment", got)
			}
			var last *[]byte
			for len(p.packets) > 0 {
				if last != nil {
					s.pool.Put(last)
				}
				last = <-p.packets
			}
			if last == nil || string((*last)[packetHeadroom+20:]) != "survives" {
				t.Fatal("unreassemblable tail took the freed slot from a complete datagram")
			}
			s.pool.Put(last)
		})
	}
}
