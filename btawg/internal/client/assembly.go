package client

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/socks"
)

type managedTunnel struct {
	c           *Client
	proxy       *socks.Server
	life        context.Context
	stop        context.CancelFunc
	done        chan struct{}
	score       atomic.Int64 // nanoseconds; zero means not eligible
	carrierMode bool
	carrier     atomic.Pointer[carrierHealth]
	stopProbe   context.CancelFunc
	probeDone   chan struct{}
}

// Managed runs independent data paths with fixed SOCKS ports and one VPN IP.
type Managed struct {
	inbound   *managed
	cfg       config.ClientConfig
	shared    *sharedTUN
	mu        sync.Mutex
	tunnels   []*managedTunnel
	fixed     []string
	pools     map[string][]string
	balance   [len(balancerPolicies)]balancer
	auth      Credentials
	closed    bool
	network   meshNode
	networkWG sync.WaitGroup
}

func NewManaged(ctx context.Context, cfg config.ClientConfig, enabled []string, listenIP netip.Addr, auth Credentials, realTUN bool, tunBase string) (*Managed, error) {
	return newManagedAt(ctx, cfg, enabled, listenIP, auth, realTUN, tunBase, ManagedPortBase, nil)
}

func newManagedAt(ctx context.Context, cfg config.ClientConfig, enabled []string, listenIP netip.Addr, auth Credentials, realTUN bool, tunBase string, portBase int, pools map[string][]string, routes ...map[int]string) (*Managed, error) {
	var selected map[int]string
	if len(routes) != 0 {
		selected = routes[0]
	}
	return newManagedWithPeer(ctx, cfg, enabled, listenIP, auth, realTUN, tunBase, portBase, pools, selected, false, PeerSettings{})
}

func newManagedWithPeer(ctx context.Context, cfg config.ClientConfig, enabled []string, listenIP netip.Addr, auth Credentials, realTUN bool, tunBase string, portBase int, pools map[string][]string, routes map[int]string, p2p bool, peer PeerSettings) (*Managed, error) {
	inbound, err := newManaged(ctx, listenIP, portBase)
	if err != nil {
		return nil, err
	}
	m := &Managed{inbound: inbound, cfg: cfg, tunnels: make([]*managedTunnel, len(config.ClientModes()))}
	m.network.owner, m.network.ctx = m, inbound.ctx
	m.applyRoutes(routes)
	for i, policy := range balancerPolicies {
		m.balance[i].policy = policy
	}
	if realTUN {
		shared, err := openSharedClientTUN(cfg, tunBase)
		if err != nil {
			inbound.Close()
			return nil, err
		}
		m.shared = shared
	}
	m.applyPeerSettings(peer)
	if m.shared != nil && p2p {
		m.shared.enableDirect(cfg)
	}
	if err := m.apply(enabled, auth, pools, nil); err != nil {
		m.Close()
		return nil, err
	}
	return m, nil
}

func (m *Managed) Apply(enabled []string, auth Credentials) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applyLocked(enabled, auth, m.pools, nil)
}

func (m *Managed) apply(enabled []string, auth Credentials, pools map[string][]string, save func() error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applyLocked(enabled, auth, pools, save)
}

