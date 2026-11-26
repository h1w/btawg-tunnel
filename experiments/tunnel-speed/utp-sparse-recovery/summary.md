# µTP: recovering sparse SACK gaps earlier

The lab overlay repairs the oldest transmitted DATA before RTO when one or two later packets are selectively acknowledged and another SACK confirms the same gap after half a smoothed RTT. RTT must be at least 50 ms; the original packet must have been transmitted once and not be an MTU probe. The repair takes the normal fast-retransmit path and the existing congestion-window cut. No wire change, timer policy change, or queue growth. A simulated loss regression showed no datagram on stock, and the overlay repaired the head, cut the window normally, and avoided an immediate duplicate retry. The focused loss/RTO regressions passed ten times under `-race` with the overlay.

## Foreground Extended/µTP, +200 ms RTT

Eight TCP downloads; goodput from receiver bytes over seconds 4–16. Two independent 1% seed pairs, one clean unshaped pair, one 10 Mbit/s downlink control with 160 packet slots including the propagation delay and no induced loss. Both builds already have the unsent-DATA timer fix.

| Profile | Stock → early Mbit/s | Excess transit drops stock → early |
|---|---:|---:|
| 1%, seed 101 | 0.625 → 0.600 | 0 → 0 |
| 1%, seed 211 | 0.603 → 0.600 | 0 → 0 |
| 0%, unshaped | 183.259 → 185.791 | 0 → 0 |
| 10 Mbit/s, 0% | 2.877 → 2.858 | 202 → 215 |

## Managed TUN smoke, +200 ms RTT

Eight TCP flows, both directions and both µTP modes, one attempt per cell, 2 s warmup plus 5 s receiver window. **16/16** valid. At 1%, down/up Mbit/s were **0.525/0.662 → 0.607/0.753** for Extended and **0.426/0.542 → 0.491/0.497** for Request; all four pairs had zero measured endpoint queue drops. At 0% the Extended down/up rates fell by about 10–12%, while the Request rates rose. These single pairs do not establish a repeatable gain or causality for the clean-path scatter.

**Decision:** keep the early-repair overlay out of production. It repairs the tested sparse gap but did not reproduce a bulk-throughput gain in the longer loss controls. A low throughput ceiling remains even without endpoint queue drops; reducing rare head-of-line waits is not a substitute for addressing the outer window limit.

[16 managed intervals](measurements.tsv) · [8 longer controls](narrow.tsv) · [Raw counters, source, binary hashes and replay](raw.tar.gz). Both TSVs reproduce byte for byte from the extracted archive with `python3 -I matrix/summarize.py early.log` and `python3 -I narrow/summarize.py narrow/early.log`. `bash replay.sh <matching-checkout>` rebuilds and repeats the measurements.
