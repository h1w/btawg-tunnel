# µTP: one allocation per outbound DATA packet

A Linux loopback `BenchmarkThroughput10MB` allocation profile attributed **44.4%** of allocated bytes to `writeOutgoingPacket`, **29.8%** to the ordered receive buffer copy and **16.0%** to out-of-order receive storage. System calls had the largest flat CPU sample share (30.1%). The candidate combines metadata and the retransmission payload for ordinary outbound DATA into one allocation. It preserves payload ownership and the existing retransmit path; controls and oversized packets use the existing allocator. No pool, wire change, queue size change, or congestion policy change.

| Loopback, median of three | Stock | Inline payload |
|---|---:|---:|
| Allocations / 10 MiB transfer | 41,098 | 33,789 |
| Allocated bytes / transfer | 23,026,794 | 23,632,454 |
| Goodput, MB/s | 131.07 | 133.74 |

The candidate cut allocation count **17.8%** but allocated **2.6%** more bytes. Loopback candidate rates ranged **32.57–136.60 MB/s**, stock **122.34–141.21 MB/s**. This does not establish a speedup.

## Real TUN A/B

Current source before the short-write wakeup fix, both endpoints changed together; Extended/µTP and Request/µTP, eight CUBIC flows, 2 s warmup and 5 s receiver interval, calibrated selective offloads, independent underlay loss in each direction, one attempt per arm/cell. Additional UDP control: 35 Mbit/s offered per stream × eight, 1200-byte datagrams, two attempts per arm in ABBA order, no induced loss or delay.

- **0%, local:** the four TCP rates changed by **−2.1% to +0.8%**. The equal-load UDP control delivered **279.1–280.0 Mbit/s** on both builds; offered rates matched 280 Mbit/s to within 0.003. One baseline Extended trial dropped 175 packets at the hub; the other seven trials had no endpoint queue drops. Both builds met the offered rate in the other Extended pair. No repeatable equal-load benefit was established.
- **0%, +200 ms:** Extended upload dropped from **185.90 to 144.41 Mbit/s**, client drops **1,776 → 2,487**. Request download moved in the other direction, **163.56 → 166.21 Mbit/s**, hub drops **3,850 → 500**, but Request upload hub/client drops were **0/3,457 → 0/10,661**.
- **1%, local:** Extended down/up improved **28.92/26.59 → 45.54/53.26 Mbit/s**, while Request declined **53.33/17.68 → 39.45/14.29 Mbit/s**. Single measurements do not establish a per-mode change.
- **1%, +200 ms:** all eight cells stayed below **0.65 Mbit/s** and recorded zero endpoint queue drops. This did not change the lossy-WAN constraint.

**Decision:** keep the smaller metadata allocation count in the lab. It did not establish throughput improvement or clean-WAN non-regression. All **40/40** attempts completed. [Measurements](measurements.tsv) · [Raw counters, profiles, source overlay and runners](raw.tar.gz). Extract and run `python3 -I matrix/summarize.py inline-matrix.log offered-280.log` to reproduce the TSV byte for byte. `bash replay.sh <matching-checkout>` rebuilds and reruns both series.
