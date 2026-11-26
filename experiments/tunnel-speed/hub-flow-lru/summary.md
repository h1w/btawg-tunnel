# Native hub flow scaling and UDP/GSO

Local real-TUN Linux namespaces; no balancer, no added delay or shaping; eight CUBIC TCP streams, 2 s warmup, 8 s nominal measurement. CPU affinity: hub 0–3, client 4–5, iperf3 6–7. Stock offloads at 0%; selective UDP segmentation changes at 1% with bidirectional calibration. The immediate baseline already had the 16-packet reads, pooled router packets, and batch timestamp. This does not overwrite `before/`.

## Change shipped in source

`flowTable` uses indexed intrusive LRU nodes instead of scanning and moving a slice on every flow hit. Lookup, expiry, and recency updates are performed under one lock. It preserves the flow cap, expiry since last authenticated pin, and least-recently-used eviction. At capacity, the evicted node is reused. Fragment expiry/eviction semantics were not changed. `go test -race ./internal/server` passed, including a test that a reverse lookup refreshes eviction order without extending the authenticated pin lifetime.

| Stored flows | Baseline median ns/lookup | Indexed LRU median ns/lookup |
|---:|---:|---:|
| 8 | 160.1 | 61.8 |
| 128 | 280.0 | 63.3 |
| 1024 | 1947.0 | 73.2 |
| 8192 | 14162.0 | 93.0 |

Both versions allocated 0 on a warmed lookup. Three cyclic single-core microbenchmark samples per row. Warmed up the real hub with 8192 distinct acknowledged UDP flows from the same authenticated client before the separate seeded cases. UDP probes end before benchmarking; they are live table entries, not 8192 independent clients or concurrent bulk transfers. One paired measurement per mode:

| Download test at 0% | Mbit/s before → after | Loaded ICMP p95, ms before → after |
|---|---:|---:|
| WG, 8192 extra live entries | 87.83 → 1295.09 | 504 → 4.36 |
| AWG, 8192 extra live entries | 76.68 → 1257.50 | 632 → 5.21 |
| WG, ordinary, median of two | 1230.91 → 1331.66 | 5.26 → 5.37 |
| AWG, ordinary, median of two | 1223.26 → 1282.50 | 4.49 → 4.84 |

The ordinary WG download pair reversed direction on the second run. At 1% loss ordinary download was WG 402.42 → 419.17 and AWG 381.95 → 394.41 Mbit/s (one pair, no robust gain claim). Upload and p95 raw readings are in `measurements.tsv`.

## UDP/crypto findings and rejected prototype

The instrumented Linux UDP bind, per mode at clean download, saw ~14.8 datagrams per GSO message and ~1.001 messages per `WriteBatch` call in both modes. ~99.9% of datagrams were within GSO messages. `WriteBatch` is the `sendmmsg` interface here; these are call-site counts, not a kernel trace of retries. No send errors were returned. Average encrypted datagram bytes, including small control messages: WG 1312, AWG 1344. Thus AWG does not fail to aggregate its UDP data in this scenario. Conservative next candidate: reduce redundant work in common header parsing or input/output staging and retest indexed paths; AWG header/padding code is in the pinned dependency, not an excuse to weaken security or fork it without measured benefit.

Hub CPU profiles put `sendmmsg` at ~15.9% (WG) and ~12.9% (AWG), the encryption worker at 12.5% and 18.3%, and ChaCha20-Poly1305 Seal alone at 10.1% and 9.6%. For AWG, header protection cipher construction was ~3.3% and `crypto/rand.Read` ~1.6%; worker percentages include subordinate calls. These are CPU shares, not percentages of throughput lost. Protected AWG configuration was unchanged.

Copy-free scatter/gather GSO was measured only on scratch binaries. It preserved compact GSO grouping but reduced download WG from 1339–1345 to 1264 Mbit/s and AWG from 1196–1220 to 1130 Mbit/s, with worse loaded p95 in the same sandwich. No dependency fork, algorithm, queue size, or offload defaults were changed for production.

32 valid measurements, 15 groups, plus microbenchmarks and CPU profiles in `raw.tar.gz`.
