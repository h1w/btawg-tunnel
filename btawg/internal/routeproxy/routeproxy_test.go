package routeproxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"bittorrent-tunnel/btawg/internal/socks"
)

var route = Request{RouteID: "route-a", Grant: "opaque-grant"}
var loop = netip.MustParseAddr("127.0.0.1")

func listenTCP(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func listenUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	u, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = u.Close() })
	return u
}

func withinTest(t *testing.T, run func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("service returned: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("service did not shut down")
		}
	})
}

func via(ln net.Listener, source string) OpenFunc {
	return func(ctx context.Context, _ Request) (net.Conn, error) {
		d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(source)}}
		return d.DialContext(ctx, "tcp4", ln.Addr().String())
	}
}

func testExitPolicy(lookups *atomic.Int32) exitPolicy {
	return exitPolicy{
		lookupIPv4: func(_ context.Context, network, host string) ([]netip.Addr, error) {
			lookups.Add(1)
			if network != "ip4" || host != "name.example" {
				return nil, fmt.Errorf("unexpected lookup %s %s", network, host)
			}
			return []netip.Addr{loop}, nil
		},
		allow: func(ip netip.Addr) bool { return ip == loop },
	}
}

func authorizeTest(previous string, hop int, decision Decision) AuthorizeFunc {
	return func(_ context.Context, peer netip.Addr, req Request) (Decision, error) {
		if peer.String() != previous || req.Hop != hop || req.RouteID != route.RouteID || req.Grant != route.Grant {
			return Decision{}, fmt.Errorf("wrong route or previous hop: %s at hop %d", peer, req.Hop)
		}
		return decision, nil
	}
}

func testRoute(t *testing.T, hops int, life context.Context, policy exitPolicy) string {
	t.Helper()
	exit, local := listenTCP(t), listenTCP(t)
	first, previous := exit, "127.0.0.2"
	if hops == 2 {
		transit := listenTCP(t)
		withinTest(t, func(ctx context.Context) error {
			return Serve(ctx, transit, authorizeTest("127.0.0.2", 0, Decision{
				Next: via(exit, "127.0.0.3"), Life: ctx,
			}))
		})
		first, previous = transit, "127.0.0.3"
	}
	withinTest(t, func(ctx context.Context) error {
		return serve(ctx, exit, authorizeTest(previous, hops-1, Decision{Life: life}), policy)
	})
	withinTest(t, func(ctx context.Context) error {
		return proxy(via(first, "127.0.0.2"), nil, route, policy.allow).Serve(ctx, local)
	})
	return local.Addr().String()
}

func echoTCP(t *testing.T) uint16 {
	t.Helper()
	ln := listenTCP(t)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer c.Close()
			_, _ = io.Copy(c, c)
		}
	}()
	return uint16(ln.Addr().(*net.TCPAddr).Port)
}

func namedUDP(t *testing.T, addr string) (net.Conn, *net.UDPConn, *net.UDPAddr) {
	t.Helper()
	ctrl, err := net.DialTimeout("tcp4", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctrl.Close() })
	if err := ctrl.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := ctrl.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var greeting [2]byte
	if _, err := io.ReadFull(ctrl, greeting[:]); err != nil || greeting != [2]byte{5, 0} {
		t.Fatalf("SOCKS greeting: %v (%v)", greeting, err)
	}
	if _, err := ctrl.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	var reply [10]byte
	if _, err := io.ReadFull(ctrl, reply[:]); err != nil {
		t.Fatal(err)
	}
	if reply[1] != 0 || reply[3] != 1 {
		t.Fatalf("SOCKS UDP association failed: %v", reply)
	}
	relay := &net.UDPAddr{IP: net.IP(reply[4:8]), Port: int(binary.BigEndian.Uint16(reply[8:10]))}
	return ctrl, listenUDP(t), relay
}

