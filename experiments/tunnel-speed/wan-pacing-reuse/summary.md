# 200 ms WAN: BBR pacing, sender buffers, and short request reuse

Real foreground SOCKS5, WG/UDP with Linux TUN MTU 1280, fixed-destination BBR relay on the hub overlay, CUBIC HTTP origin outside the impaired path. Added RTT 200 ms (100 ms each way), independent 0% or 1% loss in both directions, 300 Mbit/s downlink rate on the UDP impairment leg, netem queue limit 4096 packets. Selective veth offloads identical throughout. Hub TUN `fq` enabled for **all** variants; no test of changing the qdisc alone. The `32 MiB` setting is the `tcp_wmem` autotune ceiling in private hub/client namespaces, not a per-connection allocation or a production setting. `SO_MAX_PACING_RATE` applies only to the relay's VPN-facing BBR socket: `ss` confirmed 240/270 Mbit/s for the capped flows. Downlink only, one transfer at a time, WG only.

## 128 MiB download

One complete HTTP response per trial, aggregate payload divided by elapsed wall time including setup. Two trials per cell across 0/1/1/0 loss order. All 16 payloads SHA-256 verified. Throughput and loaded p95 RTT are medians; throughput ranges show both trials, not uncertainty intervals.

| Max send autotune | BBR pacing ceiling | At 0% Mbit/s [range] | At 1% Mbit/s [range] | Loaded p95 ms at 0% / 1% |
|---|---|---:|---:|---:|
| 4 MiB | none | 94 [94–94] | 49 [48–51] | 213 / 201 |
| 32 MiB | none | 170 [167–173] | 87 [56–117] | 248 / 220 |
| 32 MiB | 240 Mbit/s | 163 [163–163] | 112 [103–121] | 201 / 201 |
| 32 MiB | 270 Mbit/s | 175 [175–175] | 121 [117–125] | 201 / 201 |

With 32 MiB and no cap, three of four trials had non-random downlink netem drops: 48,043 total at 0% and 8154 total at 1%. Both capped variants had **zero** non-random netem drops in all eight trials. No hub `fq` drops or UDP receive-buffer errors in any of the 16 trials. The uncapped socket's sampled pacing rate peaked at ~801 Mbit/s; a paced sender on a real path with a different bottleneck is not guaranteed to benefit from the same numerical cap. At 1% the 270 cap was 1.40× the same-buffer uncapped median, and 2.47× the 4 MiB control. At 0%, 270 preserved throughput and avoided the extra ~47 ms observed in the uncapped large-buffer cases. With no cap, the buffer raised throughput but did not reliably preserve latency. Trial order of variants within each round was fixed, so this is an engineering screen, not a rollout guarantee.

## Short 64 KiB HTTP requests

Same WAN shim/rate profile and 32 MiB ceiling, BBR without additional pacing cap. Three conditions, 12 interleaved repetitions per condition and loss setting. Each condition starts with a new connection; the timed request either opens it (cold), reuses it immediately after a 1-byte HTTP response (tiny prime), or reuses it immediately after an 8 MiB HTTP response (bulk prime). All HTTP ranges verified against SHA-256, including priming bodies at runtime. The probe checks socket identity and exactly one successful SOCKS CONNECT per condition; the reused socket carries actual data, not just control traffic. Timed request starts after priming has finished; no idle interval between requests, no TLS, no cross-origin or new-destination connection reuse.

| Loss | Data connection | Median TTFB, ms | Completion median / p95, ms | Median cost of prime, ms |
|---|---|---:|---:|---:|
| 0% | Cold | 402 | 942 / 948 | — |
| 0% | Reused after 1 byte | 201 | 711 / 711 | 402 |
| 0% | Reused after 8 MiB | 201 | 203 / 203 | 2378 |
| 1% | Cold | 402 | 952 / 2586 | — |
| 1% | Reused after 1 byte | 201 | 717 / 884 | 402 |
| 1% | Reused after 8 MiB | 201 | 203 / 404 | 3324 |

Reuse alone saved ~1 RTT before the first body byte. Reuse after a genuinely substantial response saved further RTTs for the body. With only 12 observations, nearest-rank p95 is the maximum. Artificially sending 8 MiB to warm each short request is a loss: priming alone takes seconds. The result supports opportunistically reusing a channel already carrying useful traffic, not a claim that a kept-alive idle connection retains its window or that adding a generic multiplexed transport would deliver the same latency.

## Next choice

- For long managed SOCKS downloads, test BBR with a WAN-size send buffer **and** a rate ceiling tied to the link, not a global constant of 270 Mbit/s. This experiment's known-capacity 90% ceiling beat 80%; repeat under shared load and changing bandwidth before using it as an adaptive policy.
- For short flows, prefer real data-channel reuse where the application/protocol permits it. A future managed multiplexed data carrier would have to prove per-stream backpressure, behavior after idle and isolation between unrelated connections. The current SOCKS implementation does not transparently pool arbitrary CONNECT destinations; this experiment exercised ordinary HTTP keep-alive through it.

[Bulk measurements](bulk.tsv), [short request measurements](short.tsv), [raw data and harness](raw.tar.gz), [binary digests](build.sha256). The archive replay verified 16 full 128 MiB downloads and 72 timed short responses. The host configuration and production source are unchanged. Replay after extraction:

```sh
python3 check.py bulk run.lxOhmUH4:4 run.qyc3zZ1B:12
python3 check.py short run.t39fmiX7:12
```

For fresh measurements, place `btawg-client` and `btawg-server` in `bin/`, build `go build -o bin/lab-relay relay.go`, then run `./reproduce.sh b4`, `b32` or `short` on a Linux lab host with user/network namespaces, TUN and BBR.
