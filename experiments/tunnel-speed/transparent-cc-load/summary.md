# Transparent TUN: application TCP controller and offered-load controls

Real Linux TUN, managed client, identical hub/client binaries on both arms (including the unsent-DATA µTP timer fix). Selective offload settings are held constant. Eight bulk TCP flows, 2 s warmup and 5 s receiver-byte window. 100 ms added in each direction, with independently seeded 1% loss where selected and no rate shaping. Only the **application's sending TCP sockets** switch CUBIC → BBR; the receiver, tunnel transport, carrier congestion controller, host settings and queue limits are unchanged. The harness reads back `TCP_CONGESTION` on each actual sender socket.

## 1% loss at +200 ms RTT

Two seeds per arm in CUBIC/BBR/BBR/CUBIC order. Mbit/s are receiver medians of valid intervals. An interval is valid only if every flow delivered bytes in the measurement window.

| Transport | Download Mbit/s CUBIC → BBR | Upload Mbit/s CUBIC → BBR | Loaded p95 down / up, ms CUBIC → BBR |
|---|---:|---:|---:|
| Native WG | 9.28 → 347.59 | 9.75 → 389.51 | 201 / 201 → 201 / 201 |
| Native AWG | 8.69 → 338.75 | 9.71 → 417.66 | 201 / 201 → 201 / 201 |
| Extended/µTP | 0.619 → 0.825 | 0.658 → 0.637 | 4,024 / 7,082 → 2,460 / 3,443 |
| Request/µTP | 0.527 → 0.618 | 0.432 → 0.496 | 4,438 / 6,791 → 2,656 / 3,179 |

One CUBIC Request/µTP upload timed out while the listener waited to accept all eight connections, so its successful control is a single measurement, not a zero-speed sample. BBR restored native throughput by **37–43×** in this workload; it did not eliminate underlay loss and some BBR runs developed endpoint queue drops as offered load increased. There were no excess transit drops in these successful lossy runs. Both µTP carriers stayed below 1 Mbit/s in all four BBR medians despite the lower loaded RTT. The difference isolates the application sender's loss response in the native case and rules out that response as a sufficient fix for the outer µTP limit. A transparent forwarder cannot set `TCP_CONGESTION` on arbitrary remote application sockets; this result is not a product default or a SOCKS benchmark.

## 0% loss at +200 ms RTT

One attempt per arm and cell, all 16 valid. Native WG down/up increased **297.95/175.13 → 731.24/684.10 Mbit/s** and AWG **286.73/121.69 → 682.21/609.61 Mbit/s**, but WG loaded p95 rose **210/201 → 261/231 ms**, AWG **214/202 → 263/234 ms**. For example, AWG downlink combined endpoint drops rose from **7,118 to 18,149**. No excess transit overflow was measured. In the µTP modes, BBR reduced both downlink rates and raised both upload rates and loaded p95; Request downlink hub drops rose from **3,159 to 44,353**. The controller contrast is not a clean-path queue fix.

## Same offered rate, different hub CPU budget

Separate native AWG downlink UDP control: 0% underlay loss and no added delay, eight 1200-byte iperf3 streams, unchanged tunnel binaries, unchanged client/workload affinity. Only the hub's CPU affinity changes from two physical cores (`0–3`, SMT siblings included) to one (`0–1`). Order two/one/one/two, two attempts per cell. Values are observed ranges; hub queue counters cover the whole trial, including warmup, rather than only the UDP receiver interval.

| Offered Mbit/s | Receiver Mbit/s, two → one physical core | Hub queue drops, two → one | Loaded p95, ms, two → one |
|---|---:|---:|---:|
| 600 | 599.83–600.00 → 599.67–599.99 | 0–86 → 26–738 | 1.06–1.38 → 1.41–1.48 |
| 800 | 799.31–799.97 → 799.98–800.02 | 0 → 1–681 | 4.44–5.31 → 3.94–24.10 |
| 1,000 | 998.79–1,001.06 → 939.53–949.81 | 1,218–4,751 → 45,927–49,457 | 118–123 → 198–206 |

All 12 intervals completed; sender offered rates matched their target within 0.02 Mbit/s. At 1 Gbit/s, fewer hub CPU resources caused roughly **10–41×** more hub queue drops at the same offered rate; loss and CPU affinity on the other endpoints were unchanged. This implicates the hub's ability to drain/process traffic, not merely the configured buffer length. The perturbation affects both dispatcher and consumer threads, so it does not isolate a particular crypto worker or kernel stage. Together with the [enqueue CPU profile](../queue-sync-profile/summary.md), it does not justify replacing queue synchronization or hard-coding CPU affinity.

[47/48 controller intervals valid](measurements.tsv) · [Controller medians](comparison.tsv) · [12 offered-load controls](offered.tsv) · [Raw counters, workload, binaries' hashes, source snapshot and replay](raw.tar.gz). All three tables were reproduced byte for byte. Extract and run `bash replay.sh <matching-checkout>` to rebuild and repeat the trials.
