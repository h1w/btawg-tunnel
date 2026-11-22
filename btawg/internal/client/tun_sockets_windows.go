package client

import (
	"fmt"
	"net"
	"net/netip"
	"time"
)

const windowsAssemblesIngress = true

func (p *tunPath) inboundPacket(packet []byte) []byte {
	return p.fragments.push(packet, time.Now())
}

func (p *tunPath) proxySockets(c *Client, source netip.Addr) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return net.ErrClosed
	}
	o, _ := p.overlay.(*windowsOverlay)
	if o == nil {
		var err error
		o, err = newWindowsOverlay(p, source)
		if err != nil {
			return err
		}
		p.overlay = o
	}
	c.proxyTCP, c.proxyUDP = o.tcp, o.udp
	return nil
}

func (p *tunPath) deliverPackets(bufs [][]byte, offset int) (int, error) {
	for i, b := range bufs {
		if len(b) < offset || p.deliverOverlay(b[offset:]) {
			continue
		}
		if _, err := p.shared.dev.Write(bufs[i:i+1], offset); err != nil {
			return i, err
		}
	}
	return len(bufs), nil
}

func (p *tunPath) deliverOverlay(packet []byte) bool {
	if len(packet) < 20 || packet[0]>>4 != 4 {
		return false
	}
	s := p.shared
	local := [4]byte(packet[16:20])
	s.mu.RLock()
	entry := s.ports[overlayReplyKey(packet, local)]
	s.mu.RUnlock()
	var owner *tunPath
	if entry != nil {
		owner = entry.path
	}
	if owner == nil {
		return false
	}
	if owner == p || owner.directLife == nil && p.directLife == nil {
		owner.mu.Lock()
		o, _ := owner.overlay.(*windowsOverlay)
		owner.mu.Unlock()
		if o != nil && local == o.local.As4() {
			o.deliver(packet)
		}
	}
	return true
}

func (s *sharedTUN) track(key localPort, owner *socketPath) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ports[key] != nil {
		return fmt.Errorf("overlay source port %d is still owned", key.port)
	}
	s.ports[key] = owner
	return nil
}

func (s *sharedTUN) untrack(key localPort, owner *socketPath) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ports[key] == owner && owner.until.IsZero() {
		drain := overlayFragmentLifetime
		if key.proto == 6 {
			drain = 2 * time.Minute
		}
		owner.until = time.Now().Add(drain)
	}
}

func (s *sharedTUN) pruneSockets() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			s.mu.Lock()
			for key, entry := range s.ports {
				entry.release()
				delete(s.ports, key)
			}
			s.mu.Unlock()
			return
		case now := <-tick.C:
			s.mu.Lock()
			for key, entry := range s.ports {
				if !entry.until.IsZero() && !now.Before(entry.until) && (entry.retired == nil || entry.retired()) {
					entry.release()
					delete(s.ports, key)
				}
			}
			s.mu.Unlock()
		}
	}
}
