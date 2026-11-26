package awgrepair

import (
	"testing"
	"time"
)

func TestAutoNeedsRepeatedRawLossAndReleasesOnClean(t *testing.T) {
	var a, reverse autoPolicy
	now := time.Unix(1000, 0)
	ev := autoEvidence{total: 16, missing: 1, rate: 1_000_000, headroom: true}
	if a.update(ev, now, false).code {
		t.Fatal("one bad sample switched a clean link")
	}
	now = now.Add(150 * time.Millisecond)
	if !a.update(ev, now, false).code || reverse.mode.code {
		t.Fatal("did not isolate the impaired direction")
	}
	ev.missing = 0
	for i := range 120 {
		now = now.Add(40 * time.Millisecond)
		if i == 20 {
			ev.missing = 1
		}
		a.update(ev, now, false)
		ev.missing = 0
	}
	if a.mode.code || a.mode.clamp {
		t.Fatal("continuing to code or clamp after sustained clean samples")
	}
}
func TestAutoDoesNotFightNarrowQueue(t *testing.T) {
	var a autoPolicy
	now := time.Now()
	for range 100 {
		now = now.Add(50 * time.Millisecond)
		got := a.update(autoEvidence{total: 16, missing: 1, rate: 900000, queue: 25 * time.Millisecond}, now, true)
		if got.code {
			t.Fatal("adding parity on slow congested link")
		}
	}
	if !a.mode.clamp {
		t.Fatal("queue control was tied to permission to emit parity")
	}
}

func TestPropagationEstimateDoesNotPromoteOneQueuedLANProbe(t *testing.T) {
	var base rttFloor
	now := time.Unix(100, 0)
	if base.sample(now, 2) != 2 || base.sample(now.Add(time.Second), 200) != 2 {
		t.Fatal("a queued report turned an established LAN into a WAN")
	}
	for i := range 25 {
		base.sample(now.Add(time.Duration(i+2)*time.Second), 200)
	}
	if base.sample(now.Add(27*time.Second), 200) != 200 {
		t.Fatal("a persistent longer route was never reclassified")
	}
}
