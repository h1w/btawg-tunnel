package awgrepair

import (
	"encoding/binary"
	"time"
)

// The latency-oriented WAN profile bounds the amount that remote TCP senders
// can have in flight. At 200 ms, 2 MiB of payload permits ~84 Mbit/s without
// filling a multi-RTT queue. This is per enrolled peer, not per TCP socket.
const (
	flightBytes = 2 << 20
	maxFlights  = 128
	pendingIdle = 3 * time.Second
	flowIdle    = 10 * time.Minute
	flowActive  = 5 * time.Second
)

type ackFlow struct {
	src, dst     uint32
	sport, dport uint16
}

func (f ackFlow) reverse() ackFlow {
	return ackFlow{src: f.dst, dst: f.src, sport: f.dport, dport: f.sport}
}

type ackCredit struct {
	last     time.Time
	edge     uint32
	scale    byte
	synSeq   uint32
	peerSeq  uint32
	hasEdge  bool
	hasScale bool
	active   bool
}
type flightGuard struct {
	flows   map[ackFlow]ackCredit
	pending map[ackFlow]ackCredit
	sweep   time.Time
	active  int
}

func parseFlightPacket(ip []byte) (ackFlow, []byte, int, bool) {
	if len(ip) < 40 || ip[0]>>4 != 4 || ip[9] != 6 || binary.BigEndian.Uint16(ip[6:8])&0x3fff != 0 {
		return ackFlow{}, nil, 0, false
	}
	off := int(ip[0]&15) * 4
	if off < 20 || len(ip) < off+20 {
		return ackFlow{}, nil, 0, false
	}
	t := ip[off:]
	h := int(t[12]>>4) * 4
	if h < 20 || h > len(t) {
		return ackFlow{}, nil, 0, false
	}
	key := ackFlow{binary.BigEndian.Uint32(ip[12:]), binary.BigEndian.Uint32(ip[16:]), binary.BigEndian.Uint16(t), binary.BigEndian.Uint16(t[2:])}
	return key, t, h, true
}

// flightOptions accepts the ordinary options emitted by Linux and rejects
// transports or malformed layouts whose window cannot safely be rewritten.
func flightOptions(t []byte, h int) (scale byte, hasScale, ok bool) {
	for at := 20; at < h; {
		kind := t[at]
		if kind == 0 {
			break
		}
		if kind == 1 {
			at++
			continue
		}
		if at+2 > h {
			return 0, false, false
		}
		size := int(t[at+1])
		if size < 2 || at+size > h {
			return 0, false, false
		}
		switch kind {
		case 2:
			if size != 4 {
				return 0, false, false
			}
		case 3:
			if size != 3 || t[at+2] > 14 {
				return 0, false, false
			}
			scale, hasScale = t[at+2], true
		case 4:
			if size != 2 {
				return 0, false, false
			}
		case 5:
			if (size-2)%8 != 0 {
				return 0, false, false
			}
		case 8:
			if size != 10 {
				return 0, false, false
			}
		default:
			// In particular, MD5 (19) and AO (29) authenticate the window.
			return 0, false, false
		}
		at += size
	}
	return scale, hasScale, true
}

func (g *flightGuard) expire(now time.Time) {
	if now.Sub(g.sweep) < time.Second {
		return
	}
	g.active = 0
	for k, e := range g.flows {
		if now.Sub(e.last) > flowIdle {
			delete(g.flows, k)
			continue
		}
		e.active = now.Sub(e.last) <= flowActive
		if e.active {
			g.active++
		}
		g.flows[k] = e
	}
	for k, e := range g.pending {
		if now.Sub(e.last) > pendingIdle {
			delete(g.pending, k)
		}
	}
	g.sweep = now
}

func (g *flightGuard) drop(key ackFlow) {
	if e, ok := g.flows[key]; ok {
		if e.active {
			g.active--
		}
		delete(g.flows, key)
	}
}

func (g *flightGuard) refresh(key ackFlow, e ackCredit, now time.Time) {
	if !e.active {
		e.active = true
		g.active++
	}
	e.last = now
	g.flows[key] = e
}

func (g *flightGuard) remember(key ackFlow, syn ackCredit) {
	if _, ok := g.pending[key]; !ok && len(g.pending) >= maxFlights {
		return
	}
	if g.pending == nil {
		g.pending = make(map[ackFlow]ackCredit)
	}
	g.pending[key] = syn
}

func (g *flightGuard) activate(key ackFlow, local, peer ackCredit, now time.Time) {
	g.drop(key)
	if len(g.flows) >= maxFlights {
		return
	}
	if g.flows == nil {
		g.flows = make(map[ackFlow]ackCredit)
	}
	var scale byte
	if local.hasScale && peer.hasScale {
		scale = local.scale
	}
	g.flows[key] = ackCredit{last: now, scale: scale, synSeq: local.synSeq, peerSeq: peer.synSeq, active: true}
	g.active++
}

