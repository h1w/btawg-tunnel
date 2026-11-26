package server

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
	"golang.org/x/net/ipv4"

	"bittorrent-tunnel/btawg/internal/awgrepair"
	"bittorrent-tunnel/btawg/internal/config"
	"bittorrent-tunnel/btawg/internal/hubrelay"
	"bittorrent-tunnel/btawg/internal/mesh"
	"bittorrent-tunnel/btawg/internal/tunneldns"
)

const (
	routerBufSize   = 2048
	nativeBatchSize = 16
)

var tunOffset = device.MessageTransportOffsetContent // 16 bytes of virtio/transport headroom

// packetRouter multiplexes one shared inner TUN across cryptographic engines:
// one AWG device per BitTorrent carrier mode, separate native AWG and WG
// devices, and one engine per authenticated TLS session. Outbound packets read
// from the inner TUN are routed to the engine that owns the destination overlay
// address, with per-flow reverse routing taking precedence over the per-IP
// primary route. Inbound packets from an engine are filtered, have their source
// recorded, and are forwarded to the inner TUN (gateway or internet traffic) or
// injected directly into the owning engine of the peer (client-to-client).
type packetRouter struct {
	inner    tun.Device
	isKernel bool
	policy   *policySnapshot

	awg     *engineTun
	wg      *engineTun
	engines map[string]*engineTun // keyed by canonical carrier mode

	routesMu sync.RWMutex
	routes   map[netip.Addr]activeRoute

	flows         *flowTable
	tlsQueueDrops atomic.Uint64

	closed    chan struct{}
	closeOnce sync.Once
	closeErr  error

	pool           sync.Pool
	writeBatchPool sync.Pool
	readWg         sync.WaitGroup
	eventWg        sync.WaitGroup
	flowWg         sync.WaitGroup
}

type activeRoute struct {
	eng *engineTun
	at  int64
}

// Pool pointers, not slice headers: returning a slice to sync.Pool boxes it
// in a heap object on the packet hot path.
type routerPacket struct {
	buf [routerBufSize]byte
	end int
}

// engineTun is the tun.Device presented to one cryptographic engine. For the
// AWG and WG devices authSource is zero and the engine is trusted to assert
// the source address; a TLS session sets authSource to its enrolled overlay IP
// so a client cannot spoof another client's source.
type engineTun struct {
	router     *packetRouter
	authSource netip.Addr
	packets    chan *routerPacket
	events     chan tun.Event
	closed     chan struct{}
	mu         sync.Mutex
	closedFlag bool
	queueDrops atomic.Uint64
	recovery   atomic.Pointer[awgrepair.Device] // AWG engines only; admission consults it on overflow
}

// newPacketRouter creates a router around the shared inner TUN. Peer traffic
// moves directly between engines; only gateway and routed egress use the TUN.
func newPacketRouter(inner tun.Device, isKernel bool, policy *policySnapshot) *packetRouter {
	r := &packetRouter{
		inner:    inner,
		isKernel: isKernel,
		policy:   policy,
		routes:   make(map[netip.Addr]activeRoute),
		engines:  make(map[string]*engineTun),
		flows:    newFlowTable(),
		closed:   make(chan struct{}),
	}
	r.pool.New = func() interface{} {
		return new(routerPacket)
	}
	r.writeBatchPool.New = func() any { return new([conn.IdealBatchSize][]byte) }
	r.awg = r.newEngine(netip.Addr{})
	r.wg = r.newEngine(netip.Addr{})

	r.readWg.Add(1)
	go r.dispatchLoop()
	r.eventWg.Add(1)
	go r.fanoutEvents()
	r.flowWg.Add(1)
	go r.flowCleanupLoop()
	return r
}

// engineForMode returns the engine associated with a canonical carrier mode,
// creating it on first use. The caller must not use this concurrently with
// mode creation during server startup.
func (r *packetRouter) engineForMode(mode string) *engineTun {
	r.routesMu.Lock()
	defer r.routesMu.Unlock()
	e, ok := r.engines[mode]
	if !ok {
		e = r.newEngine(netip.Addr{})
		r.engines[mode] = e
	}
	return e
}

