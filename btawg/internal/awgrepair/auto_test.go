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
	if base.sample(now, 2, true) != 2 || base.sample(now.Add(time.Second), 200, true) != 2 {
		t.Fatal("a queued report turned an established LAN into a WAN")
	}
	for i := range 25 {
		base.sample(now.Add(time.Duration(i+2)*time.Second), 200, true)
	}
	if base.sample(now.Add(27*time.Second), 200, true) != 200 {
		t.Fatal("a persistent longer route was never reclassified")
	}
}

func TestEstablishedLANDetectsSustainedRTTStep(t *testing.T) {
	now := time.Unix(100, 0)
	var f rttFloor
	f.sample(now, 2, true)
	var floor uint16
	for i := 1; i <= 12; i++ {
		floor = f.sample(now.Add(time.Duration(i)*250*time.Millisecond), 200, true)
	}
	if floor < 80 {
		t.Fatalf("still skipping loss measurement after three seconds at 200 ms: floor %d", floor)
	}
	if floor = f.sample(now.Add(4*time.Second), 3, true); floor >= 80 {
		t.Fatalf("did not return to LAN on a low RTT sample: floor %d", floor)
	}
}

func TestIntermittentLANQueueDoesNotPromotePath(t *testing.T) {
	now := time.Unix(100, 0)
	var f rttFloor
	f.sample(now, 2, true)
	for i := 1; i <= 100; i++ {
		ms := uint16(180)
		if i%5 == 0 {
			ms = 4
		}
		if floor := f.sample(now.Add(time.Duration(i)*250*time.Millisecond), ms, true); floor >= 80 {
			t.Fatalf("intermittent queue promoted at probe %d: floor %d", i, floor)
		}
	}
}

func TestEarlyRTTStepNeedsContinuousStableReports(t *testing.T) {
	for _, tc := range []struct {
		name  string
		times []time.Duration
		rtts  []uint16
	}{
		{"interrupted probes", []time.Duration{250, 500, 2500, 2750}, []uint16{200, 201, 200, 201}},
		{"growing and draining queue", []time.Duration{250, 500, 750, 1000, 1250, 1500, 1750, 2000, 2250, 2500, 2750}, []uint16{100, 110, 120, 130, 140, 150, 140, 130, 120, 110, 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f rttFloor
			now := time.Unix(100, 0)
			f.sample(now, 2, true)
			for i, ms := range tc.rtts {
				if floor := f.sample(now.Add(tc.times[i]*time.Millisecond), ms, true); floor >= 80 {
					t.Fatalf("promoted without a stable train at sample %d: floor %d", i, floor)
				}
			}
		})
	}
}
