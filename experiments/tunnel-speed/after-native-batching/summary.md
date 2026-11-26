# Native WG/AWG after hub batching

Fresh production binaries; Linux TUN, local veth with transit namespace, no balancer.
No added delay or rate limit; MTU 1280; default AWG profile; TCP CUBIC, 8 streams, 2 s warmup and 8 s nominal measurement.
Three independent runs per profile; each result is receiver goodput. All directions tested separately.
At 1% loss offloads were disabled to apply random loss to individual packets instead of GSO bursts.

| Profile | Tunnel | Download, Mbps median [min–max] | Upload, Mbps median [min–max] |
|---|---|---:|---:|
| clean-defaults | socks5-wg-udp | 809.1 [630.8–1117.8] (3/3) | 1629.2 [1594.2–1835.9] (3/3) |
| clean-defaults | socks5-awg-udp | 677.2 [674.8–682.1] (3/3) | 1750.9 [1319.2–2111.1] (3/3) |
| clean-no-offloads | socks5-wg-udp | 329.8 [321.7–561.1] (3/3) | 564.1 [421.2–743.2] (3/3) |
| clean-no-offloads | socks5-awg-udp | 387.5 [327.3–536.4] (3/3) | 406.1 [365.1–599.5] (3/3) |
| loss-1pct | socks5-wg-udp | 240.5 [189.7–244.2] (3/3) | 256.3 [216.2–314.4] (3/3) |
| loss-1pct | socks5-awg-udp | 259.8 [247.3–285.9] (3/3) | 273.5 [235.4–304.5] (3/3) |

36/36 positive results. Raw archive includes separate iperf3 JSON, CPU samples, ping and queue counters.

## Matched 1% replay

One additional back-to-back run with the saved pre-batching binaries and the fresh batched binaries on the same runner, with offloads disabled in both runs. Download / upload, Mbps:

| Tunnel | Pre-batching | Batched |
|---|---:|---:|
| WG UDP | 251.3 / 278.6 | 230.0 / 244.8 |
| AWG UDP | 234.4 / 296.4 | 289.1 / 285.7 |

The replay does not show a consistent gain at 1% loss. Across the three primary 1% runs, measured netem loss was 0.97–1.06% per direction and peak CPU temperature per trial was 95.6–97.8 °C. See `matched-1pct.tar.gz` for the replay's raw readings. The original `before/` archive is unchanged.
