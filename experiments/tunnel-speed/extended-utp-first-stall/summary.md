# Extended/µTP: first data stall at 1% underlay loss

Stock AWG sender, Extended/µTP download over real Linux TUN, one CUBIC stream, zero added RTT, 1% independently in each underlay direction (`netem` seeds 211/1211). Same topology and selective offloads as the [ready-pair experiment](../extended-utp-awg-ready-pair/summary.md). A scratch Go build overlay logs µTP state at existing 500 ms timer ticks, RTO entry and errors. AF_PACKET on the two veth endpoints records header fields only, with kernel RX/TX timestamps. TCP receive progress is sampled with `ss`. No product changes.

## The unrepaired hole

Times relative to iperf3's start timestamp. Capture drops: **0 at both endpoints** in the 1% run.

| Time | Hub | Client |
|---:|---|---|
| +177.490 ms | First DATA `seq=32798` (66 bytes) | Not seen |
| +178.881 ms | Retransmits `32798` | Not seen |
| +178.930 ms | Receives `ACK=32797` with SACK for later packets | Cumulative ACK stops at `32797` |
| +329 ms | Oldest `32798`, sent twice; 66 bytes in flight, congestion window 127,377 B, peer window 24,970,650 B | Reorder queue grows; the gap blocks delivery to the stream |
| +0.329 to +28.865 s | Same oldest packet on all 58 timer ticks; expiry remains 773–997 ms *in the future*, retry count 0 | 248 SACKs for this hole reach the hub, with max interarrival 256.083 ms |
| +29 s | iperf3 timed out; no third send of `32798` | 2.5 MiB in the first omitted interval, zero in every later interval |

An earlier loss during setup did reach RTO and recover (DATA `28360`, approximately −5.520 s → −4.178 s). No hub RTO fired after the data stall began. The hub and client remained connected at the µTP layer; later packets and ACKs still crossed the underlay. The measurement's `netem` counters recorded 8 and 61 random drops toward hub and client, respectively (0.691% and 1.204%). Hub engine drops, excess qdisc drops and kernel UDP receive-buffer errors were all zero during the lossy trial.

## Why the timer never repaired it

`selectiveAck` advances `fastResendSeqNr` past the packet it just fast-retransmitted, so later SACKs cannot fast-retransmit that same hole again. That is normally fine if RTO can repair a lost retransmission. However, each newly SACKed **later** packet goes through `ackPacket`, which unconditionally sets `rtoTimeout = currentMS + rto` and clears the consecutive timeout counter. Forward cumulative ACK progress is not required. Because later packets kept arriving with gaps between SACKs shorter than the 1 s RTO, the oldest unacknowledged packet kept losing its timeout. The receiver could buffer those later bytes but could not deliver them through the ordered stream. This explains the captured stall; it does not classify every prior 1% failure.

The corresponding code is in [`purego/conn.go`](../../../btawg/third_party/go-utp/purego/conn.go): `ackPacket`, `selectiveAck`, `checkTimeouts`, and the reordering branch of `processIncoming`. A focused next test is to lose one DATA segment and its fast retransmission while continuing to SACK later segments; check that the oldest gap still reaches its deadline and ordered delivery resumes. No timer or congestion-control change was tested here.

## Control and evidence

The 0% control completed at 263.19 Mbit/s with the same diagnostic binaries. The AF_PACKET reader dropped samples at that rate, so the control is used only for TCP completion and aggregate counters, not individual datagram matching or clean performance comparisons.

[Trace excerpt](first-stall.txt) · [Measurement counters](measurements.tsv) · [Raw headers, TCP progress, state logs and replay scripts](raw.tar.gz). Extract, then run `bash run.sh <repository-root>` to capture again. Exact sequence numbers and drop positions can vary with scheduling even for a seeded qdisc.