// Prepare every requested path before persisting or changing live listeners.
// A failed start/write leaves existing sessions and config unchanged.
func (m *Managed) applyLocked(enabled []string, auth Credentials, pools map[string][]string, save func() error) error {
	if !auth.valid() {
		return fmt.Errorf("invalid SOCKS5 credentials")
	}
	modes := config.ClientModes()
	for _, mode := range enabled {
		if !slices.Contains(modes, mode) {
			return fmt.Errorf("unsupported transport %q", mode)
		}
	}
	var poolModes [len(balancerPolicies)][]string
	for policy, pool := range pools {
		if !slices.Contains(balancerPolicies[:], policy) {
			return fmt.Errorf("unknown SOCKS5 balancer %q", policy)
		}
		for _, mode := range pool {
			if !slices.Contains(modes, mode) {
				return fmt.Errorf("unsupported transport %q", mode)
			}
		}
	}
	for i, policy := range balancerPolicies {
		pool, explicit := pools[policy]
		if !explicit {
			pool = enabled
		}
		poolModes[i] = pool
	}
	usedByBalancer := func(mode string) bool {
		for _, pool := range poolModes {
			if slices.Contains(pool, mode) {
				return true
			}
		}
		return false
	}
	if m.closed {
		return fmt.Errorf("managed client closed")
	}
	pending := make([]*Client, len(modes))
	defer func() {
		for _, c := range pending {
			if c != nil {
				c.Close()
			}
		}
	}()
	for i, mode := range modes {
		if m.tunnels[i] != nil || (!slices.Contains(enabled, mode) && !usedByBalancer(mode)) {
			continue
		}
		cfg := m.cfg
		cfg.Transport = mode
		var c *Client
		var err error
		if m.shared != nil {
			path := m.shared.newPath()
			c = &Client{cfg: cfg, path: path}
			if err = path.proxySockets(c, netip.MustParseAddr(cfg.ClientIP)); err == nil {
				err = c.start(path)
			} else {
				_ = path.Close()
			}
		} else {
			c, err = New(cfg)
		}
		if err != nil {
			return fmt.Errorf("start %s: %w", mode, err)
		}
		pending[i] = c
	}
	if save != nil {
		if err := save(); err != nil {
			return err
		}
	}
	rotated := m.auth != auth
	adaptive := &m.balance[slices.Index(balancerPolicies[:], adaptivePolicy)]
	var primary *tunPath
	for i, mode := range modes {
		if c := pending[i]; c != nil {
			pending[i] = nil
			m.tunnels[i] = startManagedTunnel(m.inbound.ctx, c, mode)
		}
		tunnel := m.tunnels[i]
		fixed := slices.Contains(enabled, mode)
		wasFixed := slices.Contains(m.fixed, mode)
		if fixed {
			if primary == nil {
				primary = tunnel.c.path
			}
			if !wasFixed || rotated {
				m.inbound.replace(i, socks.NewBalancer(func() (*socks.Server, context.Context) {
					return m.network.selectProxy(ManagedPortBase+i, tunnel)
				}).WithCredentials(auth.Username, auth.Password))
			}
		} else if wasFixed {
			m.inbound.replace(i, nil)
		}
		if tunnel != nil && usedByBalancer(mode) {
			tunnel.watch(true, adaptive)
		}
	}
	if m.shared != nil {
		m.shared.primary.Store(primary)
	}
	for i, pool := range poolModes {
		paths := make([]*managedTunnel, 0, len(pool))
		policy := balancerPolicies[i]
		if _, custom := pools[policy]; custom && (policy == failoverPolicy || policy == adaptivePolicy) {
			for _, mode := range pool {
				tunnel := m.tunnels[slices.Index(modes, mode)]
				if !slices.Contains(paths, tunnel) {
					paths = append(paths, tunnel)
				}
			}
		} else {
			for j, mode := range modes {
				if slices.Contains(pool, mode) {
					paths = append(paths, m.tunnels[j])
				}
			}
		}
		b := &m.balance[i]
		b.replace(paths)
		if rotated {
			m.inbound.replace(len(modes)+i, socks.NewObservedBalancer(func(tcp bool) (*socks.Server, context.Context, func(bool, int64, time.Duration, time.Time)) {
				port := ManagedPortBase + len(modes) + i
				if b.policy != adaptivePolicy {
					proxy, life := m.network.selectProxy(port, b.pick())
					return proxy, life, nil
				}
				b.mu.Lock()
				route, catalog := m.network.selected.Load(), m.network.catalogue.Load()
				epoch := b.routeEpoch
				var path *managedTunnel
				if tcp {
					path = b.pickTCPLocked()
				} else {
					path = b.pickLocked()
				}
				var selected adaptivePath
				if path != nil {
					selected, _ = path.adaptiveScore()
				}
				b.mu.Unlock()
				if selected.tunnel == nil {
					return nil, nil, nil
				}
				if !tcp {
					proxy, life := m.network.selectProxySnapshot(port, path, route, catalog, nil)
					return proxy, life, nil
				}
				var relayed atomic.Bool
				proxy, life := m.network.selectProxySnapshot(port, path, route, catalog, &relayed)
				hub := route == nil || selectedRouteID(*route, port) == HubRouteID
				return proxy, life, func(upload bool, bytes int64, duration time.Duration, at time.Time) {
					if hub || relayed.Load() {
						b.recordScoped(selected, upload, bytes, duration, at, epoch)
					}
				}
			}).WithCredentials(auth.Username, auth.Password))
		}
	}
	for i, mode := range modes {
		t := m.tunnels[i]
		if t == nil || usedByBalancer(mode) {
			continue
		}
		if slices.Contains(enabled, mode) {
			t.watch(false, nil)
		} else {
			t.close()
			m.tunnels[i] = nil
		}
	}
	m.auth, m.fixed, m.pools = auth, slices.Clone(enabled), pools
	var paths []*managedTunnel
	for _, t := range m.tunnels {
		if t != nil {
			paths = append(paths, t)
		}
	}
	m.network.paths.Store(&paths)
	return nil
}

func startManagedTunnel(parent context.Context, c *Client, mode string) *managedTunnel {
	ctx, stop := context.WithCancel(parent)
	t := &managedTunnel{c: c, proxy: c.localProxy(), life: ctx, stop: stop, done: make(chan struct{}), carrierMode: config.IsCarrierMode(mode)}
	go func() {
		defer close(t.done)
		defer stop()
		for ctx.Err() == nil {
			err := c.runTunnel(ctx, &t.carrier)
			if ctx.Err() != nil {
				return
			}
			if err == nil || c.ip != nil && c.ip.life.Err() != nil {
				log.Printf("btawg-client: %s tunnel stopped: %v", mode, err)
				return
			}
			log.Printf("btawg-client: %s: %v", mode, err)
			timer := time.NewTimer(retryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return t
}

func (t *managedTunnel) close() {
	t.stop()
	t.watch(false, nil)
	<-t.done
	t.c.Close()
}

func (m *Managed) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	if m.shared != nil {
		m.shared.primary.Store(nil)
	}
	m.inbound.Close()
	m.networkWG.Wait()
	for _, t := range m.tunnels {
		if t != nil {
			t.close()
		}
	}
	if m.shared != nil {
		return m.shared.close()
	}
	return nil
}
