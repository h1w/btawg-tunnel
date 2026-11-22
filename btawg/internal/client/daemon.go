package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"bittorrent-tunnel/btawg/internal/config"
)

type ControlRequest struct {
	Op           string        `json:"op"`
	Mode         string        `json:"mode,omitempty"`
	Balancer     string        `json:"balancer,omitempty"`
	Enabled      []string      `json:"enabled,omitempty"`
	Port         int           `json:"port,omitempty"`
	RouteID      string        `json:"route_id,omitempty"`
	Peer         *PeerSettings `json:"peer,omitempty"`
	PeerRevision string        `json:"peer_revision,omitempty"`
}

type ControlResponse struct {
	OK                      bool                `json:"ok"`
	Enabled                 []string            `json:"enabled"`
	SOCKSBasePort           int                 `json:"socks_base_port,omitempty"`
	Balancers               map[string][]string `json:"balancers,omitempty"`
	AdaptiveMode            string              `json:"adaptive_mode,omitempty"`
	WindowsSOCKSQueueStalls uint64              `json:"windows_socks_queue_stalls,omitempty"`
	Credentials             *Credentials        `json:"credentials,omitempty"`
	Routes                  []RouteOption       `json:"routes,omitempty"`
	SelectedRoutes          map[int]string      `json:"selected_routes,omitempty"`
	P2P                     bool                `json:"p2p"`
	AutoMap                 bool                `json:"auto_map"`
	ServeEgress             bool                `json:"serve_egress"`
	PeerPort                uint16              `json:"peer_port,omitempty"`
	PeerEndpoint            string              `json:"peer_endpoint,omitempty"`
	Peer                    PeerSettings        `json:"peer"`
	PeerRevision            string              `json:"peer_revision,omitempty"`
	Error                   string              `json:"error,omitempty"`
}

// PortBase returns the first SOCKS5 listen port, including for older daemons.
func (r ControlResponse) PortBase() int { return socksPortBase(r.SOCKSBasePort) }

// RouteID returns the saved egress for port, or the implicit hub default.
func (r ControlResponse) RouteID(port int) string {
	return selectedRouteID(r.SelectedRoutes, port)
}

type clientControl struct {
	m    *Managed
	path string
	mu   sync.Mutex
	cfg  ManagedConfig
}

func RunManaged(ctx context.Context, cfg ManagedConfig, realTUN bool, tunName string, ready func() error) error {
	if err := ValidateSOCKSBasePort(cfg.PortBase()); err != nil {
		return err
	}
	return runManagedAt(ctx, cfg, ManagedConfigPath, ControlSocketPath, realTUN, tunName, cfg.PortBase(), ready)
}

func runManagedAt(ctx context.Context, cfg ManagedConfig, cfgPath, socketPath string, realTUN bool, tunName string, portBase int, ready func() error) error {
	if err := cfg.validatePeerSettings(realTUN); err != nil {
		return err
	}
	importCfg, err := config.ParseImportString(cfg.Import)
	if err != nil {
		return err
	}
	listenIP, err := netip.ParseAddr(cfg.ListenIP)
	if err != nil || !listenIP.Is4() {
		return fmt.Errorf("invalid SOCKS5 listen IPv4 address")
	}
	ln, err := listenControl(socketPath)
	if err != nil {
		return err
	}
	defer ln.Close()
	m, err := newManagedWithPeer(ctx, importCfg, cfg.Enabled, listenIP, cfg.Credentials, realTUN, tunName, portBase, cfg.Balancers, cfg.Routes, cfg.P2P, cfg.Peer)
	if err != nil {
		return err
	}
	if m.shared != nil {
		// A lost kernel device must fail the service rather than leave SOCKS
		// listeners accepting connections that can never forward traffic.
		watchCtx, stopWatch := context.WithCancel(ctx)
		watchDone := make(chan struct{})
		go func() {
			defer close(watchDone)
			select {
			case <-m.shared.done:
				ln.Close()
			case <-watchCtx.Done():
			}
		}()
		defer func() { stopWatch(); <-watchDone }()
	}
	defer m.Close()
	m.applyRoutes(cfg.Routes)
	m.startNetwork(cfg.P2P, cfg.AutoMap, cfg.AllowPermanentUPnP, cfg.ServeEgress, cfg.PeerPort, cfg.PeerEndpoint, cfg.PeerTCPEndpoint)
	d := &clientControl{m: m, path: cfgPath, cfg: cfg}
	if ready != nil {
		if err := ready(); err != nil {
			return err
		}
	}
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if m.shared != nil {
				select {
				case <-m.shared.done:
					return fmt.Errorf("read client TUN: %w", m.shared.readErr)
				default:
				}
			}
			return err
		}
		workers.Go(func() { d.serve(ctx, c) })
	}
}

