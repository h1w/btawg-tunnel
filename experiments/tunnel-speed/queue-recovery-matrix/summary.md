# Hub/client queue and µTP recovery audit

## Production changes

- TLS IP senders now forward valid segments from a recoverable `tun.ErrTooManySegments` read and keep reading. Before, the foreground TLS path abandoned the TUN reader, including the valid prefix. The regression observes all valid packets from that batch and the next before a real read error terminates forwarding.
- µTP reverse DATA carries only a cumulative ACK and no SACK extension. It no longer cancels the receiver's deferred SACK when packets behind a gap are buffered. Application reads arm the deferred ACK timer when reopening a nonzero window, even if the sender has exhausted credit and no new packet arrives. Queued DATA and retries stamp the live receive window at send time so retransmission cannot re-advertise obsolete credit. No wire-format or congestion-control default changed.
- The managed client exposes `tun_queue_drops` on its local control socket. It counts only rejected shared-TUN packets on full path queues, separately from policy/closed-path filtering and Windows SOCKS backpressure. The foreground Linux client hands its TUN directly to the engine and does not have this managed queue. Do not read a missing baseline counter as zero.
- The hub reuses a bounded array of writable TUN batch headers. Linux GRO can extend or reorder those headers, so the engine's input slice is not reused directly. A warmed four-packet upload goes from three heap allocations to zero in the forwarding regression; the payload and policy checks are unchanged.
- The hub's primary route cannot regress to a timestamp from an older authenticated batch handled by a concurrent engine. Reverse-flow pins still take precedence. The failing-then-passing regression verifies unsolicited traffic uses the newer path.
- Duplicate out-of-order µTP DATA now re-arms a SACK if its first acknowledgement was lost. The regression drops the first SACK, replays that wire DATA and requires a second SACK with the same gap information.
- The AWG carrier client now installs the hub peer after attaching the stream. With persistent keepalive enabled, the old client attempted its first handshake before the carrier existed, spending the AWG retry interval on an undeliverable packet. The first attached stream now gets the initial handshake immediately.
- µTP loss and delay-based backoff keep the congestion window at least one packet wide. The old controller could leave an idle sender unable to send a full segment and therefore unable to get any ACK to grow its window. The peer window and configured send-buffer limits still apply.
- The hub pins IPv4 fragments to the destination selected for the first fragment, not the engine that uploaded them. A regression reproduces a lost fragmented UDP reply between WG and AWG peers; both fragments now reach the recipient's reverse-flow engine even when its primary route differs.
- The managed client only remembers a fragment route after the first fragment is admitted. If the path rejects a subsequent fragment, the client discards the association so stranded tails cannot fill a freed slot ahead of complete datagrams. The first- and middle-fragment overflow regressions both failed before the fix. Queue capacity and framing are unchanged. These are targeted IPv4 delivery fixes, not measured bulk TCP gains.

`go test -race ./purego ./interop -count=1` passed with the new µTP feedback regressions. `go test -race ./internal/server ./internal/client ./internal/iptunnel ./internal/bind ./internal/carrier -count=1` passed on the forwarding changes; the route-timestamp change passed `-race` in server/client/iptunnel. Linux client/hub and Windows amd64 client built. These are correctness and build checks, not proof of a throughput gain.

For the IPv4 fragment fixes, the hub and client suites passed under `-race` in isolated Linux network namespaces, including the existing real-TUN UDP fragmentation case. Linux hub/client and Windows amd64 client builds passed. The archived throughput matrix predates these two fixes.

In `pooled-matrix`, only the hub write-buffer reuse changed. All 16 native upload trials completed (eight CUBIC streams, 0% induced loss, two measurements per arm). At +200 ms, WG median rose **141 → 199 Mbit/s** and AWG **142 → 216 Mbit/s**; loaded p95 was **221 → 212 ms** and **211 → 210 ms**. At 0 ms the result was mixed: WG **592 → 485**, AWG **609 → 622 Mbit/s**. WG fell in one local pair and was effectively unchanged in the other. The allocation reduction is deterministic; this two-pair result is not a general speedup or a clean-LAN non-regression claim.

## Baseline attribution