// Snapshot the five AWG engines without retaining the route lock during peer reload.
func (r *packetRouter) awgEngines() [5]*engineTun {
	r.routesMu.RLock()
	defer r.routesMu.RUnlock()
	return [5]*engineTun{r.awg, r.engines[config.ModeExtTCP], r.engines[config.ModeExtUTP], r.engines[config.ModeReqTCP], r.engines[config.ModeReqUTP]}
}

// newEngine creates a queue. For TLS, authSource pins the enrolled IP.
func (r *packetRouter) newEngine(authSource netip.Addr) *engineTun {
	e := &engineTun{
		router:     r,
		authSource: authSource,
		packets:    make(chan *routerPacket, 256),
		events:     make(chan tun.Event, 8),
		closed:     make(chan struct{}),
	}
	return e
}

// removeEngine drops the route only if this session still owns it and
// invalidates any pinned flows for the engine.
func (r *packetRouter) removeEngine(e *engineTun) {
	r.routesMu.Lock()
	if r.routes[e.authSource].eng == e {
		delete(r.routes, e.authSource)
	}
	r.routesMu.Unlock()
	r.flows.removeForEngine(e)
}

// close shuts down the router and the shared inner TUN once. It is safe to
// call multiple times; the parent is responsible for calling this after the
// engine devices have been closed.
func (r *packetRouter) close() error {
	r.closeOnce.Do(func() {
		close(r.closed)
		r.closeErr = r.inner.Close()
		r.readWg.Wait()
		r.eventWg.Wait()
		r.drainQueue(r.awg.packets)
		r.drainQueue(r.wg.packets)
		for _, e := range r.engines {
			r.drainQueue(e.packets)
		}
		r.flowWg.Wait()
	})
	return r.closeErr
}

func (r *packetRouter) flowCleanupLoop() {
	defer r.flowWg.Done()
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			r.flows.cleanup(time.Now())
		case <-r.closed:
			return
		}
	}
}

func (r *packetRouter) remember(ip netip.Addr, e *engineTun, now int64) {
	r.routesMu.Lock()
	if current, ok := r.routes[ip]; !ok || now > current.at {
		r.routes[ip] = activeRoute{eng: e, at: now}
	}
	r.routesMu.Unlock()
}

func (r *packetRouter) lookup(ip netip.Addr) (*engineTun, bool) {
	r.routesMu.RLock()
	route, ok := r.routes[ip]
	r.routesMu.RUnlock()
	return route.eng, ok
}

func (r *packetRouter) observeHandshake(raw, key string, e *engineTun, at time.Time) {
	p, err := netip.ParsePrefix(raw)
	if err != nil || p.Bits() != 32 || !p.Addr().Is4() || !r.policy.handshake(p.Addr(), key, at) {
		return
	}
	r.remember(p.Addr(), e, at.UnixNano())
}

// forget unconditionally drops the route and all pinned flows for ip, used
// when a client is revoked or moves to a new overlay address.
func (r *packetRouter) forget(ip netip.Addr) {
	r.routesMu.Lock()
	delete(r.routes, ip)
	r.routesMu.Unlock()
	r.flows.removeForIP(ip)
}

