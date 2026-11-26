# Native AWG: separating tunnel throughput from TCP loss response

Linux real TUN, AWG directly over UDP (`socks5-awg-udp`), no SOCKS CONNECT, BitTorrent carrier, FEC or bandwidth shaper. The comparison sends eight CUBIC or BBR flows in each direction separately. The direct TCP control uses the same endpoints and transit namespaces without the tunnel; the application workload explicitly keeps the TCP MSS matched. Netem delay is symmetric, and when enabled, each direction drops 1% independently. Offload settings are held constant across cells; TCP segmentation and UDP segmentation before the loss point are controlled separately. Receive goodput comes from `TCP_INFO` byte deltas; sender samples record MSS, smoothed RTT, congestion window and retransmits. Both controllers use the same hub and client binaries; only the sending TCP sockets select a different algorithm.

## Isolation pilot

At 1% +200 ms, the pilot delivered **6.878 Mbit/s through AWG** and **5.486 Mbit/s via direct TCP**. Both arms used eight CUBIC flows (sender MSS 1,228 bytes). The per-flow median congestion windows were **20.39 and 15.59 KiB** respectively, with about **201 ms** sender RTT. All measured AWG endpoint queue, endpoint link, kernel UDP receive-buffer and excess transit drops were zero. This reproduces the order of magnitude without AWG in the path. It is not evidence that AWG improves CUBIC: one trial per path, and packet sizes and realized loss draws differ.

At 200 ms, a single sender must keep about 7.15 MiB in flight to deliver 300 Mbit/s. Eight flows with a 20 KiB window each at that RTT have a scale of about 6.6 Mbit/s, consistent with the pilot. Propagation delay does not itself discard packets; it slows ACK feedback and extends the time needed to recover from repeated loss-induced window cuts.

## Matched matrix

All 48 ten-second receiver windows completed after three seconds of warmup. At 1% the table uses two paired seeds in CUBIC/BBR/BBR/CUBIC order; 0% is one control per cell. Values are Mbit/s, down / up.

| Loss, added RTT | Native AWG CUBIC | Native AWG BBR | Direct CUBIC | Direct BBR |
|---|---:|---:|---:|---:|
| 0%, 0 ms | 652.42 / 652.07 | 664.17 / 637.98 | 1500.72 / 1360.48 | 1478.18 / 1320.84 |
| 0%, +200 ms | 199.12 / 230.86 | 518.13 / 497.17 | 418.73 / 504.32 | 856.60 / 826.77 |
| 1%, 0 ms | 407.18 / 474.85 | 671.44 / 687.64 | 1092.81 / 1084.03 | 1421.27 / 1253.96 |
| 1%, +200 ms | 6.18 / 5.62 | 343.32 / 355.67 | 6.21 / 5.76 | 357.09 / 341.48 |

At 1% +200 ms the AWG CUBIC median sender cwnd was **17.39 / 16.49 KiB per flow**, versus **17.39 / 16.19 KiB** direct. Median sender RTT was 202–204 ms through AWG, 201–202 ms direct. No measured AWG hub/client queue, UDP receive-buffer, endpoint link or excess transit drops in those CUBIC cells. BBR raised throughput **56× down / 63× up** through the same AWG path without changing the configured loss, but raised loaded p95 from 201 ms to **234 / 227 ms** and brought back endpoint drops. At 0% +200 ms the BBR p95 was **390 / 293 ms**, versus **210 / 205 ms** with CUBIC. This rejects a blanket BBR default based on these unshaped runs. The direct and AWG 0% and local results also show forwarding/queue costs outside the diagnosed lossy WAN collapse.

## Fix boundary

For a TCP sender you control, select BBR on its socket without changing the AWG packet format or global defaults. A separate Linux namespace check verified that a destination-specific `ip route ... congctl bbr` can select BBR on a newly established local TCP connection while the global default remains CUBIC. A route metric does not select the algorithm for TCP on a remote origin or for packets forwarded through TUN. Do not set it globally or in shared hub routes based on the unshaped benchmark; clean-path queues and coexistence are separate acceptance gates.

For transparent traffic from an arbitrary origin, AWG cannot set the origin's `TCP_CONGESTION`; it forwards encrypted UDP and does not repair lost datagrams. Making arbitrary senders see fewer losses requires changing the path or mutually supported recovery across the impaired leg. The existing [lab parity experiment](../queue-recovery-matrix/summary.md#bidirectional-parity-transparent-native-tun-only) establishes potential, not a shippable default. SOCKS TCP relay results are not transparent TUN results.

[48 receiver windows](measurements.tsv) · [Grouped results](comparison.tsv) · [Pilot](pilot.tsv) · [Raw counters, scripts and binary digests](raw.tar.gz). After extracting the archive, reproduce the tables with `python3 -I matrix/summarize.py awg-rtt.log > measurements.tsv`, `python3 -I compare.py measurements.tsv > comparison.tsv`, and `python3 -I matrix/summarize.py pilot.log > pilot.tsv`. All three were compared byte for byte. `bash replay.sh <matching-checkout>` rebuilds the endpoints and repeats the matrix.
