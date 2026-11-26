# Extended/µTP: ready pairs after recovery fixes

Rerun of the [ready-pair candidate](../extended-utp-awg-ready-pair/summary.md) on the source with the µTP feedback fixes and IPv4 fragment fixes. **A** is stock; **Q** replaces only the hub's AWG sequential sender with the existing lab overlay. It can send one additional ready packet through the existing batch path without waiting or skipping an unfinished predecessor. The Linux client binary is byte-identical in both arms. No framing, congestion policy or queue capacity change.

Linux real TUN, managed client, `socks5-awg-ext-utp`, eight CUBIC downloads, 2 s warmup and 5 s receiver-byte interval. Same selective offloads, independent seeded loss per direction, fresh sessions, A/Q/Q/A for each loss level. Two attempts per cell; numbers are sample medians, not confidence intervals.

| Underlay | Goodput A → Q, Mbit/s | Loaded p95 A → Q, ms | Hub queue drops A → Q |
|---|---:|---:|---:|
| 0%, local | 330.72 → 417.25 | 38.35 → 30.70 | 112 → 127 |
| 0%, +200 ms | 209.57 → 256.42 | 441.50 → 443.00 | 6,694 → 11,315 |
| 1%, local | 28.50 → 54.62 | 1,322.50 → 1,355.50 | 686 → 899 |
| 1%, +200 ms | 0.593 → 0.700 | 3,859.50 → 3,622.00 | 0 → 0 |

All 16 intervals delivered on all eight flows with no excess transit qdisc or UDP receive-buffer drops. Unlike the older iperf-based result, this run measures receiver progress without an iperf control/result exchange; completion counts across the two reports do not isolate the recovery changes. At 1% +200 ms Q measured 0.668 and 0.731 Mbit/s versus A's 0.507 and 0.678, with no endpoint queue drops on either arm. The same candidate's higher clean WAN goodput comes with 69% more hub drops. At 1% local the gain varies between 41% and 124% across pairs.

**Decision:** useful throughput candidate for the existing Extended/µTP downlink, not a general queue-loss or lossy-WAN fix. Keep it lab-only: it does not clear the clean WAN queue gate, was not tested here with other carriers or on a narrow bottleneck, and does not recover substantial goodput at 1% +200 ms.

[All attempts](measurements.tsv) · [Comparison](comparison.tsv) · [Raw counters, overlay, hashes and runners](raw.tar.gz). From an extracted archive, `python3 -I summarize.py ready-recovery.log >measurements.tsv` and `python3 -I paired.py <measurements.tsv >comparison.tsv` reproduce both tables byte for byte. `bash replay.sh <matching-checkout>` rebuilds and repeats the experiment in private namespaces.
