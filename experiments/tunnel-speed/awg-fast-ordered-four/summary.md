# Ordered AWG carriers: release v0.14.4 vs guarded fast-activation build

Real Linux hub + standalone client and kernel TUN. Binaries match the SHA-256 of the [native experiment](../awg-fast-activation/results.tsv). No balancer, synthetic tunnel implementation, Internet routing, or payload inspection. Each run starts a fresh peer and a single selected AWG carrier. Eight CUBIC TCP flows run for 44 s, changing the underlay after 5 s without reopening sockets. Each number below is receiver TCP_INFO goodput (Mbit/s); no timeout zeros substituted. The whole-post window covers 38 complete seconds starting at the first 1 s sample after the change. Counts are from the hub: in downloads it is the parity sender; in uploads it is the receiver. FEC grants and first observed grant times are sampled at 4 Hz. Zero samples on the hub in a download does not describe client sampling.

## Underlay loss: +100 ms one way and independent 1% loss on both transit directions

| AWG carrier | Direction | Seed | Whole post: release → candidate | First 10 s: release → candidate | Late 10 s: release → candidate | Zero-delivery seconds: release → candidate |
|---|---|---:|---:|---:|---:|---:|
| Extended/µTP | wan | 47011 | 1.208 → 1.371 | 4.485 → 5.209 | 0.0354 → 0 | 11 → 34 |
| Extended/µTP | wan | 47032 | 1.613 → 3.18 | 6.037 → 12.09 | 0.0314 → 0 | 15 → 35 |
| Extended/µTP | wan-up | 48021 | 1.563 → 1.581 | 5.84 → 5.934 | 0.0383 → 0.0246 | 13 → 20 |
| Request/µTP | wan | 47011 | 1.091 → 0.7309 | 4.088 → 2.777 | 0.0216 → 0 | 5 → 35 |
| Request/µTP | wan | 47032 | 1.87 → 1.035 | 7.058 → 3.875 | 0.0138 → 0.0206 | 7 → 8 |
| Request/µTP | wan-up | 48021 | 1.013 → 1.025 | 3.781 → 3.836 | 0.0246 → 0.0236 | 6 → 8 |
| Extended/TCP | wan | 47011 | 28.88 → 28.82 | 2.552 → 1.996 | 55.55 → 60.18 | 0 → 0 |
| Extended/TCP | wan-up | 48021 | 33.66 → 30.35 | 1.603 → 1.663 | 61.85 → 59.5 | 0 → 0 |
| Request/TCP | wan | 47011 | 20.08 → 21.39 | 1.625 → 1.342 | 40.25 → 43.05 | 0 → 0 |
| Request/TCP | wan-up | 48021 | 22.44 → 30.18 | 1.297 → 1.657 | 47.29 → 50.9 | 0 → 0 |

Late 10 s starts at the first sample ≥24 s after the switch. Different seeds are independent realizations; do not derive population-wide percentage speedups from single cells.

## LAN: no added propagation delay or random loss

TBF 100 Mbit/s each direction, 1.5 MB FIFO. Cross-traffic runs for 36 s: `fifo-share` = 55 Mbit/s download; `fifo` = 103 Mbit/s download; `fifo-duplex-up` = 103 Mbit/s both ways with overlay upload. The table uses receiver goodput and underlay RTT over the loaded portion, not after the competitor stops. A 55 Mbit/s competitor does not necessarily fill a queue: use RTT and actual drops, not just the label.

| Mode | LAN load | Late 10 s: release → candidate | Underlay p95 ms: release → candidate | Both qdisc drops: release → candidate | Hub grants TX+RX: release → candidate |
|---|---|---:|---:|---:|---:|
| Extended/µTP | fifo-share | 42.02 → 38.47 | 118 → 118 | 6652 → 5415 | 0 → 0 |
| Extended/µTP | fifo-duplex-up | 0 → 0 | 240 → 240 | 52791 → 51598 | 0 → 0 |
| Request/µTP | fifo-share | 24.77 → 26.18 | 106 → 105 | 7964 → 11457 | 0 → 0 |
| Request/µTP | fifo-duplex-up | 0 → 0.0029 | 240 → 240 | 51201 → 55214 | 0 → 0 |
| Extended/TCP | fifo-share | 30.8 → 39.94 | 6.66 → 5.68 | 0 → 0 | 0 → 0 |
| Extended/TCP | fifo | 0 → 0.054 | 120 → 120 | 23625 → 23845 | 0 → 0 |
| Extended/TCP | fifo-duplex-up | 0.0128 → 0.0196 | 240 → 240 | 46440 → 46479 | 0 → 0 |
| Request/TCP | fifo-share | 27.89 → 27.05 | 5.15 → 7.21 | 0 → 0 | 0 → 0 |
| Request/TCP | fifo | 0.0314 → 0.0295 | 120 → 120 | 23714 → 23819 | 0 → 0 |
| Request/TCP | fifo-duplex-up | 0.0246 → 0 | 240 → 240 | 46854 → 46998 | 0 → 0 |

## Positive control: missing originals inside the AWG layer

Separate diagnostic condition, not an A/B performance measurement: unchanged candidate client, hub rebuilt with only the input fault-injection overlay; 50 ms per underlay direction, no configured random underlay drops. The hub skips every 100th eligible original just after AWG decryption and before either the receive meter or decoder, leaving control and parity intact. Eight paced upload flows (16 KiB burst, 40 ms sleep per flow); samples below are observed at the hub from the beginning of traffic, not from a live underlay switch.

| AWG carrier | First RX grant, s | RX grants total | Reconstructed originals | Sampled IDs / missing |
|---|---:|---:|---:|---:|
| Extended/TCP | 0.955 | 1 | 1161 | 34679 / 366 |
| Extended/µTP | 0.721 | 1 | 1175 | 34604 / 342 |
| Request/TCP | 0.943 | 1 | 1166 | 34610 / 341 |
| Request/µTP | 0.701 | 1 | 1170 | 34658 / 347 |

[All matched trials](measurements.tsv) · [Inner loss controls](inner-control.tsv) · [Allowlisted raw counters, receiver series, hashes and harness](raw.tar.gz). In the extracted harness, `run.sh` and `targeted.sh` expect binaries in `release/` and `candidate/`; the diagnostic `positive.sh` additionally expects the `inner/` hub built with `inner/inject.patch` and the unchanged candidate client.