Real Linux TUN, private networks, MTU 1280, selective offloads at **both** loss levels, eight TCP CUBIC streams per direction, five measured seconds after two seconds warm-up. The eight-flow foreground matrix covers native WG/AWG and the four AWG carrier modes; one sample per cell. Both directions are impaired independently with 1% random loss when enabled. +200 ms is 100 ms `netem` delay each way. No rate cap for this matrix.

- **42/48 completed**. The remaining six were iperf failures, not measured zero-speed transfers. All 24 clean cells completed. At 0% the hub discarded packets on all twelve downlink cells (**130–6,294** per trial), with no excess transit-qdisc or UDP receive-buffer drops. At 1% +200 ms the native TUN CUBIC rates were **7.1–13.1 Mbit/s**, with zero hub engine drops; removing hub queue loss alone cannot repair that profile. The native [SOCKS TCP relay](../wan-auto-pacing/summary.md) redrives a different workload and is not used in these TUN results.
- Managed-client controls also dropped packets before encryption, including **2,958** for one clean WG upload and **4,758** at +200 ms. Foreground and managed measurements have different client send paths and are not interchangeable.
- In the isolated SACK-only A/B (fresh foreground Extended/µTP, eight TCP download sockets, 1% +200 ms, TCP_INFO seconds 4–16), old build **0.552/0.595 Mbit/s**, fixed **0.549/0.580**. No local engine or extra transit drops. This is a protocol recovery fix, not a throughput fix for window collapse.

## Managed TUN A/B, all ten modes

`integrated-matrix` compares `base` to `repaired` (SACK preservation, window-update timer, TLS TUN reader, and client drop instrumentation; before the pooled hub write and fresh-window stamping). The fixed-interval workload starts eight CUBIC sockets and measures receiver `TCP_INFO` bytes over seconds 2–7 with no iperf control or result exchange over the tested path. Every admitted sample must deliver bytes on all eight sockets. Selected veth offloads remain identical at 0% and 1%; both directions run separately. The comparator's final WAN profile reached the job deadline and was restarted alone; the completed local profile was retained. Startup failures remain failures, not 0 Mbit/s measurements.

The complete per-mode data are in [comparison.tsv](comparison.tsv). Client queue-drops telemetry does not exist in the base binary. A failed cell with ICMP replies does not establish why an inner TCP socket failed to connect; the underlying µTP session can still be active.

| Profile | Base completed / 20 | Repaired completed / 20 |
|---|---:|---:|
| 0%, local | 20 | 20 |
| 1%, local | 20 | 19 |
| 0%, +200 ms | 20 | 19 |
| 1%, +200 ms | 18 | 19 |

Largest one-off local 1% gains: AWG Request/µTP upload **2.77 → 28.24 Mbit/s** and TLS Extended/µTP download **2.43 → 87.26 Mbit/s**. Other cells worsened, e.g. AWG Extended/TCP upload **376.72 → 200.69 Mbit/s**. At 1% +200 ms, successful µTP cells still delivered **0.36–0.87 Mbit/s** on the repaired build. Compared native WG/AWG cells delivered **7.38–10.38 Mbit/s**, with zero measured hub and client queue drops. These are individual comparisons, not a robust device- or mode-wide speedup.

`final-matrix` isolates live receive-window stamping after the pooled hub change. Both arms completed all eight 0% cells. At 1%, final completed **8/8** versus pooled **6/8**; the two missing cells share one Request/µTP tunnel that did not come up. Local 1% goodput improved in all four measured directions in this single pair, but on Extended/µTP at 1% +200 ms it fell **0.576/0.953 → 0.517/0.699 Mbit/s**. Final Request/µTP delivered **0.438/0.513 Mbit/s** in that WAN profile; no paired control speed exists for it. All four final µTP WAN cells had zero hub/client queue and zero excess transit drops. Better feedback correctness did not remove the flow-control and congestion-control limits of the ordered carrier.

## Queue experiments not adopted as defaults

The hub prototype deferred a first full enqueue and subsequent same-engine packets to the end of the current TUN batch, yielded once, then retried without blocking. Queue capacity stayed the same. One control/candidate per delay, managed client identical. Clean local WG/Extended TCP download lost throughput (679 → 617 and 349 → 285 Mbit/s). +200 ms Extended/µTP improved 109 → 123 Mbit/s with 1,631 → 1,158 hub drops, while Extended/TCP held 176 Mbit/s with 5,721 → 6,733 drops. Not a shared improvement.

