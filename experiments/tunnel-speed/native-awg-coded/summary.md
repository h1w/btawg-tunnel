# Native AWG: two-erasure repair with a low-delay TCP flight budget

Target: unchanged Linux TCP CUBIC through real IPv4 TUN and native AWG over UDP, MTU 1280, no application proxy, no global congestion-control change, no bandwidth shaper in the target cells. Netem injects 100 ms one way and 1% independent random loss per direction, with per-datagram offload calibration. Each target cell must independently deliver **>50 Mbit/s receiver payload**, **≤1.8 endpoint TX bytes/payload byte** and **≤230 ms loaded overlay ICMP p95** (at least 35 replies) over the same ten-second window. One and eight streams, seeds 101 and 211; download and upload are run separately, not simultaneous full duplex.

## Final binaries: all three conditions in the same trial

Ranges are the min/max of the two seeds, not confidence intervals. Bandwidth is receiver goodput; the cost is the **sum** of both physical-namespace virtual Ethernet TX byte deltas per useful TCP byte.

| CUBIC flows | Direction | Receiver Mbit/s | TX / payload | Overlay loaded p95, ms |
|---:|---|---:|---:|---:|
| 1 | Download | 66.107–69.680 | 1.679–1.696 | 203–206 |
| 1 | Upload | 78.509–79.539 | 1.736–1.744 | 201 |
| 8 | Download | 80.789–81.170 | 1.751 | 203–205 |
| 8 | Upload | 80.081–81.767 | 1.748–1.754 | 201–202 |

**8/8 target windows passed; 12/12 final windows were valid.** Underlay p95 was 200 ms; measured directional random loss was 0.991–1.077% across the target cells. No hub engine queue drops or UDP receive-buffer errors in those cells. Hub full-data replica counter delta was zero. It reconstructed 895/933 single-stream and 953/990 eight-stream upload packets. Median aggregate `TCP_INFO.snd_wnd` at the actual senders was 2.0 MiB in every target window. The earlier establishment/idle issue was fixed by retaining negotiated scales independently of the recent activity count and validating both TCP SYNs; unknown or authenticated option sets are not modified.

Same-binary comparison, seed 101, eight-flow upload: `wide` delivered 211.608 Mbit/s at **3.552× / 608 ms**; `coded` delivered 80.081 at **1.748× / 202 ms**. The lower peak is intentional. At eight-flow download the adaptive parity default delivered 22.425 Mbit/s at 1.242× / 201 ms; `coded` delivered 80.789 at 1.751× / 205 ms.

**10 Mbit/s shared FIFO (not a target case):** `coded` increased tunnel download from 4.837 to 6.198 Mbit/s versus the adaptive default but reduced the competing direct CUBIC from 0.770 to 0.492 Mbit/s. Overlay p95 rose from 201 to 224 ms. This profile is for paths with spare capacity and independent loss, not a blanket default or fairness fix. No guarantee on burst loss, longer transfers, different RTTs, or unobserved TCP handshakes.

## Why the previous mode was not enough

`wide` sends an original and up to two full copies. With synchronized instrumentation its eight-flow upload sent 3.545 endpoint TX bytes per delivered byte at 586 ms overlay p95 in the meter control. This is not an estimate from parity counters. `coded` instead sends originals immediately, and forms two independent parity rows per eight eligible packets. Each row carries its own compact metadata and can recover a single missing original even if the other row is lost. Both rows can recover two missing originals. Full-size rows use two inner packets each, short rows one. Recovered packets are checked by content fingerprint and peer/destination policy before being delivered. Control traffic goes through ordinary AWG; no new port, SOCKS socket, outer datagram format or encoder on the application sender.

Compact parity alone was not a latency fix: in the four `packed` pilot windows the wire cost fell to 1.674–1.718, but the eight-flow upload reached 336 ms p95. The opt-in profile therefore also bounds the data remote senders can have in flight using the advertised TCP receive window. For 200 ms RTT, 2 MiB corresponds to ~84 Mbit/s of payload before other limits. The budget is per enrolled peer and direction, not per connection. It is not a fixed bandwidth promise: its throughput scale changes with RTT. The sender keeps its own congestion controller. The profile does not increase a receiver's real window, replicate ACKs, accelerate arbitrary UDP or guarantee repair of a burst. An early version retained TCP scales for just three seconds: the first connections in an eight-flow run escaped control after setup, while the last ones were constrained. The `credit-pilot` includes the resulting latency failures rather than silently discarding them.

## How to reproduce

[Final receiver measurements](measurements.tsv) · [TCP and queue counters](trace.txt) · [Raw samples, runner, reducers, diagnostic runs and executable SHA-256](raw.tar.gz). Extract to a private scratch directory. `python3 -I final-reduce.py final.log` regenerates the measurements. `python3 -I final-reduce.py final.log | python3 -I gates.py` reports each criterion. For a new run move the archived `final.log` aside, then `LAB_RUNS="$(cat profiles.txt)" bash final.sh /path/to/matching/checkout`. Requires a Linux host with CPU IDs 0–7, TUN, private user/network namespaces and the tools from the earlier [wide lab](../native-awg-wide/summary.md).

Endpoint TX counts come from fresh `/proc/net/dev` reads on **hub0 and cli0** in their own namespaces. Both samplers follow actual receiver window-start and window-end stamps; a missing/nonpositive sample or >50 ms timestamp skew invalidates it. The ratio includes the two directions, TCP ACKs, IP/UDP/AWG overhead, probes, recoveries and packets later dropped in transit. It measures virtual Ethernet device bytes, not PHY preamble, inter-frame gap, FCS or ISP billing. Useful bytes are TCP_INFO `bytes_received` at the receiver. The reported RTT is ping **inside** the VPN under load, not the nominal 200 ms underlay. Netem loss, TUN, UDP and recovery counters cover the longer pre/post trial interval; never divide them by bytes from a different interval to calculate wire cost. Where a second direct flow is present, the TX totals and useful-byte denominator include both workloads: that ratio is not the tunnel's standalone efficiency. Experimental profiles in archive subdirectories are not combined with the final binary results. A rejected initial physical sampler produced stale zero-byte readings and was excluded before any efficiency conclusion.
