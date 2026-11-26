# Extended/µTP: where the downlink stalls

Real Linux TUN, `socks5-awg-ext-utp` with production protected-record negotiation, eight CUBIC download streams, 0% injected loss and no rate limit. Both sides of the WAN profile use 100 ms delay (about 200 ms RTT). Linux offload defaults; identical clients. Hub CPU affinity 0–3, client 4–5 and iperf 6–7. A/P/P/A: two unmodified controls and two stage-probe runs per delay, 2 s omitted + 7 s nominal. Three more runs per delay took CPU profiles or tested UDP submission granularity (8 s nominal). No production code or host settings changed.

## Stage timing

Ranges below are the **two P-run means**, not end-to-end additive estimates. AWG containers, protected records and µTP writes each have their own sampling population. µTP writes include the socket mutex and condition waits; bind sends include the protected record write. TUN queue timing samples the first routed packet of a TUN read when eligible. AWG/record/µTP timings sample one in 32 calls. Every µTP condition wait and resend is counted. Snapshots every ~500 ms; steady deltas use ~5.7 s inside the measurement window, not handshake/teardown.

| Stage | 0 ms added RTT | +200 ms added RTT |
|---|---:|---:|
| Shared TUN → engine read, queued packet | 2.03–4.86 ms | 0.78–1.01 ms |
| Admission to AWG peer queue | 15–27 µs | 5–8 µs |
| AWG encryption queue, from peer admission to worker start | 25–60 µs | 51–74 µs |
| AWG encryption work per container | 4.4–5.0 µs | 4.5–4.7 µs |
| After encryption → sequential sender takes the container | 30.0 ms | 11.4–13.8 ms |
| Bind send per sampled container | 29–32 µs | 46–47 µs |
| Protected record write | 29–30 µs | 44–45 µs |
| µTP `Write` | 26–27 µs | 36–37 µs |
| µTP socket mutex acquisition | 3.3 µs | 4.0 µs |
| Total measured µTP condition wait / steady wall time | 0% | 2.0–8.0% |

At +200 ms all observed µTP condition waits were assigned to `cwnd` at wait entry; none hit the remote receive window, local send-buffer cap or packet ring. The whole-trial waits including ramp were 0.79–1.16 s. In steady samples the mean flight was **4.85–5.24 MiB**, congestion window **21.1–21.3 MiB**, and receive/send-buffer bounds about **24 MiB**. The congestion window reached 24 MiB in both P timelines. No outer µTP data resends were recorded in the P trials; inner TCP retransmits and hub engine drops remained visible. This identifies a transient ramp constraint, not persistent µTP window starvation.

## Hot path and falsification checks

The largest measured hub delay is **after AWG encryption and before the serial per-peer send**. The hub CPU profile attributes 50–54% of samples to `RoutineSequentialSender` (inclusive), 34–35% to `sendmmsg` reached via µTP `flushWrites`, and only 3.6–3.9% to AWG encryption workers (inclusive). The sampled active stack descends from the bind into protected records, TLS, Extended framing and µTP UDP submission. These CPU percentages are not wall-time fractions. Already-ready AWG containers accumulate behind the sender; that queue can backpressure the AWG TUN reader, after which the 256-slot hub queue rejects incoming bursts. Encryption and the µTP send-buffer size are not the dominant steady-state constraints observed in this direction. The data do not uniquely explain the entire 0-to-200 ms goodput difference.

Two extra runs per delay (`D` and `B`, one each) compared the same instrumented sender with the same four-packet, half-full engine-read policy previously tried for Extended/µTP. `D` is the current single-packet read. The metric counts calls to the existing batch-submission API and direct single sends. It is a proxy for kernel submissions, not a syscall trace; if GSO bypasses either counted branch it is absent from the ratio. The measured multi-datagram flush averaged two datagrams in every arm. The sender was making about one counted submission for each 1.5 kB of µTP stream data without batching.

| Delay | Download Mbit/s D → B | Hub drops D → B | Loaded p95 ms D → B | Counted UDP submits per MiB D → B |
|---|---:|---:|---:|---:|
| 0 ms | 326 → 347 | 196 → 925 | 41.8 → 71.9 | 701 → 652 |
| +200 ms RTT | 193 → 237 | 6,387 → 1,958 | 266 → 317 | 703 → 623 |

Fewer UDP submissions per MiB and more throughput support targeting the **serial write/flush path**, not raising the window. However, the extra queueing was real: with B at +200 ms, mean wait after encryption increased to 75.6 ms. Do not ship the batching variant on this evidence. A useful next experiment is to amortize UDP submissions for data already ready to send without making the deep AWG queue hold larger batches; compare CPU cost per MiB, queue residence and loaded p95, not the drop counter alone.

A/P/P/A accounted for measurement overhead: unmodified/probed median goodput was 366/324 Mbit/s at 0 ms and 208/187 Mbit/s at +200 ms. Use P for stage attribution, not as a production speed estimate. All 14 trials completed with zero qdisc drops and UDP receive-buffer errors in the sampled counters.

[Goodput and counters](measurements.tsv) · [Stage histograms and timeline deltas](stages.tsv) · [Raw samples, pprof, overlays and runners](raw.tar.gz). Extract the archive, then run `python3 compare.py stages.log cpu.log syscall.log` and `python3 stage-summary.py . stages.log cpu.log syscall.log` to reproduce the tables. `bash run.sh <repository>` builds and reruns A/P/P/A in private namespaces.
