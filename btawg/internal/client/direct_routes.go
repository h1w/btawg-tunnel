package client

import (
	"net/netip"
	"slices"
	"time"

	"bittorrent-tunnel/btawg/internal/config"
)

var peerModeOrder = config.ClientModes()

func (s *sharedTUN) enableDirect(cfg config.ClientConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.directWanted != nil {
		return
	}
	s.direct = make(map[netip.Addr]*peerPathGroup)
	s.directGateways = map[netip.Addr]bool{netip.MustParseAddr(cfg.ServerIP): true}
	s.directOwn = netip.MustParseAddr(cfg.ClientIP)
	s.directWanted = make(chan netip.Addr, 64)
	s.peerFlows = make(map[peerTuple]peerFlow)
	s.peerFragments = make(map[fragmentID]fragmentPath)
	s.peerRequests = make(map[netip.Addr]time.Time)
	for _, raw := range cfg.Routes {
		if p, err := netip.ParsePrefix(raw); err == nil && p.Bits() > 0 {
			s.directRanges = append(s.directRanges, p)
		}
	}
	if p, err := netip.ParsePrefix(cfg.Subnet); err == nil {
		s.directRanges = append(s.directRanges, p)
	}
	s.peerEnabled.Store(true)
}

func (s *sharedTUN) applyPeerPolicies(policies *peerPolicies) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peerPolicies = policies
	for k, f := range s.peerFlows {
		if !s.peerPathAllowedLocked(netip.AddrFrom4(k.dst), f.path) {
			delete(s.peerFlows, k)
		}
	}
	for ip, g := range s.direct {
		s.updatePeerPoolLocked(ip, g)
	}
}

func (s *sharedTUN) setPeerNetwork(gateways []netip.Addr, prefixes []netip.Prefix) {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.directGateways)
	for _, addr := range gateways {
		s.directGateways[addr] = true
	}
	s.peerNetwork = slices.Clone(prefixes)
}

func (s *sharedTUN) directPath(ip netip.Addr) *tunPath {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pickPeerLocked(ip)
}

func (s *sharedTUN) wantDirect(ip netip.Addr) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.directWanted == nil || len(s.peerPolicies.forPeer(ip).Outgoing) == 0 {
		return
	}
	select {
	case s.directWanted <- ip:
	default:
	}
}

func (s *sharedTUN) setDirect(ip netip.Addr, p *tunPath) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !p.peerOutbound {
		return
	}
	i := peerModeIndex(p.peerMode)
	if i < 0 {
		return
	}
	g := s.direct[ip]
	if g == nil {
		g = &peerPathGroup{}
		s.direct[ip] = g
	}
	g.constSize[i] = p
	s.updatePeerPoolLocked(ip, g)
}

func (s *sharedTUN) removeDirect(ip netip.Addr, p *tunPath) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.direct[ip]
	if i := peerModeIndex(p.peerMode); g != nil && i >= 0 && g.constSize[i] == p {
		g.constSize[i] = nil
		s.updatePeerPoolLocked(ip, g)
		if len(g.policy.paths) == 0 {
			empty := true
			for _, v := range g.constSize {
				if v != nil {
					empty = false
					break
				}
			}
			if empty {
				delete(s.direct, ip)
			}
		}
	}
}

func peerModeIndex(mode string) int {
	for i, n := range peerModeOrder {
		if n == mode {
			return i
		}
	}
	return -1
}

func (s *sharedTUN) updatePeerPoolLocked(ip netip.Addr, g *peerPathGroup) {
	settings := s.peerPolicies.forPeer(ip)
	policy := settings.selection
	if peerModeIndex(policy) >= 0 {
		policy = failoverPolicy
	}
	if g.policy.policy != policy {
		g.policy.policy = policy
		g.policy.current, g.policy.challenger = adaptivePath{}, adaptivePath{}
		g.policy.next = 0
	}
	var paths [len(g.constSize)]*managedTunnel
	selected := paths[:0]
	for _, mode := range settings.Outgoing {
		if i := peerModeIndex(mode); i >= 0 && g.constSize[i] != nil {
			selected = append(selected, g.constSize[i].peerHealth)
		}
	}
	if slices.Equal(g.policy.paths, selected) {
		return
	}
	clear(g.selected[len(selected):])
	copy(g.selected[:], selected)
	// Both peer pool replacement and selection are serialized by s.mu.
	g.policy.paths = g.selected[:len(selected)]
	g.policy.current, g.policy.challenger = adaptivePath{}, adaptivePath{}
	if g.policy.next >= len(selected) {
		g.policy.next = 0
	}
}

func (s *sharedTUN) pickPeerLocked(ip netip.Addr) *tunPath {
	g := s.direct[ip]
	if g == nil {
		return nil
	}
	if member := g.policy.pick(); member != nil && member.c.path.validDirect() {
		return member.c.path
	}
	return nil
}

func (s *sharedTUN) readyPeerLocked(ip netip.Addr) bool {
	if g := s.direct[ip]; g != nil {
		for _, m := range g.policy.paths {
			if m.score.Load() > 0 && m.c.path.validDirect() {
				return true
			}
		}
	}
	return false
}

func (s *sharedTUN) probePeer(ip netip.Addr, p *tunPath, score time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.peerHealth.score.Store(int64(score))
	if g := s.direct[ip]; g != nil {
		g.policy.probed(p.peerHealth, nil)
	}
}

func (p *tunPath) validDirect() bool {
	select {
	case <-p.done:
		return false
	default:
	}
	return p.directLife == nil || (p.directLife.Err() == nil && time.Now().UnixNano() < p.directUntil.Load())
}

func (s *sharedTUN) peerPathAllowedLocked(ip netip.Addr, p *tunPath) bool {
	if !p.validDirect() {
		return false
	}
	settings := s.peerPolicies.forPeer(ip)
	if p.directLife == nil {
		return settings.relay
	}
	if p.peerOutbound {
		return slices.Contains(settings.Outgoing, p.peerMode)
	}
	return slices.Contains(settings.Incoming, p.peerMode)
}

func (s *sharedTUN) enqueuePacket(path *tunPath, storage *[]byte, buf, packet []byte) enqueueResult {
	if !s.peerEnabled.Load() {
		return path.enqueue(storage, buf)
	}
	dst := netip.AddrFrom4([4]byte(packet[16:20]))
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.shouldRoutePeerLocked(dst) && !s.peerPathAllowedLocked(dst, path) {
		return enqueueRejected
	}
	return path.enqueue(storage, buf)
}