Giving the managed client one scheduler yield and retry per TUN read cut upload drops, but moved congestion downstream. Two repeats per arm in control/retry/retry/control order, same hub:

| Transport / added RTT | Goodput, Mbit/s | Client queue drops | Loaded p95, ms |
|---|---:|---:|---:|
| WG / 0 ms | 528 → 536 | 2,281 → 178 | 62 → 242 |
| WG / 200 ms | 189 → 289 | 4,447 → 1,550 | 209 → 226 |
| Extended/µTP / 0 ms | 224 → 236 | 294 → 0 | 214 → 270 |
| Extended/µTP / 200 ms | 144 → 142 | 1,042 → 2 | 287 → 457 |

A reduced drop counter at one stage is not sufficient to adopt either queue policy.

Reducing the AWG per-peer encrypted send queue from 1,024 to 64 containers (both endpoints, lab-only dependency copy) also moved pressure upstream. All 32 clean trials completed. Local WG download median was **646 → 654 Mbit/s**, p95 **93 → 30 ms**, with fewer drops on both endpoints. At +200 ms WG download speed was **279 → 276 Mbit/s** but hub drops rose **2,379 → 5,915**; Extended/µTP WAN download regressed **96 → 82 Mbit/s** and hub drops rose **1,176 → 1,929**. Do not ship a global reduction in encrypted queue depth.

Marking ECT IPv4 packets with CE at 128/256 occupied slots (both shared TUN directions, both TCP endpoints negotiate ECN in all lab arms) reduced clean local queue loss with little speed change. WG download p95 **115 → 14 ms** and combined endpoint drops **7,760 → 28**; Extended/µTP upload p95 **224 → 13 ms** and client drops **454 → 24**. But at +200 ms upload goodput fell **176 → 98 Mbit/s** (WG) and **140 → 83 Mbit/s** (Extended/µTP). The checksum/DSCP regression passed, and all 32 TCP trials completed. Do not deploy this fixed occupancy threshold; lowering a counter by throttling a WAN sender is not the requested improvement.

A second lab arm signaled only after depth stayed at or above 192/256 for 4 ms, one CE mark per episode, rearmed below 64. It did not solve local/WAN tradeoffs: local WG download was **682 → 688 Mbit/s** but combined drops **6,712 → 14,619**; +200 ms WG download **300 → 279 Mbit/s**. Extended/µTP +200 ms download stayed near **102 → 99 Mbit/s** while hub drops **1,353 → 2,186**. All 32 trials completed and the threshold/hysteresis test passed. Not adopted.

Reserving the last 16 FIFO slots for pure IPv4 TCP control packets and ICMP (no dequeue reordering) also failed the shared gate. Local WG upload client drops **3,488 → 1,760** and p95 **75 → 40 ms** at similar goodput. But +200 ms WG download fell **295 → 230 Mbit/s**, combined hub/client drops **5,041 → 6,105**. Extended/µTP download had more hub drops in both profiles. All 32 trials completed. Do not ship a fixed ACK admission reserve.

## Loss-aware rollback of a µTP pacing probe

Same lab-only shared DATA scheduler on both arms as in the [shared-budget experiment](../extended-utp-shared-budget/summary.md). R reverts an outstanding 25% pacing probe on fast-loss evidence, then holds all upward rate changes for at least one smoothed RTT. R does not change congestion-window cuts, delay response, ACK bypass, or the non-probe loss policy. The regression delivers an actual SACK with sufficient later packets and verifies that the sender retries immediately at the prior rate and does not raise it during cooldown. S failed and R passed 20 repetitions under `-race`.

| Profile | Goodput S → R, Mbit/s | Extra qdisc drops S → R | In-window RTO cuts S → R |
|---|---:|---:|---:|
| 10 Mbit/s, pair 101 | 4.402 → 5.044 | 2,332 → 1,282 | 9 → 4 |
| 10 Mbit/s, pair 211 | 5.396 → 5.228 | 2,175 → 900 | 5 → 4 |
| Unshaped, 1% +200 ms, single pair | 14.493 → 11.821 | 0 → 0 | 3 → 2 |

