# Native AWG auto: cold TCP startup at 1% / 200 ms

## No tunnel probes before the transfer

Fresh client, hub, recovery session and TCP sockets per case. The transit network already drops 1% independently per direction with 100 ms propagation each way. The standalone client starts normally; no overlay ping or DF traffic is used to pre-negotiate the recovery extension. MTU 1280 was checked by reading the link configuration. Overlay pings start only after the sender is released. One and eight ordinary CUBIC streams, seeds 101 and 211. Directions tested separately, not simultaneous full duplex.

| Streams | Direction | Receiver, first 0–10 s Mbit/s | First 0–1 s Mbit/s | First full 1 s bin >50 | TX / useful byte | Loaded p95 ms |
|---:|---|---:|---:|---|---:|---:|
| 1 | Download | 65.93–71.04 | 0.92–1.47 | 2–3 s | 1.727–1.745 | 203–213 |
| 1 | Upload | 67.90–68.85 | 1.44–1.47 | 2–3 s | 1.743–1.761 | 201 |
| 8 | Download | 73.50–75.20 | 11.07–11.10 | 1–2 s | 1.751–1.753 | 202–207 |
| 8 | Upload | 74.77–76.10 | 9.95–11.19 | 1–2 s | 1.767 | 201 |

**8/8** met all three conditions together: >50 Mbit/s from the zero-byte sample to second 10, ≤1.8 endpoint TX bytes/payload byte, and ≤230 ms overlay p95. None achieved 50 Mbit/s in *the very first second*. A fresh sender started at ten MSS; for an MSS of 1,228 B a single sender needs roughly 1.25 MB outstanding to sustain 50 Mbit/s at 200 ms. FEC prevents early loss from repeatedly cutting this growing window; it does not remove slow start or the TCP/AWG connection setup time. Timings are from application sender release **after TCP connect**, not process launch or first SYN. [Each test](unprimed.tsv) · [all one-second bins](unprimed-ramp.tsv).

## What changed

The client starts its authenticated recovery hello when AWG's peer and bind are ready. The hello and challenge signal mutual cold-start support without another capability round trip. If both endpoints measure a recovery handshake RTT of at least 80 ms and support the extension, each direction gets a provisional 8+2 coded flight from its first eligible TCP payload. The data/feedback path still uses the existing recovery authentication, peer policy, inner MTU and CUBIC. TCP SYN negotiation can be observed before the recovery session is established, so the receiver can enforce its ordinary credit limit from the beginning without guessing a window scale.

The provisional phase is bounded by 4 seconds or 32 MiB of originals per peer/direction per recovery generation, not per socket. It can cost parity on a clean WAN; TCP or ICMP setup alone does not start the data budget. The usual raw-loss, queue and headroom policy continues sampling during startup, can curtail repair on congestion, and takes over on repeated loss. An older `auto` endpoint cannot confirm the capability and stays on feedback-only auto. No global route, qdisc, congestion control, initial TCP window or MTU changes.

## Matched A/B and counterexamples

On the standard stand (same startup MTU checks, no payload before the window), old → new `auto` at 1% was **2.20 → 68.37 Mbit/s** for one-flow download, seed 101; **1.46 → 62.96** for one-flow upload, seed 211. Old forced `coded` on those controls delivered 65.58 and 58.05. All eight standard final-build target windows met the speed/cost/delay goals, as did the eight no-prime windows above. These are separate stand profiles; two loss seeds are not a production speed guarantee.

The tradeoff is measurable: on the 0% unshaped WAN, eight streams (one run per arm), plain → new `auto` down was 306.24 → 188.80 Mbit/s with p95 211 → 215 ms; up was 475.82 → 327.27 with p95 512 → 287 ms, measured over the same first ten seconds. On the shared 10 Mbit/s FIFO, `auto` vs existing adaptive parity delivered 4.95 vs 4.26 Mbit/s tunnel download; competing TCP got 0.659 vs 0.666 and overlay p95 was 216 vs 207 ms. This is not a fairness guarantee. The latency and throughput targets apply to the tested lossy unshaped path, not an already saturated clean or 10 Mbit/s link.

In a separate clean WAN downlink with continuous payload, the hub emitted 11,120 coded pieces during startup, then the counter stayed unchanged for the final 18 consecutive polls (200 ms polling interval) with no continuing raw-loss grant. The `clean-watch/` archive holds the counter series. A prior client with the final hub and a final client with the prior hub each completed a clean transfer without negotiating provisional repair (`mixed/`); these are interoperability checks, not lossy speed results.

## Control and reproduction

The original receiver runner discarded the first two seconds; this runner does not. Receiver `TCP_INFO.bytes_received` must be zero **before** the sender-release stamp. It then samples every second for ten seconds. Fresh `hub0` and `cli0` TX byte snapshots are aligned to the endpoints of this window (≤50 ms skew). TX includes both directions, repair, ACKs, AWG/UDP overhead and traffic discarded in transit, not physical preamble or ISP billing. In the 10 Mbit/s shared control both direct and tunnel payload are counted in the wire ratio. Queue/error/recovery counters span the whole trial and are not used as the useful-byte denominator.

[Matched standard stand](measurements.tsv) · [its per-second ramp](ramp.tsv) · [prior binaries, no payload warmup](prior.tsv) · [raw allowlisted files and replay scripts](raw.tar.gz). Standard namespace setup verifies the overlay with DF pings before connecting TCP; the **no-prime** variant above omits those pings. Diagnostic variants `pilot.log`, `deferred.log` and `no-prime/eager-pilot.log` are not final-build samples. Deferring recovery negotiation until application traffic left one no-prime upload at 2.90 Mbit/s; eager negotiation without retaining early SYNs left two one-flow downloads at 238–249 ms loaded p95. The archive includes those controls rather than pooling or deleting them.

Extract into a fresh scratch directory. `python3 -I final-reduce.py final.log` produces the standard receiver TSV; `python3 -I startup.py no-prime/final.log` produces the strict startup bins. To repeat: move the archived `final.log` files aside, run `LAB_RUNS="$(cat final.log.profiles)" bash final.sh /path/to/matching/checkout`, then from `no-prime/` use its own `final.log.profiles` and `final.sh` the same way. Real TUN, Linux user/net namespaces, CPUs 0–7 and the standard tunnel-speed lab utilities are required. No live credentials, keys, sockets, databases or packet payloads are archived.
