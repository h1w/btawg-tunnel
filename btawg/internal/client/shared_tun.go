package client

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"

	"bittorrent-tunnel/btawg/internal/mesh"
)

const (
	packetHeadroom = 16 // Linux virtio and the tunnel engines both fit here.
	pathQueueSize  = 256
	pathBatchSize  = 16
)

// sharedTUN is the single kernel interface. Each transport gets a queue.
// Explicit proxy flows stay on their selected path; ordinary traffic uses
// the lowest-index enabled transport unless a peer policy chooses another.
type sharedTUN struct {
	dev         tun.Device
	name        string
	mtu         int
	closeRoute  func() error
	primary     atomic.Pointer[tunPath]
	servicePath atomic.Pointer[tunPath]
	pool        sync.Pool
	workers     sync.WaitGroup
	done        chan struct{}
	readErr     error // published by closing done

	mu             sync.RWMutex
	ports          map[localPort]*socketPath
	direct         map[netip.Addr]*peerPathGroup
	directRanges   []netip.Prefix
	peerNetwork    []netip.Prefix
	directGateways map[netip.Addr]bool
	directOwn      netip.Addr
	directWanted   chan netip.Addr
	peerPolicies   *peerPolicies
	peerFlows      map[peerTuple]peerFlow
	peerRequests   map[netip.Addr]time.Time
	peerEnabled    atomic.Bool
	peerFragments  map[fragmentID]fragmentPath
	peerSweep      time.Time
}

type localPort struct {
	port   uint16
	proto  byte
	remote netip.AddrPort // TCP has a fixed peer; UDP associations do not.
}

type socketPath struct {
	path    *tunPath
	until   time.Time // nonzero only while a closed socket drains
	release func()
	retired func() bool
}

type tunPath struct {
	shared       *sharedTUN
	directLife   context.Context
	directUntil  *atomic.Int64
	peerOutbound bool
	peerMode     string
	peerHealth   *managedTunnel
	packets      chan *[]byte
	done         chan struct{}
	events       chan tun.Event
	mu           sync.Mutex // serialize close and enqueue
	closed       bool
	overlay      overlaySockets
	fragments    overlayReassembly
}

func newSharedTUN(dev tun.Device, name string, mtu int) *sharedTUN {
	s := &sharedTUN{dev: dev, name: name, mtu: mtu, ports: make(map[localPort]*socketPath), done: make(chan struct{})}
	s.pool.New = func() any { return newPacketBuffer(mtu) }
	s.workers.Go(s.dispatch)
	s.workers.Go(func() {
		// The daemon manages the link; engines own only their packet queues.
		for range dev.Events() {
		}
	})
	s.workers.Go(s.pruneSockets)
	return s
}

// A pointer keeps slice headers from allocating on every sync.Pool Put.
// Queue and reader ownership are exclusive until the engine has copied out.
func newPacketBuffer(mtu int) *[]byte {
	buf := make([]byte, packetHeadroom+mtu)
	return &buf
}

func (s *sharedTUN) newPath() *tunPath {
	return &tunPath{shared: s, packets: make(chan *[]byte, pathQueueSize), done: make(chan struct{}), events: make(chan tun.Event)}
}

func (s *sharedTUN) close() error {
	err := s.dev.Close()
	s.workers.Wait()
	if s.closeRoute != nil {
		return errors.Join(err, s.closeRoute())
	}
	return err
}

// Linux splits large UDP datagrams into consecutive IPv4 fragments. Only
// the first fragment carries the source port; never send its tail on a
// different path when a proxy port or the primary path changes.
type fragmentID struct {
	src, dst [4]byte
	id       uint16
	proto    byte
}

type fragmentPath struct {
	path   *tunPath
	before time.Time
}

func (s *sharedTUN) dispatch() {
	defer close(s.done)
	batch := max(1, s.dev.BatchSize())
	bufs := make([][]byte, batch)
	storage := make([]*[]byte, batch)
	sizes := make([]int, batch)
	for i := range bufs {
		storage[i] = s.pool.Get().(*[]byte)
		bufs[i] = (*storage[i])[:cap(*storage[i])]
	}
	defer func() {
		for _, b := range storage {
			s.pool.Put(b)
		}
	}()
	fragments := make(map[fragmentID]fragmentPath)
	var nextPrune time.Time
	for {
		n, err := s.dev.Read(bufs, sizes, packetHeadroom)
		if err != nil && !errors.Is(err, tun.ErrTooManySegments) {
			s.readErr = err
			return
		}
		for i := range n {
			b := bufs[i][:packetHeadroom+sizes[i]]
			p := b[packetHeadroom:]
			if len(p) < 20 || p[0]>>4 != 4 {
				continue
			}
			hlen := int(p[0]&15) * 4
			if hlen < 20 || len(p) < hlen {
				continue
			}
			flags := binary.BigEndian.Uint16(p[6:8])
			var path *tunPath
			if flags&0x3fff != 0 {
				now := time.Now()
				id := fragmentID{[4]byte(p[12:16]), [4]byte(p[16:20]), binary.BigEndian.Uint16(p[4:6]), p[9]}
				if flags&0x1fff == 0 {
					if len(fragments) >= 4096 && !now.Before(nextPrune) {
						for k, f := range fragments {
							if !now.Before(f.before) {
								delete(fragments, k)
							}
						}
						nextPrune = now.Add(time.Second)
					}
					if len(fragments) >= 4096 {
						continue
					}
					path = s.packetPath(p, hlen)
					fragments[id] = fragmentPath{path, now.Add(30 * time.Second)}
				} else if f, ok := fragments[id]; ok && now.Before(f.before) {
					path = f.path
					if flags&0x2000 == 0 {
						delete(fragments, id)
					}
				}
			} else {
				path = s.packetPath(p, hlen)
			}
			if path != nil && s.enqueuePacket(path, storage[i], b, p) {
				storage[i] = s.pool.Get().(*[]byte)
				bufs[i] = (*storage[i])[:cap(*storage[i])]
			}
		}
	}
}

