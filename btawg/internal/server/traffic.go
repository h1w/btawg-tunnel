package server

import (
	"sync/atomic"

	"bittorrent-tunnel/btawg/internal/awgrepair"
	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/store"
)

type trafficCounter struct {
	up, down atomic.Uint64
	lastSeen atomic.Int64
}

func (c *trafficCounter) observe(at int64) {
	for prev := c.lastSeen.Load(); at > prev; prev = c.lastSeen.Load() {
		if c.lastSeen.CompareAndSwap(prev, at) {
			return
		}
	}
}

func (c *trafficCounter) snapshot() store.Traffic {
	return store.Traffic{Up: c.up.Load(), Down: c.down.Load(), LastSeen: c.lastSeen.Load()}
}

// Traffic returns a snapshot keyed by stable client ID, including revoked peers.
func (s *Server) Traffic() map[string]store.Traffic {
	s.policy.mu.RLock()
	defer s.policy.mu.RUnlock()
	out := make(map[string]store.Traffic, len(s.policy.traffic))
	for id, c := range s.policy.traffic {
		out[id] = c.snapshot()
	}
	return out
}

// EngineQueueDrops returns cumulative queue-full packet counts for this hub
// process. TLS sessions are counted together, including sessions that ended.
func (s *Server) EngineQueueDrops() map[string]uint64 {
	r := s.router
	r.routesMu.RLock()
	defer r.routesMu.RUnlock()
	out := make(map[string]uint64, len(r.engines)+3)
	out[config.ModeWG] = r.wg.queueDrops.Load()
	out[config.ModeAWG] = r.awg.queueDrops.Load()
	out["tls"] = r.tlsQueueDrops.Load()
	for mode, e := range r.engines {
		out[mode] = e.queueDrops.Load()
	}
	return out
}

// AWGRepairStats sums the native and four carrier AWG engines, never TLS-IP or WG.
func (s *Server) AWGRepairStats() awgrepair.Stats {
	var total awgrepair.Stats
	if s.router == nil {
		return total
	}
	for _, e := range s.router.awgEngines() {
		if e == nil {
			continue
		}
		if recovery := e.recovery.Load(); recovery != nil {
			v := recovery.Stats()
			total.ParityBlocks += v.ParityBlocks
			total.Recovered += v.Recovered
			total.ControlBytes += v.ControlBytes
			total.Expired += v.Expired
			total.ReplicaPackets += v.ReplicaPackets
			total.ReplicaBytes += v.ReplicaBytes
			total.Duplicates += v.Duplicates
			total.CodedPieces += v.CodedPieces
			total.WindowClamps += v.WindowClamps
			total.AutoTxOn += v.AutoTxOn
			total.AutoTxOff += v.AutoTxOff
			total.AutoRxOn += v.AutoRxOn
			total.AutoRxOff += v.AutoRxOff
			total.AutoSamples += v.AutoSamples
			total.AutoMissing += v.AutoMissing
		}
	}
	return total
}

// flushTraffic is called by the sampler, then on shutdown after the sampler
// and both device engines have stopped. Failed commits leave records dirty.
func (s *Server) flushTraffic() error {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	changed := s.Traffic()
	for id, v := range changed {
		if v == s.savedTraffic[id] {
			delete(changed, id)
		}
	}
	if err := s.st.SaveTraffic(changed); err != nil {
		return err
	}
	for id, v := range changed {
		s.savedTraffic[id] = v
	}
	return nil
}
