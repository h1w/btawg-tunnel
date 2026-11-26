# Managed AWG: native UDP and four unchanged carriers

**Contract.** `socks5-awg-udp`, `socks5-awg-{ext,req}-{tcp,utp}` use the same inner AWG recovery implementation, with independent peer/transport/session keys and feedback. TLS-IP without AWG and plain WG are not wrapped. No outer carrier congestion-control, framing, TLS-IP, port or µTP source changes from the input checkout. The updated client uses the subscription v2 profile; refresh preserves selected modes, balancers and other local settings. Refresh downloads configuration, not a binary.

**What changed on a clean path.** The previous cold-start stand measured an aggregate advertised window of 2 MiB and ~83.9 Mbit/s on a 200 ms path in seconds 3 and 4 regardless of actual loss. The revised policy separates provisional coding from the credit limit, releases early on verified intact groups, and checks delay against the measured propagation floor instead of classifying a filled LAN queue as a WAN. The Linux client interrupts the existing pollable TUN read for control instead of adding a poll syscall to each read. Routine LAN batches and pure ACKs avoid recovery bookkeeping; peer control, TCP setup and teardown still take the stateful path. Queue wait is bounded per TUN batch; changing an AWG carrier does not remove the native WG retry. The inner repair data are also encrypted under the peer session keys: an already drained frame cannot disclose its payload to a newly assigned peer.

## Measurement contract

Linux real kernel TUN, MTU 1280, CUBIC, no added rate limit except the separate shared-link control. Traffic never leaves the isolated namespaces. Each trial has a fresh hub, client, enrollment and TCP connection(s); no prior overlay ping and no omitted data warmup. Rates are useful receiver bytes over the first 10 seconds **after TCP connect**, not per-second peaks and not time from launching the VPN. Wire cost is the sum of both endpoint veth TX counters (including repair, ACKs and transport overhead) divided by receiver bytes over the aligned window. Upload and download are separate trials. Identical selective offload settings for both arms. The reference client/server match the hashes of the earlier [cold-start experiment](../native-awg-auto-cold/summary.md); `shim` disables the old client's repair. Reference and candidate never mix profile formats. Compare only rows from the same mode, direction, stream count and network profile.

Clean native goodput: eight streams, 0 and +200 ms RTT; paired A/B/B/A then B/A/A/B, four trials per arm and direction. The acceptance threshold is ≥95% of the corresponding plain native AWG median. Native loss target: 1% per direction, +200 ms RTT, one or eight streams, two seeds, >50 Mbit/s, wire cost ≤1.8 and loaded overlay p95 ≤230 ms simultaneously. Carrier controls: one stream, each direction, 0% and 1%, +200 ms; one sample per cell per arm is a transfer and negotiation check, **not evidence of statistical equivalence**. Loss inside an ordered outer stream causes delayed delivery rather than a missing AWG original. Do not transfer the native goodput target to the four carrier modes.

[All measured receiver windows](measurements.tsv) · [continuous native clean → 1% → clean](transitions.tsv) · [allowlisted raw counters, binary digests and runner](raw.tar.gz).

## Native qualification and initial carrier A/B (`final`)

| Native AWG, eight streams, 0% induced loss | Plain median Mbit/s | Auto median Mbit/s | Change |
|---|---:|---:|---:|
| Local download | 778.53 | 768.34 | −1.31% |
| Local upload | 788.28 | 784.65 | −0.46% |
| +200 ms download | 326.03 | 331.25 | +1.60% |
| +200 ms upload | 519.25 | 503.28 | −3.07% |

All four clean medians meet the 5% non-regression tolerance. This is not phase-by-phase equality or a claim that loaded RTT stays low on saturated clean links. At 1% / +200 ms native AWG met the three simultaneous goals in **8/8** cold transfers: one flow 56.68–68.73 Mbit/s, eight flows 74.84–75.91 Mbit/s, 1.721–1.764 TX/useful byte and 201–207 ms loaded p95. Direction-only loss: download 67.91 Mbit/s (one flow, no artificial uplink loss), upload 75.50 (eight flows, no artificial downlink loss). Separate 10 Mbit/s shared downlink: 4.18 Mbit/s tunnel, 0.716 direct TCP, 218 ms overlay p95; not a fairness guarantee. On uninterrupted eight-stream download clean → loss → clean, receiver goodput was 329.15 → 83.41 → 435.57 Mbit/s in the three phase windows. Hub repair switched on 2.87 s after loss began and off 3.38 s after it ended; no tunnel reconnect.

