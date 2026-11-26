# Automatic WAN relay send window and pacing

Real Linux SOCKS5 native WG/TUN through the integrated hub relay, one 128 MiB HTTP body, same client and offload settings, CUBIC origin off the impaired leg. +200 ms RTT, 300 Mbit/s downlink and 0% or 1% independent random loss per direction. No host sysctl or qdisc changes. All speed samples are completed payload divided by wall time (startup included). The live hub queue drop API, socket `ss -mti`, transit qdiscs and UDP receive errors are preserved.

## Why a bigger buffer alone was rejected

The stock hub BBR socket reached a 4 MiB send memory cap with 23% send-buffer-limited time (1% loss) and 56% (0%). Receiver-window limitation was below 1% in those samples. Asking Linux for a larger socket buffer was capped to 8 MiB accounted memory on this host, not the requested 16 MiB. One sample per cell:

| Profile | At 0% Mbit/s / p95 ms | At 1% Mbit/s / p95 ms | WAN queue drops at 0% / 1% |
|---|---:|---:|---:|
| Current, Linux BBR | 91.62 / 213 | 46.70 / 201 | 0 / 0 |
| 8 MiB accounted buffer, no additional cap | 111.86 / 240 | 41.80 / 201 | 15,285 / 0 |
| 8 MiB + `TCP_NOTSENT_LOWAT` 256 KiB | 111.69 / 250 | 80.94 / 214 | 13,855 / 2207 |
| Same plus lab TUN `fq` | 132.82 / 253 | 85.87 / 220 | 5422 / 2254 |
| Delivery-rate/RTT paced prototype plus 8 MiB | 123.30 / 206 | 50.86 / 201 | 0 / 0 |

`TCP_NOTSENT_LOWAT` was inherited by accepted sockets but did not constrain the existing splice path to 256 KiB: the traces still sampled nearly 8 MiB notsent. Do not interpret the single fast LOWAT sample as an enforced pacing control. The wider buffer without a cap increased burst pressure in the WAN qdisc and sometimes caused hub user-queue drops. Fixed rate ceilings were deliberately not shipped.

Production uses the wider buffer only on established BBR relay sockets with a measured RTT of 20 ms or more. It leaves existing autotune alone when it already provides an equal or larger ceiling or when the host cap would prevent an increase. It measures acknowledged delivery over at least one path RTT and caps BBR pacing probes, cuts on queued RTT growth and probes upward when RTT is low. It waits for 512 KiB acknowledged before setting a pacing ceiling and clears the cap when the socket is drained. No raw WAN rate constant; the existing hub session cap still bounds the number of enlarged sockets. The receive side remains on kernel autotune. LAN, non-BBR and non-relay sockets are unchanged.

## Final hub build A/B

| Profile | Baseline Mbit/s | Final Mbit/s | Loaded p95 baseline → final |
|---|---:|---:|---:|
| +200 ms, 0% | 91.62 | 125.32 | 213 → 207 ms |
| +200 ms, 1% | 46.70 | 54.18 | 201 → 201 ms |
| +200 ms, 1%, 130 Mbit/s UDP competitor for 6 s | 42.11 | 48.90 | 201 → 201 ms, including competitive phase |
| Local unshaped, no added delay, 0% | 461.88 | 459.30 | 13.5 → 14.9 ms |

No WAN queue overflow, TUN qdisc drops, hub engine queue drops or UDP receive-buffer errors in the displayed baseline/final comparisons. The competing flow delivered 128.73 → 128.64 Mbit/s. Socket samples confirm unchanged autotune (4 MiB) and no extra pacing ceiling on the LAN control, and varying `SO_MAX_PACING_RATE` on the WAN path. This is one sample per cell, not a universal speed/latency guarantee. The final code leaves host TCP settings untouched (verified `tcp_wmem` maximum: 4 MiB).

[Measurements](measurements.tsv), [binary digests](build.sha256), [raw archive and lab sources](raw.tar.gz). From the extracted archive: `mapfile -t profiles < profiles.txt; python3 verify.py "${profiles[@]}"`. Replay verified SHA-256 and socket/queue conditions for all 17 completed transfers. The files `tcp_linux.go` and `tcp_egress.go` in the archive are lab-only overlay variants, not the final production implementation.
