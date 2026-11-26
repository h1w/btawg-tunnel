# Extended/µTP: bounded upward pacing probes

Lab-only follow-up to the [unlocked adaptive pacer](../extended-utp-adaptive-pacing/summary.md). E is the unchanged unlocked policy. P adds a 25% upward probe when the writer waited for the pacer, at least 4,200 estimated wire bytes were charged to new writes in the round, and ACKs cover at least 75% of that charged byte count. This branch is reached only when the regular ACK-budget increase does not trigger. The first full RTT-sized round at the new target is ignored for settling; the next must deliver at least 6.25% more bytes/s than the pre-probe sample, otherwise P restores the old target and waits at least one round before probing again. Queue feedback at or above 12.5 ms in a round and RTO cancel a probe and lower the rate. No production code, framing, retransmission scheduler, or default changed.

Both arms retain the earlier RTT/RTO fixes and pace new writes outside the socket mutex. The estimated sent count includes new µTP payload and header overhead, not retransmissions; ACK and new-write totals are measured on the same time interval, not matched per-packet flight cohorts. That makes the utilization test and rollback rule approximate. Both variants leave retransmissions outside new-write pacing.

Eight TCP downloads over fresh Linux Extended/µTP tunnels. Goodput is `TCP_INFO.tcpi_bytes_received` delta over seconds 4–16. Added RTT 200 ms, calibrated selective offloads, independent 1% loss each way when enabled. Narrow profile: 10 Mbit/s downstream, 160-packet netem queue, no injected loss. Each profile runs E/P/P/E with seed labels 101 and 211; equal seeds do not imply identical dropped stream bytes. The probe regression failed on E (no increase from 262,144 B/s); four deterministic policy tests passed on P 20 times under `-race`.

## Results

| Profile | E, Mbit/s | P, Mbit/s | Per-pair gain |
|---|---:|---:|---:|
| 0% loss | 2.358, 2.243 | 8.062, 7.240 | 3.42×, 3.23× |
| 1% loss | 2.199, 2.155 | 7.714, 7.079 | 3.51×, 3.28× |
| 10 Mbit/s link | 2.467, 2.512 | 3.749, 3.921 | 1.52×, 1.56× |

All 12 receiver measurements were valid. P's median pacing targets rose to 18.3–33.0 Mbit/s on the clean profile and 15.3–18.5 Mbit/s at 1%. Targets are not wire or application delivery rates. On the unshaped profiles no excess qdisc or UDP receive-buffer drops were observed, but hub engine drops increased. Most upward probes rolled back even on the clean profile, which had no queue-triggered or timeout rate cuts in the measured intervals. The cause of those no-gain decisions was not isolated.

The narrow link exposed the cost. P peaked at 13.39 and 16.90 Mbit/s in target rate, recorded **1,597 and 1,657 excess qdisc drops** over the two runs (E: zero), and took **5 and 7 timeout cuts** in the measured intervals. Only one round per run crossed the queue-delay trigger. P gained useful delivery over E, but the delay trigger did not prevent persistent overshoot/recovery on this queue.

**Decision: lab only.** Probing substantially improves the previous adaptive pacer, but does not recover the clean throughput observed with the product's unpaced sender in the prior, separately run matrix. On the 10 Mbit/s control it adds queue loss and RTO. The next discriminator is a matched trace of actual wire send rate and queue occupancy through a probe, including retransmissions; don't treat the target rate as the offered load.

[All measurements](measurements.tsv) · [Raw counters, policy tests, overlays and runners](raw.tar.gz). `python3 -I reduce.py bench.log` reproduces the TSV byte for byte from a clean extraction; run `bash run-probe.sh <repository-root>` to repeat the matrix.
