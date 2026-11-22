package client

import (
	"io"
	"net/netip"
	"testing"
	"time"

	"bittorrent-tunnel/btawg/internal/mesh"
)

func (b *balancer) record(path adaptivePath, upload bool, bytes int64, duration time.Duration, now time.Time) {
	b.recordScoped(path, upload, bytes, duration, now, b.routeEpoch)
}

func (b *balancer) pickTCP() *managedTunnel {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pickTCPLocked()
}

func TestManagedBalancerPortsAuthenticateAndFailClosed(t *testing.T) {
	auth := Credentials{Username: "operator", Password: "secret"}
	m, err := newManagedAt(t.Context(), managedCfg(t), nil, netip.MustParseAddr("127.0.0.1"), auth, false, "btawg", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, slot := range []int{10, 11, 12} {
		if slot >= len(m.inbound.slots) {
			t.Fatalf("SOCKS5 inbound %d is missing", 1080+slot)
		}
		addr := m.inbound.slots[slot].ln.Addr().String()
		c, err := proxyAuth(t, addr, auth)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write([]byte{5, 1, 0, 1, 1, 1, 1, 1, 0, 80}); err != nil {
			t.Fatal(err)
		}
		var reply [10]byte
		_, err = io.ReadFull(c, reply[:])
		c.Close()
		if err != nil || reply[1] == 0 {
			t.Fatalf("empty pool did not return a SOCKS5 failure: %v, %v", reply, err)
		}
	}
}

func TestBalancerSelectionSkipsFailedPathsAndRecovers(t *testing.T) {
	var first, second, dead managedTunnel
	var firstHealth, secondHealth probeHealth
	first.score.Store(int64(firstHealth.observe(40 * time.Millisecond)))
	second.score.Store(int64(secondHealth.observe(10 * time.Millisecond)))
	paths := []*managedTunnel{&first, &dead, &second}

	failover := balancer{paths: paths}
	adaptive := balancer{paths: paths, policy: "adaptive"}
	roundRobin := balancer{paths: paths, policy: "round-robin"}
	assertPick := func(b *balancer, want *managedTunnel) {
		t.Helper()
		if got := b.pick(); got != want {
			t.Fatalf("%q picked %p, want %p", b.policy, got, want)
		}
	}
	assertPick(&failover, &first)
	assertPick(&adaptive, &second)
	for _, want := range []*managedTunnel{&first, &second, &first} {
		assertPick(&roundRobin, want)
	}
	first.score.Store(int64(firstHealth.observe(0)))
	assertPick(&failover, &second)
	first.score.Store(int64(firstHealth.observe(time.Millisecond)))
	assertPick(&failover, &second)
	first.score.Store(int64(firstHealth.observe(time.Millisecond)))
	assertPick(&failover, &first)
	assertPick(&adaptive, &second)
}

func TestAdaptivePrefersConfiguredOrderForComparablePaths(t *testing.T) {
	var first, second managedTunnel
	first.score.Store(int64(22 * time.Millisecond))
	second.score.Store(int64(20 * time.Millisecond))
	b := balancer{policy: adaptivePolicy}
	b.replace([]*managedTunnel{&first, &second})
	if b.pick() != &first {
		t.Fatal("minor timing difference overrode saved priority")
	}
	b.replace([]*managedTunnel{&second, &first})
	if b.pick() != &second {
		t.Fatal("reordering did not affect the next choice")
	}
}

func TestAdaptiveRequiresTwoMeaningfullyBetterProbes(t *testing.T) {
	for _, tc := range []struct {
		name                string
		current, challenger time.Duration
		shouldSwitch        bool
	}{
		{"under five milliseconds", 20 * time.Millisecond, 16 * time.Millisecond, false},
		{"under fifteen percent", 100 * time.Millisecond, 86 * time.Millisecond, false},
		{"meets both thresholds", 40 * time.Millisecond, 34 * time.Millisecond, true},
		{"reaches absolute threshold", 20 * time.Millisecond, 15 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var current, challenger managedTunnel
			current.score.Store(int64(tc.current))
			b := balancer{policy: adaptivePolicy, paths: []*managedTunnel{&current, &challenger}}
			if b.pick() != &current {
				t.Fatal("no initial route")
			}
			challenger.score.Store(int64(tc.challenger))
			b.probed(&challenger, nil)
			b.probed(&current, nil)
			for range 3 {
				if b.pick() != &current {
					t.Fatal("one challenger probe switched traffic")
				}
			}
			b.probed(&challenger, nil)
			if (b.pick() == &challenger) != tc.shouldSwitch {
				t.Fatal("wrong route after two challenger probes")
			}
		})
	}
}

