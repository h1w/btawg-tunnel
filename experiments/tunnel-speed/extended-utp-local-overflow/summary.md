# Extended/µTP: local drops under equal load and TCP saturation

Follow-up to the [split batching matrix](../extended-utp-split-batching/summary.md). **A** unchanged sender, **Q** ready-only AWG collection, **B** Q plus Extended frame batching. Same diagnostic router and UDP counters in all binaries; original client. Linux real TUN, 0 ms added delay, 0% configured loss, stock offloads, eight download streams, 2 s omitted + 6 s measured. Arm order `A Q B B Q A`. UDP rows use 1200-byte application datagrams and equal per-stream iperf pacing instead of adaptive TCP; they do not replicate TCP's burst pattern. Rates below are receiver goodput; medians of two and raw drop ranges. No host or product settings changed.

| Test | Arm | Receiver Mbit/s | Hub engine drops [range] | Drops / TUN packets | Loaded p95 ms |
|---|---|---:|---:|---:|---:|
| TCP, uncapped | A | 379.2 | 501 [359–643] | 0.160% | 33.90 |
| TCP, uncapped | Q | 534.7 | 486 [439–532] | 0.112% | 24.40 |
| TCP, uncapped | B | 665.3 | 5,247 [4,780–5,714] | 0.949% | 6.71 |
| UDP, 280 Mbit/s offered | A | 280.0 | 0 [0–0] | 0% | 1.99 |
| UDP, 280 Mbit/s offered | Q | 280.0 | 0 [0–0] | 0% | 1.20 |
| UDP, 280 Mbit/s offered | B | 280.0 | 0 [0–0] | 0% | 1.32 |
| UDP, 480 Mbit/s offered | A | 420.6 | 49,152 [31,792–66,512] | 12.283% | 33.60 |
| UDP, 480 Mbit/s offered | Q | 480.0 | 19 [0–38] | 0.005% | 3.77 |
| UDP, 480 Mbit/s offered | B | 479.0 | 590 [0–1,180] | 0.147% | 2.11 |

The UDP sender rates matched across arms to within 0.02 Mbit/s from the target. Both candidates substantially improved delivery at the same 480 Mbit/s; Q's earlier uncapped TCP drop-rate increase versus A did not reproduce in this instrumented pair. B's uncapped TCP overflow did reproduce. This rules out a blanket claim that either optimization loses more packets at the same offered rate. It does not prove equality for TCP at the same mean rate: congestion feedback and GSO differ from paced UDP.

## What fills the queue

For every shared TUN read the diagnostic hub records read size, remaining destination queue slots just before the first eligible enqueue, packets considered and packets rejected. The isolated benchmark has one destination engine. Counters cover warmup and transfer; the drop probe exactly matched the engine counter in all trials. No segmentation errors. About **74–77%** of uncapped TCP packets came in reads of **nine or more**, in all three arms. There is no evidence that B raises the fraction of large reads. Rather, B much more often started a read with **eight or fewer** free slots: **1.41–1.76%** of reads, versus **0.17–0.21%** for Q and **0.37–0.40%** for A. **92–94%** of B drops occurred on those near-full reads and **67–68%** on reads of nine or more; the categories overlap. At paced UDP 480, TUN read size was effectively one. A still overflowed and saw 556–580 namespace UDP receive-buffer errors, while Q/B had none. Large reads amplify loss but are neither necessary nor sufficient for it.

## Same-binary burst-size control

Separate `B B8 B8 B` series: same B hub/client binaries and uncapped eight-stream TCP, changing only the hub TUN `gso_max_segs` attribute in the private namespace. B8 capped it at eight; no reads of nine or more were observed. The control had 73–74% of packets in those reads.

| Hub TUN | Receiver Mbit/s | Hub drops [range] | Drops / TUN packets | Loaded p95 ms |
|---|---:|---:|---:|---:|
| Default GSO | 633.2 | 5,305 [5,224–5,385] | 1.018% | 6.98 |
| GSO max 8 segments | 638.1 | 4,203 [3,971–4,434] | 0.801% | 7.50 |

Smaller TUN bursts reduced drops by **21%** at similar throughput; thousands remained and all were on reads that started nearly full. The measured cause is saturation of the 256-slot engine queue under the uncapped B workload, aggravated by bursts, not added wire loss. The measurements do not isolate which downstream scheduling interaction keeps that queue full in B. Do not change the shared GSO default from this control: the previous [mixed-mode test](../carrier-queue-overflow/summary.md) measured a WAN Extended/µTP regression and it affects other modes. Q is the measured better starting point if pursuing fewer local drops without giving up the WAN speed found in the split matrix; keep B on the bench until queue pressure is addressed.

**22/22 trials valid.** Zero qdisc drops, outgoing UDP submission errors or TUN segmentation errors. [Measurements](measurements.tsv) · [Queue buckets](queue-cells.tsv) · [Raw counters and replay scripts](raw.tar.gz). Archive replay reproduced both TSVs byte for byte. Extract and run `python3 local-summary.py queue-cells.tsv local.log gso.log > measurements.tsv`. To rerun: `LAB_ARMS='A Q B B Q A' bash run.sh <repository-root>`, then `bash gso.sh` with the same built binaries.
