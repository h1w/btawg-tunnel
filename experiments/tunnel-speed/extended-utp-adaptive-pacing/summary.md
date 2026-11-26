# Extended/µTP: adaptive pacing at +200 ms RTT

Lab-only experiment. All arms retain the [oldest-packet RTO and RTT-aware backoff fixes](../extended-utp-rtt-backoff/summary.md); no AWG batching, framing or production-default changes. Eight TCP downloads over a fresh Extended/µTP tunnel per attempt. Goodput is the sum of `TCP_INFO.tcpi_bytes_received` deltas over seconds 4–16, with no iperf3 control exchange. Selective offload settings and per-datagram loss calibration are shared. The 1% profile injects independent loss in both directions; the narrow profile has no injected loss and shapes the downlink to 10 Mbit/s with a 160-packet queue.

- **A:** current controller unpaced or with the existing fixed 200 Mbit/s pacing option on both endpoints.
- **D:** per-connection rate starts at 2.10 Mbit/s. Each round (at least one smoothed RTT and at least 50 ms) measures newly ACKed/SACKed paced packet bytes. A quiet, sufficiently utilized round can raise the rate by up to 50%; reported queue delay of at least 12.5 ms lowers the rate, and RTO halves it. Rate limits: 0.524–536.871 Mbit/s. Like the fixed-paced arm, it uses the paced recovery settings and a 90% rate×base-RTT window floor when delay is low. Pacing waits in the send path under the shared socket mutex.
- **E:** same adaptive policy; new-data pacing waits outside the socket mutex, then rechecks connection state and window before sending a bounded chunk. Retransmissions are not charged to E's pacer. The variants are not production implementations.

The deterministic policy probes cover sparse ACKs, bounded growth, no increase on a compressed ACK before the sampling interval, queue backoff and timeout backoff. Both revisions passed 20 repetitions under `-race`; the tests exercise the decision policy, not real scheduling or path capacity.

## Results

| Profile | A unpaced | A fixed 200 | D lock-held | E unlocked |
|---|---:|---:|---:|---:|
| 0%, Mbit/s | 178.291 | 88.429 | 0.402 | 2.207, 2.234 |
| 1%, Mbit/s | 0.606 | 14.770 | 0.387 | 2.260, 2.262 |
| 10 Mbit/s link, Mbit/s | 2.690 | 2.783 | 0.395 | 2.317, 2.351 |
| 10 Mbit/s link, excess qdisc drops | 226 | 16,418 | 0 | 0, 0 |

All 15 runs produced valid TCP receiver counter deltas. A/D are single samples per cell, E has two; scheduling and packet order can differ even with a matched impairment seed. A narrow-channel drop count spans the entire load run. Neither adaptive revision saturated the 10 Mbit/s link, so zero qdisc drops here do not demonstrate a successful congestion response at capacity.

D held the socket mutex while waiting to send, delaying other work on that socket. Its clean delay estimate exceeded the 12.5 ms threshold; sampled pacing dropped to the minimum. E removed that wait from the critical section and the clean delay estimate stayed below 0.20 ms. E still only targeted about 4 Mbit/s in the clean interval while delivering 2.2 Mbit/s. Its sampled sender window was not full on any clean tick, and no fast retransmit or RTO cut happened in that interval. Those results are consistent with a pacing/probing limit, not the original collapse of the µTP send window. This experiment did not separate wake-up rounding, bidirectional small-packet pacing and the 75% utilization threshold.

**Decision: neither adaptive mode is suitable for the default.** E improved 1% goodput about 3.7× over A unpaced without overloading the narrow queue, but lost about 99% of clean throughput in this lab. A credible next candidate needs to account for actual send utilization and probe upward when the pacer itself is the bottleneck, without holding the ACK lock or inventing capacity from a fixed ceiling. No product changes.

[Measurements](measurements.tsv) · [Raw counter traces, overlays and runners](raw.tar.gz). `python3 -I reduce.py bench.log repeat.log` from a clean extraction reproduces the table byte for byte. Use `bash run.sh <repository-root>` for A/D and `bash run-v2.sh <repository-root>` for E.
