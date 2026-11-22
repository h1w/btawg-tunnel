// Package portmap manages optional IPv4 router mappings for an existing UDP listener.
package portmap

import (
	"context"
	"errors"
	"net/netip"
	"time"
)

const (
	requestedSeconds = 600
	probeTimeout     = 2 * time.Second
	cleanupTimeout   = 2 * time.Second
	probeInterval    = 30 * time.Second
)

var errLeaseLost = errors.New("router mapping no longer confirmed")

// router identifies the default interface and its route-selected IPv4 source.
// Control traffic uses separate sockets, never the application UDP listener.
type router struct {
	gateway            netip.Addr
	source             netip.Addr
	ifindex            int
	luid               uint64
	natPort            uint16
	ssdp               netip.AddrPort
	allowPermanentUPnP bool
}

type mapping interface {
	refresh(context.Context, lease) (lease, error)
	remove(context.Context, lease) error
}

type lease struct {
	endpoint netip.AddrPort
	started  time.Time
	expires  time.Time
	// Permanent entries have no expiration, but still need revalidation.
	permanent bool
	// An entry eligible for best-effort, backend-verified removal.
	owned bool
	// NAT-PMP has no nonce; record the router epoch when it acknowledged this mapping.
	epoch uint32
}

func acknowledgedLease(endpoint netip.AddrPort, seconds uint32, started time.Time, owned bool) lease {
	return lease{endpoint: endpoint, started: started, expires: started.Add(time.Duration(seconds) * time.Second), owned: owned}
}

func (l lease) validAt(now time.Time) bool {
	return l.permanent || now.Before(l.expires)
}

// withoutCandidate keeps the port for router cleanup, but makes the
// endpoint ineligible for publication.
func (l lease) withoutCandidate() lease {
	if l.endpoint.IsValid() {
		l.endpoint = netip.AddrPortFrom(netip.IPv4Unspecified(), l.endpoint.Port())
	}
	return l
}

