# Hub downlink fast path

Local real-TUN Linux namespaces, no balancer/rate limit or added delay, 8 CUBIC TCP streams, 2 s warmup and 6 s nominal measurement. Hub, client and iperf pinned to separate physical cores. Stock offloads at 0% loss; selective UDP segmentation disabled for independent packet loss at 1%. The original `before/` is not used as the A/B control: `ClockA*` is the build immediately before this change, including the prior 16-packet read and overflow recovery. `ClockB*` adds pooled packet pointers and one timestamp per non-blocking read batch.

| Loss | Mode | Download Mbit/s A → B | Upload Mbit/s A → B | Download p95 RTT ms A → B | Upload p95 RTT ms A → B |
|---|---|---:|---:|---:|---:|
| 0% | WG | 680 → 1205 | 1502 → 1772 | 8.36 → 5.22 | 45.90 → 39.90 |
| 0% | AWG | 701 → 803 | 1548 → 1622 | 7.05 → 7.00 | 50.00 → 49.30 |
| 1% | WG | 167 → 145 | 369 → 179 | 8.59 → 12.00 | 3.36 → 6.31 |
| 1% | AWG | 233 → 353 | 220 → 376 | 5.52 → 2.83 | 5.95 → 2.37 |

The 0% rows are medians of two ABBA measurements. One of the two AWG pairs was slower, so there is no consistent AWG win. The single 1% pair ran at ~2.0–2.4 GHz versus ~3.5–3.7 GHz in the final clean pairs, under unequal total host load. It is not a reliable gain or non-regression claim. Neither clean AWG downlink nor the lossy modes reached a stable gigabit.

- A diagnostic downlink profile attributed 5.41 of 31.89 CPU sample seconds (17%) to `time.Now` called from `engineTun.Read`. The route-lookup timestamp is now acquired after a blocking receive and reused for the rest of the ready batch. Ownership and ACL checks still run on every packet.
- Queue residence sampled every 64 enqueues: p95 was in the 1024–2047 µs bucket for both modes. Hub queue drops were 1.11% for WG and 1.08% for AWG in that separate run, with no errors reported by the UDP send calls.
- The packet pool used to allocate one boxed slice header per recycled packet. A warmed-up regression went from 1 to 0 allocations per packet with typed packet pointers. The allocation-only A/B was mixed and did not demonstrate a shared throughput gain.
- A clean 32/64-packet exploratory sweep had no common advantage over 16. AWG downlink in this sweep was 923/452 Mbit/s versus 1083 with 16; runs had different temperatures. No queue cap, batch size, protocol or host sysctl defaults were changed.

70 valid trials in `measurements.tsv`; `raw.tar.gz` contains per-direction iperf JSON, loaded ping samples, CPU/temperature captures, queue histogram, and pprof output.