func sendNamedUDP(t *testing.T, app *net.UDPConn, relay *net.UDPAddr, port uint16, payload string) {
	t.Helper()
	const host = "name.example"
	wire := []byte{0, 0, 0, 3, byte(len(host))}
	wire = append(wire, host...)
	wire = binary.BigEndian.AppendUint16(wire, port)
	if _, err := app.WriteToUDP(append(wire, payload...), relay); err != nil {
		t.Fatal(err)
	}
}

func TestHostnameTCPAndUDPThroughSelectedHops(t *testing.T) {
	for _, hops := range []int{1, 2} {
		t.Run(strconv.Itoa(hops), func(t *testing.T) {
			var lookups atomic.Int32
			local := testRoute(t, hops, t.Context(), testExitPolicy(&lookups))
			c, err := socks.NewClient(local).Dial(t.Context(), "tcp", net.JoinHostPort("name.example", strconv.Itoa(int(echoTCP(t)))))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(3 * time.Second))
			if _, err := io.WriteString(c, "stream"); err != nil {
				t.Fatal(err)
			}
			var buf [128]byte
			if _, err := io.ReadFull(c, buf[:6]); err != nil || string(buf[:6]) != "stream" {
				t.Fatalf("TCP echo: %q (%v)", buf[:6], err)
			}

			wan := listenUDP(t)
			_, app, relay := namedUDP(t, local)
			port := uint16(wan.LocalAddr().(*net.UDPAddr).Port)
			sendNamedUDP(t, app, relay, port, "packet")
			_ = wan.SetDeadline(time.Now().Add(3 * time.Second))
			n, from, err := wan.ReadFromUDP(buf[:])
			if err != nil || string(buf[:n]) != "packet" {
				t.Fatalf("UDP exit: %q (%v)", buf[:n], err)
			}
			if _, err := wan.WriteToUDP([]byte("answer"), from); err != nil {
				t.Fatal(err)
			}
			_ = app.SetReadDeadline(time.Now().Add(3 * time.Second))
			n, _, err = app.ReadFromUDP(buf[:])
			want := binary.BigEndian.AppendUint16([]byte{0, 0, 0, 1, 127, 0, 0, 1}, port)
			if err != nil || !bytes.Equal(buf[:n], append(want, "answer"...)) {
				t.Fatalf("UDP reply: %v (%v)", buf[:n], err)
			}
			if got := lookups.Load(); got != 2 {
				t.Fatalf("exit lookups = %d, want one per named destination", got)
			}
		})
	}
}

func TestAuthorizationAndNextHopFailureDoNotReportReadinessOrResolve(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision func(context.Context, OpenFunc) (Decision, error)
	}{
		{"denied", func(ctx context.Context, open OpenFunc) (Decision, error) {
			return Decision{Life: ctx, Next: open}, errors.New("invalid grant")
		}},
		{"downstream-denied", func(ctx context.Context, open OpenFunc) (Decision, error) {
			return Decision{Life: ctx, Next: open}, nil
		}},
		{"next-unavailable", func(ctx context.Context, _ OpenFunc) (Decision, error) {
			return Decision{Life: ctx, Next: func(context.Context, Request) (net.Conn, error) {
				return nil, errors.New("overlay unavailable")
			}}, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, next, ln := listenTCP(t), listenTCP(t), listenTCP(t)
			var lookups, opened, checked atomic.Int32
			withinTest(t, func(ctx context.Context) error {
				return serve(ctx, next, func(context.Context, netip.Addr, Request) (Decision, error) {
					checked.Add(1)
					return Decision{}, errors.New("denied at final exit")
				}, testExitPolicy(&lookups))
			})
			withinTest(t, func(ctx context.Context) error {
				return Serve(ctx, first, func(context.Context, netip.Addr, Request) (Decision, error) {
					return tc.decision(ctx, func(c context.Context, r Request) (net.Conn, error) {
						opened.Add(1)
						return via(next, "127.0.0.3")(c, r)
					})
				})
			})
			withinTest(t, func(ctx context.Context) error {
				return Proxy(via(first, "127.0.0.2"), route).Serve(ctx, ln)
			})
			client := socks.NewClient(ln.Addr().String())
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			if c, err := client.Dial(ctx, "tcp", "name.example:443"); err == nil {
				c.Close()
				t.Error("CONNECT reported success")
			}
			if u, err := client.DialUDP(ctx); err == nil {
				u.Close()
				t.Error("UDP ASSOCIATE reported success")
			}
			if ctx.Err() != nil {
				t.Fatal("did not promptly report failure")
			}
			var wantDownstream int32
			if tc.name == "downstream-denied" {
				wantDownstream = 2
			}
			if lookups.Load() != 0 || opened.Load() != wantDownstream || checked.Load() != wantDownstream {
				t.Fatalf("lookups=%d opened=%d downstream checks=%d", lookups.Load(), opened.Load(), checked.Load())
			}
		})
	}
}

