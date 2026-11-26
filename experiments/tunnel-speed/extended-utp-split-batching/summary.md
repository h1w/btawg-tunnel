# Extended/µTP: separating AWG queue drain and frame batching

The [combined experiment](../extended-utp-ready-udp/summary.md) changed both layers at once. This series rebuilds the exact archived overlays as four arms: **A** unchanged data path, **Q** only nonblocking collection of up to four encrypted AWG packets for the same peer, **F** only batch submission of Extended frames already present in one TLS `Write`, **B** both. Identical hub UDP counters and byte-identical clients in every arm. No timer, queue capacity, protocol or host settings changed. No production source changes.

Real Linux TUN, Extended/µTP with the existing protected-record negotiation, stock offloads, 0% injected loss, no rate ceiling. CUBIC download, one and eight streams, 2 s omitted + 6 s nominal, 0 or 100 ms one-way delay. Run order `A Q F B B F Q A`, two observations per cell. Table entries are medians of two. Drops per hub TUN packet normalize the hub engine counter by hub TUN qdisc packet count from the same window; this is not end-to-end loss. Submit counts are application calls including single, batch and GSO writes, normalized by hub UDP payload MiB. CPU and submission ratios use steady samples; drop counts include warmup.

| Added RTT / streams | Arm | Download Mbit/s | Loaded p95 ms | Engine drops | Drops / TUN | UDP submits / MiB | Hub CPU s / UDP GiB |
|---|---|---:|---:|---:|---:|---:|---:|
| 0 ms / 1 | A | 365.7 | 35.05 | 98 | 0.033% | 683 | 41.4 |
| 0 ms / 1 | Q | 512.1 | 8.34 | 285 | 0.068% | 558 | 32.2 |
| 0 ms / 1 | F | 365.7 | 36.45 | 77 | 0.026% | 683 | 41.6 |
| 0 ms / 1 | B | 633.3 | 5.01 | 343 | 0.067% | 192 | 26.3 |
| 0 ms / 8 | A | 355.9 | 36.15 | 197 | 0.068% | 683 | 44.5 |
| 0 ms / 8 | Q | 495.7 | 27.40 | 769 | 0.189% | 558 | 35.2 |
| 0 ms / 8 | F | 355.7 | 36.70 | 127 | 0.043% | 683 | 44.5 |
| 0 ms / 8 | B | 644.5 | 7.57 | 4,645 | 0.875% | 190 | 27.2 |
| +200 ms / 1 | A | 66.0 | 201.5 | 2,756 | 5.324% | 685 | 45.4 |
| +200 ms / 1 | Q | 72.4 | 202.0 | 1,930 | 3.543% | 580 | 42.2 |
| +200 ms / 1 | F | 60.8 | 202.5 | 2,895 | 6.161% | 689 | 55.2 |
| +200 ms / 1 | B | 48.7 | 201.5 | 3,736 | 8.477% | 217 | 35.2 |
| +200 ms / 8 | A | 202.6 | 272.0 | 6,862 | 4.345% | 686 | 61.5 |
| +200 ms / 8 | Q | 273.3 | 218.5 | 7,881 | 3.764% | 564 | 42.1 |
| +200 ms / 8 | F | 211.6 | 243.5 | 7,943 | 4.774% | 686 | 62.5 |
| +200 ms / 8 | B | 242.9 | 212.0 | 4,997 | 2.682% | 210 | 36.9 |

- **Local eight-stream drops:** Q alone increased the normalized rate (0.068% → 0.189%); F alone did not. Adding F to Q increased drops much further (0.189% → 0.875%, inner TCP retransmits 218 → 3,215). Both B observations exceeded both Q observations for each of those loss metrics. This is a strong interaction, not an F-only regression.
- **WAN speed:** adding F to Q reduced goodput in both measured stream counts. The Q and B ranges were disjoint: 62–82 vs 43–55 Mbit/s for one stream, 268–278 vs 242–244 for eight. The previous combined B-on-eight speed regression versus A did **not** reproduce: B now beat A (189–217 Mbit/s). B still lost 11% vs Q at eight streams, while lowering p95 and normalized drops. One-stream Q vs A ranges overlap.
- **UDP/CPU:** F alone made almost no change in counted submissions. Q alone cut 15–18%; Q+F cut 68–72%. F is only reached for oversized stream writes; Q supplies more AWG payloads already ready for the existing protected-record packer. This explains why the two layers amplify submission savings; it does not establish the mechanism behind the observed drop or speed changes.

**Decision:** Q is the better next candidate when throughput with long RTT is the priority. It is not a general drop fix: local eight-stream drops still rose. Keep B as a throughput/latency/CPU tradeoff experiment, not a default. No claim beyond the tested zero-injected-loss download profiles and two trials per cell.

All 32 transfers completed. No sampled qdisc drops or UDP receive-buffer errors; no counted UDP submission errors. [Per-trial goodput](measurements.tsv) · [Submissions and CPU](submissions.tsv) · [Medians, ranges and contrasts](comparison.tsv) · [Raw traces, overlays and runners](raw.tar.gz). Archive replay reproduced all three tables byte for byte. After extracting, run `python3 compare.py run.log > measurements.tsv`, `python3 submissions.py run.log > submissions.tsv`, then `python3 factorial.py measurements.tsv submissions.tsv`. `bash run.sh <repository-root>` rebuilds and reruns the matrix.
