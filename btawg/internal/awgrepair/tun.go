package awgrepair

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
)

const (
	controlSlots = 64
	maxLive      = 128
	fresh        = 2 * time.Second
	queueMS      = 8
)

var origin = time.Now()

func clock(now time.Time) uint32 { return uint32(now.Sub(origin) / time.Millisecond) }

// Peer identifies a single enrolled AWG peer and its own group gateway.
type Peer struct {
	Local, Remote netip.Addr
	Public        string
}

// Stats counts inner recovery traffic, without key material or packet content.
type Stats struct {
	ParityBlocks   uint64 `json:"parity_blocks"`
	Recovered      uint64 `json:"recovered_packets"`
	ControlBytes   uint64 `json:"control_bytes"`
	Expired        uint64 `json:"expired_controls"`
	ReplicaPackets uint64 `json:"replica_packets"`
	ReplicaBytes   uint64 `json:"replica_bytes"`
	Duplicates     uint64 `json:"suppressed_replicas"`
	CodedPieces    uint64 `json:"coded_pieces"`
	WindowClamps   uint64 `json:"window_clamps"`
	AutoTxOn       uint64 `json:"auto_tx_on"`
	AutoTxOff      uint64 `json:"auto_tx_off"`
	AutoRxOn       uint64 `json:"auto_rx_on"`
	AutoRxOff      uint64 `json:"auto_rx_off"`
	AutoSamples    uint64 `json:"auto_samples"`
	AutoMissing    uint64 `json:"auto_missing"`
}
type path struct {
	local, remote netip.Addr
	k             key
	session       innerSession
	state         *stream
	preflight     flightGuard // observed SYNs only; transferred to a fresh recovery generation
}
type stream struct {
	generation            uint64
	tx                    block
	recent                originals
	rx                    decoder
	train                 arrivalTrain
	txRate, rxRate        cleanRate
	keepCacheUntil        time.Time
	lastProbe             time.Time
	group                 uint32
	credit                int
	active                bool
	quiet                 int
	report, hold, blockAt time.Time
	base, peak            uint32
	baseSet               bool
	total, missing        uint16
	lateBuf               [maxOriginal]byte
	repairs               [2 * codedStateCount]struct {
		id    uint64
		until time.Time
	}
	nextRepair          uint64
	lateUntil           time.Time
	wideUntil, nextWide time.Time
	copies              *replicaCache
	encoder             *codedTX
	decoder             *codedRX
	flight              flightGuard
	adapt               *autoDirection
}

type queued struct {
	p          *path
	generation uint64
	at         time.Time
	size       int
	replicas   uint8 // remaining ordinary data transmissions; zero denotes a control
	packet     [innerMTU]byte
}
type readBatch struct {
	sizes []int
	bufs  [][]byte
	n     int
	err   error
}

type wakeReader interface {
	ReadWithWake(bufs [][]byte, sizes []int, offset int, wake <-chan struct{}) (int, error)
}

// Device sits above AWG encryption. No control path owns an underlay socket.
// Write can wake an idle Read with a handshake response or feedback, even
// for a strictly one-way application. The bounded fallback hands its single
// read buffer back before another underlying read.
type Device struct {
	tun.Device
	client         bool
	private        string
	transport      string // included in the static key derivation on both ends
	profile        byte   // negotiated request kind, or zero for adaptive parity
	mu             sync.Mutex
	paths          map[netip.Addr]*path
	upstream       *path
	localIngress   atomic.Bool  // client has one upstream; no codec state touched on a LAN bulk batch
	localReadUntil atomic.Int64 // monotonic offset from origin, expires at the next RTT probe
	queuedControl  atomic.Bool
	live           int
	stats          Stats
	q              [controlSlots]queued
	head, count    int
	wake           chan struct{}
	direct         wakeReader
	native         *nativeReader
	data           chan *readBatch
	ack            chan struct{}
	done           chan struct{}
	start, stop    sync.Once
	workers        sync.WaitGroup
	closed         bool
	closeErr       error
}