// dispatchLoop reads batches from the shared inner TUN and routes each valid
// IPv4 packet to the engine responsible for its destination. Unknown paths
// and disallowed packets are dropped; buffers are returned to the pool.
func (r *packetRouter) dispatchLoop() {
	defer r.readWg.Done()

	batch := r.inner.BatchSize()
	if batch < 1 {
		batch = 1
	}
	bufs := make([][]byte, batch)
	storage := make([]*routerPacket, batch)
	sizes := make([]int, batch)
	for i := range bufs {
		storage[i] = r.pool.Get().(*routerPacket)
		bufs[i] = storage[i].buf[:]
	}
	defer func() {
		for _, p := range storage {
			r.pool.Put(p)
		}
	}()

	var burstTimer *time.Timer // one reader; reused across all pressured batches
	defer func() {
		if burstTimer != nil {
			burstTimer.Stop()
		}
	}()
	for {
		n, err := r.inner.Read(bufs, sizes, tunOffset)
		if err != nil && !errors.Is(err, tun.ErrTooManySegments) {
			return
		}
		now := time.Now()
		nativeYielded, carrierYielded := false, false
		var burstDeadline <-chan time.Time
		budgetSpent := false
		for i := range n {
			if sizes[i] < 1 || sizes[i] > len(bufs[i])-tunOffset {
				continue
			}
			packet := bufs[i][tunOffset : tunOffset+sizes[i]]
			src, srcOK := ipv4Src(packet)
			dst, dstOK := ipv4Dst(packet)
			if !srcOK || !dstOK {
				continue
			}
			if !r.policy.downlink(src, dst, 0) {
				continue
			}
			eng := r.destinationEngine(dst, packet, now, true)
			if eng == nil {
				continue // unknown path: drop
			}
			p := storage[i]
			p.end = tunOffset + sizes[i]
			accepted, full := eng.tryEnqueue(p)
			// Carrier pressure cannot consume the original native WG/AWG retry.
			yielded := &nativeYielded
			if eng != r.wg && eng != r.awg {
				yielded = &carrierYielded
			}
			if full && !*yielded && (eng == r.wg || eng == r.awg || eng.recovery.Load() != nil) {
				*yielded = true
				runtime.Gosched()
				accepted, full = eng.tryEnqueue(p)
			}
			if full && !budgetSpent {
				if repair := eng.recovery.Load(); repair != nil && repair.WideActive(dst, now) {
					// Bound waiting across the whole TUN read, not per engine or peer.
					if burstDeadline == nil {
						if burstTimer == nil {
							burstTimer = time.NewTimer(time.Millisecond)
						} else {
							burstTimer.Reset(time.Millisecond)
						}
						burstDeadline = burstTimer.C
					}
					accepted, full = eng.admitBurst(p, burstDeadline)
					budgetSpent = full
				}
			}
			if full {
				eng.countQueueDrop()
			}
			if accepted {
				storage[i] = r.pool.Get().(*routerPacket)
				bufs[i] = storage[i].buf[:]
			}
		}
	}
}

// destinationEngine selects the recipient by reverse flow, then primary route.
// On admission, the first fragment records that decision for the rest of the
// datagram. Dequeue rechecks the recorded destination without changing it.
func (r *packetRouter) destinationEngine(dst netip.Addr, packet []byte, now time.Time, admit bool) *engineTun {
	if fragKey, ok := fragmentKeyFromPacket(packet); ok {
		if admit && fragmentOffset(packet) == 0 {
			eng := r.lookupFlowEngine(dst, packet, now)
			if eng != nil {
				r.flows.pinFragment(fragKey, eng)
			}
			return eng
		}
		if eng, ok := r.flows.lookupFragment(fragKey, now); ok {
			return eng
		}
	}
	return r.lookupFlowEngine(dst, packet, now)
}

func (r *packetRouter) lookupFlowEngine(dst netip.Addr, packet []byte, now time.Time) *engineTun {
	if key, ok := flowKeyFromPacket(packet); ok {
		if eng, ok := r.flows.lookup(key, now); ok {
			return eng
		}
	}
	if eng, ok := r.lookup(dst); ok {
		return eng
	}
	return nil
}

// fanoutEvents copies events from the inner TUN to each engine's event
// channel so all devices observe MTU updates and down events. Events for an
// already-closed engine are dropped.
func (r *packetRouter) fanoutEvents() {
	defer r.eventWg.Done()
	src := r.inner.Events()
	if src == nil {
		return
	}
	for {
		select {
		case ev, ok := <-src:
			if !ok {
				return
			}
			r.awg.sendEvent(ev)
			r.wg.sendEvent(ev)
			r.routesMu.RLock()
			engines := make([]*engineTun, 0, len(r.engines))
			for _, e := range r.engines {
				engines = append(engines, e)
			}
			r.routesMu.RUnlock()
			for _, e := range engines {
				e.sendEvent(ev)
			}
		case <-r.closed:
			return
		}
	}
}

