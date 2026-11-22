package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/mesh"
	"bittorrent-tunnel/btawg/internal/routeproxy"
	"bittorrent-tunnel/btawg/internal/socks"
	"bittorrent-tunnel/btawg/internal/tunsetup"
)

type catalogue struct {
	routes   []mesh.Route
	gateways []netip.Addr
	until    time.Time
}

type adaptiveRoute struct {
	id       string
	identity [32]byte
}

// The hub-authenticated catalogue contains a signed, readable grant. Its
// payload identifies the ordered chain; expiration and MAC change on renewal.
// This decoding is only for local measurement scope, never authorization.
func adaptiveRouteIdentity(id string, catalog *catalogue) adaptiveRoute {
	route := adaptiveRoute{id: id}
	if id == mesh.HubRoute || catalog == nil {
		return route
	}
	for _, item := range catalog.routes {
		if item.ID != id || item.Grant == "" {
			continue
		}
		if len(item.Grant) > 2048 {
			route.identity = sha256.Sum256([]byte(item.Grant))
			return route
		}
		decoded, err := base64.RawURLEncoding.Strict().DecodeString(item.Grant)
		if err != nil || len(decoded) <= sha256.Size || len(decoded) > 2048 {
			route.identity = sha256.Sum256([]byte(item.Grant))
			return route
		}
		var grant mesh.Grant
		if json.Unmarshal(decoded[:len(decoded)-sha256.Size], &grant) != nil || grant.RouteID != id || len(grant.Hops) < 1 || len(grant.Hops) > 2 {
			route.identity = sha256.Sum256([]byte(item.Grant))
			return route
		}
		body, _ := json.Marshal(grant.Hops)
		route.identity = sha256.Sum256(body)
		return route
	}
	return route
}

func adaptiveRouteFor(selection *map[int]string, catalog *catalogue) adaptiveRoute {
	id := mesh.HubRoute
	if selection != nil {
		id = selectedRouteID(*selection, ManagedPortBase+len(config.ClientModes())+2)
	}
	return adaptiveRouteIdentity(id, catalog)
}

type meshNode struct {
	owner     *Managed
	ctx       context.Context
	catalogue atomic.Pointer[catalogue]
	paths     atomic.Pointer[[]*managedTunnel]
	selected  atomic.Pointer[map[int]string]
	peer      atomic.Pointer[peerPolicies]
}

func (m *Managed) RouteOptions() []RouteOption {
	catalog := m.network.catalogue.Load()
	if catalog == nil || time.Now().After(catalog.until) {
		return nil
	}
	opts := make([]RouteOption, 0, len(catalog.routes))
	for _, r := range catalog.routes {
		opts = append(opts, RouteOption{ID: r.ID, Name: r.Name})
	}
	return opts
}

func (m *Managed) applyRoutes(routes map[int]string) {
	b := &m.balance[slices.Index(balancerPolicies[:], adaptivePolicy)]
	b.mu.Lock()
	m.network.selected.Store(&routes)
	b.setRouteLocked(adaptiveRouteFor(&routes, m.network.catalogue.Load()))
	b.mu.Unlock()
}

func (m *Managed) applyPeerSettings(cfg PeerSettings) {
	policies := compilePeerPolicies(cfg)
	if m.shared != nil {
		m.shared.applyPeerPolicies(policies)
	}
	m.network.peer.Store(policies)
}

func (m *Managed) startNetwork(p2p, autoMap, allowPermanentUPnP, serve bool, port uint16, endpoint, tcpEndpoint string) {
	m.network.start(p2p, autoMap, allowPermanentUPnP, serve, port, endpoint, tcpEndpoint)
}

func (n *meshNode) selectProxy(port int, path *managedTunnel) (*socks.Server, context.Context) {
	return n.selectProxySnapshot(port, path, n.selected.Load(), n.catalogue.Load(), nil)
}

