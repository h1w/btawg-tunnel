# Native AWG: original-first repair under one byte budget

Real Linux TUN, AWG directly over UDP, no SOCKS or BitTorrent. New network namespaces for every trial. Downlink through the hub into the client with inner TCP CUBIC kept unchanged. The control shim (`shim`) just passes through AWG datagrams. The two paced shim arms share the same encoded format, original-packet scheduler, budget, bidirectional feedback and client uplink with repair off. The only difference between `cap` and `priority` is whether the hub generates adaptive repair. The lab sidecar's feedback is unauthenticated and must not be deployed.

The hub token bucket counts IPv4 plus UDP headers and all payload bytes, including feedback and parity. Its burst is 3,200 bytes. Originals and feedback have a bounded 4,800-byte FIFO; repair is sent only after all ready originals and only when immediately affordable. Repair cannot wait in this FIFO. The pacer stays receptive while waiting for tokens. Coded packets are timestamped at outer UDP submission so the receiver's queue estimate excludes time waiting in that FIFO; the shim logs maximum local wait separately. The 16+2 code and 8 ms queue veto are unchanged from the previous lab candidate.

All profiles add 100 ms delay per direction; at 1%, both directions have independent seeded random loss. Without a shared bandwidth shaper we use eight AWG flows and an 80 Mbit/s lab budget. On the common 10 Mbit/s/32 KB FIFO we use a 2 Mbit/s budget and compare 1:1 and 8:1 inner/direct TCP flows with a separate TCP-only run. Two paired seeds at 1%, one at 0% on the common FIFO. A direct TCP flow starts first. Goodput uses receiver byte deltas for seconds 5–17; queue, UDP, and recovery counters span the whole trial.

Acceptance: on the unshaped lossy leg, ≥1.5× both controls and ≤35 ms increase in loaded overlay p95. On the shared link: direct CUBIC ≥80% of solo and ≥90% of both coexistence controls; underlay p95 ≤10 ms above either control. AWG must gain ≥10% at 1% and keep ≥90% at 0% versus identical capped framing without repair. This test does not select a deployable automatic rate or cover uploads.

## Queue-size discriminator on the unshaped lossy leg

The 4,800-byte FIFO dropped originals even though the measured mean rate was far below the 80 Mbit/s budget. A separate six-trial repeat on the same host enlarged only the original FIFO to 48,000 bytes; the 3,200-byte burst, priorities, rate and repair policy stayed fixed. It overlapped part of the shared-link matrix on the host, so the cross-series figures are not a CPU-isolated queue-size A/B. Within the repeat all three arms used the same settings except for framing/pacing versus pass-through and the repair switch between paced arms.

| 1% / seed | Pass-through, Mbit/s | Cap only, Mbit/s | Cap + priority repair, Mbit/s | Repair sent / rejected / recovered |
|---|---:|---:|---:|---:|
| 101 | 6.462 | 5.358 | 5.612 | 25 / 1,031 / 4 |
| 211 | 5.345 | 4.478 | 5.364 | 35 / 893 / 8 |

At p95 202 ms in all six trials, neither pair cleared the 1.5× gate. Only 2.4–3.8% of generated repair left the hub. The larger FIFO reduced the obvious batch-admission handicap but did not give parity enough immediate credit at group completion; this test does not say a deadline-bounded parity reservation would fail. [Six windows](wide.tsv) · [Raw queue discriminator](wide.tar.gz). After extraction, `python3 -I reduce.py wide.log` reproduces the TSV byte for byte; `bash wide.sh <matching-checkout>` rebuilds and repeats the runs.

## Shared 10 Mbit/s FIFO

| Loss | AWG : direct flows | Direct TCP, cap only → repair, Mbit/s | AWG, cap only → repair, Mbit/s |
|---|---|---:|---:|
| 0% | 1:1 | 7.685 → 7.715 | 1.421 → 1.430 |
| 0% | 8:1 | 7.361 → 7.633 | 1.671 → 1.650 |
| 1%, seed 101 | 1:1 | 0.624 → 0.718 | 0.534 → 0.573 |
| 1%, seed 211 | 1:1 | 0.821 → 0.656 | 0.422 → 0.539 |
| 1%, seed 101 | 8:1 | 0.787 → 0.654 | 1.578 → 1.574 |
| 1%, seed 211 | 8:1 | 0.779 → 0.673 | 1.581 → 1.607 |

Both clean pairs passed, with direct TCP keeping 85–86% of solo throughput. No lossy pair passed both gates. At 8:1 the hub emitted no repair in either lossy trial. At 1:1 only 3–6 repair datagrams were emitted and 0–2 originals reported recovered, so throughput scatter cannot be attributed to FEC. Underlay p95 was unchanged versus the cap-only arm for all four lossy shared-link pairs. Maximum measured local pacing wait was 16.6–18.5 ms there; it was not included in one-way feedback timestamps.

## Unshaped lossy leg, 4.8 KB queue

Pass-through was **5.49–6.33 Mbit/s**; the capped controls reached **2.41–2.56**, and priority repair **2.57–2.64**. The hub discarded 193–208 originals locally in the priority arm and sent just 17–23 out of 304–346 generated repair datagrams. The separate 48 KB repeat above removed most of the rate penalty but still gave no repeatable gain over pass-through.

**Do not ship.** Immediate spare-credit-only repair was starved precisely when original traffic arrived in batches. The experiment does not fix the native AWG slowdown or qualify another transport. No production code, SOCKS behavior, or host defaults changed. Do not read a negative shim payload delta as compression: it includes originals dropped before UDP submission.

[27 receiver windows](measurements.tsv) · [Paired gates](comparison.tsv) · [Raw data, lab sources and binary digests](raw.tar.gz). The two TSVs reproduce byte for byte with `python3 -I reduce.py priority.log > measurements.tsv` and `python3 -I compare.py measurements.tsv > comparison.tsv` after extraction. `bash replay.sh <matching-checkout>` rebuilds and repeats the matrix.
