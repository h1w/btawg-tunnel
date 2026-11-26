# Extended/µTP: oldest-unacknowledged retransmit timer

Follow-up to the [two-sided first-stall trace](../extended-utp-first-stall/summary.md): a DATA segment and its fast retransmission were lost, then SACKs acknowledging later segments kept postponing RTO for the hole. In the patched [`ackPacket`](../../../btawg/third_party/go-utp/purego/conn.go), ACKs still free in-flight bytes and sample RTT when applicable, but the retransmission deadline and consecutive timeout count reset only when the oldest outstanding packet is acknowledged. No fast-resend, congestion-control, AWG batching, or wire changes.

[`TestLostFastRetransmitExpiresDespiteLaterSACK`](../../../btawg/third_party/go-utp/purego/retransmit_test.go) drives two real protocol state machines with a manual timer clock. It discards both sends of the first DATA segment, delivers a later DATA and SACK just before the original deadline, and checks that timeout retry restores all five segments in order. Stock failed at the missing timeout retry; fixed passed 20 runs under `-race`. The affected `purego` package suite also passed.

## Method

A = stock timer, Q = fixed timer, same current sources otherwise. Both client and server run their respective arm. Fresh hub/client/carrier every attempt, real Linux TUN, Extended/µTP TCP CUBIC download, 2 s warmup + 6 s measured, no rate cap. 1 and 8 flows, 0 and +200 ms added RTT, 0% or independent 1% random underlay loss in each direction. Same selective offloads at both loss levels; per-datagram calibration in both directions on every trial. Each cell ran A/Q/Q/A with two samples per arm, paired netem seeds 101 and 211 (offset 1000 for the opposite direction). Packet order differs across builds, so a matched seed is not a matched set of lost stream bytes. A timeout is a failed attempt, not a zero-speed measurement.

## 0% underlay loss

All 16 attempts completed. Two-run medians and observed ranges, not confidence intervals.

| Added RTT | Streams | Goodput A → Q, Mbit/s [range] | Loaded p95 RTT A → Q, ms |
|---:|---:|---:|---:|
| 0 ms | 1 | 309.28 [309.02–309.54] → 309.07 [306.19–311.95] | 34.45 → 40.90 |
| 0 ms | 8 | 257.43 [209.37–305.48] → 282.34 [281.56–283.12] | 44.10 → 52.95 |
| +200 ms | 1 | 56.09 [48.58–63.61] → 62.23 [59.77–64.68] | 205.00 → 204.50 |
| +200 ms | 8 | 148.85 [141.56–156.14] → 162.09 [154.14–170.05] | 274.00 → 225.00 |

Goodput ranges overlap in all four cells. Local loaded RTT medians rose, while the +200 ms eight-stream median fell; two repeats do not establish a consistent latency effect.

## 1% underlay loss per direction

| Added RTT | Streams | Completed A → Q | Goodput of successful runs A → Q, Mbit/s |
|---:|---:|---:|---|
| 0 ms | 1 | 0/2 → 2/2 | — → 11.71, 32.33 |
| 0 ms | 8 | 1/2 → 2/2 | 5.59 → 3.50, 6.99 |
| +200 ms | 1 | 2/2 → 2/2 | 0.52, 0.52 → 0.52, 0.70 |
| +200 ms | 8 | 0/2 → 0/2 | — |

Completion improved from 3/8 to 6/8, entirely in the local cells (1/4 → 4/4). The p95 RTT for the two completed patched local eight-stream runs was 4.20–6.60 s, versus 2.15 s in the only completed stock run. This does not establish a speed or latency gain for eight flows. At +200 ms RTT both patched eight-stream trials timed out after all eight data connections were established, with zero measured bytes and no hub engine, excess qdisc or UDP receive-buffer drops. Overlay pings still got replies (134/228 and 135/228); the remaining stall mechanism was not traced in this experiment.

[Measurements](measurements.tsv) · [Medians and ranges](comparison.tsv) · [Failed attempts](failures.tsv) · [Raw data, source snapshots and replay scripts](raw.tar.gz). All three TSVs reproduce byte for byte from a clean extraction. Extract the archive, run `bash build.sh <repository-root>`, then `bash replay.sh`.
