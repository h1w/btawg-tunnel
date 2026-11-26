# Extended/µTP: forward loss versus reverse feedback

Fresh foreground Linux Extended/µTP tunnel per interval, eight inner TCP downloads, 100 ms added each way, calibrated selective offloads, receiver-byte window seconds 4–16. Both arms have the unsent-DATA timer fix. One-way profiles drop 1% of all UDP datagrams in just one direction, not only DATA or only ACK by type. Two seed pairs per directional/bidirectional loss profile, one clean and one 10 Mbit/s downlink control (160 slots including propagation). No production changes.

## Isolating loss direction

| Stock, +200 ms RTT | Receiver Mbit/s, two seeds | Hub queue drops | Excess transit drops |
|---|---:|---:|---:|
| Forward-only 1% | 0.597, 0.605 | 0, 0 | 0, 0 |
| Reverse-only 1% | 3.396, 2.778 | 0, 0 | 0, 0 |
| Bidirectional 1% | 0.621, 0.623 | 0, 0 | 0, 0 |

No capture drops in these lossy trials. Forward-only loss reproduced the full goodput collapse, with 15–16 forward DATA retries in the interval; reverse-only loss had no captured forward retry in either interval. RTT from unambiguous same-interface DATA/ACK associations was **201.33–201.35 ms p95** with forward loss and **201.43–201.52 ms** with reverse loss. One reverse-only sample reached 400 ms, but that did not coincide with the worse forward-loss goodput. This, plus the [window attribution](../extended-utp-wan-cwnd/summary.md), identifies the sender's response to forward loss as the primary limiter here. It does not prove that every reverse loss stall has the same cause.

## Shorter deferred ACK timer

The lab overlay changes only the µTP deferred ACK timer from 1 ms to 100 µs on both endpoints. It retains framing, congestion response and queue limits.

| +200 ms profile | Stock → faster ACK Mbit/s | Excess transit drops stock → faster ACK |
|---|---:|---:|
| 1%, pair 101 | 0.621 → 0.665 | 0 → 0 |
| 1%, pair 211 | 0.623 → 0.629 | 0 → 0 |
| 0%, unshaped | 171.732 → 150.289 | 0 → 0 |
| 10 Mbit/s, 0% | 2.707 → 2.257 | 207 → 233 |

The faster timer increased captured standalone ACK arrivals **24 → 227** and **26 → 145** in the two lossy pairs, but receiver rates barely changed. Standalone ACK counts exclude piggybacked acknowledgements, and send/arrival counts from the same wall-time interval need not match because of propagation. Both clean unshaped captures dropped samples, so use those two rows for receiver goodput and independent queue counters only. The clean hub queue count rose **2,708 → 8,280**. Keep the shorter timer out of production: it did not remove the window limit and the clean and narrow controls regressed.

[12 receiver intervals](measurements.tsv) · [Header and RTT counts](ack.tsv) · [Raw captures, source, hashes and replay](raw.tar.gz). Both TSVs reproduced byte for byte. `python3 -I lab/summarize.py lab/feedback.log` and `python3 -I lab/ack-summary.py lab/feedback.log` reduce the extracted archive; `bash replay.sh <matching-checkout>` repeats the trials.
