# Automatic native SOCKS TCP relay: +200 ms RTT

Linux real TUN, native WG/AWG, one client, selective veth offloads, 100 ms one-way transit delay, 0% or 1% random per-direction loss, 300 Mbit/s downlink cap. The origin was in a separate namespace outside the impaired hop. Client/hub socket snapshots verify BBR on the actual overlay TCP connection; the origin-facing connection is separate and the origin keeps CUBIC. No host-wide TCP settings, qdisc defaults or WireGuard packet format changes. Results include connect/setup time. One observation per cell; do not infer steady-state capacity or a throughput guarantee.

## Matched 64 MiB download, native WG

Both clients use the same hub build. The baseline binary builds the same client tree with the old Linux foreground dial path (overlay NAT and no TCP relay). The integrated client uses the new automatic relay. No DNAT or stand-in relay is present.

| Loss | Old NAT | Integrated hub relay | Loaded p95 RTT old → new | Hub engine queue drops old → new |
|---|---|---|---|---|
| 0% | 59.17 Mbit/s (9.07 s) | 77.68 Mbit/s (6.91 s) | 204 → 211 ms | 3160 → 0 |
| 1% | Deadline at 30 s, 3,325,188 bytes received | 46.36 Mbit/s (11.58 s) | 201 → 202 ms | 0 → 0 |

The deadline is not a completed baseline rate. The clean baseline was also dropping locally in this particular test; it is evidence about the integrated paths, not a controlled estimate of the BBR algorithm alone.

## Native WG/AWG smoke

Both modes completed 8 MiB download and 4 MiB upload at 0% and 1% via real SOCKS CONNECT and the integrated hub relay. At 1%, WG/AWG download was 18.83 / 18.71 Mbit/s and upload 10.04 / 10.76 Mbit/s for these short transfers. No hub-engine queue drops in the eight smoke trials. See [smoke samples](smoke.tsv) and [matched comparison](comparison.tsv); all complete payloads were verified against SHA-256. [Raw data and sources](raw.tar.gz).

At 200 ms, a 300 Mbit/s path needs 7.15 MiB in flight before kernel buffer overhead; at 1 Gbit/s, 23.84 MiB. A flight window of 32 KiB can sustain only about 1.31 Mbit/s at that RTT before other overheads. Split TCP gives us control of the VPN leg's congestion algorithm and window independently of the origin. It does not eliminate propagation delay or recover native WG packet loss for arbitrary transparent TUN traffic. Larger windows plus measured pacing can improve long flows; use connection reuse for latency-sensitive bursts, not artificial priming.

The [final build repeat](final.tsv), with relay identity pinned before request parsing, completed lossy 64 MiB WG download at 44.16 Mbit/s in 12.16 s, with 201 ms loaded p95 and zero hub engine queue drops. A direct TCP connection to the relay port on the WAN interface was refused. Archive replay verifies 12 completed transfers and the one partial timeout; raw socket logs and payload digests are in the archive, not the payloads or lab credentials.