func newDevice(base tun.Device, client bool) *Device {
	d := &Device{Device: base, client: client, paths: make(map[netip.Addr]*path), wake: make(chan struct{}, 1), data: make(chan *readBatch), ack: make(chan struct{}, 1), done: make(chan struct{})}
	d.direct, _ = base.(wakeReader)
	if d.direct == nil {
		d.native = newNativeReader(base)
	}
	return d
}
func (d *Device) Stats() Stats {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stats
}

// WideActive is consulted under hub queue pressure, before this peer's
// originals reach encryption. No recovery state is created by the lookup.
func (d *Device) WideActive(remote netip.Addr, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.paths[remote]
	if p == nil || p.state == nil || !p.session.ready(now) || p.state.generation != p.session.generation() || !now.Before(p.state.wideUntil) {
		return false
	}
	a := p.state.adapt
	return d.profile != autoRequest || (a != nil && ((a.tx.code && now.Before(a.txUntil)) || a.startTX.active(now)))
}

func NewHub(base tun.Device, private string, mode string) *Device {
	d := newDevice(base, false)
	d.profile, d.transport = autoRequest, mode
	d.private = private
	return d
}
func NewNode(base tun.Device, private string, peer Peer, mode string) (*Device, error) {
	d := newDevice(base, true)
	d.profile, d.transport = autoRequest, mode
	d.private = private
	if err := d.SetPeers([]Peer{peer}); err != nil {
		d.native.close() // base is still owned by the caller on failure
		return nil, err
	}
	return d, nil
}

// StartAuto is called after AWG has installed its peer and brought up its bind.
// Start recovery negotiation without waiting for the first application packet.
func (d *Device) StartAuto() {
	if d.profile != autoRequest || !d.client {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if p := d.upstream; p != nil && !d.closed {
		// Called once per attachment, including reconnect to a restarted hub.
		// Replace the path identity so queued work from a previous attachment
		// cannot be drained for the new one, even before its first handshake.
		if p.session.established || !p.session.pendingAt.IsZero() {
			if p.state != nil {
				d.live--
			}
			p = &path{local: p.local, remote: p.remote, k: p.k, session: innerSession{offerCold: true}, preflight: p.preflight}
			d.upstream = p
			d.paths[p.remote] = p
			d.localIngress.Store(false)
			d.localReadUntil.Store(0)
		}
		now := time.Now()
		d.appendControl(p, now, func(out []byte) []byte { return p.session.begin(&p.k, p.local, p.remote, now, out) })
	}
}

func (d *Device) SetPeers(peers []Peer) error {
	next := make(map[netip.Addr]*path, len(peers))
	for _, peer := range peers {
		if !peer.Local.Is4() || !peer.Remote.Is4() || peer.Local == peer.Remote || next[peer.Remote] != nil {
			return errors.New("invalid AWG recovery peer addresses")
		}
		k, err := deriveTransportKey(d.private, peer.Public, d.client, d.transport)
		if err != nil {
			return fmt.Errorf("AWG recovery identity: %w", err)
		}
		next[peer.Remote] = &path{local: peer.Local, remote: peer.Remote, k: k, session: innerSession{offerCold: d.profile == autoRequest}}
	}
	d.replace(next)
	return nil
}
func (d *Device) replace(next map[netip.Addr]*path) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.live = 0
	d.upstream = nil
	d.localIngress.Store(false)
	d.localReadUntil.Store(0)
	for a, p := range next {
		if old := d.paths[a]; old != nil && old.local == p.local && old.k == p.k {
			p = old
			next[a] = p
		}
		if p.state != nil {
			d.live++
		}
		if d.client {
			d.upstream = p
		}
	}
	d.paths = next
}
func (d *Device) Close() error {
	d.stop.Do(func() {
		d.mu.Lock()
		d.closed = true
		close(d.done)
		d.mu.Unlock()
		d.native.interrupt()
		d.closeErr = d.Device.Close()
		d.native.close()
		d.workers.Wait()
	})
	return d.closeErr
}
func (d *Device) readLoop() {
	defer d.workers.Done()
	n := max(1, d.Device.BatchSize())
	mtu, err := d.Device.MTU()
	if err != nil || mtu < innerMTU {
		mtu = innerMTU
	}
	mtu = max(mtu, 2048)
	batch := readBatch{bufs: make([][]byte, n), sizes: make([]int, n)}
	area := make([]byte, n*mtu)
	for i := range batch.bufs {
		batch.bufs[i] = area[i*mtu : (i+1)*mtu : (i+1)*mtu]
	}
	for {
		batch.n, batch.err = d.Device.Read(batch.bufs, batch.sizes, 0)
		select {
		case d.data <- &batch:
		case <-d.done:
			return
		}
		select {
		case <-d.ack:
		case <-d.done:
			return
		}
		if batch.err != nil && !errors.Is(batch.err, tun.ErrTooManySegments) {
			return
		}
	}
}

