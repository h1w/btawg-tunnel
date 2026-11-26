package awgrepair

import "time"

// A recent train of full data datagrams delivered across distinct reads can
// show spare serialization capacity even when the loss-limited TCP average
// is small. Never infer capacity from a single GRO/read burst or idle time.
// This is a conservative eligibility signal, not an available-bandwidth cap.
type arrivalTrain struct {
	start, last, fast time.Time
	bytes, reads      int
}

func (a *arrivalTrain) record(now time.Time, size int) {
	if size < 900 {
		return
	}
	if a.start.IsZero() || now.Sub(a.last) > 4*time.Millisecond || now.Sub(a.start) > 20*time.Millisecond {
		a.start, a.last, a.bytes, a.reads = now, now, 0, 1
		return
	}
	if now.Equal(a.start) {
		return
	}
	if !now.Equal(a.last) {
		a.reads++
	}
	a.last = now
	a.bytes += size
	elapsed := now.Sub(a.start)
	if a.bytes >= 12*1024 && a.reads >= 3 && int64(a.bytes)*int64(time.Second) >= 4_000_000*int64(elapsed) {
		a.fast = now
	}
}

func (a *arrivalTrain) available(now time.Time) bool {
	return !a.fast.IsZero() && now.Sub(a.fast) <= 2*time.Second
}
