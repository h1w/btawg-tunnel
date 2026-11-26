# Native WG/AWG + AWG carrier replay

Same baseline runner: no load balancer, no rate shaping, Linux TUN, selective mode scope only. Original offloads disabled on all four veth interfaces, independent 1% per direction, one-way delay 0/60/100 ms. `iperf3 -P 1/8`, 2 s warm-up + 8 s nominal runtime, directions tested separately. Current stock binaries; AWG header protection and random trailers confirmed in each lab import. No CPU pinning. Four TLS-only modes not re-run.

before: 67/72 positive final readings; 5 rescued by a repeat; after: 69/72 positive final readings; 6 rescued by a repeat.

Cells are **before → after**, Mbit/s, download / upload. †: successful fresh-tunnel retry; 0*: zero-byte stall; —: failed or unavailable.

## 8 TCP streams

| Mode | +0 ms RTT | +120 ms RTT | +200 ms RTT |
|---|---:|---:|---:|
| socks5-awg-ext-tcp | 206.04 → 187.57 / 397.12 → 214.21 | 26.48 → 28.71 / 18.45 → 18.71 | 15.20 → 15.60 / 7.63 → 18.76 |
| socks5-awg-req-tcp | 214.44 → 126.37 / 199.03 → 185.16 | 15.34 → 22.81† / 19.05 → 18.16† | 14.55† → 12.85 / 9.06 → 10.24 |
| socks5-awg-ext-utp | 2.75 → 5.77 / 1.39 → 0.84 | — → 0.66† / 0.10 → 0.68† | 0.13 → 0* / — → 0.63 |
| socks5-awg-req-utp | 3.67† → 0.26 / 1.57 → 1.76 | 0.39† → — / — → 0.45 | — → 0* / — → 0.12 |
| socks5-awg-udp | 436.33 → 438.69 / 480.84 → 446.20 | 10.49 → 8.39 / 9.81 → 9.94 | 7.73 → 11.27 / 7.29† → 7.54 |
| socks5-wg-udp | 445.65 → 459.03 / 492.36 → 471.92 | 9.57 → 10.22 / 9.94 → 8.13 | 8.91 → 8.39 / 9.21 → 8.57 |

## 1 TCP streams

| Mode | +0 ms RTT | +120 ms RTT | +200 ms RTT |
|---|---:|---:|---:|
| socks5-awg-ext-tcp | 301.99 → 163.58 / 299.61 → 253.96 | 16.38 → 18.74 / 11.50 → 10.82 | 11.93 → 8.26 / 8.25 → 4.28 |
| socks5-awg-req-tcp | 190.45 → 134.48 / 188.62 → 142.09 | 8.13 → 8.39 / 8.60 → 12.61† | 2.88 → 4.85 / 9.09 → 3.41 |
| socks5-awg-ext-utp | 4.06 → 18.74 / 9.89 → 4.43 | 0.79 → 0.66 / 1.12 → 1.59 | 0.66 → 0.39 / 0.74† → 0.50 |
| socks5-awg-req-utp | 2.36 → 11.67 / 1.31 → 9.70 | 0.79 → 0.66 / 0.88 → 0.53 | 0.66 → 0.52† / 0.41 → 0.61 |
| socks5-awg-udp | 90.83 → 76.15 / 92.40 → 78.84 | 1.18 → 1.18 / 0.90 → 0.90 | 0.92 → 1.31 / 0.89 → 0.89 |
| socks5-wg-udp | 98.57 → 88.87 / 93.31 → 97.64 | 1.44 → 1.18 / 1.01 → 1.29 | 1.31 → 0.52 / 0.90 → 1.28 |

## Loaded RTT (p50), after, ms

| Туннель | +0 мс RTT | +120 мс RTT | +200 мс RTT |
|---|---:|---:|---:|
| socks5-awg-ext-tcp | 25.2 / 302.0 | 341.0 / 420.0 | 552.0 / 390.0 |
| socks5-awg-ext-utp | 1189.0 / — | 3503.0 / 3204.0 | 4248.0 / 2543.0 |
| socks5-awg-req-tcp | 110.0 / 352.0 | 356.0 / 357.0 | 609.0 / 584.0 |
| socks5-awg-req-utp | 5223.0 / 1304.0 | — / 4124.0 | 3581.0 / 3974.5 |
| socks5-awg-udp | 0.9 / 0.7 | 120.0 / 120.0 | 200.0 / 200.0 |
| socks5-wg-udp | 0.7 / 0.7 | 120.0 / 120.0 | 200.0 / 200.0 |
