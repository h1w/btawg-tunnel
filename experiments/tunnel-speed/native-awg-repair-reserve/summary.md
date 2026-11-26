# Native AWG: deadline-bounded repair reservation at 1% and +200 ms

Real Linux TUN; native AWG directly over UDP, no SOCKS, BitTorrent or application TCP changes. Fresh namespaces per run. A pass-through shim is the AWG control. Two other arms share identical 16+2 (four interleaved groups) framing, a pacer, feedback, and uplink with repair disabled. Only one of the matched paced arms emits downlink repair. All shims and reports are lab-only; feedback is unauthenticated. No production code or defaults changed.

The candidate lets up to eight repair datagrams wait for up to 12 ms, separately from the bounded 48 KB original queue. It accrues repair credit at one eighth of transmitted original bytes with a two-datagram credit ceiling; eligible repair can go first. Both classes and feedback spend the same total byte tokens (UDP and IPv4 headers included), at 80 Mbit/s on the unshaped test and 2 Mbit/s on the shared 10 Mbit/s FIFO. The token bucket burst stays at 3,200 bytes. Outgoing timestamping follows the local queue, so the remote delay guard does not count local shaping wait. This is not an automatic rate-selection policy.

All trials use 100 ms propagation each way, and at 1% use independent seeded random loss in each direction. The goodput window is seconds 5–17 of the bulk transfer. A trial's repair, queue and loss counts include warmup. Two seeds at 1% and one clean control seed on the shared link. The thresholds were ≥1.5× both unshaped controls with ≤35 ms more overlay p95; on the shared FIFO, ≥10% AWG gain at 1% and ≥90% AWG retention at 0% versus the identical cap without repair with ≤20 ms additional overlay p95. In each shared cell direct CUBIC must retain ≥80% of its solo rate and ≥90% of both coexistence controls, with ≤10 ms extra underlay p95.

## Unshaped downlink, 1% and +200 ms

| Seed | Plain AWG, Mbit/s | Cap with wire framing, no repair | Cap with repair reserve | Overlay p95 plain → reserve, ms | Repair sent / recovered |
|---:|---:|---:|---:|---:|---:|
| 101 | 6.147 | 4.946 | 9.562 | 202 → 202 | 1,066 / 249 |
| 211 | 5.429 | 4.850 | 13.524 | 202 → 204 | 1,653 / 374 |

Both seeds passed the speed gate: **1.56× and 2.49×** over pass-through, **1.93× and 2.79×** over the matched paced arm. Zero measured hub engine, UDP receive-buffer or excess netem drops; the hub pacer itself dropped 101 and 222 originals. The repair scheduler expired 670 and 604 shards; this is distinct from loss after sending. The larger speedups of earlier unpaced experiments used different policies and are not a matched comparison.

## Shared 10 Mbit/s FIFO

| Loss / seed | AWG : direct flows | AWG, no repair → reserve, Mbit/s | Direct TCP solo / plain AWG / cap only / reserve, Mbit/s | Overlay p95 plain / cap only / reserve, ms |
|---|---|---:|---:|---:|
| 0% / 101 | 1:1 | 1.750 → 1.732 | 8.950 / 4.722 / 7.317 / 7.369 | 225 / 387 / 380 |
| 0% / 101 | 8:1 | 1.763 → 1.736 | 8.950 / 1.233 / 7.208 / 7.216 | 225 / 397 / 402 |
| 1% / 101 | 1:1 | 0.637 → 0.636 | 0.847 / 0.710 / 0.490 / 0.691 | 202 / 205 / 202 |
| 1% / 101 | 8:1 | 1.744 → 1.703 | 0.847 / 0.842 / 0.619 / 0.551 | 202 / 384 / 390 |
| 1% / 211 | 1:1 | 0.858 → 0.478 | 0.751 / 1.054 / 0.495 / 0.643 | 202 / 206 / 205 |
| 1% / 211 | 8:1 | 1.743 → 1.691 | 0.751 / 0.624 / 0.624 / 0.551 | 204 / 385 / 390 |

**0/4 lossy shared-link pairs met both gates.** At 8:1 the client recovered 53–64 datagrams but AWG was 2–3% slower and direct TCP 11–12% slower versus the identical cap without repair. Both clean pairs met the *incremental* gates versus cap only; that does not mean the cap is acceptable: relative to native pass-through overlay p95 rose by **155–177 ms** in those pairs. At lossy 8:1, p95 rose by **186–188 ms** versus pass-through; the no-repair limiter incurred almost all of the same increase. A 48 KB queue can consume about 192 ms at 2 Mbit/s; omitting local wait from feedback does not eliminate it from user-visible latency.

**Decision:** keep the measured standalone gain and reject this combined shaper as a product fix. The next test should decouple affordable repair admission from the original-packet latency budget, e.g. use a time-bounded original backlog rather than the same byte queue at both rates. Neither reverting to instantaneous-only repair nor adding another large queue is supported by the data. Only downlink was tested; no SOCKS or µTP changes.

[27 receiver windows](measurements.tsv) · [Paired results](comparison.tsv) · [Raw counters, source and hashes](raw.tar.gz). Both tables reproduced byte for byte after extraction:

```sh
python3 -I reduce.py solo.log shared.log > measurements.tsv
python3 -I compare.py measurements.tsv > comparison.tsv
```

`bash replay.sh <matching-checkout>` rebuilds and repeats the unshaped arms. `LAB_MATRIX=shared LAB_REUSE_BINARIES=1 bash replay.sh <matching-checkout>` repeats the shared-link arms with the same binaries.
