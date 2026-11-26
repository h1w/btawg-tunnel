# WAN recovery: FEC, TCP flight size, and SOCKS startup

Current hub/client binaries, real Linux TUN, MTU 1280, selective veth offloads held fixed, no rate cap, 60 ms delay each way, independent seeded per-datagram loss on transit. Hub/client/load CPU affinity 0–3/4–5/6–7; lab FEC processes share cores 3 and 5. All measurements are downlink. Separate one-shot controls, not confidence intervals. No product source or host sysctl change.

## Independent FEC experiment

A pair of namespace-only UDP shims forwards opaque, still-encrypted WG/AWG datagrams and optionally adds one or two systematic parity shards per group. Data is forwarded immediately; parity can recover an absent datagram without a WAN round trip. The 128-group receive ring holds bounded state. No retry or full-block wait for data; an incomplete final group has no parity. This is a lab protocol, not interoperable with native clients without the peer shim. Do not deploy it: there is no negotiated protection, pacing, burst interleaving or adaptive parity. All reconstructed packets still pass original WG/AWG authentication. A deterministic check covers single/two-erasure recovery, loss of the first parity shard, byte-for-byte payload restoration and duplicate recovery suppression.

WG, one CUBIC flow, 1% per direction, 4 s omit + 8 s measurement:

| Configuration | Receiver Mbit/s | Loaded p95 RTT, ms | Extra hub shim UDP payload bytes |
|---|---:|---:|---:|
| Native | 1.05 | 121 | — |
| Shim without parity | 1.05 | 121 | 0% |
| 8 data + 1 parity | 17.30 | 121 | 13.4% |
| 16 data + 2 parity | 43.25 | 122 | 13.4% |
| 12 data + 2 parity | 66.72 | 121 | 17.6% |

Wire overhead column is UDP payload bytes sent by the hub shim, relative to the original encrypted datagrams. The reverse direction costs more **as a percentage of that direction**: approximately 24% (8+1/16+2) or 29% (12+2) because ACK-sized datagrams have more header overhead. These are not wire rates including IP/Ethernet. No receive-buffer drops, fragmentation or local shim send errors in this screen. 12+2 recovered 868 datagrams at the client; CUBIC median sender cwnd rose from 16 to 1007 KiB. The single-flow goodput is still modest.

AWG confirmation at the same loss and RTT (5 s omit + 9 s measurement): shim without parity 1.05 Mbit/s, 16+2 53.01 Mbit/s, 12+2 98.33 Mbit/s. Loaded p95 121/123/122 ms respectively, and no UDP receive-buffer errors, fragmentation or local shim send errors. AWG 12+2 hub overhead was 17.55%; 1296 downlink datagrams were recovered at its client across the session.

## Independent TCP sender buffer experiment

WG, one BBR flow, 6 s omit + 14 s measurement. Changed only `tcp_wmem`'s maximum (4 to 32 MiB) in private hub/client network namespaces; min/default and receive autotune unchanged. For scale, 500 Mbit/s at 120 ms requires 7.15 MiB of application data in flight, and 1 Gbit/s takes 14.31 MiB, before TCP skb-memory cost. Results:

| Loss | Path | Send autotune cap | Mbit/s | Loaded p95 RTT, ms | UDP receive-buffer errors, both endpoints |
|---|---|---:|---:|---:|---:|
| 1% | Native | 4 MiB | 94.37 | 121 | 0 |
| 1% | 12+2 FEC shim | 4 MiB | 160.06 | 121 | 0 |
| 1% | Native | 32 MiB | 369.10 | 124 | 1587 |
| 1% | 12+2 FEC shim | 32 MiB | 365.28 | 194 | 28,453 |
| 0% | Native | 32 MiB | 652.44 | 227 | 0 |

With the default cap the late native sender snapshot spent 34.9% of its busy time send-buffer limited. It stopped reporting that limit at 32 MiB and the late snapshot reported receive-window limiting instead. FEC plus the higher limit did not beat native BBR. It moved the bottleneck to local socket drops/queueing, with 13,921 TCP retransmits versus 5487 native (FEC without tuning had 19). More buffering is not a default recommendation: validate a pacing/latency guard and memory per active flow on the actual relay first.