// Run maintains mappings for the caller's existing IPv4 UDP listen port until
// ctx ends. It snapshots the Linux default route when started. report runs
// synchronously; it must return promptly. It gets a fresh slice on changes
// (nil initially and when no mapping is usable). A router claim is a candidate,
// not a guarantee of end-to-end reachability. Router failures are nonfatal and
// retried. Run returns an error only for invalid input or route discovery
// failure, and returns nil on cancellation after bounded, best-effort cleanup.
// allowPermanentUPnP permits a permanent entry only after IGD rejects a
// finite lease. A permanent entry may survive a crash or failed cleanup.
func Run(ctx context.Context, internalPort uint16, allowPermanentUPnP bool, report func([]netip.AddrPort)) error {
	if internalPort == 0 || ctx.Err() != nil {
		if report != nil {
			report(nil)
		}
		if internalPort == 0 {
			return errors.New("port mapping requires a nonzero UDP listener port")
		}
		return nil
	}
	r, err := defaultRouter(ctx)
	if err != nil {
		if report != nil {
			report(nil)
		}
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	r.allowPermanentUPnP = allowPermanentUPnP
	return maintain(ctx, r, internalPort, report)
}

func maintain(ctx context.Context, r router, port uint16, report func([]netip.AddrPort)) error {
	if report != nil {
		report(nil)
	}
	var nextBackend int
	var active mapping
	var current lease
	var published netip.AddrPort
	publish := func(endpoint netip.AddrPort) {
		if endpoint == published {
			return
		}
		published = endpoint
		if report != nil {
			if endpoint.IsValid() {
				report([]netip.AddrPort{endpoint})
			} else {
				report(nil)
			}
		}
	}
	defer func() {
		publish(netip.AddrPort{})
		if active != nil && current.owned && current.validAt(time.Now()) {
			_ = removeLease(ctx, active, current)
		}
	}()

	for ctx.Err() == nil {
		if active == nil {
			active, current, nextBackend = r.acquire(ctx, port, nextBackend)
			if active == nil {
				wait(ctx, probeInterval)
				continue
			}
		}

		if !current.validAt(time.Now()) {
			publish(netip.AddrPort{})
			active = nil
			wait(ctx, probeInterval)
			continue
		}
		if !usableEndpoint(current.endpoint) {
			publish(netip.AddrPort{})
			if current.owned {
				err := removeLease(ctx, active, current)
				if err == nil || current.permanent && errors.Is(err, errLeaseLost) {
					active = nil
					continue
				}
			} else if current.endpoint.IsValid() {
				active = nil
				continue
			}
			if current.permanent {
				wait(ctx, probeInterval)
				continue
			}
			// An unacknowledged write may still have created a lease. Don't
			// start a second backend on the same port while it may exist.
			wait(ctx, time.Until(current.expires))
			continue
		}
		if ctx.Err() == nil {
			publish(current.endpoint)
		}
		renewal, deadline := current.started.Add(current.expires.Sub(current.started)/2), current.expires
		if current.permanent {
			renewal = current.started.Add(probeInterval)
		}
		if !wait(ctx, time.Until(renewal)) {
			break
		}
		if current.permanent {
			deadline = time.Now().Add(probeTimeout)
		}
		renewCtx, cancel := context.WithDeadline(ctx, deadline)
		newLease, err := active.refresh(renewCtx, current)
		cancel()
		if err == nil {
			current = newLease
			continue
		}
		if errors.Is(err, errLeaseLost) {
			publish(netip.AddrPort{})
			current = newLease.withoutCandidate()
			if !current.permanent && current.expires.IsZero() {
				active = nil
				wait(ctx, probeInterval)
			}
			continue
		}
		if current.permanent {
			// An unreachable router can no longer confirm the candidate.
			// Keep the exact port for ownership-checked cleanup.
			publish(netip.AddrPort{})
			current = current.withoutCandidate()
			continue
		}
		// The last acknowledgement remains usable until its own expiry. If
		// the renewal reached the router but its reply was lost, hold off on
		// switching backends past the likely new lease's expiry as well.
		wait(ctx, time.Until(current.expires))
		if ctx.Err() == nil && newLease.expires.After(current.expires) {
			current = newLease
		}
	}
	return nil
}

func (r router) acquire(ctx context.Context, port uint16, first int) (mapping, lease, int) {
	// Activate one backend at a time: PCP and NAT-PMP may share a table.
	// After an expired or lost lease, try the other protocols before retrying
	// the failed one so a broken MAP implementation cannot starve fallback.
	probes := [...]func(context.Context, uint16) (mapping, error){r.probePCP, r.probePMP, r.probeIGD}
	for offset := range len(probes) {
		index := (first + offset) % len(probes)
		if ctx.Err() != nil {
			break
		}
		backend, err := probes[index](ctx, port)
		if err != nil {
			continue
		}
		current, err := backend.refresh(ctx, lease{})
		next := (index + 1) % len(probes)
		if err == nil && usableEndpoint(current.endpoint) {
			return backend, current, next
		}
		if !current.permanent && current.expires.IsZero() {
			continue
		}
		if !current.owned && !current.endpoint.IsValid() {
			return backend, current.withoutCandidate(), next
		}
		if current.owned {
			err := removeLease(ctx, backend, current)
			if err != nil && !(current.permanent && errors.Is(err, errLeaseLost)) {
				return backend, current.withoutCandidate(), next
			}
			continue
		}
	}
	return nil, lease{}, first
}

func removeLease(ctx context.Context, backend mapping, current lease) error {
	// Cancellation should stop probing/renewal, not prevent removal. Never
	// start a deletion with a lease that may already have been reassigned.
	deadline := time.Now().Add(cleanupTimeout)
	if !current.permanent {
		deadline = minTime(current.expires, deadline)
	}
	cleanup, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancel()
	if err := cleanup.Err(); err != nil {
		return err
	}
	return backend.remove(cleanup, current)
}

func wait(ctx context.Context, duration time.Duration) bool {
	if duration <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

// Non-public IANA ranges not rejected by IsGlobalUnicast and IsPrivate.
var nonPublic = [...]netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

func usableEndpoint(endpoint netip.AddrPort) bool {
	addr := endpoint.Addr()
	if endpoint.Port() == 0 || !addr.Is4() || !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}
	for _, prefix := range nonPublic {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}