func packetAddrs(p []byte) (netip.Addr, netip.Addr, bool) {
	if len(p) < 20 || p[0]>>4 != 4 || int(p[0]&15)*4 < 20 || int(p[0]&15)*4 > len(p) || int(binary.BigEndian.Uint16(p[2:4])) != len(p) {
		return netip.Addr{}, netip.Addr{}, false
	}
	return netip.AddrFrom4([4]byte(p[12:16])), netip.AddrFrom4([4]byte(p[16:20])), true
}
func (d *Device) appendControl(p *path, now time.Time, makePacket func([]byte) []byte) bool {
	if !d.queueControl(p, now, makePacket) {
		return false
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
	d.native.signal()
	return true
}

// Reader-produced parity is drained by the current Read; only a producer
// running outside Read needs to interrupt a sleeping TUN reader.
func (d *Device) queueControl(p *path, now time.Time, makePacket func([]byte) []byte) bool {
	if d.count == controlSlots {
		return false
	}
	q := &d.q[(d.head+d.count)%controlSlots]
	frame := makePacket(q.packet[:0])
	if len(frame) == 0 {
		return false
	}
	q.replicas = 0
	q.size = len(frame)
	q.p = p
	q.generation = p.session.generation()
	q.at = now
	d.count++
	if d.count == 1 {
		d.queuedControl.Store(true)
	}
	return true
}
func (d *Device) drain(bufs [][]byte, sizes []int, offset int, now time.Time) (int, error) {
	n := 0
	for d.count > 0 && n < len(bufs) {
		q := &d.q[d.head]
		p := q.p
		valid := d.paths[p.remote] == p && p.session.generation() == q.generation && now.Sub(q.at) <= 100*time.Millisecond
		if q.replicas != 0 {
			valid = valid && p.session.ready(now) && p.state != nil && now.Before(p.state.wideUntil)
		}
		if valid {
			if len(bufs[n])-offset < q.size {
				return n, io.ErrShortBuffer
			}
			sizes[n] = copy(bufs[n][offset:], q.packet[:q.size])
			n++
			if q.replicas != 0 {
				d.stats.ReplicaPackets++
				d.stats.ReplicaBytes += uint64(q.size)
				q.replicas--
				if q.replicas != 0 {
					continue
				}
			} else {
				d.stats.ControlBytes += uint64(q.size)
			}
		} else if q.replicas == 0 {
			d.stats.Expired++
		}
		q.p = nil
		d.head = (d.head + 1) % controlSlots
		d.count--
	}
	if d.count == 0 && d.queuedControl.Load() {
		d.queuedControl.Store(false)
	}
	return n, nil
}

func (d *Device) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	if len(bufs) == 0 || len(sizes) < len(bufs) || offset < 0 {
		return 0, io.ErrShortBuffer
	}
	for {
		if d.queuedControl.Load() {
			d.mu.Lock()
			n, err := d.drain(bufs, sizes, offset, time.Now())
			d.mu.Unlock()
			if n > 0 || err != nil {
				return n, err
			}
		}
		select {
		case <-d.done:
			return 0, net.ErrClosed
		default:
		}

		var (
			count   int
			readErr error
			batch   *readBatch
		)
		switch {
		case d.direct != nil:
			count, readErr = d.direct.ReadWithWake(bufs, sizes, offset, d.wake)
		case d.native != nil:
			count, readErr = d.native.read(bufs, sizes, offset)
		default:
			d.start.Do(func() {
				d.mu.Lock()
				defer d.mu.Unlock()
				if !d.closed {
					d.workers.Add(1)
					go d.readLoop()
				}
			})
			select {
			case <-d.done:
				return 0, net.ErrClosed
			case <-d.wake:
				continue
			case batch = <-d.data:
				count, readErr = batch.n, batch.err
			}
		}

		// Direct/native reads already own these buffers. Skip bookkeeping for
		// ordinary LAN traffic between RTT probes, never for a new TCP handshake.
		if until := d.localReadUntil.Load(); batch == nil && until != 0 && count > 0 && int64(time.Since(origin)) < until && !d.queuedControl.Load() {
			fast := true
			for i := range count {
				l := sizes[i]
				if l < 20 || l > len(bufs[i])-offset || hasTCPFlags(bufs[i][offset:offset+l], 7) {
					fast = false
					break
				}
			}
			if fast {
				return count, readErr
			}
		}
		n, err := d.finishRead(bufs, sizes, offset, count, readErr, batch)
		if batch != nil {
			d.ack <- struct{}{}
		}
		if n > 0 || err != nil {
			return n, err
		}
	}
}