func TestAdaptiveDisconnectInvalidatesCarrierScore(t *testing.T) {
	var stream, native managedTunnel
	stream.carrierMode = true
	done := make(chan struct{})
	old := &carrierHealth{done: done}
	old.score.Store(int64(10 * time.Millisecond))
	stream.carrier.Store(old)
	stream.score.Store(int64(10 * time.Millisecond))
	native.score.Store(int64(30 * time.Millisecond))
	b := balancer{policy: adaptivePolicy, paths: []*managedTunnel{&stream, &native}}
	if b.pick() != &stream {
		t.Fatal("connected carrier should be preferred")
	}
	close(done)
	if b.pick() != &native {
		t.Fatal("closed carrier remained eligible before the next probe")
	}
	failover := balancer{paths: b.paths}
	if failover.pick() != &stream {
		t.Fatal("Adaptive carrier state changed Failover's probe-only behavior")
	}
	stream.carrier.Store(&carrierHealth{done: make(chan struct{})})
	if b.pick() != &native {
		t.Fatal("replacement carrier inherited the previous connection's score")
	}
}

func TestAdaptiveAvoidsPoorUploadWithoutDisturbingHealthyPinning(t *testing.T) {
	var first, second managedTunnel
	first.score.Store(int64(20 * time.Millisecond))
	second.score.Store(int64(20 * time.Millisecond))
	b := balancer{policy: adaptivePolicy, paths: []*managedTunnel{&first, &second}}
	if b.pick() != &first {
		t.Fatal("initial priority not respected")
	}
	now := time.Now()
	b.record(adaptivePath{tunnel: &first}, true, 1<<20, 4*time.Second, now)
	b.record(adaptivePath{tunnel: &second}, true, 8<<20, 4*time.Second, now)
	b.probed(&second, nil)
	if b.pick() != &first {
		t.Fatal("one observation switched the pinned choice")
	}
	b.probed(&second, nil)
	if b.pick() != &second {
		t.Fatal("poor upload did not move subsequent connections")
	}
}

func TestAdaptiveIgnoresStaleAndInsufficientThroughput(t *testing.T) {
	var first, second managedTunnel
	first.score.Store(int64(20 * time.Millisecond))
	second.score.Store(int64(20 * time.Millisecond))
	b := balancer{policy: adaptivePolicy, paths: []*managedTunnel{&first, &second}}
	b.pick()
	b.record(adaptivePath{tunnel: &first}, true, 1<<20, 4*time.Second, time.Now().Add(-10*time.Minute))
	b.record(adaptivePath{tunnel: &second}, true, 8<<20, 4*time.Second, time.Now())
	b.probed(&second, nil)
	b.probed(&second, nil)
	if b.pick() != &first {
		t.Fatal("stale sample penalized a path")
	}
	b.record(adaptivePath{tunnel: &first}, true, 1024, 4*time.Second, time.Now())
	b.probed(&second, nil)
	b.probed(&second, nil)
	if b.pick() != &first {
		t.Fatal("small transfer penalized a path")
	}
}

func TestAdaptiveRouteAndCarrierChangesDiscardMeasurements(t *testing.T) {
	var first, second managedTunnel
	first.carrierMode = true
	old := &carrierHealth{done: make(chan struct{})}
	old.score.Store(int64(20 * time.Millisecond))
	first.carrier.Store(old)
	second.score.Store(int64(20 * time.Millisecond))
	b := balancer{policy: adaptivePolicy, paths: []*managedTunnel{&first, &second}}
	routeA, routeB := map[int]string{1: "a"}, map[int]string{1: "b"}
	b.setRouteLocked(adaptiveRoute{id: selectedRouteID(routeA, 1)})
	b.pick()
	b.record(adaptivePath{tunnel: &first, carrier: old}, true, 1<<20, 4*time.Second, time.Now())
	b.record(adaptivePath{tunnel: &second}, true, 8<<20, 4*time.Second, time.Now())
	b.setRouteLocked(adaptiveRoute{id: selectedRouteID(routeB, 1)})
	b.probed(&second, nil)
	b.probed(&second, nil)
	if b.pick() != &first {
		t.Fatal("old route measurements affected new route")
	}
	b.record(adaptivePath{tunnel: &first, carrier: old}, true, 1<<20, 4*time.Second, time.Now())
	b.record(adaptivePath{tunnel: &second}, true, 8<<20, 4*time.Second, time.Now())
	replacement := &carrierHealth{done: make(chan struct{})}
	replacement.score.Store(int64(20 * time.Millisecond))
	first.carrier.Store(replacement)
	b.probed(&second, nil)
	b.probed(&second, nil)
	if b.pick() != &first {
		t.Fatal("replacement carrier inherited throughput")
	}
}

func TestAdaptiveExploresOnlyNewConnections(t *testing.T) {
	var first, second managedTunnel
	first.score.Store(int64(20 * time.Millisecond))
	second.score.Store(int64(20 * time.Millisecond))
	b := balancer{policy: adaptivePolicy, paths: []*managedTunnel{&first, &second}}
	if b.pick() != &first {
		t.Fatal("missing preferred path")
	}
	if b.pickTCP() != &second {
		t.Fatal("unobserved alternative was never explored")
	}
	if b.pickTCP() != &first || b.pick() != &first {
		t.Fatal("exploration changed the selected path")
	}
}