func TestExitLeaseRevokesTCPAndUDP(t *testing.T) {
	var lookups atomic.Int32
	life, revoke := context.WithCancel(t.Context())
	defer revoke()
	local := testRoute(t, 2, life, testExitPolicy(&lookups))
	c, err := socks.NewClient(local).Dial(t.Context(), "tcp", net.JoinHostPort("name.example", strconv.Itoa(int(echoTCP(t)))))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctrl, _, _ := namedUDP(t, local)
	revoke()
	for _, conn := range []net.Conn{c, ctrl} {
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var b [1]byte
		_, err := conn.Read(b[:])
		var ne net.Error
		if err == nil || (errors.As(err, &ne) && ne.Timeout()) {
			t.Fatalf("revoked session still open: %v", err)
		}
	}
}

func TestExitUDPDiscardsUnsolicitedAndDuplicateReplies(t *testing.T) {
	var lookups atomic.Int32
	local := testRoute(t, 1, t.Context(), testExitPolicy(&lookups))
	wan, other := listenUDP(t), listenUDP(t)
	_, app, relay := namedUDP(t, local)
	sendNamedUDP(t, app, relay, uint16(wan.LocalAddr().(*net.UDPAddr).Port), "request")
	_ = wan.SetReadDeadline(time.Now().Add(3 * time.Second))
	var b [128]byte
	if _, from, err := wan.ReadFromUDP(b[:]); err != nil {
		t.Fatal(err)
	} else {
		for _, packet := range []struct {
			c *net.UDPConn
			s string
		}{{other, "different-port"}, {wan, "legitimate"}, {wan, "duplicate"}} {
			if _, err := packet.c.WriteToUDP([]byte(packet.s), from); err != nil {
				t.Fatal(err)
			}
		}
	}
	_ = app.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _, err := app.ReadFromUDP(b[:])
	if err != nil || n < 10 || string(b[10:n]) != "legitimate" {
		t.Fatalf("unexpected reply %q (%v)", b[:n], err)
	}
	_ = app.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, _, err := app.ReadFromUDP(b[:]); !errors.Is(err, context.DeadlineExceeded) {
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("unexpected second reply: %v", err)
		}
	}
}

