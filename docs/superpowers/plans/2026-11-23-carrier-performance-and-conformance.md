# BitTorrent carrier framing and traffic-analysis experiments

> **Execution:** `superpowers:subagent-driven-development`. Fresh implementer and reviewer for each independently testable slice; controller owns integration and measurements. Preserve the current uncommitted AWG and handshake changes as the comparison baseline. No deployment or live default changes.

**Goal:** Implement and measure candidates 2–4, 6–9 and 12–13 from the discussion. Evaluate whether each change weakens a specific observable correlation and what it costs in throughput, latency, and bytes. A non-detection result is limited to the tested analyzers, controls, and network conditions.

**Architecture:** Keep the existing `ext` and `req` versions available; negotiate new wire behavior before using it. Keep scheduling separate from record format. A peer that explicitly requires protection must fail closed, never silently fall back. Use authenticated standard TLS with the existing pinned identities for protected records, not an AEAD keyed by the public infohash or by an AWG header-protection key.

## Global constraints

- Preserve the `carrier.Transport` contract: ordered nonempty datagrams, maximum `BlockSize-2`, bounded buffering, close unblocks concurrent operations. Preserve AWG cookie protection and authentication, enrollment, discovery and group routing.
- Preserve upgraded-to-legacy interoperability for the existing format wherever the AWG profiles already match. Test both mixed-version directions. Keep the native WireGuard/AWG paths unchanged.
- Every experimental change must be measurable independently. No artificial delay for isolated sends, no arbitrary in-session parser switches, no dependency on TCP segment boundaries. Keep idle shaping opt-in and bounded.
- No guesses about external DPI based on nDPI's outer `BitTorrent` label. This is a controlled lab experiment, not a claim of indistinguishability.
- Activation is explicit. `BTAWG_REQ_EXPERIMENT=window|varied` enables a negotiated request/piece experiment; empty is baseline. `BTAWG_EXT_EXPERIMENT=protected|packed|padded` selects increasingly enabled protected ext capabilities; empty is baseline. The hub and client must enable the same wire experiment. Failure does not retry in baseline mode. Existing BEP 10 support signaling is a conformance correction, not an experimental wire format.
- Backlog and idle behavior are separate controls; native and direct peers must not inherit protected hub `ext` settings.


## 1. `ext` extension negotiation

- [ ] **RED:** In `btawg/internal/carrier/accept_test.go`, cover different receiver-assigned IDs, malformed and missing `m` entries, unknown messages, and old peers. Assert accepted carrier payload, not just successful handshake.
- [ ] **GREEN:** In `handshake.go` and `extended.go`, advertise BEP 10 support, strictly bound and validate negotiation, and use the remote receiver's ID for sends. Skip unknown extension data without delivering it as carrier data. Keep the old fixed-ID path only for the explicitly compatible existing handshake. Retain the current opaque name and random hub peer ID; evaluate varying extension IDs as part of negotiation, not as secret metadata.
- [ ] Verify simultaneous bidirectional traffic and malformed-input shutdown. Measure which fields remain observable after TCP reassembly.

## 2. `ext` protected records, packing and sizes

- [ ] **RED:** Test wrong credentials, truncated record, split delivery, multiple short datagrams in one protected record, maximum datagram size, and a single send with no batching wait.
- [ ] **GREEN:** In `internal/carrier` and `internal/iptunnel`, add a separately negotiated protected `ext` version and use the existing pinned identities on the hub/client path. Carry the TLS byte stream in `extended` messages; lengths, padding metadata, and datagram boundaries must be inside the protected stream. Preserve old and direct peer paths without forcing the new version onto them.
- [ ] Pack only ready datagrams; permit records to cross outer message boundaries with bounded reassembly. Add bounded variable-size padding as a separately measurable option. Compare baseline, protected records, packing, and padding in separate runs. Measure TLS handshake visibility separately from record privacy.

## 3. `req` credit window, block size and ordering

- [ ] **RED:** In `btawg/internal/carrier/carrier_test.go`, check idle connections do not emit the full wide window, sustained bidirectional consumption grows beyond the initial window, a response must match the outstanding request, and legacy peers still transfer data. Retain blocked-writer, duplicate-index, ordering and shutdown checks.
- [ ] **GREEN:** In `requestpiece.go`, separate initial credit from the negotiated maximum. Ramp issuance with actual sustained consumption; never exceed negotiated caps. Measure this change first with the current format.
- [ ] Add separately negotiated variable block lengths and variable valid request placement/order. Each `piece` must match its outstanding `request`, and outer reply order must not reorder inner AWG datagrams. Reject unsolicited data, mismatched lengths/offsets, and unbounded reassembly. Compare window-only and variable-block/order variants separately.
- [ ] Record initial request count, startup TCP bytes, credit-stall time, and goodput including 200 ms RTT.

## 4. Bounded scheduling and idle traffic

- [ ] **RED:** Check that an isolated packet does not wait on shaping, ready control/credit messages are not blocked behind a pacing wait, real data wins over idle traffic, and close terminates outstanding waits.
- [ ] **GREEN:** Add an optional backlog-only scheduler, independently measurable with framing held constant. Add opt-in idle traffic with explicit time and byte budgets; no idle payload is delivered to the application. Verify bounded queues and no lock held during a pacing wait.
- [ ] Compare backlog scheduling on/off and idle traffic on/off. Report p99 under concurrent bulk and extra bytes per idle minute; do not recommend enabling a candidate without a measured payoff.

## 5. Integration and acceptance

- [ ] Run `ext` and `req` on TCP and µTP through the hub/client paths. Use real SOCKS5 echo with payload verification. Compare baseline and each change on an isolated link at 40 and 200 ms RTT with identical workloads. Include cold start, quiet session, short requests during bulk, and bidirectional bulk. Record repetitions, settings, capture drops, and PCAPs.
- [ ] Measure startup bytes, time to first useful data, goodput, p95/p99, bytes per useful byte and idle bytes. Report distributions rather than a single best run.
- [ ] Include real BitTorrent controls. Freeze feature definitions before evaluating, split by whole sessions, and report false positives and false negatives for prefix-only and full-session views. Compare visible lengths, message transitions, and timing separately. nDPI remains an outer-label sanity check only.
- [ ] Require targeted carrier and end-to-end tests plus independent review of negotiation, crypto boundaries, and shutdown. Keep experimental settings off by default and report regressions and unsupported transitions explicitly.
