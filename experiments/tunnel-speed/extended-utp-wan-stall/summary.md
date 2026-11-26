# Extended/µTP: +200 ms / eight-stream timeout after the RTO fix

The tunnel did not stop. TCP data reached the client throughout the run, but iperf3 reported zero until a full read block returned. The benchmark then timed out during its control/result exchange. A smaller block exposed the data without making the result exchange finish. This is not the permanently stuck µTP sequence from the [first-stall trace](../extended-utp-first-stall/summary.md).

## Method

Five fresh Extended/µTP tunnels, all with the [oldest-packet RTO fix](../extended-utp-rto-fix/summary.md), no AWG sender batching. One-way delay 100 ms, independently configured 1% underlay loss each way, seeds 101 and 211 with client-bound offset 1000; real Linux TUN, TCP CUBIC reverse/down, 8 streams, 2 s omitted + 6 s measured, 29 s external deadline. Both endpoint AF_PACKET captures record µTP headers, with no capture drops on any run. `ss -tin` samples the overlay TCP data and control sockets in both namespaces. The last three trials also intercept iperf3's block-read and result-exchange entry/return via `LD_PRELOAD`, without changing return values or data. Go overlays add transport state logging only. No product code changed.

## Why it looked like zero throughput

| Run | iperf3 block | Client TCP bytes by ~7.8 s¹ | iperf3 measured bytes² | Client result exchange |
|---|---:|---:|---:|---|
| `LHQCiNn7` | 128 KiB | 458,044 | 0 | Timed out |
| `qhQ1I3GH` | 128 KiB | 338,928 | 0 | Timed out |
| `6vz677J2` | 128 KiB | 466,640 | 0 | Entered; no return before kill |
| `pplNl8Ad` | 4 KiB | 495,724 | 327,680 | Returned error after 10.009 s |
| `5uwDg467` | 4 KiB | 300,472 | 356,352 | Returned error after 10.010 s |

¹ Relative to the JSON start timestamp, which precedes `TEST_RUNNING` during stream setup. These samples are not the iperf3 reporting window. ² Sum of non-omitted interval bytes in incomplete runs; not a completed benchmark result.

In `6vz677J2`, at +7.746 s the eight TCP sockets had received 39,296–85,960 B each, and every `Recv-Q` was zero. All eight application reads requested 131,072 B, and the first returned only at +12.063 s, after the client sent `TEST_END` at +10.347 s. iperf3 3.22's [`Nrecv_no_select`](https://github.com/esnet/iperf/blob/3.22/src/net.c#L519-L552) loops until a block is filled or returns early on error/EOF; [`iperf_tcp_recv`](https://github.com/esnet/iperf/blob/3.22/src/iperf_tcp.c#L55-L80) counts the returned bytes only while `TEST_RUNNING`. The 4 KiB runs returned 121 and 73 blocks by +8 s. This explains the apparent zero, not the low underlying delivery rate.

## Why completion still timed out

In the instrumented runs the hub accepted `TEST_END` and sent `EXCHANGE_RESULTS` within ~100 ms. The client did not enter result exchange until another **6.18–8.69 s** had elapsed. `ss` showed the hub's control socket holding an unacknowledged one-byte state, including TCP retransmits and backoff. After receiving the client's report the hub queued its report and `DISPLAY_RESULTS`, but **1,359–1,370 B** remained outstanding on that control socket in late samples. The client was waiting for the report; both 4 KiB runs returned an error after about 10 s in that exchange and did not complete normally. This is a delayed control exchange, not a failure to establish the eight data sockets.

One ordered µTP stream carries all eight TCP data flows and the iperf3 control flow. It stayed active: DATA arrived through +27.98–28.57 s and cumulative ACK advanced. From +3 to +27 s, the hub was `connected-full` on **239/240** timer samples; cwnd ranged from **1,382 to 42,789 B**, while the smallest advertised client receive window was **25,086,227 B**. Data-phase hub RTO counts were 0, 1, 2, 0, 0. No hub engine drops, excess qdisc drops, or UDP receive-buffer errors were counted. The observed transport limit was a saturated sender congestion window, not a zero receiver window or an unrepaired fixed sequence. The traces do not identify where each control retransmission waited within the shared data path or isolate why cwnd stayed this small.

**Next benchmark:** keep the same WAN profile, measure receiver TCP-byte deltas over a fixed interval independently of iperf3's result exchange, and track control completion separately. This separates µTP throughput from the probe's block accounting and teardown. Reducing `-l` alone is not a transport fix.

[Socket and iperf timeline](timeline.txt) · [Carrier window and capture counters](carrier.txt) · [Loss counters](measurements.tsv) · [Raw headers, scripts and logging overlays](raw.tar.gz). For replay, extract then run `bash run.sh <repository-root>`; for the three instrumented trials, build the probe with `cc -O2 -shared -fPIC iperf-trace.c -o iperf-trace.so -ldl` and run `LAB_REUSE_BINARIES=1 LAB_IPERF_TRACE=1 LAB_PROFILES='1:101 1:101:4K 1:211:4K' bash run.sh <repository-root>`.