func (s *sharedTUN) packetPath(p []byte, hlen int) *tunPath {
	if proto := p[9]; (proto == 6 || proto == 17) && len(p) >= hlen+4 {
		key := localPort{port: binary.BigEndian.Uint16(p[hlen:]), proto: proto}
		if proto == 6 {
			key.remote = netip.AddrPortFrom(netip.AddrFrom4([4]byte(p[16:20])), binary.BigEndian.Uint16(p[hlen+2:]))
		}
		s.mu.RLock()
		owner := s.ports[key]
		s.mu.RUnlock()
		if owner != nil {
			if !owner.path.validDirect() {
				return nil
			}
			return owner.path
		}
	}
	if primary := s.primary.Load(); primary == nil && p[9] == 6 && len(p) >= hlen+4 && binary.BigEndian.Uint16(p[hlen:]) == mesh.EgressPort {
		return s.servicePath.Load()
	}
	dst := netip.AddrFrom4([4]byte(p[16:20]))
	if path, handled := s.choosePeerPacket(p, hlen, dst); handled {
		return path
	}
	return s.primary.Load()
}

func (p *tunPath) enqueue(storage *[]byte, packet []byte) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	*storage = packet
	select {
	case p.packets <- storage:
		return true
	default:
		return false
	}
}

func (p *tunPath) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	for n := 0; n < len(bufs); {
		var b *[]byte
		if n == 0 {
			select {
			case <-p.done:
				return 0, net.ErrClosed
			case <-p.shared.done:
				return 0, p.shared.readErr
			case b = <-p.packets:
			}
		} else {
			select {
			case <-p.done:
				return n, nil
			case <-p.shared.done:
				return n, nil
			case b = <-p.packets:
			default:
				return n, nil
			}
		}
		if !p.validDirect() {
			p.shared.pool.Put(b)
			return n, net.ErrClosed
		}
		packet := (*b)[packetHeadroom:]
		if p.shared.peerEnabled.Load() && len(packet) >= 20 {
			dst := netip.AddrFrom4([4]byte(packet[16:20]))
			p.shared.mu.RLock()
			allowed := !p.shared.shouldRoutePeerLocked(dst) || p.shared.peerPathAllowedLocked(dst, p)
			p.shared.mu.RUnlock()
			if !allowed {
				p.shared.pool.Put(b)
				continue
			}
		}
		if len(packet) > len(bufs[n])-offset {
			p.shared.pool.Put(b)
			return n, io.ErrShortBuffer
		}
		sizes[n] = copy(bufs[n][offset:], packet)
		p.shared.pool.Put(b)
		n++
	}
	return len(bufs), nil
}

func (p *tunPath) Write(bufs [][]byte, offset int) (int, error) {
	if windowsAssemblesIngress && p.directLife == nil {
		for i, b := range bufs {
			if len(b) < offset+20 {
				continue
			}
			packet := p.inboundPacket(b[offset:])
			if packet == nil || p.shared.peerEnabled.Load() && !p.shared.acceptHubPacket(p, packet) {
				continue
			}
			batch := [1][]byte{packet}
			if _, err := p.deliverPackets(batch[:], 0); err != nil {
				return i, err
			}
		}
		return len(bufs), nil
	}
	if p.directLife != nil || !p.shared.peerEnabled.Load() {
		return p.deliverPackets(bufs, offset)
	}
	for i, b := range bufs {
		if len(b) < offset+20 || !p.shared.acceptHubPacket(p, b[offset:]) {
			// The engine owns these buffers; compact into a local batch only
			// when policy filters at least one packet.
			var filtered [pathBatchSize][]byte
			allowed := append(filtered[:0], bufs[:i]...)
			for _, next := range bufs[i+1:] {
				if len(next) >= offset+20 && p.shared.acceptHubPacket(p, next[offset:]) {
					allowed = append(allowed, next)
				}
			}
			if len(allowed) > 0 {
				if _, err := p.deliverPackets(allowed, offset); err != nil {
					return 0, err
				}
			}
			return len(bufs), nil
		}
	}
	return p.deliverPackets(bufs, offset)
}

func (p *tunPath) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	close(p.done)
	close(p.events)
	overlay := p.overlay
	p.mu.Unlock()
	if overlay != nil {
		overlay.close()
	}
	for {
		select {
		case b := <-p.packets:
			p.shared.pool.Put(b)
		default:
			return nil
		}
	}
}

type overlaySockets interface{ close() }

func (p *tunPath) File() *os.File           { return nil }
func (p *tunPath) MTU() (int, error)        { return p.shared.mtu, nil }
func (p *tunPath) Name() (string, error)    { return p.shared.name, nil }
func (p *tunPath) Events() <-chan tun.Event { return p.events }
func (p *tunPath) BatchSize() int           { return pathBatchSize }