func (d *clientControl) serve(ctx context.Context, c net.Conn) {
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	var req ControlRequest
	if json.NewDecoder(io.LimitReader(c, 64<<10)).Decode(&req) != nil {
		return
	}
	_ = json.NewEncoder(c).Encode(d.handle(req))
}

func (d *clientControl) handle(req ControlRequest) ControlResponse {
	d.mu.Lock()
	defer d.mu.Unlock()
	next := d.cfg
	options := d.m.RouteOptions()
	if err := next.update(req, options, true); err != nil {
		return ControlResponse{Error: err.Error()}
	}
	if req.Op == "set-peer" {
		if err := next.Save(d.path); err != nil {
			return ControlResponse{Error: err.Error()}
		}
		d.m.applyPeerSettings(next.Peer)
		d.cfg = next
	} else if req.Op == "set-route" {
		if err := next.Save(d.path); err != nil {
			return ControlResponse{Error: err.Error()}
		}
		d.m.applyRoutes(next.Routes)
		d.cfg = next
	} else if req.mutates() {
		if err := d.m.apply(next.Enabled, next.Credentials, next.Balancers, func() error { return next.Save(d.path) }); err != nil {
			return ControlResponse{Error: err.Error()}
		}
		d.cfg = next
	}
	r := next.response(req.Op)
	r.Routes = options
	b := &d.m.balance[slices.Index(balancerPolicies[:], adaptivePolicy)]
	b.mu.Lock()
	if path := b.current.tunnel; path != nil && path.c != nil && b.routeSet {
		if current, score := path.adaptiveScore(); current == b.current && score > 0 {
			r.AdaptiveMode = path.c.cfg.Transport
		}
	}
	b.mu.Unlock()
	if d.m.shared != nil {
		r.WindowsSOCKSQueueStalls = d.m.shared.overlayQueueStalls.Load()
	}
	return r
}

func (r ControlRequest) mutates() bool {
	switch r.Op {
	case "apply", "toggle", "rotate", "set-balancer", "set-route", "set-peer":
		return true
	}
	return false
}

