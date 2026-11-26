# Extended/µTP: shared pacing budget for new DATA and retransmissions

Lab-only A/B on the [10 Mbit/s queue profile](../extended-utp-queue-trace/summary.md), not a production change. P is the prior probe-based pacer with new-write pacing only. S charges each transmitted non-empty DATA datagram (IP/UDP/µTP headers included) to one per-connection clock, whether new or resent. The SACK/RTO repair path queues retries without waiting inside the socket lock; ACKed entries are skipped. Repairs get the next DATA slot before new writes. A one-shot timer resumes work when the slot opens; ACKs keep their existing µTP path and do not share DATA credit. SYN, FIN and empty handshake DATA are also exempt from the DATA budget (FIN remains ordered behind queued DATA). S sends individual paced datagrams instead of P's paced new-write chunks; at most 1 ms of scheduler lateness can carry forward as credit. Probe/backoff policy is otherwise unchanged; the probe utilization estimator still counts newly sent bytes only. This comparison includes the change in burst granularity; it cannot isolate retransmission charging independently of that change.

Real Linux TUN, +200 ms added RTT, eight TCP downloads, downlink `netem` at 10 Mbit/s and 160 packet slots with no injected random loss. Fresh hub/client/tunnel per attempt, P/S/S/P order. TCP goodput comes from eight receiver `TCP_INFO` counters over seconds 4–16. Packet attribution matches pre/post `netem` captures over seconds 0–16; no payload is recorded. Two profiles are labeled 101 and 211 for pairing; the narrow profile does not inject seeded random drops.

The regression feeds an actual SACK into the state machine with exhausted DATA credit. The old sender emits a retry immediately. S defers it, emits an ACK without waiting for DATA credit, and delivers the retry before new DATA when credit becomes available.
It failed on P for the immediate retry and passed 20 times under `-race` on S. This verifies the ACK bypass locally; it does not guarantee that ACKs avoid drops in a downstream full queue.

## Results

| Pair | Goodput P → S, Mbit/s | Total qdisc drops P → S | New DATA drops P → S | Retry drops P → S | ACK drops P → S | Peak 10 ms retry ingress P → S, Mbit/s equivalent |
|---|---:|---:|---:|---:|---:|---:|
| 101 | 4.600 → 5.264 | 1,578 → 2,265 | 290 → 2,060 | 1,272 → 158 | 16 → 47 | 177.294 → 11.262 |
| 211 | 3.950 → 4.452 | 1,429 → 1,872 | 285 → 1,713 | 1,124 → 108 | 20 → 51 | 181.113 → 13.165 |

All four trials completed. Neither capture socket dropped samples, and the matched missing counts equal the independent qdisc counts for all four load intervals. Goodput improved **14.4% and 12.7%**. Retry drops fell **87.6% and 90.4%** even though S transmitted more retries in both pairs. Total losses increased **43.5% and 31.0%**: new DATA accounted for roughly 91% of drops with S. The ACK bypass does not prioritize ACKs inside `netem`; ACK drops also increased.

Every trial reached all 160 queue slots. First loss was new DATA, with no repeats yet in the queue. S first dropped at +2.26–2.37 s versus +4.36–5.07 s for P. This experiment does not isolate charging from finer send granularity, so the onset shift is not attributable to one of those changes alone. The observations do rule out unpaced retries as the cause of the initial overflow. S still sampled targets above 16 Mbit/s and took six and eight timeout cuts in its two measurement windows. Better repair delivery did not remove the oversized probes or the limited packet-slot budget.

**Decision: lab only.** Shared DATA credit suppresses retry bursts and improves goodput, but does not meet a narrow-link queue-loss gate. Leave the probe policy unchanged in this comparison; test loss-aware probe rollback separately before considering a default.

[Measurements](measurements.tsv) · [Per-packet attribution](queue-attribution.txt) · [Raw traces and build overlays](raw.tar.gz). Both derived outputs reproduce byte for byte from a clean extraction. To repeat all four trials run `bash run-shared.sh <repository-root>`.
