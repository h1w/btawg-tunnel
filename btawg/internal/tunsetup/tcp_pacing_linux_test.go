package tunsetup

import (
	"testing"
	"time"
)

func TestRelayPacingProbesAndBacksOff(t *testing.T) {
	var p relayPacing
	now := time.Unix(1, 0)
	const delivered = 20_000_000
	const rtt = 200 * time.Millisecond
	for _, tc := range []struct {
		elapsed, queue time.Duration
		want           uint64
	}{
		{0, 0, 25_000_000},
		{rtt, 0, 30_000_000},
		{2 * rtt, 20 * time.Millisecond, 18_000_000},
		{3 * rtt, 0, 18_000_000},
		{5 * rtt, 0, 22_500_000},
	} {
		if got := p.update(now.Add(tc.elapsed), rtt, tc.queue, delivered); got != tc.want {
			t.Fatalf("at %v: pacing cap = %d B/s, want %d", tc.elapsed, got, tc.want)
		}
	}
}
