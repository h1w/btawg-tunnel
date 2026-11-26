package awgrepair

import (
	"testing"
	"time"
)

func TestCleanBypassRechecksWhenBulkFlowBecomesSlow(t *testing.T) {
	var g cleanRate
	now := time.Unix(10, 0)
	for i := range 1000 {
		g.bypass(now.Add(time.Duration(i)*10*time.Microsecond), 1280, false)
	}
	if !g.bypass(now.Add(100*time.Millisecond), 1280, false) {
		t.Fatal("healthy bulk path was still paying for per-packet recovery probes")
	}
	if g.bypass(now.Add(101*time.Millisecond), 1280, true) {
		t.Fatal("lost the cache on a path currently recovering packets")
	}
	if g.bypass(now.Add(3*time.Second), 1280, false) {
		t.Fatal("a slower new flight never returned to loss sampling")
	}
}