The narrow profile has a 160-packet downlink queue including propagation, and no random loss. All four attribution traces exactly match qdisc drops and all four reach 160 slots. The first drop is always new DATA, before fast recovery can react. Rollback reduces excess drops **45–59%**, but does not avoid overflow and reduces the measured unshaped lossy goodput **18%** in this single pair. A SACK gap occurs under both random loss and queue overflow; this signal alone does not distinguish them. Keep this controller out of the product.

## Delay-guarded µTP window regrowth

A separate unpaced overlay only increases additive regrowth after a loss cut when smoothed RTT is at least 50 ms and the one-way delay estimate is below 2.5 ms. Growth remains bounded to the larger of the existing 3,000 B/RTT and one eighth of the window, capped at 64 KiB/RTT. Loss response itself is unchanged. Measured at +200 ms: 1% unshaped downlink **0.595/0.634 → 0.620/0.625 Mbit/s**; on the 10 Mbit/s queue **2.857 → 2.977 Mbit/s** but extra drops **199 → 217**. All six receiver intervals were valid. No convincing WAN gain; not adopted. [Regrowth data](regrowth.tsv).

## Actual SOCKS CONNECT, separate from the transparent TUN matrix

Native WG/AWG, foreground real TUN, base versus pooled hub/client; one SHA-256-checked 32 MiB download and 16 MiB upload per cell. Independent 0%/1% random losses at 0/+200 ms, no rate cap. An origin in the transit namespace is outside the encrypted impaired leg. All **32/32** transfers completed, with zero hub engine or extra transit drops and the overlay-only relay inaccessible from the underlay. These short, startup-inclusive rates are not sustainable bulk-throughput estimates.

At 1% +200 ms, WG download/upload were **15.03/19.87 → 19.34/20.38 Mbit/s**, AWG **19.17/19.91 → 19.74/20.30 Mbit/s**. Loaded p95 **201–202 ms** in both arms. One result per cell; improvements cannot be assigned to the pooled write path from this single run. See [SOCKS counters and integrity results](socks.tsv).

## Bidirectional parity, transparent native TUN only

One-to-one UDP shims in both arms, identical final hub and foreground client binaries, eight inner CUBIC streams, independent configured loss per direction, MTU 1280. The coded shim uses two repair datagrams per 16 original datagrams in each direction and four interleaved groups. No product negotiation, adaptive bypass or shared rate budget. Receiver `TCP_INFO` bytes over seconds 3–10; one measurement per zero-loss cell and two per lossy cell. This result is not a SOCKS benchmark.

| Mode / profile | Download without → with parity, Mbit/s | Upload without → with parity, Mbit/s |
|---|---:|---:|
| WG, local 1% | 277 → 389 | 315 → 410 |
| AWG, local 1% | 276 → 396 | 302 → 413 |
| WG, 1% +200 ms | 6.44 → 128.85 | 7.14 → 204.73 |
| AWG, 1% +200 ms (fresh session per cell) | 7.86 → 159.75 | 6.42 → 163.78 |
| WG, 0% +200 ms | 302 → 262 | 317 → 252 |
| AWG, 0% +200 ms (fresh session per cell) | 283 → 283 | 332 → 288 |

The initial shared-namespace AWG WAN series reused the shim port while old delayed packets could still arrive. It showed 371/828 invalid input events on the coded client side and almost no useful recovery. Those rows are retained but excluded from the table. Starting a new hub/client and shim pair for each AWG WAN cell eliminated invalid input and restored thousands of packets. The fixed observations do not independently isolate the exact cause of the original invalid packets. All isolated AWG cells and the isolated codec checks completed.

At +200 ms loss, parity cost about **14%** more UDP payload and raised loaded p95: WG **203/203 → 223/262 ms**, AWG **203/206 → 258/288 ms**. Hub queue drops reappeared as recovered TCP increased its offered load (median WG **0 → 2,216**, AWG **0 → 1,422** on download). Under 0% it can slow the clean path and add UDP receive-buffer drops while recovering nothing. **Lab only.** The largest remaining WAN throughput gain requires recovery that is enabled only when delayed/lost data warrants its cost and that accounts for repair bytes in the actual output budget. [Original shim counts](transparent-parity.tsv) · [Isolated AWG shim counts](awg-isolated-parity.tsv).

## Receiver-triggered parity discriminator

