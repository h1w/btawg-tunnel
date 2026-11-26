# Clean native WG/AWG: original vs current binaries

The original deployment binaries match the SHA-256 in `before/raw.tar.gz`. Both builds were measured again on the same isolated real-TUN Linux stand; 0% configured and observed loss, no added delay, Linux veth offload defaults, no rate shaping or balancers, MTU 1280, protected AWG defaults. TCP CUBIC, 2 s warmup and 8 s nominal measurement, two runs per build in ABBA order. CPU affinity: hub 0–3, client 4–5, iperf 6–7. WG/AWG mode order reversed for the second pair.

| Tunnel | TCP streams | Direction | Before Mbit/s median [range] | After Mbit/s median [range] | Change | Loaded p95 ms before → after |
|---|---:|---|---:|---:|---:|---:|
| socks5-wg-udp | 8 | down | 635.3 [617.6–653.0] | 1517.1 [1056.4–1977.8] | +138.8% | 20.65 → 4.68 |
| socks5-wg-udp | 8 | up | 2493.4 [2412.1–2574.6] | 2116.2 [1750.4–2482.1] | -15.1% | 20.85 → 26.90 |
| socks5-awg-udp | 8 | down | 635.8 [620.4–651.2] | 1802.2 [1673.4–1931.0] | +183.5% | 20.05 → 4.29 |
| socks5-awg-udp | 8 | up | 2296.1 [2236.6–2355.6] | 2402.8 [2387.8–2417.8] | +4.6% | 23.05 → 20.70 |
| socks5-wg-udp | 1 | down | 634.8 [614.8–654.8] | 1851.9 [1690.7–2013.0] | +191.7% | 13.40 → 2.50 |
| socks5-wg-udp | 1 | up | 2389.1 [2293.0–2485.2] | 2418.5 [2306.5–2530.4] | +1.2% | 16.10 → 17.45 |
| socks5-awg-udp | 1 | down | 638.5 [625.1–651.8] | 1881.3 [1765.9–1996.8] | +194.7% | 17.60 → 2.50 |
| socks5-awg-udp | 1 | up | 2179.1 [2162.4–2195.9] | 2386.7 [2332.4–2441.1] | +9.5% | 18.10 → 14.00 |

32/32 valid goodput samples. Temperature, CPU, idle underlay RTT, and sampled load RTT are included in the archive. Two repetitions describe observed scatter, not a confidence interval. The historical 1% `before/` is unchanged.