func TestProductionPolicyBlocksPrivateSpecialAndLocalIPv4(t *testing.T) {
	for _, raw := range []string{"0.0.0.0", "127.0.0.1", "10.40.0.1", "169.254.1.1", "100.64.0.1", "192.0.2.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "255.255.255.255", "2001:4860::1"} {
		if err := (exitPolicy{}).check([]netip.Addr{netip.MustParseAddr(raw)}); err == nil {
			t.Errorf("production policy allowed %s", raw)
		}
	}
	if err := (exitPolicy{blocked: []netip.Prefix{netip.MustParsePrefix("45.1.0.0/24")}}).check([]netip.Addr{netip.MustParseAddr("45.1.0.1")}); err == nil {
		t.Error("production policy allowed globally numbered overlay address")
	}
	// Synthetic interface enumeration leaves the production policy itself intact.
	p := exitPolicy{interfaceAddrs: func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.IPv4(8, 8, 4, 4), Mask: net.CIDRMask(24, 32)}}, nil
	}}
	if err := p.check([]netip.Addr{netip.MustParseAddr("8.8.8.8")}); err != nil {
		t.Fatalf("public address denied: %v", err)
	}
	if err := p.check([]netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("8.8.4.4")}); err == nil {
		t.Error("mixed DNS answers included the exit host's interface address")
	}
	if err := p.check([]netip.Addr{netip.MustParseAddr("8.8.4.255")}); err == nil {
		t.Error("production policy allowed the exit LAN's directed broadcast address")
	}
}

func TestProxyDropsInvalidUDPReplyAddress(t *testing.T) {
	wan, local := listenTCP(t), listenTCP(t)
	withinTest(t, func(ctx context.Context) error {
		return Proxy(via(wan, "127.0.0.2"), route).Serve(ctx, local)
	})
	ctrl, app, relay := listenForgedExit(t, wan, local)
	sendNamedUDP(t, app, relay, 53, "request")
	_ = ctrl.SetReadDeadline(time.Now().Add(3 * time.Second))
	var b [1]byte
	if _, err := ctrl.Read(b[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("invalid exit reply did not close association: %v", err)
	}
	_ = app.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, _, err := app.ReadFromUDP(b[:]); err == nil {
		t.Fatal("private source reported to SOCKS client")
	}
}

func listenForgedExit(t *testing.T, wan, local net.Listener) (net.Conn, *net.UDPConn, *net.UDPAddr) {
	t.Helper()
	withinTest(t, func(context.Context) error {
		c, err := wan.Accept()
		if err != nil {
			return err
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := readCommand(c); err != nil {
			return err
		}
		if _, err := c.Write([]byte{2}); err != nil {
			return err
		}
		var confirm [1]byte
		if _, err := io.ReadFull(c, confirm[:]); err != nil {
			return err
		}
		if _, err := c.Write([]byte{0}); err != nil {
			return err
		}
		if _, _, err := readDatagram(c, make([]byte, maxHost+maxDatagram)); err != nil {
			return err
		}
		// A three-byte reply purporting to come from 10.0.0.1:53.
		_, err = c.Write([]byte{0, 3, 10, 0, 0, 1, 0, 53, 'b', 'a', 'd'})
		return err
	})
	return namedUDP(t, local.Addr().String())
}

func TestReadInvocationLeavesApplicationBytesUnread(t *testing.T) {
	// Hand-encoded version 1 TCP request: id=a, grant=b, hop=1, host=xyz, port=443.
	invocation := []byte{'B', 'R', 'P', 1, 1, 1, 0, 1, 0, 1, 3, 1, 187, 'a', 'b', 'x', 'y', 'z'}
	stream := bytes.NewBuffer(append(bytes.Clone(invocation), "application"...))
	req, frame, err := ReadInvocation(stream)
	if err != nil || req != (Request{RouteID: "a", Grant: "b", Hop: 1}) || !bytes.Equal(frame, invocation) || stream.String() != "application" {
		t.Fatalf("request=%+v frame=%v remaining=%q error=%v", req, frame, stream.String(), err)
	}
}

func TestRealExitRejectsLocalTCPAndReleasesGrant(t *testing.T) {
	exit, local := listenTCP(t), listenTCP(t)
	released := make(chan struct{}, 1)
	withinTest(t, func(ctx context.Context) error {
		return Serve(ctx, exit, authorizeTest("127.0.0.2", 0, Decision{
			Life: ctx, Release: func() { released <- struct{}{} },
		}))
	})
	withinTest(t, func(ctx context.Context) error {
		return Proxy(via(exit, "127.0.0.2"), route).Serve(ctx, local)
	})
	dst := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(echoTCP(t))))
	if c, err := socks.NewClient(local.Addr().String()).Dial(t.Context(), "tcp", dst); err == nil {
		c.Close()
		t.Fatal("local destination advertised as a successful CONNECT")
	}
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("failed session did not release its authorization")
	}
}