One fresh AWG tunnel per arm, same +200 ms profile. The lab reports missing originals after 16 newer encoder groups; parity decisions happen at group boundaries, with a 1.5 s feedback expiry. It starts with parity off, sends none in the clean control, and enables it after 0.3–3% observed gaps in a report window. This experiment feeds no delay signal into the decision, has no real shared pacing budget, and its feedback is **not authenticated**. The 0% arm still pays about **1%** for data sequence headers and reports; this is no-parity, not native wire bypass.

| At +200 ms | Uncoded down / up, Mbit/s | Receiver-triggered down / up | Always-on down / up |
|---|---:|---:|---:|
| 0% | 246.91 / 331.83 | 284.93 / 327.77 | — |
| 1% | 8.81 / 6.93 | 33.63 / 152.65 | 84.09 / 194.81 |

All 10 intervals completed, zero excess transit drops or local shim send/invalid-input events. At 1%, adaptive p95 was **205 ms down / 311 ms up**, versus **204 / 202 ms** uncoded. Adaptive recovered **828 / 1,644** packets across both workloads. Clean startup did not emit parity; loss restored a large part of the available uplink goodput but the downlink ramp remained slower than always-on parity. One sample per cell. **Lab only**: useful activation evidence, not a safe product policy under contention. [Adaptive overhead and recovery counts](adaptive-parity.tsv).

## Shared original/repair wire budget under shaping

Native WG real TUN at 1% +200 ms, eight downlink TCP streams. Same shim in three arms: uncoded, always-on two-way parity, and the same parity with a hub token bucket charging original and repair UDP datagrams plus IP/UDP headers. Budget arms use a known test capacity (60 Mbit/s on 120 Mbit/s or 6 on 10), a 6,000-byte burst, and no independent receive service while waiting inside the output batch. This is not a portable auto-rate or fairness policy.

| Profile | Uncoded / parity / shared budget goodput, Mbit/s | Excess transit drops | Loaded p95, ms |
|---|---:|---:|---:|
| 120 Mbit/s + 45 Mbit/s UDP competitor | 7.01 / 59.19 / 47.72 | 0 / 3,112 / 0 | 203 / 281 / 482 |
| 10 Mbit/s, 160 slots | 5.96 / 7.99 / 4.84 | 169 / 499 / 0 | 268 / 318 / 1,373 |

All six intervals valid; zero hub engine drops. The competitor delivered **44.57 / 43.98 / 44.51 Mbit/s**. The fixed token budget removed measured transit overflow, but delayed feedback and the narrow arm saw 11 local shim send drops. Loaded RTT and goodput reject this lockstep scheduler. No inference about how a separately scheduled, bounded original/repair budget would behave. [Shaped budget measurements](wire-budget.tsv).

A follow-up preserves the same token rates but keeps two fixed output batches around and services UDP receives while paced output waits. Compared with the lockstep budget, this eliminated measured local send drops (**23 → 0** with competition, **37 → 0** on 10 Mbit/s; shutdown discards counted separately). Excess transit drops stayed zero. Goodput stayed **47.94 → 47.93** and **4.80 → 4.78 Mbit/s**; p95 **500 → 500** and **1,239 → 1,501 ms**. Thus blocked receive service contributed local loss, but separating it alone did not solve the loaded latency or rate ceiling. Four valid samples, one per arm per profile; lab only. [Split scheduler measurements](split-wire-budget.tsv).

## Current source build acceptance smoke

`current-matrix` uses the source after the route timestamp correction. All 16 real managed WG/AWG TUN trials completed with calibrated selective offloads. At 0% the observed local range was **617–695 Mbit/s** and +200 ms **146–271 Mbit/s**; queue overflow remains (e.g. **16,616** client path drops on one local AWG download). At 1% +200 ms, rates stayed **7.59–10.69 Mbit/s** while the hub had zero drops and the client had zero except **30** in AWG upload. No excess transit qdisc or UDP receive-buffer drops in these 16 cells. These are current build smoke observations, not a paired speed gain. The loss-resilient parity candidate and queue feedback probes remain lab-only.

`sackdup-matrix` compares that source with the duplicate out-of-order DATA SACK fix, only at 1% loss on the two AWG µTP modes. The old arm completed 6/8; one Request carrier did not come up, leaving both +200 ms cells missing. New arm completed 8/8. Extended/µTP +200 ms download/upload **0.595/0.690 → 0.495/0.837 Mbit/s**. Local Extended/µTP improved, local Request/µTP slowed in this single pair. This verifies the fixed build transfers traffic, not that its congestion-control throughput or intermittent carrier startup is repaired.