func TestAdaptiveUsesDownloadMeasurementsIndependently(t *testing.T) {
	var first, second managedTunnel
	first.score.Store(int64(20 * time.Millisecond))
	second.score.Store(int64(20 * time.Millisecond))
	b := balancer{policy: adaptivePolicy, paths: []*managedTunnel{&first, &second}}
	b.pick()
	now := time.Now()
	b.record(adaptivePath{tunnel: &first}, false, 1<<20, 4*time.Second, now)
	b.record(adaptivePath{tunnel: &second}, false, 8<<20, 4*time.Second, now)
	b.probed(&second, nil)
	b.probed(&second, nil)
	if b.pick() != &second {
		t.Fatal("poor download did not change later selection")
	}
}

func TestAdaptiveLatencyChallengerSkipsThroughputVeto(t *testing.T) {
	var incumbent, fast, alternate managedTunnel
	incumbent.score.Store(int64(40 * time.Millisecond))
	b := balancer{policy: adaptivePolicy, paths: []*managedTunnel{&incumbent, &fast, &alternate}}
	b.pick()
	fast.score.Store(int64(10 * time.Millisecond))
	alternate.score.Store(int64(20 * time.Millisecond))
	now := time.Now()
	b.record(adaptivePath{tunnel: &incumbent}, true, 8<<20, 4*time.Second, now)
	b.record(adaptivePath{tunnel: &fast}, true, 1<<20, 4*time.Second, now)
	b.probed(&alternate, nil)
	b.probed(&alternate, nil)
	if b.pick() != &alternate {
		t.Fatal("throughput veto blocked a healthy latency challenger")
	}
}

func TestAdaptiveEquivalentRoutePublicationPreservesMeasurements(t *testing.T) {
	var first, second managedTunnel
	first.score.Store(int64(20 * time.Millisecond))
	second.score.Store(int64(20 * time.Millisecond))
	m := &Managed{}
	b := &m.balance[2]
	b.policy = adaptivePolicy
	b.replace([]*managedTunnel{&first, &second})
	m.applyRoutes(map[int]string{ManagedPortBase: "a"})
	b.pick()
	b.record(adaptivePath{tunnel: &first}, true, 1<<20, 4*time.Second, time.Now())
	b.record(adaptivePath{tunnel: &second}, true, 8<<20, 4*time.Second, time.Now())
	m.applyRoutes(map[int]string{ManagedPortBase: "b"})
	b.probed(&second, nil)
	b.probed(&second, nil)
	if b.pick() != &second {
		t.Fatal("unrelated route update discarded adaptive measurements")
	}
}

func TestAdaptiveRouteGrantRenewalAndHopChanges(t *testing.T) {
	var key [32]byte
	now := time.Now()
	origin := mesh.Node{ID: "origin", Key: "key", IP: "10.0.0.1"}
	a := []mesh.Node{{ID: "hop", Key: "one", IP: "10.0.0.2"}, {ID: "exit", Key: "two", IP: "10.0.0.3"}}
	grant := func(hops []mesh.Node, at time.Time) string {
		value, err := mesh.SealGrant(key, "route", origin, hops, at, 1)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	first := adaptiveRouteIdentity("route", &catalogue{routes: []mesh.Route{{ID: "route", Grant: grant(a, now)}}})
	renewed := adaptiveRouteIdentity("route", &catalogue{routes: []mesh.Route{{ID: "route", Grant: grant(a, now.Add(time.Second))}}})
	if first != renewed {
		t.Fatal("lease renewal changed route identity")
	}
	a[1].Key = "replacement"
	changed := adaptiveRouteIdentity("route", &catalogue{routes: []mesh.Route{{ID: "route", Grant: grant(a, now)}}})
	if first == changed {
		t.Fatal("changed hop did not change route identity")
	}
}

func TestAdaptiveRouteEpochRejectsOldCompletionAfterReturning(t *testing.T) {
	var path managedTunnel
	path.score.Store(int64(20 * time.Millisecond))
	b := balancer{policy: adaptivePolicy, paths: []*managedTunnel{&path}}
	b.setRouteLocked(adaptiveRoute{id: "a"})
	epoch := b.routeEpoch
	b.setRouteLocked(adaptiveRoute{id: "b"})
	b.setRouteLocked(adaptiveRoute{id: "a"})
	b.recordScoped(adaptivePath{tunnel: &path}, true, 1<<20, 4*time.Second, time.Now(), epoch)
	if len(b.samples) != 0 {
		t.Fatal("old route completion was charged to a new route generation")
	}
}