// finishRead observes original packets and appends any newly queued controls.
// A non-nil batch is the private bounded fallback buffer and requires a copy;
// wake-aware and native reads already own the caller-provided destination.
func (d *Device) finishRead(bufs [][]byte, sizes []int, offset, count int, readErr error, batch *readBatch) (int, error) {
	if count < 0 || count > len(bufs) {
		return 0, io.ErrShortBuffer
	}
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range count {
		l := sizes[i]
		if batch != nil {
			l = batch.sizes[i]
			if l < 0 || l > len(batch.bufs[i]) || l > len(bufs[i])-offset {
				return i, io.ErrShortBuffer
			}
			sizes[i] = copy(bufs[i][offset:], batch.bufs[i][:l])
		}
		if l < 0 || l > len(bufs[i])-offset {
			return i, io.ErrShortBuffer
		}
		d.sent(bufs[i][offset:offset+l], now)
	}
	extra, err := d.drain(bufs[count:], sizes[count:], offset, now)
	if err != nil {
		return count + extra, err
	}
	return count + extra, readErr
}

func (d *Device) state(p *path, now time.Time) *stream {
	if !p.session.ready(now) {
		return nil
	}
	if p.state != nil && p.state.generation == p.session.generation() {
		return p.state
	}
	if p.state == nil && d.live >= maxLive {
		for _, x := range d.paths {
			if x.state != nil && !x.session.ready(now) {
				x.state = nil
				d.live--
			}
		}
		if d.live >= maxLive {
			return nil
		}
	}
	if p.state == nil {
		d.live++
	}
	p.state = &stream{generation: p.session.generation()}
	p.state.flight = p.preflight
	p.preflight = flightGuard{}
	if d.profile == autoRequest && p.session.handshakeRTT > 0 {
		a := p.state.autoState()
		a.rttMS = a.floor.sample(now, uint16(min(p.session.handshakeRTT/time.Millisecond, 65535)))
		a.localTX = p.session.handshakeRTT < coldMinRTT
		a.localRX = a.localTX
		if d.client {
			d.localIngress.Store(a.localRX)
		}
	}
	if p.session.peerCold && p.session.handshakeRTT >= coldMinRTT {
		// An ordered carrier repairs underlay gaps before AWG sees them. Blind
		// parity competes with TCP slow start there; retain the same codec and
		// activate it when the receiver actually observes missing inner packets.
		p.state.autoState().coldCap = d.transport == "socks5-awg-udp"
		p.state.wideUntil = now.Add(5 * time.Second)
	}
	return p.state
}

func isTCPSYN(data []byte) bool {
	return hasTCPFlags(data, 2)
}
func hasTCPFlags(data []byte, mask byte) bool {
	off := int(data[0]&15) * 4
	return data[9] == 6 && len(data) >= off+20 && data[off+13]&mask != 0
}

