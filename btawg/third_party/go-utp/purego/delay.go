package purego

import "math"

// Number of recent delay samples the current delay estimate is the minimum of.
const curDelaySize = 3

// Number of one minute delay base slots kept. Experiments suggest a clock skew of 10ms per 325
// seconds is not impossible, so the base is rotated out over 13 minutes. The skew itself is dealt
// with by watching the delay base in the other direction and shifting our own up when the peer's
// keeps going down.
const delayBaseHistory = 13

// Compares two counters that wrap within mask, returning whether lhs is behind rhs. The shorter
// direction around the ring wins.
func wrappingCompareLess(lhs, rhs, mask uint32) bool {
	distDown := (lhs - rhs) & mask
	distUp := (rhs - lhs) & mask
	return distUp < distDown
}

// Tracks the queuing delay in one direction, as the difference between recent one way delay
// samples and the lowest sample seen recently. Only the difference means anything: the samples
// themselves are the two peers' unsynchronized clocks subtracted from each other.
type delayHist struct {
	delayBase uint32
	// Recent samples, with delayBase removed. Always positive, in microseconds.
	curDelayHist [curDelaySize]uint32
	curDelayIdx  int
	// The history of delayBase. Only relative values are meaningful.
	delayBaseHist [delayBaseHistory]uint32
	delayBaseIdx  int
	// When we last stepped delayBaseIdx.
	delayBaseTime uint64

	initialized bool
}

func (h *delayHist) clear(currentMS uint64) {
	*h = delayHist{delayBaseTime: currentMS}
}

// Adds offset to every delay base, to account for observed clock skew.
func (h *delayHist) shift(offset uint32) {
	for i := range h.delayBaseHist {
		h.delayBaseHist[i] += offset
	}
	h.delayBase += offset
}

func (h *delayHist) addSample(sample uint32, currentMS uint64) {
	// All the arithmetic here is deliberately wrapping. The delay base drifts with the peers'
	// clocks and will eventually cross the 32 bit boundary in one direction or the other, and
	// wrapping subtraction is what makes the comparison come out right either way.
	if !h.initialized {
		for i := range h.delayBaseHist {
			h.delayBaseHist[i] = sample
		}
		h.delayBase = sample
		h.initialized = true
	}
	if wrappingCompareLess(sample, h.delayBaseHist[h.delayBaseIdx], math.MaxUint32) {
		h.delayBaseHist[h.delayBaseIdx] = sample
	}
	if wrappingCompareLess(sample, h.delayBase, math.MaxUint32) {
		h.delayBase = sample
	}
	h.curDelayHist[h.curDelayIdx] = sample - h.delayBase
	h.curDelayIdx = (h.curDelayIdx + 1) % curDelaySize
	// Once a minute, retire the oldest delay base slot and take the base from the remaining
	// history. That bounds how long a spuriously low sample can hold the base down.
	if currentMS-h.delayBaseTime > 60*1000 {
		h.delayBaseTime = currentMS
		h.delayBaseIdx = (h.delayBaseIdx + 1) % delayBaseHistory
		h.delayBaseHist[h.delayBaseIdx] = sample
		h.delayBase = h.delayBaseHist[0]
		for _, b := range h.delayBaseHist {
			if wrappingCompareLess(b, h.delayBase, math.MaxUint32) {
				h.delayBase = b
			}
		}
	}
}

// The current delay estimate in microseconds: the smallest of the recent samples. Reads as zero
// until there have been samples, since the history starts out zeroed.
func (h *delayHist) value() uint32 {
	value := uint32(math.MaxUint32)
	for _, d := range h.curDelayHist {
		if d < value {
			value = d
		}
	}
	return value
}

// A route step raises both one-way delay floors. A filled forward FIFO does
// not. Keep half the common rise as delay pressure: this is evidence from
// ACKs, not a way to distinguish an arbitrary pair of full queues from WAN.
type propagationStep struct {
	start, last uint64 // milliseconds
	low, high   uint32 // microseconds, across both directions
}

func (p *propagationStep) observe(ms uint64, forward, reverse uint32, rttRise, srttRise int64) uint32 {
	lo, hi := min(forward, reverse), max(forward, reverse)
	// Compare additional RTT with additional delay, not absolute RTT.
	if lo < 20_000 || hi-lo > 8_000 || rttRise < int64(lo)*2-8_000 || rttRise > int64(hi)*2+8_000 || srttRise > int64(hi)*2+20_000 {
		p.start = 0
		return 0
	}
	if p.start == 0 || ms-p.last > 750 || max(p.high, hi)-min(p.low, lo) > 8_000 {
		*p = propagationStep{start: ms, last: ms, low: lo, high: hi}
		return 0
	}
	p.last, p.low, p.high = ms, min(p.low, lo), max(p.high, hi)
	if ms-p.start < 2_000 {
		return 0
	}
	p.start = 0
	return p.low / 2
}
