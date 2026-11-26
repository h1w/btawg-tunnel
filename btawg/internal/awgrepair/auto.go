package awgrepair

import (
	"encoding/binary"
	"time"
)

const autoTTL = 3 * time.Second

// Feedback is receiver-driven. RX measures originals before local repair;
// TX is a short-lived grant, never evidence that the opposite path is lossy.
type autoMode struct{ code, clamp bool }
type autoPolicy struct {
	mode                                  autoMode
	last, lossAt, firstLoss, blockedUntil time.Time
	cleanCount                            int
	pressure                              int
	budget                                uint32
	baseRTT                               uint16
}

func (a *autoPolicy) update(e autoEvidence, now time.Time, provisional bool) autoMode {
	if e.total <= 0 || e.missing < 0 || e.missing > e.total {
		return a.mode
	}
	if !a.last.IsZero() && now.Sub(a.last) > autoTTL {
		*a = autoPolicy{}
	}
	a.last = now
	if e.queue >= 15*time.Millisecond {
		a.pressure = min(a.pressure+1, 8)
	} else {
		a.pressure = max(a.pressure-1, 0)
	}
	if a.pressure >= 3 && e.rate < 3_000_000 && !e.headroom {
		a.blockedUntil = now.Add(2 * time.Second)
	}
	if e.missing > 0 && e.missing*2 <= e.total {
		a.cleanCount = 0
		if a.firstLoss.IsZero() || now.Sub(a.lossAt) > 2*time.Second {
			a.firstLoss = now
		}
		a.lossAt = now
		if !a.firstLoss.Equal(now) && !now.Before(a.blockedUntil) && e.headroom && (a.mode.code || provisional || e.rate < 8_000_000) {
			a.mode.code = true
		}
	} else if e.missing == 0 {
		a.cleanCount = min(a.cleanCount+e.total, 1<<16)
	}
	if now.Before(a.blockedUntil) || (a.cleanCount >= 256 && now.Sub(a.lossAt) >= 3*time.Second) {
		a.mode.code = false
	}
	if a.mode.code {
		a.budget = flightBytes
	} else if a.pressure >= 3 && !e.headroom && e.rate < 3_000_000 {
		ms := max(uint64(a.baseRTT), 80) + 5
		a.budget = uint32(max(uint64(64<<10), e.rate*ms/1000))
	} else {
		a.budget = 0
	}
	a.mode.clamp = a.budget != 0
	return a.mode
}

// A new WAN session has no data-path samples. Protect the initial growth
// of CUBIC, then return control to raw-loss feedback. The budget is per
// peer and direction, not per TCP flow; keepalives cannot renew it.
type coldFlight struct {
	until time.Time
	bytes uint32
}

const coldFlightBytes = 32 << 20
const coldCapVersion = 1
const coldMinRTT = 80 * time.Millisecond

func (f *coldFlight) active(now time.Time) bool {
	return now.Before(f.until) && f.bytes < coldFlightBytes
}
func (f *coldFlight) data(now time.Time, n int) bool {
	if f.until.IsZero() {
		f.until = now.Add(4 * time.Second)
	}
	if !f.active(now) {
		return false
	}
	if uint32(n) > coldFlightBytes-f.bytes {
		f.bytes = coldFlightBytes
		return false
	}
	f.bytes += uint32(n)
	return true
}

// A filled LAN queue must not turn on WAN-only work. Keep the minimum
// of two adjacent ten-second windows so a genuine route change ages in.
type rttFloor struct {
	start             time.Time
	current, previous uint16
	hasPrevious       bool
}

func (f *rttFloor) sample(now time.Time, ms uint16) uint16 {
	const period = 10 * time.Second
	age := now.Sub(f.start)
	switch {
	case f.start.IsZero() || age >= 2*period:
		f.start, f.current, f.hasPrevious = now, ms, false
	case age >= period:
		f.start = f.start.Add(period)
		f.previous, f.current, f.hasPrevious = f.current, ms, true
	default:
		f.current = min(f.current, ms)
	}
	if f.hasPrevious {
		return min(f.previous, f.current)
	}
	return f.current
}

