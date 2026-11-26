# Quiet WAN µTP loss backoff

Existing AWG Extended/µTP, real Linux TUN, eight CUBIC downloads, +200 ms added RTT (100 ms per direction), selective offloads and the same current binaries except one Go overlay. The candidate retains 7/8 of the congestion window instead of half after a fast retransmit only with smoothed RTT ≥50 ms and a fresh one-way queue estimate below 2.5 ms. It leaves retransmit timing, RTO, receive credit, pacing, BitTorrent framing and wire format unchanged. A deterministic regression failed on the baseline's quiet WAN window and passed 20 times under `-race`; loaded, unknown-delay and local paths retain the existing cut.

| Profile | Baseline → candidate goodput, Mbit/s | Hub engine drops | Excess transit drops |
|---|---:|---:|---:|
| 1% / seed 101, unshaped | 0.582 → 1.502 | 0 → 0 | 0 → 0 |
| 1% / seed 211, unshaped | 0.606 → 1.458 | 0 → 0 | 0 → 0 |
| 10 Mbit/s / 160 slots, no random loss | 2.494 → 3.321 | 364 → 683 | 216 → 386 |

All six receiver-counter intervals valid, seconds 4–16. The narrow queue reached all 160 slots in both arms. It was sampled at or above 150 slots 18 times on control and 68 times with the candidate; these are counts of queue samples, not packet loss or time fractions. A low recent one-way minimum was insufficient to distinguish congestion from random loss, so the candidate is **not** enabled in production. Other production µTP fixes do not rely on a heuristic change to the loss response.

[Measurements](measurements.tsv) · [Raw data, overlay, tests and replay](raw.tar.gz). On extraction, run `python3 -I summarize.py bench.log` to reproduce the TSV. The narrow queue includes the propagation delay, so the experiment does not diagnose every real 10 Mbit/s bottleneck.
