# Native WG/AWG and AWG carriers: 0% injected loss, 0 vs 100 ms one way

Current working-tree Linux hub/client binaries (`2c77c9d` plus local changes; full binary SHA-256s in the archive). Real TUN MTU 1280 in isolated Linux namespaces, Ryzen 3 5300U, default link offloads and qdiscs on the endpoints. Transit veth uses its default noqueue at 0 ms and delay-only `netem` (100 ms each way, limit 262144, no rate or loss option) at +200 ms RTT. First-pass idle underlay RTT averaged 0.071 / 200.122 ms; 10/10 ping replies in each profile. No balancer, µTP pacing environment variable, or iperf rate limit. No TLS modes measured.

iperf 3.22 TCP CUBIC bound to the overlay interface; **transparent TUN, not SOCKS CONNECT or the BBR TCP relay**, despite transport names. One run per cell, 2 s omitted and 8 s nominal measurement, download and upload in separate trials. Numbers are receiver Mbit/s, not a latency-only regression estimate against earlier builds. CPU affinity: hub 0–3, client 4–5, both iperf endpoints 6–7.

| Transport | 0 ms: 1 stream down / up | 0 ms: 8 streams down / up | 100 ms: 1 stream down / up | 100 ms: 8 streams down / up |
|---|---:|---:|---:|---:|
| `socks5-wg-udp` | 1094.2 / 1697.8 | 1145.7 / 1713.1 | 70.7 / 124.5 | 220.2¹ / 1052.1 |
| `socks5-awg-udp` | 1037.1 / 1521.3 | 1141.0 / 1530.8 | 76.9 / 120.2 | 379.8 / 1101.0 |
| `socks5-awg-ext-tcp` | 506.3 / 787.3 | 512.8 / 734.0 | 73.3 / 107.5 | 163.4 / 190.5 |
| `socks5-awg-req-tcp` | 454.1 / 540.7 | 452.7 / 479.9 | 56.6 / 123.1 | 139.9 / 149.0 |
| `socks5-awg-ext-utp` | 335.4 / 461.6 | 321.1 / 434.9 | 61.9¹ / 114.2 | 215.0 / 253.1 |
| `socks5-awg-req-utp` | 280.6 / 319.9 | 314.8 / 331.9 | 61.7 / 113.6 | 176.3 / 183.5 |

¹ First attempt timed out after 32 s before iperf started moving payload (server accepted the control connection; 3/8 WG and 0/1 µTP data connections reported by the client). The overlay was still answering pings. The fresh-tunnel repeat succeeded in both cases; both original attempts are preserved. The retry underlay ping sampled 200.090 ms minimum, with one delayed response near 400 ms; loaded overlay p95 for each retry is in the TSV. Overall: 46/48 first-pass successes, 48/48 cells after two targeted repeats. Cause of the stalls was not established.

No transit qdisc, endpoint qdisc, or interface drops were recorded in any trial. This does **not** imply end-to-end losslessness: the hub engine queue discarded packets in all 12 completed downloads per profile, with maxima of 16756 at 0 ms and 8040 at 100 ms one way. Native eight-stream uploads also had UDP receive-buffer errors in both profiles. These are not configured loss. At +200 ms, AWG Extended/µTP was the fastest BitTorrent carrier on eight streams in both directions; its loaded p95 was 255 ms download and 536 ms upload. The native modes retained the highest measured eight-stream goodput; all modes delivered substantially less with one stream at +200 ms than at 0 ms.

[Measurements and drop deltas](measurements.tsv) · [raw iperf, RTT, counters, binary digests and replay scripts](raw.tar.gz). Verify after extracting into a scratch directory: `python3 summarize.py run.8PritkzU retry/run.WUy54klz > measurements.tsv`. For a fresh first pass, build the two binaries into `bin/` next to the extracted scripts, then run `bash reproduce.sh`.
