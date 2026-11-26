# Extended/µTP: what fills the 10 Mbit/s queue

**New DATA fills the queue first. Retransmissions appear about one RTT later and dominate the losses that follow.** The queue is limited to 160 packets, not a byte budget. The first drop can happen even while the 100 ms mean ingress IP rate is below the configured 10 Mbit/s.

## Method

Same [bounded-probe bottleneck profile](../extended-utp-paced-probe/summary.md): +200 ms RTT, 10 Mbit/s downstream `netem` with 160 packet slots including the 100 ms propagation delay, no injected loss, eight TCP download sockets. One E control without upward probes, two P runs with probes. AF_PACKET recorded only headers and IP lengths on transit `tohub` ingress and `tocli` egress; zero capture drops. A DATA sequence seen before on the same flow, with 16-bit wrap handled, is a retransmission. Ingress/egress matching uses ports, µTP identifiers and timestamps, IPv4 ID and lengths. Queue snapshots used `tc -s` about every 10–14 ms; per-packet residence is reconstructed between matched ingress and egress. A separate log records exact pacer rate changes; no transport behavior changes.

| Run | TCP goodput, Mbit/s (4–16 s) | Peak qlen | qdisc drops (0–16 s, sampled boundaries) | Matched missing: new DATA / retransmit / ACK |
|---|---:|---:|---:|---:|
| E control | 2.269 | 71 | 0 | 0 / 0 / 0 |
| P-101 | 4.797 | 160 | 1,947 | 397 / 1,516 / 29 |
| P-211 | 4.083 | 160 | 1,547 | 316 / 1,217 / 9 |

The independent qdisc counts exceed matched µTP drops by five in both sampled windows. qdisc includes other traffic and its sample cutoffs differ slightly from the packet timestamps; those five were not assigned a µTP type. Aggregate TCP speed comes from receiver byte deltas, not from IP lengths.

## First overflow

| P run | First dropped packet | Inferred resident packets at drop: new / repeat / ACK | Ingress → egress packet/s in prior 100 ms | Ingress IP Mbit/s in prior 100 ms | First repeat after drop |
|---|---|---:|---:|---:|---:|
| 101 | new DATA `seq=11815`, +4.145798 s | 135 / 0 / 25 | 1,580 → 1,420 | 8.725 | +205.265 ms, same seq |
| 211 | new DATA `seq=18155`, +4.114787 s | 139 / 0 / 21 | 1,600 → 1,270 | 9.214 | +206.057 ms, same seq |

At onset the pacer had just increased its target to 11.63 and 12.87 Mbit/s respectively. Neither preceding 100 ms window nor following 200 ms window contained a repeat. The latter windows already had 5 and 26 dropped new DATA packets. Small µTP ACKs use about a seventh of the slots at onset despite consuming around 1% of ingress IP bytes in the preceding 100 ms. A time-average below 10 Mbit/s is not a guarantee against a full packet queue: packet arrival rate exceeded departure rate and the delay line already held packets. This profile combines shaping and delay in one finite queue; it is not a universal packet-rate cap for real 10 Mbit/s links.

## After the first overflow

Retries contributed only **11.3–12.6% of offered IP bytes** over the 16 s load, but **78.1–78.9% of matched drops**. The peak 10 ms ingress bins contained only retransmitted DATA: **177.9 and 165.4 Mbit/s equivalent IP rate**, not sustained link throughput. Across near-full queue samples (150–160 slots), new DATA still accounted for ~82% of reconstructed packet residence, retries ~7–9%, and ACKs ~9–11%. Retries are therefore not the majority of the backlog; bursts of retries hit a queue already occupied by new data.

The pacer continued increasing its target after the first drop, with no queue-triggered rate cuts in the 4–16 s samples; each P run took eight RTO rate cuts there. The trace separates the two mechanisms: the probe fills the packet queue with new data, and retries outside the pacer's budget amplify loss. Pacing only retries would not prevent the first overflow. A next comparison should budget bulk new sends and retries together, reserve timely ACK service, and separately test probe rollback when delivery stops improving under congestion. No such policy was changed here.

[Packet/queue attribution](queue-attribution.txt) · [Receiver and controller counters](measurements.tsv) · [Raw headers, queue samples and replay sources](raw.tar.gz). Both derived artifacts reproduce byte for byte from a clean archive extraction. Use `bash run-probe.sh <repository-root>` to repeat the three captures.