func (c *ManagedConfig) update(req ControlRequest, options []RouteOption, running bool) error {
	switch req.Op {
	case "status", "show":
	case "set-peer":
		if req.Peer == nil {
			return errors.New("peer settings required")
		}
		peer := *req.Peer
		if req.PeerRevision == "" {
			// A dashboard still running after an upgrade can only edit defaults.
			peer.Overrides = c.Peer.Overrides
		} else if req.PeerRevision != peerRevision(c.Peer) {
			return errors.New("peer settings changed; reopen the editor and try again")
		}
		if err := peer.validate(); err != nil {
			return err
		}
		c.Peer = peer
	case "apply":
		for _, m := range req.Enabled {
			if !slices.Contains(config.ClientModes(), m) {
				return fmt.Errorf("unsupported transport %q", m)
			}
		}
		c.Enabled = req.Enabled
	case "toggle":
		if !slices.Contains(config.ClientModes(), req.Mode) {
			return fmt.Errorf("unsupported transport %q", req.Mode)
		}
		modes := slices.Clone(c.Enabled)
		if i := slices.Index(modes, req.Mode); i >= 0 {
			modes = slices.Delete(modes, i, i+1)
		} else {
			modes = append(modes, req.Mode)
		}
		c.Enabled = modes
	case "set-route":
		offset := req.Port - c.PortBase()
		if offset < 0 || offset >= socksPortCount() {
			return fmt.Errorf("unknown SOCKS5 port %d", req.Port)
		}
		slot := ManagedPortBase + offset
		current := selectedRouteID(c.Routes, slot)
		if req.RouteID == "" || running && !slices.ContainsFunc(options, func(option RouteOption) bool { return option.ID == req.RouteID }) {
			return fmt.Errorf("egress route %q is not available", req.RouteID)
		}
		if !running && req.RouteID != current && req.RouteID != HubRouteID {
			return errors.New("start the service to select another egress route")
		}
		if req.RouteID != current {
			routes := make(map[int]string, len(c.Routes)+1)
			maps.Copy(routes, c.Routes)
			if req.RouteID == HubRouteID {
				delete(routes, slot)
			} else {
				routes[slot] = req.RouteID
			}
			c.Routes = routes
		}
	case "set-balancer":
		if !slices.Contains(balancerPolicies[:], req.Balancer) {
			return fmt.Errorf("unknown SOCKS5 balancer %q", req.Balancer)
		}
		modes := config.ClientModes()
		for _, mode := range req.Enabled {
			if !slices.Contains(modes, mode) {
				return fmt.Errorf("unsupported transport %q", mode)
			}
		}
		order := modes
		if req.Balancer == failoverPolicy || req.Balancer == adaptivePolicy {
			order = req.Enabled
		}
		pool := make([]string, 0, min(len(req.Enabled), len(modes)))
		for _, mode := range order {
			if slices.Contains(req.Enabled, mode) && !slices.Contains(pool, mode) {
				pool = append(pool, mode)
			}
		}
		pools := make(map[string][]string, len(c.Balancers)+1)
		maps.Copy(pools, c.Balancers)
		pools[req.Balancer] = pool
		c.Balancers = pools
	case "rotate":
		creds, err := GenerateCredentials()
		if err != nil {
			return err
		}
		c.Credentials = creds
	default:
		return fmt.Errorf("unknown operation %q", req.Op)
	}
	return nil
}

func peerRevision(p PeerSettings) string {
	data, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (c ManagedConfig) response(op string) ControlResponse {
	r := ControlResponse{
		OK: true, Enabled: c.Enabled, SOCKSBasePort: c.PortBase(), Balancers: c.Balancers,
		P2P: c.P2P, AutoMap: c.AutoMap, ServeEgress: c.ServeEgress,
		PeerPort: c.PeerPort, PeerEndpoint: c.PeerEndpoint,
		Peer: c.Peer,
	}
	if r.SOCKSBasePort == ManagedPortBase {
		r.SelectedRoutes = c.Routes
	} else if len(c.Routes) > 0 {
		r.SelectedRoutes = make(map[int]string, len(c.Routes))
		for slot, route := range c.Routes {
			r.SelectedRoutes[r.SOCKSBasePort+(slot-ManagedPortBase)] = route
		}
	}
	if op == "status" {
		r.PeerRevision = peerRevision(c.Peer)
	}
	if op == "show" || op == "rotate" {
		r.Credentials = &c.Credentials
	}
	return r
}

var ErrNotRunning = errors.New("btawg-client control socket is not listening")

func request(path string, req ControlRequest) (ControlResponse, error) {
	var r ControlResponse
	c, err := dialControl(path)
	if err != nil {
		return r, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return r, err
	}
	if err := json.NewDecoder(c).Decode(&r); err != nil {
		return r, err
	}
	if !r.OK {
		return r, errors.New(r.Error)
	}
	return r, nil
}