func (e *engineTun) sendEvent(ev tun.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closedFlag {
		return
	}
	select {
	case e.events <- ev:
	default:
	}
}

// inject places a copy of packet into an engine's inbound queue.
func (r *packetRouter) inject(e *engineTun, packet []byte) {
	p := r.pool.Get().(*routerPacket)
	p.end = tunOffset + copy(p.buf[tunOffset:], packet)
	if !e.enqueue(p) {
		r.pool.Put(p)
	}
}

func (e *engineTun) enqueue(p *routerPacket) bool {
	accepted, full := e.tryEnqueue(p)
	if full {
		e.countQueueDrop()
	}
	return accepted
}

func (e *engineTun) tryEnqueue(p *routerPacket) (accepted, full bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closedFlag {
		return false, false
	}
	select {
	case <-e.router.closed:
		return false, false
	case e.packets <- p:
		return true, false
	default:
		return false, true
	}
}

// admitBurst waits only for the dispatcher's remaining batch budget. Native
// queues are drained after the shared reader stops; packets is never closed.
func (e *engineTun) admitBurst(p *routerPacket, deadline <-chan time.Time) (accepted, full bool) {
	select {
	case <-e.closed:
	case <-e.router.closed:
	case e.packets <- p:
		return true, false
	case <-deadline:
		return false, true
	}
	return false, false
}

func (e *engineTun) countQueueDrop() {
	if e.authSource.IsValid() {
		e.router.tlsQueueDrops.Add(1)
	} else {
		e.queueDrops.Add(1)
	}
}

func (r *packetRouter) drainQueue(ch <-chan *routerPacket) {
	for {
		select {
		case p := <-ch:
			r.pool.Put(p)
		default:
			return
		}
	}
}

// Read waits for the first accepted packet, then drains any packets already
// queued up to the engine's batch size. Recheck route ownership and policy on
// each packet so routing changes and revocation also apply to queued packets.
func (e *engineTun) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	return e.read(bufs, sizes, offset, nil)
}

// ReadWithWake lets an outer control queue interrupt only the blocking first
// packet wait. Once a packet is available, batch draining remains non-blocking.
func (e *engineTun) ReadWithWake(bufs [][]byte, sizes []int, offset int, wake <-chan struct{}) (int, error) {
	return e.read(bufs, sizes, offset, wake)
}

func (e *engineTun) read(bufs [][]byte, sizes []int, offset int, wake <-chan struct{}) (int, error) {
	if len(bufs) == 0 || len(sizes) < len(bufs) || offset < 0 || len(bufs[0]) <= offset {
		return 0, errShortBuffer
	}
	limit := min(len(bufs), e.BatchSize())
	n := 0
	var now time.Time
	for n < limit {
		var p *routerPacket
		if n == 0 {
			select {
			case <-e.closed:
				return 0, net.ErrClosed
			case <-e.router.closed:
				return 0, net.ErrClosed
			case <-wake:
				return 0, nil
			case p = <-e.packets:
			}
		} else {
			select {
			case <-e.closed:
				return n, net.ErrClosed
			case <-e.router.closed:
				return n, net.ErrClosed
			case p = <-e.packets:
			default:
				return n, nil
			}
		}
		if p.end <= tunOffset {
			e.router.pool.Put(p)
			continue
		}
		packet := p.buf[tunOffset:p.end]
		if len(packet) > len(bufs[n])-offset {
			e.router.pool.Put(p)
			return n, errShortBuffer
		}
		src, srcOK := ipv4Src(packet)
		dst, dstOK := ipv4Dst(packet)
		if n == 0 {
			// Take the timestamp after the blocking receive, then reuse it
			// for the rest of this non-blocking batch, as dispatchLoop does.
			now = time.Now()
		}
		if !srcOK || !dstOK || e.router.destinationEngine(dst, packet, now, false) != e ||
			!e.router.policy.downlink(src, dst, len(packet)) {
			e.router.pool.Put(p)
			continue
		}
		sizes[n] = copy(bufs[n][offset:], packet)
		e.router.pool.Put(p)
		n++
	}
	return n, nil
}