func (n *meshNode) selectProxySnapshot(port int, path *managedTunnel, selection *map[int]string, c *catalogue, relayed *atomic.Bool) (*socks.Server, context.Context) {
	if path == nil || path.life.Err() != nil {
		return nil, nil
	}
	id := mesh.HubRoute
	if selection != nil {
		if v := (*selection)[port]; v != "" {
			id = v
		}
	}
	if id == mesh.HubRoute {
		return path.proxy, path.life
	}
	if c == nil || !time.Now().Before(c.until) {
		return nil, nil
	}
	for _, r := range c.routes {
		if r.ID != id || r.Grant == "" {
			continue
		}
		req := routeproxy.Request{RouteID: r.ID, Grant: r.Grant}
		return routeproxy.ProxyWithFallback(func(ctx context.Context, _ routeproxy.Request) (net.Conn, error) {
			return n.dialDirectRoute(ctx, r.First)
		}, func(ctx context.Context, req routeproxy.Request) (net.Conn, error) {
			conn, err := n.dialRouteRelay(ctx, path, req)
			if err == nil && relayed != nil {
				relayed.Store(true)
			}
			return conn, err
		}, req), path.life
	}
	return nil, nil
}

func (n *meshNode) path() *managedTunnel {
	paths := n.paths.Load()
	if paths == nil {
		return nil
	}
	for _, p := range *paths {
		if p.life.Err() == nil && (!p.carrierMode || p.carrier.Load() != nil) {
			return p
		}
	}
	return nil
}

func (n *meshNode) exchange(ctx context.Context, req mesh.Request) (mesh.Reply, error) {
	var reply mesh.Reply
	paths := n.paths.Load()
	if paths == nil {
		return reply, errors.New("no active hub tunnel")
	}
	var last error = errors.New("no active hub tunnel")
	for _, p := range *paths {
		if ctx.Err() != nil {
			return reply, ctx.Err()
		}
		if p.life.Err() != nil || p.carrierMode && p.carrier.Load() == nil {
			continue
		}
		try, cancel := context.WithTimeout(ctx, 900*time.Millisecond)
		reply, last = n.exchangeOn(try, p, req)
		cancel()
		if last == nil || errors.Is(last, errMeshDenied) {
			return reply, last
		}
	}
	return mesh.Reply{}, last
}

var errMeshDenied = errors.New("mesh authorization denied")