func TestReadInvocationRejectsOversizedFieldsBeforeReadingBody(t *testing.T) {
	// Route ID claims 65535 bytes, but the bounded parser must never read any.
	stream := bytes.NewBuffer([]byte{'B', 'R', 'P', 1, 2, 0, 255, 255, 0, 1, 0, 0, 0, 'b', 'o', 'd', 'y'})
	_, frame, err := ReadInvocation(stream)
	if err == nil || frame != nil || stream.String() != "body" {
		t.Fatalf("frame=%v remaining=%q error=%v", frame, stream.String(), err)
	}
}

func TestTCPHalfCloseKeepsGrantUntilResponseCompletes(t *testing.T) {
	exit, local, wan := listenTCP(t), listenTCP(t), listenTCP(t)
	released := make(chan struct{}, 1)
	originResult := make(chan error, 1)
	go func() {
		c, err := wan.Accept()
		if err != nil {
			originResult <- err
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		if got, err := io.ReadAll(c); err != nil || string(got) != "request" {
			originResult <- fmt.Errorf("WAN read = %q, %v", got, err)
			return
		}
		_, err = c.Write([]byte("response"))
		originResult <- err
	}()
	withinTest(t, func(ctx context.Context) error {
		return serve(ctx, exit, authorizeTest("127.0.0.2", 0, Decision{
			Life: ctx, Release: func() { released <- struct{}{} },
		}), exitPolicy{allow: func(ip netip.Addr) bool { return ip == loop }})
	})
	withinTest(t, func(ctx context.Context) error {
		return Proxy(via(exit, "127.0.0.2"), route).Serve(ctx, local)
	})
	c, err := socks.NewClient(local.Addr().String()).Dial(t.Context(), "tcp", wan.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(c); err != nil || string(got) != "response" {
		t.Fatalf("routed response after CloseWrite = %q, %v", got, err)
	}
	if err := <-originResult; err != nil {
		t.Fatal(err)
	}
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("completed TCP stream kept an active grant")
	}
}

func TestRealExitRejectsWrongPreviousPeerAndSkippedHop(t *testing.T) {
	exit := listenTCP(t)
	var lookups atomic.Int32
	withinTest(t, func(ctx context.Context) error {
		return serve(ctx, exit, authorizeTest("127.0.0.3", 1, Decision{Life: ctx}), testExitPolicy(&lookups))
	})
	for _, tc := range []struct {
		source string
		hop    byte
	}{{"127.0.0.2", 1}, {"127.0.0.3", 0}} {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		c, err := via(exit, tc.source)(ctx, route)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		invocation := []byte{'B', 'R', 'P', 1, 1, tc.hop, 0, 7, 0, 12, 12, 0, 53}
		invocation = append(invocation, "route-aopaque-grantname.example"...)
		if _, err := c.Write(invocation); err != nil {
			c.Close()
			t.Fatal(err)
		}
		var status [1]byte
		_, readErr := io.ReadFull(c, status[:])
		if readErr == nil && status[0] == 2 {
			if _, err := c.Write([]byte{3}); err != nil {
				c.Close()
				t.Fatal(err)
			}
			_, readErr = io.ReadFull(c, status[:])
		}
		c.Close()
		if readErr != nil || status[0] == 0 {
			t.Fatalf("peer %s at hop %d reported status %d (%v)", tc.source, tc.hop, status[0], readErr)
		}
	}
	if lookups.Load() != 0 {
		t.Fatal("rejected route still resolved a hostname")
	}
}

func TestUDPAssociationWaitsForExitStatus(t *testing.T) {
	upstream, local := listenTCP(t), listenTCP(t)
	withinTest(t, func(ctx context.Context) error {
		return Proxy(via(upstream, "127.0.0.2"), route).Serve(ctx, local)
	})
	withinTest(t, func(context.Context) error {
		c, err := upstream.Accept()
		if err != nil {
			return err
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		_, _, err = ReadInvocation(c)
		// Simulate an exit that failed before allocation and never sent status.
		return err
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if u, err := socks.NewClient(local.Addr().String()).DialUDP(ctx); err == nil {
		u.Close()
		t.Fatal("UDP association succeeded without exit status")
	}
}

func TestMissingAuthorizationLifetimeFailsClosed(t *testing.T) {
	exit, local := listenTCP(t), listenTCP(t)
	released := make(chan struct{}, 1)
	withinTest(t, func(ctx context.Context) error {
		return Serve(ctx, exit, authorizeTest("127.0.0.2", 0, Decision{
			Release: func() { released <- struct{}{} },
		}))
	})
	withinTest(t, func(ctx context.Context) error {
		return Proxy(via(exit, "127.0.0.2"), route).Serve(ctx, local)
	})
	if u, err := socks.NewClient(local.Addr().String()).DialUDP(t.Context()); err == nil {
		u.Close()
		t.Fatal("UDP association started without an authorization lifetime")
	}
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("invalid decision did not release its authorization")
	}
}

func TestActiveUDPAssociationAppliesUpdatedDestinationPolicy(t *testing.T) {
	exit, local := listenTCP(t), listenTCP(t)
	wan := listenUDP(t)
	other, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 4)})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	otherIP := netip.MustParseAddr("127.0.0.4")
	var blocked atomic.Bool
	withinTest(t, func(ctx context.Context) error {
		return serve(ctx, exit, authorizeTest("127.0.0.2", 0, Decision{
			Life: ctx,
			AllowDestination: func(ip netip.Addr) bool {
				return ip != loop || !blocked.Load()
			},
		}), exitPolicy{allow: func(ip netip.Addr) bool { return ip == loop || ip == otherIP }})
	})
	withinTest(t, func(ctx context.Context) error {
		return proxy(via(exit, "127.0.0.2"), nil, route, func(ip netip.Addr) bool {
			return ip == loop || ip == otherIP
		}).Serve(ctx, local)
	})
	u, err := socks.NewClient(local.Addr().String()).DialUDP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	var b [32]byte
	if _, err := u.WriteTo([]byte("before"), wan.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	_ = wan.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, from, err := wan.ReadFromUDP(b[:])
	if err != nil || string(b[:n]) != "before" {
		t.Fatalf("initial UDP request: %q (%v)", b[:n], err)
	}
	blocked.Store(true)
	if _, err := wan.WriteToUDP([]byte("late prohibited reply"), from); err != nil {
		t.Fatal(err)
	}
	if _, err := u.WriteTo([]byte("prohibited request"), wan.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	if _, err := u.WriteTo([]byte("still allowed"), other.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	_ = other.SetDeadline(time.Now().Add(3 * time.Second))
	n, from, err = other.ReadFromUDP(b[:])
	if err != nil || string(b[:n]) != "still allowed" {
		t.Fatalf("allowed UDP request after update: %q (%v)", b[:n], err)
	}
	if _, err := other.WriteToUDP([]byte("allowed reply"), from); err != nil {
		t.Fatal(err)
	}
	_ = u.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _, err = u.ReadFrom(b[:])
	if err != nil || string(b[:n]) != "allowed reply" {
		t.Fatalf("client received %q instead of allowed reply (%v)", b[:n], err)
	}
	_ = u.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, _, err := u.ReadFrom(b[:]); err == nil {
		t.Fatal("client received a late reply from a newly prohibited address")
	}
	_ = wan.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, _, err := wan.ReadFromUDP(b[:]); err == nil {
		t.Fatal("newly prohibited address received another datagram")
	}
}