// Write receives decrypted packets from the engine, filters them, pins the
// authenticated reverse flow to this engine, and forwards them toward their
// destination.
func (e *engineTun) Write(bufs [][]byte, offset int) (int, error) {
	var innerBufs [][]byte
	var scratch *[conn.IdealBatchSize][]byte
	r := e.router
	now := time.Now()
	for _, b := range bufs {
		if len(b) < offset+ipv4.HeaderLen {
			continue
		}
		packet := b[offset:]
		src, srcOK := ipv4Src(packet)
		dst, dstOK := ipv4Dst(packet)
		if !srcOK || !dstOK {
			continue
		}
		if e.authSource.IsValid() && src != e.authSource {
			continue
		}
		direct := e.router.isKernel && !e.router.policy.isClient(dst) && !e.router.policy.isGateway(dst)
		if !e.router.policy.upload(src, dst, direct, len(packet), now) {
			continue
		}
		// Pin the reverse flow so replies return to this engine. This is the
		// only moment a flow's ownership is established; route changes and
		// later handshakes cannot steal an active flow.
		if !e.pinReverseFlow(packet, now) || !e.router.policy.isGateway(dst) {
			e.router.remember(src, e, now.UnixNano())
		}
		if e.router.policy.isGateway(dst) || direct {
			if innerBufs == nil {
				// Linux GRO may extend or reorder the slice headers.
				if len(bufs) <= conn.IdealBatchSize {
					scratch = e.router.writeBatchPool.Get().(*[conn.IdealBatchSize][]byte)
					innerBufs = scratch[:0]
				} else {
					innerBufs = make([][]byte, 0, len(bufs))
				}
			}
			innerBufs = append(innerBufs, b)
			continue
		}
		if e.router.policy.isClient(dst) {
			eng := r.destinationEngine(dst, packet, now, true)
			if eng == nil {
				continue
			}
			e.router.inject(eng, packet)
			continue
		}
	}
	if len(innerBufs) > 0 {
		_, err := r.inner.Write(innerBufs, offset)
		if scratch != nil {
			clear(innerBufs)
			r.writeBatchPool.Put(scratch)
		}
		if err != nil {
			return 0, err
		}
	}
	return len(bufs), nil
}

// pinReverseFlow records the sender's engine for reply traffic. It reports a
// hub service flow, which must not let health/control requests change
// unrelated routes.
func (e *engineTun) pinReverseFlow(packet []byte, now time.Time) (service bool) {
	if key, ok := flowKeyFromPacket(packet); ok {
		e.router.flows.pin(key.reverse(), e, now)
		service = key.proto == 6 && (key.b == tunneldns.Port || key.b == mesh.ControlPort || key.b == mesh.RelayPort || key.b == hubrelay.Port)
	}
	return service
}

// Close shuts down this engine's virtual view. It closes the engine's event
// stream so the owning device can finish its close sequence, but it does not
// close the shared inner TUN; the parent owns that.
func (e *engineTun) Close() error {
	e.mu.Lock()
	if e.closedFlag {
		e.mu.Unlock()
		return nil
	}
	e.closedFlag = true
	close(e.closed)
	close(e.events)
	if e.authSource.IsValid() {
		e.router.drainQueue(e.packets)
	}
	e.mu.Unlock()
	e.router.flows.removeForEngine(e)
	return nil
}

func (e *engineTun) MTU() (int, error)        { return e.router.inner.MTU() }
func (e *engineTun) Name() (string, error)    { return e.router.inner.Name() }
func (e *engineTun) Events() <-chan tun.Event { return e.events }
func (e *engineTun) File() *os.File           { return e.router.inner.File() }
func (e *engineTun) BatchSize() int {
	if e == e.router.awg || e == e.router.wg || e.recovery.Load() != nil {
		return nativeBatchSize
	}
	return 1
}