// peerHandshake observes inbound SYNs without modifying them. A flow becomes
// eligible only after sequence/ack association proves both sides negotiated.
func (g *flightGuard) peerHandshake(ip []byte, now time.Time) {
	key, t, h, ok := parseFlightPacket(ip)
	if !ok || t[13]&2 == 0 || t[13]&(1|4) != 0 {
		return
	}
	g.expire(now)
	scale, hasScale, ok := flightOptions(t, h)
	if !ok {
		return
	}
	peer := ackCredit{last: now, scale: scale, synSeq: binary.BigEndian.Uint32(t[4:]), hasScale: hasScale}
	localKey := key.reverse()
	if t[13]&16 == 0 {
		if e, ok := g.flows[localKey]; ok && e.peerSeq == peer.synSeq {
			g.refresh(localKey, e, now)
			return
		}
		g.drop(localKey)
		g.remember(key, peer)
		return
	}
	ack := binary.BigEndian.Uint32(t[8:])
	local, ok := g.pending[localKey]
	if ok && ack == local.synSeq+1 {
		g.activate(localKey, local, peer, now)
		delete(g.pending, localKey)
		delete(g.pending, key)
		return
	}
	if e, ok := g.flows[localKey]; ok && e.synSeq+1 == ack && e.peerSeq == peer.synSeq {
		g.refresh(localKey, e, now)
	}
}

// observe rewrites an eligible outgoing ACK and reports whether its window
// changed. Pending and idle established flows remain bounded independently.
func (g *flightGuard) observe(ip []byte, now time.Time) bool {
	return g.observeMode(ip, now, flightBytes)
}

// Keep the negotiated scales even when auto observes a clean path. This lets
// the same live TCP connection enable and release credit without a reconnect.
func (g *flightGuard) observeMode(ip []byte, now time.Time, budget uint32) bool {
	key, t, h, ok := parseFlightPacket(ip)
	if !ok {
		return false
	}
	g.expire(now)
	flags := t[13]
	if flags&(1|4) != 0 {
		g.drop(key)
		delete(g.pending, key)
		return false
	}
	if flags&2 != 0 {
		scale, hasScale, ok := flightOptions(t, h)
		if !ok {
			g.drop(key)
			delete(g.pending, key)
			return false
		}
		local := ackCredit{last: now, scale: scale, synSeq: binary.BigEndian.Uint32(t[4:]), hasScale: hasScale}
		if flags&16 == 0 {
			if e, ok := g.flows[key]; ok && e.synSeq == local.synSeq {
				g.refresh(key, e, now)
				return false
			}
			g.drop(key)
			g.remember(key, local)
			return false
		}
		reverse := key.reverse()
		peer, ok := g.pending[reverse]
		ack := binary.BigEndian.Uint32(t[8:])
		if ok && ack == peer.synSeq+1 {
			g.activate(key, local, peer, now)
			delete(g.pending, reverse)
			delete(g.pending, key)
			return false
		}
		if e, ok := g.flows[key]; ok && e.synSeq == local.synSeq && e.peerSeq+1 == ack {
			g.refresh(key, e, now)
			return false
		}
		g.drop(key)
		delete(g.pending, key)
		return false
	}
	if flags&16 == 0 {
		return false
	}
	e, ok := g.flows[key]
	if !ok {
		return false
	}
	if _, _, ok := flightOptions(t, h); !ok {
		g.drop(key)
		return false
	}
	g.refresh(key, e, now)
	e = g.flows[key]
	// Never enlarge the real advertised window (including zero), or retract an
	// earlier right edge merely because a new flow started sharing the budget.
	old := binary.BigEndian.Uint16(t[14:])
	ack := binary.BigEndian.Uint32(t[8:])
	if budget == 0 {
		e.edge = ack + uint32(old)<<e.scale
		e.hasEdge = true
		g.flows[key] = e
		return false
	}
	step := uint32(1) << e.scale
	limit := budget / uint32(max(g.active, 1)) / step
	if e.hasEdge {
		if outstanding := int32(e.edge - ack); outstanding > 0 {
			limit = max(limit, (uint32(outstanding)+step-1)/step)
		}
	}
	next := uint16(min(uint32(old), limit))
	e.edge = ack + uint32(next)*step
	e.hasEdge = true
	g.flows[key] = e
	if next == old {
		return false
	}
	binary.BigEndian.PutUint16(t[14:], next)
	// RFC 1624: replace one aligned 16-bit word of the TCP checksum input.
	sum := uint32(^binary.BigEndian.Uint16(t[16:])) + uint32(^old) + uint32(next)
	sum = (sum & 65535) + (sum >> 16)
	sum = (sum & 65535) + (sum >> 16)
	binary.BigEndian.PutUint16(t[16:], ^uint16(sum))
	return true
}