func (n *meshNode) exchangeOn(ctx context.Context, p *managedTunnel, msg mesh.Request) (mesh.Reply, error) {
	var reply mesh.Reply
	body, err := json.Marshal(msg)
	if err != nil {
		return reply, err
	}
	ap := netip.AddrPortFrom(netip.MustParseAddr(n.owner.cfg.ServerIP), mesh.ControlPort)
	c, err := p.c.proxyTCP(ctx, ap)
	if err != nil {
		return reply, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+ap.String()+mesh.Path, bytes.NewReader(body))
	if err != nil {
		return reply, err
	}
	request.Close = true
	request.Header.Set("Content-Type", "application/json")
	if err := request.Write(c); err != nil {
		return reply, err
	}
	response, err := http.ReadResponse(bufio.NewReader(c), request)
	if err != nil {
		return reply, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusForbidden {
		return reply, errMeshDenied
	}
	if response.StatusCode != http.StatusOK {
		return reply, fmt.Errorf("mesh control HTTP %d", response.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, mesh.MaxReply+1))
	if err != nil {
		return reply, err
	}
	if len(b) > mesh.MaxReply {
		return reply, errors.New("mesh reply too large")
	}
	err = json.Unmarshal(b, &reply)
	return reply, err
}

func (n *meshNode) dialDirectRoute(ctx context.Context, target netip.Addr) (net.Conn, error) {
	shared := n.owner.shared
	if shared == nil || !target.Is4() || ctx.Err() != nil {
		return nil, net.ErrClosed
	}
	shared.wantDirect(target)
	p := shared.directPath(target)
	if p == nil || p.directLife.Err() != nil {
		return nil, net.ErrClosed
	}
	var c Client
	if err := p.proxySockets(&c, netip.MustParseAddr(n.owner.cfg.ClientIP)); err != nil {
		return nil, err
	}
	conn, err := c.proxyTCP(ctx, netip.AddrPortFrom(target, mesh.EgressPort))
	if err != nil {
		return nil, err
	}
	return routeproxy.BindConn(p.directLife, conn), nil
}

func (n *meshNode) dialRouteRelay(ctx context.Context, path *managedTunnel, request routeproxy.Request) (net.Conn, error) {
	if path == nil || path.life.Err() != nil || ctx.Err() != nil {
		return nil, net.ErrClosed
	}
	gateway := netip.AddrPortFrom(netip.MustParseAddr(n.owner.cfg.ServerIP), mesh.RelayPort)
	c, err := path.c.proxyTCP(ctx, gateway)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if until, ok := ctx.Deadline(); ok {
		if err = c.SetDeadline(until); err != nil {
			c.Close()
			return nil, err
		}
	}
	err = mesh.WriteRelay(c, mesh.Check{ID: request.RouteID, Grant: request.Grant, Hop: request.Hop})
	if err == nil {
		var status [1]byte
		_, err = io.ReadFull(c, status[:])
		if err == nil && status[0] != 0 {
			err = errMeshDenied
		}
	}
	if err == nil {
		err = c.SetDeadline(time.Time{})
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (n *meshNode) allowRoute(ctx context.Context, peer netip.Addr, request routeproxy.Request) (routeproxy.Decision, error) {
	// Check the source against hub state, including the complete ordered route.
	check := mesh.Check{ID: request.RouteID, Grant: request.Grant, Previous: peer, Hop: request.Hop}
	reply, err := n.exchange(ctx, mesh.Request{Check: &check})
	if err != nil {
		return routeproxy.Decision{}, err
	}
	if reply.Session == "" || len(reply.Prefixes) == 0 {
		return routeproxy.Decision{}, errMeshDenied
	}
	check.Session = reply.Session
	life, cancel := context.WithCancel(n.ctx)
	timer := time.AfterFunc(mesh.Lease, cancel)
	release := func() { timer.Stop(); cancel() }
	prefixes := reply.Prefixes
	go n.renew(life, release, timer, check, reply.Next, prefixes)
	d := routeproxy.Decision{Life: life, Release: release, AllowDestination: func(ip netip.Addr) bool {
		if life.Err() != nil {
			return false
		}
		for _, p := range prefixes {
			if p.Contains(ip) {
				return false
			}
		}
		return true
	}}
	if reply.Next.IsValid() {
		d.Next = func(ctx context.Context, _ routeproxy.Request) (net.Conn, error) {
			return n.dialDirectRoute(ctx, reply.Next)
		}
		d.Fallback = func(ctx context.Context, req routeproxy.Request) (net.Conn, error) {
			return n.dialRouteRelay(ctx, n.path(), req)
		}
	}
	return d, nil
}

func (n *meshNode) renew(ctx context.Context, cancel context.CancelFunc, timer *time.Timer, check mesh.Check, next netip.Addr, prefixes []netip.Prefix) {
	defer cancel()
	check.Renew = true
	until := time.Now().Add(mesh.Lease)
	tick := time.NewTicker(mesh.Poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		try, stop := context.WithDeadline(ctx, minTime(until, time.Now().Add(mesh.Poll)))
		reply, err := n.exchange(try, mesh.Request{Check: &check})
		stop()
		if errors.Is(err, errMeshDenied) || !time.Now().Before(until) || err == nil && (reply.Next != next || reply.Session != check.Session || len(reply.Prefixes) == 0) {
			return
		}
		if err == nil {
			if !slices.Equal(prefixes, reply.Prefixes) {
				return
			}
			// On prefix changes every stream re-admits against the new list;
			// a live TCP stream cannot revalidate its already dialed destination.
			if !timer.Reset(mesh.Lease) || ctx.Err() != nil {
				return
			}
			until = time.Now().Add(mesh.Lease)
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (n *meshNode) runExit(ctx context.Context, path *managedTunnel, running *atomic.Int32) error {
	ap := netip.AddrPortFrom(netip.MustParseAddr(n.owner.cfg.ClientIP), mesh.EgressPort)
	var ln net.Listener
	var err error
	if n.owner.shared == nil {
		ln, err = path.c.ns.ListenTCPAddrPort(ap)
	} else {
		lc := net.ListenConfig{Control: tunsetup.BindOwnedDevice(n.owner.shared.dev, n.owner.shared.name)}
		ln, err = lc.Listen(ctx, "tcp4", ap.String())
	}
	if err != nil {
		return err
	}
	defer ln.Close()
	running.Add(1)
	if n.owner.shared != nil {
		n.owner.shared.servicePath.Store(path.c.path)
		defer n.owner.shared.servicePath.CompareAndSwap(path.c.path, nil)
	}
	defer running.Add(-1)
	var blocked []netip.Prefix
	for _, v := range append(slices.Clone(n.owner.cfg.Routes), n.owner.cfg.Subnet) {
		if p, err := netip.ParsePrefix(v); err == nil && p.Bits() > 0 {
			blocked = append(blocked, p)
		}
	}
	return routeproxy.ServeWithBlockedPrefixes(ctx, ln, n.allowRoute, blocked)
}

func (n *meshNode) start(p2p, autoMap, allowPermanentUPnP, serve bool, port uint16, endpoint, tcpEndpoint string) {
	n.owner.networkWG.Go(func() {
		var exitRunning atomic.Int32
		if serve {
			n.owner.networkWG.Go(func() { n.serveExit(&exitRunning) })
		}
		if p2p && n.owner.shared != nil {
			n.owner.networkWG.Go(n.servePeerProbe)
		}
		n.run(p2p, autoMap, allowPermanentUPnP, port, endpoint, tcpEndpoint, &exitRunning)
	})
}

func (n *meshNode) serveExit(running *atomic.Int32) {
	var workers sync.WaitGroup
	defer workers.Wait()
	active := make(map[*managedTunnel]chan struct{})
	for n.ctx.Err() == nil {
		for p, done := range active {
			select {
			case <-done:
				delete(active, p)
			default:
			}
		}
		paths := n.paths.Load()
		if paths != nil {
			for _, p := range *paths {
				if p.life.Err() != nil || active[p] != nil {
					continue
				}
				if n.owner.shared != nil && len(active) != 0 {
					break
				}
				done := make(chan struct{})
				active[p] = done
				workers.Go(func() {
					defer close(done)
					ctx, cancel := context.WithCancel(n.ctx)
					defer cancel()
					stop := context.AfterFunc(p.life, cancel)
					defer stop()
					if err := n.runExit(ctx, p, running); err != nil && ctx.Err() == nil {
						log.Printf("btawg-client: exit listener: %v", err)
					}
				})
			}
		}
		select {
		case <-n.ctx.Done():
			return
		case <-time.After(mesh.Poll):
		}
	}
}

func (n *meshNode) register(reg mesh.Registration) (mesh.Reply, error) {
	ctx, cancel := context.WithTimeout(n.ctx, mesh.Poll)
	defer cancel()
	reply, err := n.exchange(ctx, mesh.Request{Registration: &reg})
	if err == nil {
		b := &n.owner.balance[slices.Index(balancerPolicies[:], adaptivePolicy)]
		b.mu.Lock()
		catalog := &catalogue{reply.Routes, reply.Gateways, time.Now().Add(mesh.Lease)}
		n.catalogue.Store(catalog)
		b.setRouteLocked(adaptiveRouteFor(n.selected.Load(), catalog))
		b.mu.Unlock()
	}
	if errors.Is(err, errMeshDenied) {
		b := &n.owner.balance[slices.Index(balancerPolicies[:], adaptivePolicy)]
		b.mu.Lock()
		n.catalogue.Store(nil)
		b.setRouteLocked(adaptiveRouteFor(n.selected.Load(), nil))
		b.mu.Unlock()
	}
	return reply, err
}