type autoDirection struct {
	meter            autoMeter
	rx               autoPolicy
	tx               autoMode
	txUntil          time.Time
	lastStamp        uint32
	rxStamp          uint32
	hasRXStamp       bool
	rttMS            uint16
	floor            rttFloor
	hasStamp         bool
	coldCap          bool
	cleanStart       bool // repeated in reports in case the first release is lost
	localTX, localRX bool // low-RTT path: periodic RTT probes, no per-packet identity cache
	nextRTT          time.Time
	startTX, startRX coldFlight
}

func (s *stream) autoState() *autoDirection {
	if s.adapt == nil {
		s.adapt = new(autoDirection)
	}
	return s.adapt
}
func (d *Device) autoProbe(p *path, s *stream, payload []byte, now time.Time) {
	a := s.autoState()
	if len(payload) < 2 {
		return
	}
	rtt := binary.BigEndian.Uint16(payload)
	wan := rtt >= 80
	payload = payload[2:]
	if len(payload) < 5 {
		return
	}
	stamp := binary.BigEndian.Uint32(payload)
	if a.hasRXStamp && int32(stamp-a.rxStamp) <= 0 {
		return
	}
	ev, ok := a.meter.probe(payload, now)
	if !ok {
		return
	}
	a.rxStamp, a.hasRXStamp = ev.stamp, true
	a.localRX = !wan
	if d.client {
		d.localIngress.Store(a.localRX && !a.startRX.active(now) && !now.Before(s.lateUntil))
	}
	before := a.rx.mode
	mode := autoMode{}
	if wan {
		if a.rx.baseRTT == 0 || rtt < a.rx.baseRTT {
			a.rx.baseRTT = rtt
		}
		// A successful provisional flight must not disqualify itself by rate.
		// Keeping it still requires repeated raw loss and serialization headroom.
		// A full decoder group proves delivery of eight original packets, not
		// just the sparse IDs sampled by the loss meter. A reconstruction in
		// this generation disqualifies the early clean release.
		if !a.cleanStart && a.startRX.active(now) && s.decoder != nil && s.decoder.intactGroups >= 64 && s.nextRepair == 0 && a.rx.lossAt.IsZero() && ev.missing == 0 {
			a.cleanStart = true
			a.startRX.until = now
		}
		mode = a.rx.update(ev, now, a.startRX.active(now))
		if mode.clamp && d.client {
			d.localReadUntil.Store(0)
		}
		if now.Before(a.rx.blockedUntil) {
			a.startRX.until = now
		}
	} else {
		a.rx = autoPolicy{}
	}
	if before.code != mode.code {
		if mode.code {
			d.stats.AutoRxOn++
		} else {
			d.stats.AutoRxOff++
		}
	}
	d.stats.AutoSamples += uint64(ev.total)
	d.stats.AutoMissing += uint64(ev.missing)
	var report [5]byte
	binary.BigEndian.PutUint32(report[:], ev.stamp)
	if mode.code {
		report[4] |= 1
	}
	if (a.cleanStart || now.Before(a.rx.blockedUntil)) && a.coldCap {
		report[4] |= 2
	}
	// Only the receiver limits advertised TCP credit. The sender only needs
	// the coding decision for its own direction.
	d.appendControl(p, now, func(out []byte) []byte { return p.session.send(autoReport, p.local, p.remote, report[:], out) })
}
func (d *Device) autoFeedback(s *stream, payload []byte, now time.Time) {
	if len(payload) != 5 || payload[4] > 3 {
		return
	}
	stamp := binary.BigEndian.Uint32(payload)
	rtt := time.Duration(clock(now)-stamp) * time.Millisecond
	a := s.autoState()
	if rtt > autoTTL || a.hasStamp && int32(stamp-a.lastStamp) <= 0 {
		return
	}
	a.hasStamp = true
	a.lastStamp = stamp
	a.rttMS = a.floor.sample(now, uint16(rtt/time.Millisecond))
	a.localTX = time.Duration(a.rttMS)*time.Millisecond < coldMinRTT
	if d.client && !a.localTX {
		d.localReadUntil.Store(0)
	}
	if payload[4]&2 != 0 && a.coldCap {
		a.startTX.bytes = coldFlightBytes
	}
	code := payload[4]&1 != 0 && !a.localTX
	if code != a.tx.code {
		if code {
			d.stats.AutoTxOn++
		} else {
			d.stats.AutoTxOff++
		}
		if s.encoder != nil {
			s.encoder.reset()
		}
	}
	a.tx = autoMode{code: code}
	a.txUntil = now.Add(autoTTL)
}