## Cold Request/µTP startup at 1% +200 ms

Eight fresh foreground tunnels per arm, matched seed order, same hub binary, no bulk load. Before, every trial logged one `no carrier` send and one AWG retry; after, neither occurred. First overlay ping median **6.074 → 1.463 s**, ranges **5.952–9.404 → 1.459–3.853 s**; **8/8 ready** in each arm. Both ends' transit header captures had zero capture drops. This isolates a cold startup defect with persistent keepalive, not all intermittent stalls or steady-state goodput. The client regression failed at the three-second no-datagram deadline before the fix; it and the carrier experiment e2e cases passed under `-race` after it. [Startup measurements](startup.tsv).

## Sendable µTP window after backoff

The deterministic regression failed before the one-segment congestion floor: a full segment with available receiver credit was not sent after halving. It now sends immediately and still does not send through zero or insufficient peer credit. Pure Go µTP and libutp interop passed under `-race`.

At 1% +200 ms, all 16 managed A/B attempts delivered data on all eight flows with zero hub/client queue drops. Request/µTP download median **0.318 → 0.451 Mbit/s**, p95 **4.97 → 3.46 s**; upload **0.569 → 0.512 Mbit/s**. Extended/µTP remained **0.60–0.66 Mbit/s** on the patched build. The exact send-stall is fixed; this is not a substantial WAN throughput gain.

The 0% control completed **16/16** across both arms, 0 and +200 ms, two modes and both directions. Local goodput changed by approximately **−0.2% to +2.1%** (one sample per cell); at +200 ms the four changes ranged **−16% to +16%**, with queue drops still visible. No clean-path speedup is claimed.

## Receiver credit and idle transmit clock

The vendored µTP sender now flushes queued DATA when a standalone peer receive-window update increases credit, even if the cumulative ACK does not advance. The regression drives an actual STATE packet and checks that both queued DATA segments are sent before the socket timer. A second regression reproduces an early retry after an idle period: the first write used a cached socket time older than the actual send. The sender refreshes that clock only before starting a flight, without changing the retransmit interval. Both regressions failed before their fixes; `go test -race ./purego ./interop -count=1` passed afterward. The four AWG BitTorrent carrier formats and negotiated options are unchanged.

The `window-credit-matrix` comparison completed **64/64** trials across four carriers, both directions, 0/1% and 0/+200 ms. The two TCP modes are controls for the µTP-only change; their rates shifted too (for example Extended/TCP downlink **12.47 → 18.66 Mbit/s** at 1% +200 ms), showing that one trial per cell cannot establish a speed gain. µTP still delivered under 1 Mbit/s without internal queue drops in that WAN profile.

The `fresh-flight-matrix` build before the queued-tail fix completed **31/32** attempts. In the one failed clean +200 ms Request/TCP upload, an inner TCP socket timed out connecting while the carrier remained connected and the tunnel answered 96/97 ICMP probes. No hub/client queue or transit drops were measured in that trial; its cause is not established. At 1% +200 ms, receiver download/upload Mbit/s were Extended/TCP **14.70/12.75**, Request/TCP **13.35/17.25**, Extended/µTP **0.629/0.870**, Request/µTP **0.503/0.363**. Both µTP modes had zero measured hub/client queue drops. At 0%, queue saturation still appears under sustained load; one Extended/TCP WAN upload had **9,137** client queue drops. These are smoke results, not an overall throughput win.

Six fresh Request/TCP tunnels at 0% +200 ms then repeated the eight-flow workload down and up with TCP header capture on both TUNs and socket-state sampling. All **12/12** intervals completed; all 24 capture sockets reported zero capture drops. The original connect timeout did not recur. The traces and runner are in `raw.tar.gz` under `req-tcp-trace/`. No Request/TCP startup fix is claimed.

## Queued short µTP segment respects peer credit

A cumulative ACK previously had a separate Nagle flush that sent the last queued segment without checking the newly advertised receiver window. It now uses the existing normal flush. The wire regression queues four bytes behind a full segment, delivers an ACK advertising zero credit, then standalone updates for three and four bytes. The old path sent on zero credit; the fix holds the segment until all four bytes fit. Pure Go and libutp wire interop passed under `-race`. No framing, window sizing, or retransmit-interval change.

