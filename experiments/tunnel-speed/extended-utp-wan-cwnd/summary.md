# Extended/µTP WAN: TCP delivery and congestion-window accounting

At +200 ms added RTT and eight TCP downloads, the receiver measured **0.580–0.582 Mbit/s at 1% loss**, independently of iperf3. In both lossy runs the window reductions in the sampled interval came entirely from fast-retransmit backoff. The controller's delay estimate stayed below 1 ms; there were no in-window hub RTO cuts.

## Matched receiver-byte measurement

Four fresh tunnels, Extended/µTP with the [RTO fix](../extended-utp-rto-fix/summary.md), no AWG batching or µTP pacing. 100 ms added each way, no rate cap, identical selective offload settings, two per-datagram calibrations per trial. Trial order: clean, loss seed 101, loss seed 211, clean. The 1% loss is independently configured in each direction, with the client-bound seed offset by 1000. One hub listener starts eight bulk TCP writers once the eight client sockets have connected; receiver threads drain sockets as data becomes available. No iperf3 or network control/result socket. Use the sum of the eight `TCP_INFO.tcpi_bytes_received` deltas across the same 4–16 s interval from sender start, divided by actual monotonic elapsed time. Linux x86-64 TCP_INFO offset 128 was checked against `<linux/tcp.h>`. All eight streams advanced in all four runs.

| Underlay loss | Run label | Received TCP bytes | Goodput Mbit/s | Hub cwnd median [sample min–max] |
|---:|---:|---:|---:|---:|
| 0% | 101 | 354,427,816 | 236.285 | 24 MiB [24–24 MiB] |
| 1% | 101 | 872,556 | 0.582 | 18.17 KiB [9.93–34.54 KiB] |
| 1% | 211 | 870,652 | 0.580 | 17.87 KiB [9.50–31.73 KiB] |
| 0% | 211 | 214,112,852 | 142.737 | 24 MiB [24–24 MiB] |

Clean/loss ratios by run label: 406× and 246×; clean scatter is large, so these are observed ratios, not predictions. The actual lossy netem drop ratios were 0.704–0.751% toward hub and 0.970–1.123% toward client. Qdisc/capture-boundary counters span the whole load run, not only 4–16 s. No engine queue, excess qdisc or kernel UDP receive-buffer drops in either lossy trial. The clean runs did have 1,621 and 2,057 hub engine drops; 0% refers to underlay only.

## Where the window went

A lab overlay records cumulative changes within each existing ~500 ms timer tick. Every window delta balances exactly against its categorized changes. Only intervals between samples wholly inside 4–16 s are used for attribution; the initial/ending partial ticks are not counted.

| 1% seed | Fast-retransmit events / window halvings | Fast-retransmit decrease | LEDBAT increase / decrease | RTO cuts | Delay-estimate mean / max | Sender full ticks |
|---:|---:|---:|---:|---:|---:|---:|
| 101 | 12 / 12 | 153.88 KiB | 154.91 / 0 KiB | 0 | 0.061 / 0.546 ms | 24/24 |
| 211 | 15 / 14 | 172.28 KiB | 158.43 / 0 KiB | 0 | 0.041 / 0.328 ms | 23/24 |

No idle or slow-start window change contributed in these lossy intervals. The receiver advertised at least **23.97 MiB**; it was not the limiting window. A median cwnd of ~18 KiB divided by the bare 200 ms RTT is a scale of ~0.73–0.74 Mbit/s before protocol overhead and recovery. The measured TCP rate is consistent with that bottleneck; the quotient is not an upper-bound proof based on a time-varying window.

[`Conn.selectiveAck` → `maybeDecayWin`](../../../btawg/third_party/go-utp/purego/conn.go#L783-L804) fast-retransmits SACK gaps and halves cwnd when the last cut was at least `maxWindowDecay` (**100 ms**) ago, then exits slow start. The unpaced [`applyCControl`](../../../btawg/third_party/go-utp/purego/conn.go#L809-L880) grows its window on ACKs at a nominal **3,000 B per RTT** at zero queue delay; at 200 ms that is about **15 KB/s** in the ideal full-window case. In the two 1% runs 12 and 14 halvings over ~11.5 s canceled essentially all additive regrowth. There was no LEDBAT negative adjustment: estimated queue delay stayed far below the **100 ms** target. The trace measures which controller branches moved the window; it does not match every reported SACK hole to a particular wire drop or prove a safe alternative response to real congestion. No production changes or policy variants were tested.

[Measurements and raw counters](measurements.tsv) · [Raw TCP samples, window events, scripts and overlay](raw.tar.gz). Extract and run `bash run.sh <repository-root>` for a new run, or `python3 -I reduce.py bench.log` to reproduce the recorded TSV byte for byte.
