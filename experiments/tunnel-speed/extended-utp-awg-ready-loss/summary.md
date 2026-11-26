# Extended/µTP: ready AWG collection at 0% and 1% underlay loss

A compares the current outbound sender with **Q**, the unchanged AWG-only lab overlay from the [split experiment](../extended-utp-split-batching/summary.md): the per-peer sender takes up to four already-encrypted packets without blocking for another container. Same client binary, same UDP diagnostic counters on both hubs; no Extended frame batching, queue capacity, µTP controller, GSO policy or protocol changes. Q is a lab build, not applied to the product.

Real Linux TUN, CUBIC downloads, one and eight streams, two repeats per arm, 2 s omitted + 6 s nominal, 0 or 100 ms one-way added delay. No rate shaping. Both 0% and 1% profiles use identical selective veth offload settings; 1% is independent random loss in each direction. In every namespace run, a separate calibration verified that a UDP_SEGMENT burst was split into individually droppable packets before `netem`. Successful trials at 1% saw 0.839–0.989% downstream qdisc loss; the lighter upstream leg ranged from 0.192–1.065%. These are qdisc counts, not inner TCP loss. The trial timeout was 29 s, with a 5 s control-connect timeout. A client carrier was reused across stream counts within each delay; see the independent fresh-tunnel control below.

| Loss | Added RTT | Streams | Completed A / Q | Receiver goodput Mbit/s A → Q | Loaded p95 ms A → Q | Engine drops / TUN packets A → Q |
|---|---:|---:|---:|---:|---:|---:|
| 0% | 0 ms | 1 | 2/2 · 2/2 | 310.4 → 431.6 | 40.15 → 26.70 | 0.026% → 0.030% |
| 0% | 0 ms | 8 | 2/2 · 2/2 | 306.1 → 422.5 | 41.65 → 30.80 | 0.082% → 0.073% |
| 0% | +200 ms | 1 | 2/2 · 2/2 | 60.5 → 63.6 | 201.5 → 201.5 | 8.340% → 8.028% |
| 0% | +200 ms | 8 | 2/2 · 2/2 | 210.7 → 285.2 | 249.0 → 224.5 | 4.793% → 2.846% |
| 1% | 0 ms | 1 | 0/2 · 0/2 | — | — | — |
| 1% | 0 ms | 8 | 0/2 · 0/2 | — | — | — |
| 1% | +200 ms | 1 | 2/2 · 2/2 | 0.61 → 0.87 | 4049 → 3063 | 0% → 0% |
| 1% | +200 ms | 8 | 0/2 · 0/2 | — | — | — |

Values are medians of completed trials, not estimates for stalled transfers. At 0%, local one/eight-stream goodput rose by 39%/38%, and the +200 ms eight-stream download rose by 35%; one-stream WAN ranges overlap. At 0%, hub CPU per UDP GiB fell by 17–27% across the four cells. At 1%, both arms completed exactly 2/8 matrix trials; the only comparable cell had 0.52–0.70 Mbit/s in A and 0.52–1.22 in Q, with multi-second loaded RTT. In failed runs, netem did drop datagrams, but engine queue, excess qdisc, and kernel UDP receive-buffer drop counters did not move. A one-stream transfer at 0 added RTT stalled after setup; the next test on that same carrier did not connect. At +200 ms the eight-stream trials reached eight data connections before timing out. The stall is not evidence of the hub queue overflow that Q targets, nor attributable to Q alone. The exact µTP recovery state was not profiled in these runs.

The sender probe checked that a ready prefix is flushed while the next container's encryption mutex is held, and the remaining sends keep FIFO and the four-packet cap through the stop sentinel. It failed on the stock sender (`[1]` instead of `[1 2]`) and passed 20 times under `-race` on Q. This proves those invariants, not general carrier reliability.


For the independent 1% control at 0 added RTT and eight streams, each attempt started a fresh carrier (`A Q Q A`). A finished **2/2** at **3.84 and 5.42 Mbit/s**, p95 **3663 and 4446 ms**. Q finished **0/2**: a timeout after setup with zero measured data, and a results-exchange failure after partial data. Both Q trials reached eight connections; the latter still answered 220/220 overlay pings, so not every failure was a dead tunnel. This small control removes carryover from the preceding single-stream stall. It is a regression signal, not a causal diagnosis or a reliable estimate of failure rate.

**Decision: do not adopt Q yet.** It passes clean-path and sender-invariant checks but not the lossy completion gate. Do not add a dependency fork or an automatic selector on this evidence. The next discriminator is an isolated, repeatable µTP loss-recovery trace around the first stall: congestion window, unacked sequence, RTO and retransmissions on both arms. Keep the Extended-frame batching experiment out of this work.

[Matrix](measurements.tsv) · [Medians/ranges](comparison.tsv) · [UDP/CPU counters](submissions.tsv) · [Fresh-tunnel control](fresh.tsv) · [Failure classifications](failures.tsv) · [Raw traces, overlays, test and runners](raw.tar.gz). All five tables reproduce byte for byte. Run `bash run.sh <repository-root>` and then `bash fresh.sh` from the extracted archive to repeat the measurements.