Separate 0%-induced-loss matched shim A/B (WG, BBR, 32 MiB, 5 s omit + 9 s measurement): shim without parity 401.72 Mbit/s versus 12+2 366.77 Mbit/s (−8.7%), p95 186 versus 188 ms. Both had local UDP receive-buffer errors (27,778 / 17,467); this is a negative result for always-on parity in this lab, not a clean physical-link capacity comparison. Do not compare this pair against the differently timed native no-shim row above.

## Completed SOCKS files: startup versus send autotune

Real client SOCKS5 through WG, fixed-destination lab BBR relay, 1% in both directions, +120 ms RTT. Payload sizes differ only across the startup comparison; the buffer comparison uses the identical 128 MiB file. Wall time includes connection setup. All three receiver SHA-256 checks and relay BBR socket samples verified.

| File | Send autotune cap | Completion time, s | Mbit/s | Loaded p95, ms |
|---|---:|---:|---:|---:|
| 4 MiB | 4 MiB | 2.05 | 16.35 | 121 |
| 128 MiB | 4 MiB | 13.12 | 81.83 | 121 |
| 128 MiB | 32 MiB | 5.21 | 205.94 | 124 |

One trial per cell. Startup dominates the small-file result; the larger buffer improved equal-payload completion time 2.52×. `ss` reported 22.4% send-buffer-limited busy time late in the default long transfer. The tuned long transfer reported 36.1% receiver-window-limited late in the trace; do not interpret a drop in `sndbuf_limited` alone as proof that the path is saturated.

## Decision

- **First candidate for managed SOCKS:** size/autotune the relay's VPN-facing buffers for WAN BDP, scope it to those sockets or a dedicated service namespace, and add pacing and memory limits before enabling it. Test measured 1% random loss and a real rate-limited bottleneck, multiple concurrent clients, 0% and 1% at both 120 and 200 ms, both directions. Do not blindly apply the experiment's namespace sysctl to the production host. The existing socket caps and the Windows path need independent verification.
- **Small files:** evaluate a pre-established, authenticated WAN data path, with per-flow backpressure. Reusing a control connection alone does not supply a warm congestion window to a new TCP data flow. A steady-state throughput figure does not promise short-transfer goodput; report time to first byte and complete-file latency separately. Connection reuse and pacing are not measured here.
- **Transparent TUN with an uncontrolled CUBIC sender:** parity remains a candidate, not a rollout. Control reverse-direction overhead, keep clean bypass, measure iid and correlated loss under a real rate cap, and show that loss-recovery overhead does not cause local drops. With sender BBR and adequate buffers, the experimental FEC stacking was worse on latency without a throughput gain.


## Evidence

[Native TCP/FEC measurements](native.tsv), [verified SOCKS files](socks.tsv), [raw counters and lab source](raw.tar.gz), [binary digests](build.sha256), [environment](environment.txt). Replay from the extracted archive:

```sh
cc -std=gnu17 -Wall -Wextra -Werror -O3 -o fec-test fec-test.c
./fec-test
python3 wan-verify.py run.LSB3bkPf:5:60 run.gh64FBoI:2:60 run.FwPA0qh2:3:60 run.DPdKuVLo:3:60 run.JeMEXKj7:2:60
S=$(awk '$2=="short.bin" {print $1}' payload.sha256)
L=$(awk '$2=="long.bin" {print $1}' payload.sha256)
python3 verify.py "socks:run.GFOTJHQu:1:$S,4194304"
python3 verify.py "socks:run.z9yRfkuJ:1:$L,134217728"
python3 verify.py "socks:run.Ag1iiYpo:1:$L,134217728"
```

15 valid native trials and 3 complete SOCKS trials. Archive excludes lab credentials and test payloads. The host's `tcp_wmem` maximum remained 4 MiB.