func (d *Device) sent(data []byte, now time.Time) {
	src, dst, ok := packetAddrs(data)
	if !ok || data[9] == innerProto {
		return
	}
	p := d.paths[dst]
	if d.client {
		p = d.upstream
	}
	if p == nil {
		return
	}
	if !p.session.ready(now) {
		if d.profile == autoRequest && isTCPSYN(data) {
			p.preflight.observeMode(data, now, 0)
		}
		if d.client && src == p.local {
			d.appendControl(p, now, func(out []byte) []byte { return p.session.begin(&p.k, p.local, p.remote, now, out) })
		}
		return
	}
	if d.profile != 0 {
		if s := d.state(p, now); s != nil {
			if d.client && !now.Before(s.nextWide) {
				d.appendControl(p, now, func(out []byte) []byte { return p.session.send(d.profile, p.local, p.remote, nil, out) })
				s.nextWide = now.Add(time.Second)
			}
			leased := now.Before(s.wideUntil)
			coding := d.profile == codedRequest
			if d.profile == autoRequest && leased {
				a := s.autoState()
				if a.localTX && !a.rx.mode.clamp && !a.startRX.active(now) {
					if hasTCPFlags(data, 7) {
						s.flight.observeMode(data, now, 0)
					}
					if !now.Before(a.nextRTT) {
						a.nextRTT = now.Add(250 * time.Millisecond)
						if d.client {
							d.localReadUntil.Store(int64(a.nextRTT.Sub(origin)))
						}
						var pulse [7]byte
						binary.BigEndian.PutUint16(pulse[:], a.rttMS)
						binary.BigEndian.PutUint32(pulse[2:], clock(now))
						d.queueControl(p, now, func(out []byte) []byte { return p.session.send(autoSample, p.local, p.remote, pulse[:], out) })
					}
					return
				}
				if a.tx.code && !now.Before(a.txUntil) {
					a.tx = autoMode{}
					d.stats.AutoTxOff++
					if s.encoder != nil {
						s.encoder.reset()
					}
				}
				coding = a.tx.code && now.Before(a.txUntil)
				var budget uint32
				if a.rx.mode.clamp && now.Sub(a.rx.last) < autoTTL {
					budget = a.rx.budget
				}
				if a.startRX.active(now) && (s.nextRepair != 0 || !a.rx.lossAt.IsZero()) {
					budget = flightBytes
				}
				if s.flight.observeMode(data, now, budget) {
					d.stats.WindowClamps++
				}
				if eligibleReplica(data) {
					if a.coldCap && a.startTX.data(now, len(data)) {
						coding = true
					}
					a.meter.sent(data, now, func(probe []byte) {
						d.queueControl(p, now, func(out []byte) []byte {
							var packet [innerMaxData]byte
							if len(probe) > len(packet)-2 {
								return nil
							}
							binary.BigEndian.PutUint16(packet[:], a.rttMS)
							copy(packet[2:], probe)
							return p.session.send(autoSample, p.local, p.remote, packet[:2+len(probe)], out)
						})
					})
				}
			} else if coding && leased && s.flight.observe(data, now) {
				d.stats.WindowClamps++
			}
			if coding && leased && eligibleReplica(data) {
				if s.encoder == nil {
					s.encoder = new(codedTX)
				}
				s.encoder.add(data, func(piece []byte) bool {
					ok := d.queueControl(p, now, func(out []byte) []byte { return p.session.send(codedParity, p.local, p.remote, piece, out) })
					if ok {
						d.stats.CodedPieces++
					}
					return ok
				})
				return
			}
			if d.profile == autoRequest {
				return
			} // never mix legacy repair with auto measurement
			if d.profile == wideRequest && now.Before(s.wideUntil) && eligibleReplica(data) {
				// Original is already in the encryption batch. Never block it for a copy.
				// Leave queue space for negotiation, feedback and other peers.
				if d.count < controlSlots-4 {
					q := &d.q[(d.head+d.count)%controlSlots]
					q.size, q.p, q.at, q.generation, q.replicas = copy(q.packet[:], data), p, now, p.session.generation(), 2
					d.count++ // called by the reader itself; no eventfd wake needed
					if d.count == 1 {
						d.queuedControl.Store(true)
					}
				}
				return
			}
		}
	}
	if len(data) < minOriginal || len(data) > maxOriginal {
		return
	}
	s := d.state(p, now)
	if s == nil {
		return
	}
	if s.txRate.bypass(now, len(data), s.active && now.Sub(s.report) <= fresh) {
		if s.tx.count != 0 {
			s.tx.reset()
		}
		return
	}
	s.credit = min(2*innerMTU*6, s.credit+len(data))
	if s.tx.count > 0 && now.Sub(s.blockAt) > fresh {
		s.tx.reset()
	}
	if s.tx.count == 0 {
		s.blockAt = now
		s.tx.protected = s.active && now.Sub(s.report) <= fresh
	}
	s.tx.add(data)
	if s.tx.count != blockSize {
		return
	}
	s.group++
	protect := s.tx.protected && s.active && now.Sub(s.report) <= fresh
	// Charge every byte of the inner control packet plus conservative outer
	// overhead. Never wait for repair credit or delay an original to fill a block.
	pieces, innerBytes := s.tx.repairBudget()
	cost := 6 * (innerBytes + pieces*96)
	protect = protect && s.credit >= cost && controlSlots-d.count >= pieces
	if protect || s.group%4 == 0 || now.Sub(s.lastProbe) >= 100*time.Millisecond {
		s.lastProbe = now
		if protect {
			s.credit -= cost
			d.stats.ParityBlocks++
		}
		s.tx.parts(s.group, clock(now), protect, func(part []byte) bool {
			return d.appendControl(p, now, func(out []byte) []byte { return p.session.send(measure, p.local, p.remote, part, out) })
		})
	}
	s.tx.reset()
}

