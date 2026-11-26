# Native AWG: build recovery groups from transmitted datagrams

Linux real TUN, native AWG UDP, unchanged inner CUBIC, no SOCKS or BitTorrent. Relative to the [bounded-backlog candidate](../native-awg-bounded-backlog/summary.md), assign original group/slot numbers at UDP submission and advance the encoder only after successful submission. Local scheduler drops therefore neither consume a group slot nor appear as a gap in receiver reports. Both paced arms share this change; `cap` disables repair. On both arms the original queue keeps the same 12 ms expiry; repair keeps its separate eight-slot, 12 ms reservation and shares total wire tokens. This is an unauthenticated, fixed-endpoint lab, not a product protocol.

The deterministic wire check drops an expired original and expects the fresh one in slot zero of group one. The old preassignment path fails that assertion; the send-order path sends only the fresh original with the expected sequence and payload. The replay rebuilds and exercises this check and the preexisting codec and policy checks.

## 1% independent loss and +200 ms RTT, unshaped

| Seed | Pass-through, Mbit/s | Same 80 Mbit/s budget, no repair | Same budget, send-order repair | Overlay p95, all arms | Repair sent / recovered |
|---:|---:|---:|---:|---:|---:|
| 101 | 5.943 | 5.599 | 9.785 | 202 ms | 1,141 / 95 |
| 211 | 5.958 | 5.186 | 9.251 | 202 ms | 932 / 82 |

The candidate clears the solo gate in both pairs: 1.55–1.65× pass-through and 1.75–1.78× the equally paced baseline. No original expired locally. The candidate still dropped 91 and 191 originals on admission; the reported recovered counts exclude the local scheduler's dropped originals by construction. Neither seed recorded endpoint UDP receive errors or hub engine drops.

## Shared 10 Mbit/s FIFO, 2 Mbit/s AWG budget

| Loss / seed | AWG : direct flows | AWG, cap only → repair, Mbit/s | Direct TCP, cap only → repair, Mbit/s | Overlay p95 cap only → repair, ms |
|---|---|---:|---:|---:|
| 0% / 101 | 1:1 | 1.376 → 1.033 | 7.733 → 8.091 | 224 → 223 |
| 0% / 101 | 8:1 | 1.578 → 1.492 | 7.483 → 7.653 | 229 → 225 |
| 1% / 101 | 1:1 | 0.427 → 0.569 | 0.763 → 0.605 | 202 → 204 |
| 1% / 101 | 8:1 | 1.435 → 1.319 | 0.590 → 0.599 | 212 → 212 |
| 1% / 211 | 1:1 | 0.412 → 0.569 | 0.809 → 0.578 | 202 → 202 |
| 1% / 211 | 8:1 | 1.472 → 1.355 | 0.507 → 0.893 | 211 → 211 |

None of the four shared lossy pairs passed both gates. At 1:1 the client recovered zero datagrams in both pairs; the throughput difference does not demonstrate a repair benefit. At 8:1 repair recovered 3 and 9 datagrams, but reduced AWG goodput about 8% versus the same cap without repair. Clean 1:1 also failed AWG retention. Do not enable this fixed-rate lab variant in production.

[27 receiver windows](measurements.tsv) · [Paired gates](comparison.tsv) · [Raw data, source and replay](raw.tar.gz). Both tables reproduced byte for byte after extraction with `python3 -I reduce.py solo.log shared.log > measurements.tsv` and `python3 -I compare.py measurements.tsv > comparison.tsv`. `bash replay.sh <matching-checkout>` runs the unshaped trials; `LAB_MATRIX=shared LAB_REUSE_BINARIES=1 bash replay.sh <matching-checkout>` runs the shared-link trials.
