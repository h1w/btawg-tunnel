package client

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"slices"
	"sync"
	"time"

	"bittorrent-tunnel/btawg/internal/carrier"
	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/direct"
	"bittorrent-tunnel/btawg/internal/mesh"
	"bittorrent-tunnel/btawg/internal/nativepeer"
	"bittorrent-tunnel/btawg/internal/portmap"
)

type socketObserver interface {
	Port() uint16
	Observe(context.Context, netip.AddrPort) (netip.AddrPort, error)
}

type peerSocket struct {
	socketObserver
	mode     string
	receive  bool
	local    func() []mesh.Candidate
	mapped   []netip.AddrPort
	observed netip.AddrPort
}

type peerSockets struct {
	stream *direct.Socket
	// Multiple directional associations to the same peer cannot share a
	// native (unwrapped) UDP five-tuple. Use distinct receive/send ports.
	native map[string][2]*nativepeer.Socket
	all    []*peerSocket
	mu     sync.Mutex
	worker sync.WaitGroup
	cancel context.CancelFunc
}

func openPeerSockets(ctx context.Context, port uint16, autoMap, allowPermanentUPnP bool) (*peerSockets, error) {
	ctx, cancel := context.WithCancel(ctx)
	s := &peerSockets{native: make(map[string][2]*nativepeer.Socket), cancel: cancel}
	var err error
	s.stream, err = direct.ListenWithTCP(port, true)
	if err != nil {
		s.close()
		return nil, err
	}
	s.all = append(s.all, &peerSocket{socketObserver: s.stream, local: func() []mesh.Candidate {
		cs := s.stream.LocalCandidates()
		out := make([]mesh.Candidate, 0, len(cs))
		for _, c := range cs {
			out = append(out, mesh.Candidate{Addr: c.Addr, Kind: c.Kind, Protocol: c.Protocol})
		}
		return out
	}})
	for _, mode := range []string{config.ModeAWG, config.ModeWG} {
		var pair [2]*nativepeer.Socket
		for i := range pair {
			sock, err := nativepeer.Listen(0)
			if err != nil {
				s.native[mode] = pair
				s.close()
				return nil, fmt.Errorf("listen for %s: %w", mode, err)
			}
			pair[i] = sock
			s.all = append(s.all, &peerSocket{socketObserver: sock, mode: mode, receive: i == 1, local: func() []mesh.Candidate {
				var out []mesh.Candidate
				for _, a := range sock.LocalCandidates() {
					out = append(out, mesh.Candidate{Addr: a, Kind: candidateKind(a.Addr())})
				}
				return out
			}})
		}
		s.native[mode] = pair
	}
	if autoMap {
		for _, entry := range s.all {
			s.worker.Go(func() {
				if err := portmap.Run(ctx, entry.Port(), allowPermanentUPnP, func(aps []netip.AddrPort) {
					s.mu.Lock()
					entry.mapped = slices.Clone(aps)
					s.mu.Unlock()
				}); err != nil && ctx.Err() == nil {
					log.Printf("btawg-client: peer port mapping: %v", err)
				}
			})
		}
	}
	return s, nil
}

func (s *peerSockets) close() {
	s.cancel()
	s.worker.Wait()
	if s.stream != nil {
		s.stream.Close()
	}
	for _, pair := range s.native {
		for _, n := range pair {
			if n != nil {
				n.Close()
			}
		}
	}
}

func (s *peerSockets) observe(ctx context.Context, observer netip.AddrPort) {
	var wg sync.WaitGroup
	for _, entry := range s.all {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			defer cancel()
			addr, _ := entry.Observe(ctx, observer)
			s.mu.Lock()
			entry.observed = addr
			s.mu.Unlock()
		})
	}
	wg.Wait()
}

func candidateKind(ip netip.Addr) string {
	if ip.Is6() {
		return "ipv6"
	}
	if config.PublicIPv4(ip) {
		return "public"
	}
	return "lan"
}

func (s *peerSockets) candidates(endpoint, tcpEndpoint string, incoming, outgoing []string) []mesh.Candidate {
	var result []mesh.Candidate
	if ep, err := netip.ParseAddrPort(endpoint); err == nil {
		kind := candidateKind(ep.Addr())
		result = append(result, mesh.Candidate{Addr: ep, Kind: kind})
	}
	if ep, err := netip.ParseAddrPort(tcpEndpoint); err == nil && s.stream.TCPPort() != 0 {
		result = append(result, mesh.Candidate{Addr: ep, Kind: candidateKind(ep.Addr()), Protocol: "tcp"})
	}
	// Reserve slots for stream UDP, stream TCP, and both directions of
	// each native mode within the hub's 64-candidate registration limit.
	const perDirection = 10
	for _, entry := range s.all {
		if entry.mode != "" && (entry.receive && !slices.Contains(incoming, entry.mode) || !entry.receive && !slices.Contains(outgoing, entry.mode)) {
			continue
		}
		var counts [2]int
		if entry.mode == "" {
			for _, c := range result {
				if c.Protocol == "tcp" {
					counts[1]++
				} else {
					counts[0]++
				}
			}
		}
		local := entry.local()
		s.mu.Lock()
		var found []mesh.Candidate
		for _, addr := range entry.mapped {
			found = append(found, mesh.Candidate{Addr: addr, Kind: "mapped"})
		}
		if entry.observed.IsValid() {
			found = append(found, mesh.Candidate{Addr: entry.observed, Kind: "observed"})
		}
		s.mu.Unlock()
		for _, c := range append(found, local...) {
			c.Mode, c.Receive = entry.mode, entry.receive
			group := 0
			if c.Protocol == "tcp" {
				group = 1
			}
			if counts[group] >= perDirection || slices.Contains(result, c) {
				continue
			}
			result = append(result, c)
			counts[group]++
		}
	}
	return result
}

func peerUnderlayCandidates(candidates []mesh.Candidate, vpnIP netip.Addr) []mesh.Candidate {
	return slices.DeleteFunc(candidates, func(c mesh.Candidate) bool { return c.Addr.Addr().Unmap() == vpnIP })
}

func (s *peerSockets) nativeConnect(ctx context.Context, p mesh.Pair) (carrier.Transport, error) {
	offer := nativepeer.Offer{Secret: p.Secret, Initiator: p.Initiator, Expires: p.Expires}
	for _, c := range p.Candidates {
		offer.Candidates = append(offer.Candidates, c.Addr)
	}
	pair := s.native[p.Mode]
	if p.Initiator {
		return pair[0].Connect(ctx, offer)
	}
	return pair[1].Connect(ctx, offer)
}