After the fix, the four 1% +200 ms cells delivered Extended/µTP **0.599 / 0.845 Mbit/s** and Request/µTP **0.377 / 0.623 Mbit/s** (down / up). Loaded p95 was **4.84–5.67 s**, with zero hub engine, client TUN, excess transit qdisc or UDP receive-buffer drops. With 0% loss at +200 ms, the fix did not eliminate overload: Extended/µTP downlink had **8,108** hub engine drops in one run. This is a flow-control correctness fix, not a congestion-controller change.

The `tail-credit-matrix` and `tail-credit-resume` series compare `fresh-flight` against `tail-credit`, one attempt per arm for each of two modes, two directions, two loss levels and two delays. The first runner reached its deadline partway through the candidate's lossy WAN profile; its completed profiles were retained and the incomplete profile was rerun in fresh namespaces. **14/16** baseline and **16/16** candidate intervals transferred on all eight flows. Both invalid baseline cells were 1% local uploads with one zero-byte flow each; no repeat establishes a completion-rate gain. Local 0% goodput changed by **−2.9% to +17.0%**; at 0% +200 ms it changed by **−13.6% to +3.8%**. At 1% +200 ms both arms completed all four intervals with no measured endpoint queue overflow. The four rates remained below 1 Mbit/s in both arms.

Queue overflow and lossy WAN throughput are separate constraints. At 0% induced loss, bursts reach full endpoint queues under sustained load, including at +200 ms; growing queues or retrying admission made loaded latency or another mode worse in the paired controls above. At 1% +200 ms, the µTP collapse persists even when endpoint queues do not overflow. The separate [window accounting experiment](../extended-utp-wan-cwnd/summary.md) attributes that Extended/µTP profile to fast-loss window cuts, not RTO, delay backoff, or remote receive credit. The [RTT-aware backoff experiment](../extended-utp-rtt-backoff/summary.md) confirms that cutting at most once per RTT does not by itself restore bulk throughput. Do not infer an end-to-end speedup from the recovery regressions.

## Blocked short µTP write resumes on sufficient credit

The wait state previously woke writers only when a full segment fit, even if a pending `Write` needed just a few bytes. A wire-state regression starts a four-byte `Write` with no receive credit and delivers a standalone ACK granting exactly four bytes. The original write timed out; the fixed writer immediately sends the four payload bytes. Waiting writers recheck their own write size after a wakeup, and short concurrent writes still pass the congestion, send-buffer, receiver and packet-ring checks. No wire or congestion-policy change. Pure Go µTP and libutp interop passed under `-race`.

The separate [32-attempt smoke](../utp-short-write-credit/summary.md) covered both µTP modes in both directions at 0/1% and 0/+200 ms. All 16 fixed intervals completed; the four fixed lossy WAN cells remained below 1 Mbit/s with no endpoint queue drops.

## µTP retransmission starts after transmission

A closed receive window could strand an unsent short segment in the queue. A leftover retransmission deadline then sent that segment as if it had already been sent and lost, bypassing peer credit. The timer now starts when the oldest outstanding packet actually goes out and is disarmed for an unsent head. The regression observes no DATA at expiry with zero credit, prompt first DATA when credit opens, and the actual retransmission only after a full timeout. Pure Go µTP and libutp interop passed under `-race`; Linux hub/client and Windows amd64 client built. This is not a congestion-control or window-size change.

The first-flight deadline uses the outgoing packet timestamp, not the cached socket clock. A deterministic regression sends DATA 100 ms after that clock sample and verifies a retry at the actual deadline, not one millisecond before it.

## µTP MTU probe follows the packet sent

Reopening peer credit can send an older queued datagram while `seqNr-1` refers to the queue tail. The probe now records the sequence in the sent header. Both cumulative and selective ACKs can confirm the probe. See [wire regressions and managed TUN results](../utp-mtu-probe/summary.md).

[All 686 matrix trials](measurements.tsv) · [80 paired path/profile cells](comparison.tsv) · [SACK A/B](sack-wan.tsv) · [Rollback](rollback.tsv) · [Packet attribution](rollback-attribution.txt) · [Raw data, SHA-256s and replay sources](raw.tar.gz). All thirteen derived files reproduce byte for byte from a clean extraction. No global queue or system congestion-control default was changed.
