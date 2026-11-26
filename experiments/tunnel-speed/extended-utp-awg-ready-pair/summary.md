# Extended/µTP: collect at most two ready AWG packets

Follow-up to the [four-packet candidate](../extended-utp-awg-ready-loss/summary.md), which failed both fresh-tunnel eight-stream tests at 1%. This experiment reduces the cap to two; it does not try to fix µTP loss recovery.

**A** is the current source build. **Q** replaces only `device/send.go` in the same pinned AWG module: after taking a singleton, peek at one FIFO successor and take it only if already encrypted and also a singleton. An unfinished successor stays next in FIFO; there is no wait to fill a pair. The Extended frame format, existing carrier send path, µTP, queues and offload policies are unchanged. A and Q use a byte-identical client. The overlay is lab-only.

When Q emits a pair, the existing `Bind.Send` uses the existing Extended `SendBatch` path. No new frame or UDP batching code is part of Q; the change is how often two AWG packets reach that existing path together.

Real Linux TUN, Extended/µTP download, TCP CUBIC, 2 s omitted + 6 s nominal, MTU 1280, one and eight streams, 0 and 100 ms one-way delay. Every attempt starts a new hub, client and carrier. Each cell runs `A Q Q A`, with two repeats per arm. Identical selective veth offloads at 0% and 1%; per-datagram loss calibration on every attempt. At 1%, the first A/Q and second Q/A share netem PRNG seeds (`101`, `211`, plus `1000` in the reverse qdisc). This matches the PRNG inputs, not drop positions in the application stream: the two arms can emit different datagram sequences.

## 0% underlay loss

All 16 attempts completed. Medians of two attempts per arm; exact readings are in the raw data.

| Added RTT | Streams | Goodput A → Q, Mbit/s | Loaded p95 RTT A → Q, ms | Engine drops / emitted hub TUN packets A → Q |
|---:|---:|---:|---:|---:|
| 0 ms | 1 | 315.44 → 388.76 | 40.30 → 31.65 | 0.025% → 0.065% |
| 0 ms | 8 | 291.16 → 375.56 | 45.80 → 35.35 | 0.064% → 0.188% |
| +200 ms | 1 | 63.18 → 54.35 | 204.00 → 204.00 | 2.656% → 4.108% |
| +200 ms | 8 | 170.82 → 218.02 | 252.50 → 218.00 | 2.468% → 5.040% |

Local goodput rose 23%/29% for one/eight streams and loaded RTT fell. At +200 ms, the single-stream samples overlap; eight-stream goodput rose 28% while the normalized queue-drop median doubled. These queue drops are not underlay packet loss.

## 1% underlay loss per direction

| Added RTT | Streams | Completed A → Q | Goodput of completed attempts A → Q, Mbit/s | Loaded p95 RTT of completed attempts A → Q, ms |
|---:|---:|---:|---:|---:|
| 0 ms | 1 | 0/2 → 0/2 | — | — |
| 0 ms | 8 | 1/2 → 2/2 | 14.33 → 0.17, 5.24 | 2382 → 5654, 13003 |
| +200 ms | 1 | 2/2 → 1/2 | 0.52, 0.52 → 0.70 | 1680, 2358 → 1738 |
| +200 ms | 8 | 0/2 → 0/2 | — | — |

Both builds completed 3/8. Nine of the ten failed runs timed out; the other failed the iperf3 result exchange even though all its overlay pings returned. Nine of ten failed runs had no hub engine drops. No excess qdisc drops or kernel UDP receive-buffer drops were counted in any run. Measured random drop ratios were 0.676–1.247% toward the hub and 0.886–1.266% toward the client. The completed local eight-stream attempts are not a reliable throughput comparison: A has only one sample and the two Q samples differ by roughly 30×.

**Decision: keep Q lab-only.** A cap of two preserved clean local gains but did not make the 1% profile reliable or show a repeatable speed gain there. The drop counters exclude hub queue overflow as the sole explanation, not µTP recovery as a proven root cause.

The sender probe failed against stock (`[2]` instead of `[2 3]`) and passed 20 runs under `-race` with the overlay, covering immediate flush before an unfinished encrypt, FIFO order, the pair cap and delivery of the last packet before shutdown. All 32 attempts and both per-attempt loss calibrations are in the archive. A clean extraction reproduced all three TSVs byte for byte.

[Measurements](measurements.tsv) · [Medians and ranges](comparison.tsv) · [Failed attempts](failures.tsv) · [Raw archive, sender patch and replay scripts](raw.tar.gz). To repeat on the same source, extract the archive, run `bash build.sh <repository-root>`, then `bash replay.sh`. `environment.txt`, `tracked-source.patch` and `untracked-source.tar.gz` identify the uncommitted baseline used for both arms.
