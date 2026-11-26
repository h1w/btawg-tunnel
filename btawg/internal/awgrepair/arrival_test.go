package awgrepair

import (
	"testing"
	"time"
)

func TestRepairHeadroomRejectsNarrowAndSingleReadBursts(t *testing.T) {
	now := time.Unix(100, 0)
	var slow, stalled, fast arrivalTrain
	for i := range 16 {
		slow.record(now.Add(time.Duration(i)*time.Millisecond), 1200)
		stalled.record(now, 1200)
		fast.record(now.Add(time.Duration(i)*100*time.Microsecond), 1200)
	}
	if slow.available(now.Add(15*time.Millisecond)) || stalled.available(now) {
		t.Fatal("serialization on a narrow link or one coalesced read qualified as headroom")
	}
	if !fast.available(now.Add(2*time.Millisecond)) || fast.available(now.Add(4*time.Second)) {
		t.Fatal("fresh fast packet train should enable repair, stale evidence should not")
	}
}
