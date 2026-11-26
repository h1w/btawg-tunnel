# Native queue synchronization at equal offered load

Real Linux TUN, managed client, WG and AWG native, 0% induced loss and no added RTT, selective offloads, eight UDP streams offering 100 Mbit/s each. Both endpoints have an identical lab-only loopback pprof listener. CPU profiles sample five seconds during the load. Mutex profiles are cumulative since process start; they are attribution samples, not elapsed trial time. All 12 intervals completed.

In the initial four down/up runs, the measured sender offered **799.95–800.00 Mbit/s**. Receiver delivered **797.87–800.00 Mbit/s**. One AWG downlink had zero queue drops; the other three recorded 577–2,369 full-queue rejects. `engineTun.tryEnqueue` used **1.36–1.61%** of hub downlink CPU inclusive; `tunPath.enqueue` used **0.24–0.61%** of client upload CPU inclusive. No enqueue close-mutex site appeared in the sampled mutex profiles; crypto container synchronization dominated. This does not establish zero contention for other clients or workloads. It does not justify replacing the queue or weakening lifetime synchronization.

## Isolating route clock overhead

Stock versus a lab-only router overlay reconstructing the two dispatch/dequeue timestamps from one monotonic origin. No admission, buffer, routing, encryption, or timer changes. The client is byte-identical. Two pairs in A/B/B/A order, downlink, 2 s warmup and 7 s nominal load. All eight offered rates were within 0.01 Mbit/s of 800; no excess transit or UDP receive-buffer drops.

| Mode | Hub clock samples stock → overlay, ms | Total hub CPU stock → overlay, ms | Receiver stock → overlay, Mbit/s |
|---|---:|---:|---:|
| WG | 1,265 → 630 | 10,370 → 10,440 | 798.62 → 799.15 |
| AWG | 1,390 → 735 | 10,655 → 10,515 | 788.46 → 781.46 |

Values are medians of two stock and two overlay runs, with CPU clock samples restricted to stacks under the dispatcher and engine read. Lower clock samples did not yield a shared CPU or goodput win. The second AWG pair was noisy in both arms: loaded p95 **149 → 182 ms**, queue drops **9,689 → 14,245**. The first AWG pair had zero router queue drops. Do not adopt this clock substitution or replace queue synchronization on these results.

[Measurements](measurements.tsv) · [CPU and offered-rate evidence](cpu.tsv) · [Raw pprof, counters, overlays, source and replay](raw.tar.gz). Extract and run `python3 -I matrix/summarize.py profile.log mono.log >measurements.tsv` and `python3 -I cpu-summary.py <measurements.tsv >cpu.tsv`; both tables reproduced byte for byte. `bash replay.sh <matching-checkout>` reruns the benchmark.