func (d *Device) incoming(data, space []byte, now time.Time, restored func([]byte)) ([]byte, bool) {
	src, dst, ok := packetAddrs(data)
	if !ok {
		return nil, false
	}
	p := d.paths[src]
	if d.client {
		p = d.upstream
	}
	if p == nil {
		return nil, false
	}
	if data[9] != innerProto || dst != p.local || src != p.remote || !isInnerControl(data) {
		if d.profile == autoRequest && !p.session.ready(now) && isTCPSYN(data) {
			p.preflight.peerHandshake(data, now)
		}
		if p.state != nil && p.state.adapt != nil && p.state.adapt.localRX && !p.state.adapt.startRX.active(now) && !now.Before(p.state.lateUntil) && !isTCPSYN(data) {
			return nil, false
		}
		if (d.profile == codedRequest || d.profile == autoRequest) && p.state != nil && now.Before(p.state.wideUntil) && p.session.ready(now) && data[9] == 6 {
			off := int(data[0]&15) * 4
			if len(data) >= off+20 && data[off+13]&2 != 0 {
				p.state.flight.peerHandshake(data, now)
			}
		}
		if d.profile != 0 && p.state != nil && now.Before(p.state.wideUntil) && p.session.ready(now) && eligibleReplica(data) {
			s := p.state
			if now.Before(s.lateUntil) {
				id := packetID(data)
				for i := range s.repairs {
					if s.repairs[i].id == id && now.Before(s.repairs[i].until) {
						s.repairs[i].until = time.Time{}
						return nil, true
					}
				}
			}
			if d.profile == autoRequest {
				a := s.autoState()
				a.meter.received(data, now)
				starting := a.coldCap && a.startRX.data(now, len(data))
				if !starting && (!a.rx.mode.code || now.Sub(a.rx.last) > autoTTL) {
					return nil, false
				}
			}
			if d.profile == codedRequest || d.profile == autoRequest {
				if s.decoder == nil {
					s.decoder = new(codedRX)
				}
				s.decoder.original(data, func(rec []byte) {
					if eligibleReplica(rec) && d.acceptRecovered(p, s, rec, now) {
						restored(rec)
					}
				})
				return nil, false
			}
			if s.copies == nil {
				s.copies = new(replicaCache)
			}
			dup := s.copies.duplicate(data, now)
			if dup {
				d.stats.Duplicates++
			}
			return nil, dup
		}
		if d.profile == autoRequest {
			return nil, false
		}
		if len(data) >= minOriginal && len(data) <= maxOriginal && data[9] != innerProto {
			if s := d.state(p, now); s != nil {
				if s.nextRepair != 0 {
					id := packetID(data)
					for i := range s.repairs {
						if s.repairs[i].id == id && now.Before(s.repairs[i].until) {
							s.repairs[i].until = time.Time{}
							return nil, true
						}
					}
				}
				if s.rxRate.bypass(now, len(data), now.Before(s.keepCacheUntil)) {
					return nil, false
				}
				s.recent.remember(data)
				s.train.record(now, len(data))
				if rec := s.rx.late(data, &s.recent, s.lateBuf[:]); d.acceptRecovered(p, s, rec, now) {
					return rec, false // deliver both the original and the reconstruction
				}
			}
		}
		return nil, false
	}
	var response [innerMTU]byte
	kind, payload, reply, consumed := p.session.receive(&p.k, p.local, p.remote, d.client, data, now, response[:0])
	if !consumed {
		return nil, false
	}
	if len(reply) != 0 {
		d.appendControl(p, now, func(out []byte) []byte { return append(out, reply...) })
	}
	s := d.state(p, now)
	if s == nil {
		return nil, true
	}
	switch kind {
	case wideRequest, wideAccept, codedRequest, codedAccept, autoRequest, autoAccept:
		if d.profile != 0 && len(payload) == 0 && ((!d.client && kind == d.profile) || (d.client && kind == d.profile+1)) {
			s.wideUntil = now.Add(5 * time.Second)
			if !d.client {
				d.appendControl(p, now, func(out []byte) []byte { return p.session.send(d.profile+1, p.local, p.remote, nil, out) })
			}
		}
	case autoSample:
		if d.profile == autoRequest && now.Before(s.wideUntil) {
			d.autoProbe(p, s, payload, now)
		}
	case autoReport:
		if d.profile == autoRequest && now.Before(s.wideUntil) {
			d.autoFeedback(s, payload, now)
		}
	case codedParity:
		if (d.profile == codedRequest || (d.profile == autoRequest && s.adapt != nil && (s.adapt.rx.mode.code || s.adapt.startRX.active(now)))) && now.Before(s.wideUntil) {
			if s.decoder == nil {
				s.decoder = new(codedRX)
			}
			s.decoder.repair(payload, func(rec []byte) {
				if eligibleReplica(rec) && d.acceptRecovered(p, s, rec, now) {
					restored(rec)
				}
			})
		}
	case measure:
		if len(payload) >= 10 && (payload[8] == parityKind || (payload[8] == descKind && payload[9]&descFlagParity != 0)) {
			s.keepCacheUntil = now.Add(fresh)
		}
		rec, miss, measured, stamp := s.rx.receive(payload, &s.recent, space)
		if measured {
			delay := clock(now) - stamp
			if !s.baseSet || int32(delay-s.base) < 0 {
				s.base = delay
				s.baseSet = true
			}
			s.peak = max(s.peak, delay-s.base)
			s.total += blockSize
			s.missing += uint16(miss)
			if s.total >= 64 || s.peak >= queueMS {
				var fb [13]byte
				binary.BigEndian.PutUint32(fb[:], stamp)
				binary.BigEndian.PutUint32(fb[4:], s.peak)
				binary.BigEndian.PutUint16(fb[8:], s.total)
				binary.BigEndian.PutUint16(fb[10:], s.missing)
				if s.train.available(now) {
					fb[12] = 1
				}
				d.appendControl(p, now, func(out []byte) []byte { return p.session.send(feedback, p.local, p.remote, fb[:], out) })
				s.peak, s.total, s.missing = 0, 0, 0
			}
		}
		if d.acceptRecovered(p, s, rec, now) {
			return rec, true
		}
	case feedback:
		if len(payload) != 13 {
			return nil, true
		}
		rtt := time.Duration(clock(now)-binary.BigEndian.Uint32(payload)) * time.Millisecond
		if rtt > 5*time.Second {
			return nil, true
		}
		queue := binary.BigEndian.Uint32(payload[4:])
		total := uint32(binary.BigEndian.Uint16(payload[8:]))
		loss := uint32(binary.BigEndian.Uint16(payload[10:]))
		s.report = now
		if queue >= queueMS {
			s.hold = now.Add(max(4*rtt, time.Second))
		}
		if now.Before(s.hold) || payload[12] != 1 || rtt < 100*time.Millisecond || loss > total {
			s.active = false
			s.quiet = 0
		} else if total >= 64 {
			if loss*1000 >= 3*total && loss*100 <= 3*total {
				s.active = true
				s.quiet = 0
			} else {
				s.quiet++
				if s.quiet >= 3 {
					s.active = false
				}
			}
		}
	}
	return nil, true
}

