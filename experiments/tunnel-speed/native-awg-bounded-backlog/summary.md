# Native AWG: original backlog lifetime under a shared repair budget

This is a lab-only AWG UDP/TUN experiment, not a SOCKS or BitTorrent path. It uses the same namespace layout, independently seeded random loss, eight-flow solo downlink and 10 Mbit/s FIFO 1:1/8:1 competition as the [repair reservation trial](../native-awg-repair-reserve/summary.md). All runs are at +200 ms propagation RTT. Sender TCP remains CUBIC. The original and repair queues share the same wire tokens. The only new behavior in both paced arms is that original datagrams now expire after 12 ms locally; the existing repair deadline, loss gate, feedback and wire rates are unchanged. The 48 KB queue remains a memory cap, not a permission to wait 192 ms at 2 Mbit/s. Repair feedback remains unauthenticated and confined to this isolated lab.

The deterministic wire check gives the pacer an expired original followed by a fresh one. The old scheduler sent the expired datagram first; the new scheduler sends only the fresh one and clears both entries. Replay builds and exercises this check, plus the existing codec and activation checks.

## 1% losses, unshaped path

| Seed | Pass-through, Mbit/s | Capped, no repair, Mbit/s | Capped with repair, Mbit/s | Overlay p95 pass-through → repair, ms | Repair sent / recovered |
|---:|---:|---:|---:|---:|---:|
| 101 | 6.215 | 5.064 | 10.826 | 202 → 202 | 1,230 / 251 |
| 211 | 5.683 | 5.112 | 8.307 | 204 → 202 | 843 / 189 |

This repeat delivered 1.74× and 1.46× pass-through; the latter misses the predefined 1.5× speed gate. No originals expired in any of these four paced runs, so neither speed contrast measures a beneficial effect of the new expiry rule on this profile. The paced repair arm dropped 66 and 101 originals on admission; no measured hub engine, kernel UDP buffer or excess netem drops occurred.

## Shared 10 Mbit/s FIFO

| Loss / seed | AWG : direct flows | AWG, no repair → repair, Mbit/s | Direct TCP, no repair → repair, Mbit/s | Overlay p95 plain / no repair / repair, ms |
|---|---|---:|---:|---:|
| 0% / 101 | 1:1 | 1.295 → 1.360 | 7.663 → 7.774 | 223 / 224 / 224 |
| 0% / 101 | 8:1 | 1.539 → 1.536 | 7.523 → 7.600 | 224 / 224 / 227 |
| 1% / 101 | 1:1 | 0.547 → 0.442 | 0.654 → 0.609 | 201 / 206 / 204 |
| 1% / 101 | 8:1 | 1.470 → 1.400 | 0.749 → 0.642 | 204 / 211 / 211 |
| 1% / 211 | 1:1 | 0.681 → 0.463 | 0.628 → 0.750 | 202 / 210 / 202 |
| 1% / 211 | 8:1 | 1.474 → 1.493 | 0.608 → 0.585 | 204 / 210 / 211 |

The maximum wait of a transmitted original was below 12 ms in all trials. On the lossy 8:1 link the repair arm expired 187 and 197 originals and sent zero repair datagrams; loaded overlay p95 was 211 ms. The previous wide-backlog cohort had 390 ms in those cells; this is a cross-cohort latency contrast, not a paired expiry A/B. The same-cap controls above isolate the effect of enabling repair, and none of the four lossy pairs passed both speed and TCP gates. Shortening the queue eliminated the extra waiting but did not make a fixed 2 Mbit/s cap with FEC an AWG fix. No production code or defaults changed.

[27 receiver windows](measurements.tsv) · [Paired gates](comparison.tsv) · [Raw data, source and replay](raw.tar.gz). The tables reproduce byte for byte after extraction with `python3 -I reduce.py solo.log shared.log > measurements.tsv` and `python3 -I compare.py measurements.tsv > comparison.tsv`. `bash replay.sh <matching-checkout>` runs the unshaped trials; `LAB_MATRIX=shared LAB_REUSE_BINARIES=1 bash replay.sh <matching-checkout>` runs the shared-link trials.