The initial integrated build reached 2.67–3.80 Mbit/s over TCP and 0.38–0.54 over µTP at 1% / +200 ms, one stream. The unchanged reference carriers were also slow. Three additional paired trials for each of the worst clean discrepancies identified one repeatable startup issue: Request/TCP download medians **25.75 → 8.20 Mbit/s** (four samples/arm), even with no sender retransmits in the inspected pair. Immediate parity added traffic before the single stream finished slow start. Request/µTP download showed much wider scatter (54.20 → 49.39); Extended/TCP upload medians were 95.52 → 93.83. Measurements from this build are retained separately, not pooled with the follow-up that starts ordered carriers in feedback-driven mode.

## Shipping binaries (`ordered`)

Only the ordered carriers' startup decision changed after the native A/B: no speculative FEC until the receiver observes actual inner loss. The code handling native UDP was not changed. On the shipping build, clean native download/upload measured **765.40 / 785.40 Mbit/s** local and **316.42 / 515.60 Mbit/s** at +200 ms (one confirmation per cell, not the four-trial medians above). At 1% / +200 ms **8/8** shipping-build cold transfers passed together: 59.55–76.07 Mbit/s, 1.728–1.769 wire bytes per useful byte, p95 201–205 ms.

| Shipping carrier, 1 stream, +200 ms RTT | 0% download / upload Mbit/s | 1% download / upload Mbit/s |
|---|---:|---:|
| AWG Extended/TCP | 62.37 / 129.82 | 3.25 / 6.68 |
| AWG Request/TCP | 25.99 / 25.38 | 3.16 / 2.61 |
| AWG Extended/µTP | 60.50 / 94.93 | 0.69 / 0.78 |
| AWG Request/µTP | 50.35 / 107.57 | 0.57 / 0.44 |

The repeated clean Request/TCP download recovered from **8.20** in the unconditional-parity pilot to **25.82 Mbit/s median** in the shipping variant (five runs), versus **25.75** in its original four plain controls. All 16 shipping carrier workload/direction cells delivered useful data. The observed TCP/µTP limitations are still visible: the native lossy speed and latency are not guarantees for the carrier modes. Across all control, pilot and shipping series, **131/131** cold receiver windows were valid, plus the separately timed 64 s transition. Build `ordered` in `sha256.txt` matches the local distribution under `dist/awg-v2-c552677a4282/`.

## Replay

Extract `raw.tar.gz` into a private directory. `python3 -I final-reduce.py <series>-<build>-<mode>-<one-way-ms>.log` recomputes the matching rows; `python3 -I transition-reduce.py transition.log` recomputes the transition. The archive contains runner code and allowlisted measurement files, not VPN identities, imports, cookies, login credentials or packet payloads.

To repeat the shipping series, put this checkout's Linux client and server in `ordered/` and the reference binaries in `reference/`. Move the archived case logs aside and run `bash ordered-matrix.sh`. `final-hash.sh /absolute/path/to/checkout` is the earlier qualification series using `reference/` and `final/`. Binary SHA-256 digests are in `sha256.txt`. Rootless user/network namespaces, `/dev/net/tun`, `tc`, and CPUs 0–7 are required. The clean → loss → clean trace uses the same sockets throughout; its last-ten-second phase windows are not part of the cold A/B.

## Functional checks independent of throughput

`go test -race ./... -count=1` passed in `btawg/`. `TestAutoColdFirstFlightSurvivesBeforeLossReport` verifies byte-exact first-flight reconstruction on native UDP and no unsolicited cold parity on the four ordered carriers. `TestAutoTransitionsOnOneLiveSession` verifies activation, reconstruction and release on real missing **inner** packets in each of the five AWG key domains. `TestEndToEndSocksAndLan` and `TestMixedTransportStableAddress` run actual client/hub sessions, including unchanged WG and TLS-only control paths. Reassignment, stale/reordered controls, rapid reattachment and late originals have separate regressions. `TestRefreshSubscriptionMigratesV1ImportInPlace` uses HTTPS and a managed configuration file; it checks that replacing the import preserves local settings. It does not upgrade a binary. Windows amd64 was cross-built, not benchmarked on Windows.
