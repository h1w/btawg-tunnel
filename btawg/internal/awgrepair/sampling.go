package awgrepair

import "time"

// Once unassisted delivery already sustains 32 Mbit/s there is no reason to
// hash/cache every original to discover a slow loss-limited path. Recheck
// the native rate every 100 ms. Do not suppress an active repair flight.
type cleanRate struct {
	since time.Time
	bytes int64
	fast  bool
}

func (g *cleanRate) bypass(now time.Time, size int, repairing bool) bool {
	if g.since.IsZero() {
		g.since = now
	}
	if elapsed := now.Sub(g.since); elapsed >= 100*time.Millisecond {
		g.fast = elapsed <= time.Second && g.bytes*int64(time.Second) >= 4_000_000*int64(elapsed)
		g.since = now
		g.bytes = 0
	}
	g.bytes += int64(size)
	return g.fast && !repairing
}