func (d *Device) acceptRecovered(p *path, s *stream, rec []byte, now time.Time) bool {
	if rec == nil {
		return false
	}
	src, dst, valid := packetAddrs(rec)
	// Reconstruction must not bypass the AWG allowed-IPs lookup.
	if !valid || rec[9] == innerProto || (!d.client && src != p.remote) || (d.client && dst != p.local) {
		return false
	}
	r := &s.repairs[s.nextRepair%uint64(len(s.repairs))]
	r.id = packetID(rec)
	r.until = now.Add(time.Second)
	s.lateUntil = r.until
	if d.client {
		d.localIngress.Store(false)
	}
	s.nextRepair++
	d.stats.Recovered++
	return true
}

func (d *Device) Write(bufs [][]byte, offset int) (int, error) {
	if offset < 0 {
		return 0, io.ErrShortBuffer
	}
	if d.profile == autoRequest {
		local := d.localIngress.Load()
		bulk := true
		for _, b := range bufs {
			if len(b) < offset || len(b[offset:]) < 20 || b[offset+9] == innerProto || isTCPSYN(b[offset:]) || (!local && eligibleReplica(b[offset:])) {
				bulk = false
				break
			}
		}
		if bulk {
			if _, err := d.Device.Write(bufs, offset); err != nil {
				return 0, err
			}
			return len(bufs), nil
		}
	}
	now := time.Now()
	// Preserve batch order and headroom for Linux GRO. Only split at controls;
	// ordinary authenticated traffic is passed to the underlying TUN in bulk.
	start := 0
	for i, buf := range bufs {
		if len(buf) < offset {
			return i, io.ErrShortBuffer
		}
		data := buf[offset:]
		d.mu.Lock()
		var extra [][]byte
		// Codec buffers remain session-owned and Write may run concurrently.
		// Copy only actual reconstructions, never ordinary data or empty controls.
		appendRecovered := func(packet []byte) {
			b := make([]byte, offset+len(packet))
			copy(b[offset:], packet)
			extra = append(extra, b)
		}
		rec, handled := d.incoming(data, buf[offset:cap(buf)], now, appendRecovered)
		if !handled && len(rec) != 0 {
			appendRecovered(rec)
		}
		d.mu.Unlock()
		if len(extra) > 0 {
			end := i
			if !handled {
				end++
			}
			if end > start {
				if _, err := d.Device.Write(bufs[start:end], offset); err != nil {
					return start, err
				}
			}
			if _, err := d.Device.Write(extra, offset); err != nil {
				return end, err
			}
			start = i + 1
			continue
		}
		if !handled {
			continue
		}
		if i > start {
			if _, err := d.Device.Write(bufs[start:i], offset); err != nil {
				return start, err
			}
		}
		if len(rec) > 0 {
			one := [1][]byte{buf[:offset+len(rec)]}
			if _, err := d.Device.Write(one[:], offset); err != nil {
				return i, err
			}
		}
		start = i + 1
	}
	if start < len(bufs) {
		if _, err := d.Device.Write(bufs[start:], offset); err != nil {
			return start, err
		}
	}
	return len(bufs), nil
}

var _ tun.Device = (*Device)(nil)
