# Native AWG: high-loss, spare-bandwidth profile

Transparent Linux TUN and native AWG over UDP, IPv4 MTU 1280. No bandwidth shaper in the target cases; **independent 1% random loss in each direction**, 100 ms propagation each way. The transit netem sees individual UDP datagrams (calibrated separately in both directions), not UDP GSO super-packets. Sender sockets explicitly use CUBIC; no SOCKS CONNECT, application relay, host congestion-control override or production `tc` rule. Download and upload are separate TCP tests, not simultaneous full duplex. Goodput is the receiver's `TCP_INFO.bytes_received` delta over a 10 s window after 2 s of traffic warmup. The separate 10 Mbit/s shared-link control is not a target cell.

## Final build: receiver Mbit/s

| CUBIC workload | Default parity, seed 101 | Wide, seed 101 | Wide, seed 211 |
|---|---:|---:|---:|
| 1 flow, down | 1.39 | 102.77 | 84.72 |
| 1 flow, up | 0.68 | 110.78 | 110.65 |
| 8 flows, down | 28.56 | 232.69 | 230.31 |
| 8 flows, up | 35.84 | 211.37 | 218.21 |

**8/8 target windows exceeded 50 Mbit/s**; all 14 final windows completed. In those eight target cases the realized combined transit loss was 1.001–1.022%; excess netem drops and all four TUN RX/TX drop counters were zero. In the single-flow downloads, hub engine drops fell from 7463/5776 before bounded admission to 12/5. They were not eliminated: the wide eight-flow downloads had 22/59 hub queue drops and 2838/1212 UDP receive-buffer errors, respectively. The mechanism moved the bottleneck; it did not make the network lossless.

**Latency and capacity cost:** in wide mode, overlay loaded p95 was 231–267 ms for one flow and 268–274 ms for eight-flow downloads. Eight-flow upload reached **616–654 ms** despite underlay p95 of 200 ms. On a separate 10 Mbit/s FIFO shared with one direct CUBIC flow, enabling wide reduced the tunnel from 4.99 to 3.30 Mbit/s, competing TCP from 0.68 to 0.29 Mbit/s, and raised FIFO drops from 106 to 1887. Do not use this profile to fix congestion or on a latency-sensitive shared link. No blanket clean-path non-regression or real-world speed floor is claimed.

## What the trace isolates

1. The [matched-MSS direct-vs-AWG experiment](../native-awg-rtt/summary.md) reproduced the original low CUBIC speed without either tunnel endpoint. In the [previous encrypted-repair runs](../native-awg-inner-recovery/measurements.tsv), the slow eight-flow cases showed 77–140 sender `Tcp.RetransSegs` per trial with zero hub engine queue drops, UDP receive-buffer errors or excess netem drops. The 1% impairment is real: shrinking TCP windows at long RTT limit application goodput even when the tunnel does not overflow.
2. Diagnostic intervention: leave binaries and CUBIC alone, duplicate ciphertext on the **sending** endpoint's private lab veth before the individually impaired link. Native AWG's replay check admits only the first copy. 153.37 down / 162.01 up Mbit/s with eight flows; underlay cloning is a causal control, **not** shipped. Its scope includes traffic other than TCP data; not an equal-cost speed comparison to the patch.
3. One extra ordinary copy inside AWG raised eight-flow goodput but left the one-flow download at 35.95 Mbit/s in the pilot. With **two** extra copies but no hub admission fix, both one-flow downloads still failed (25.91 / 36.55). Sender `TCP_INFO.total_retrans` deltas were **7463 / 5776**, equal to the corresponding hub native engine queue drop deltas. Neither TUN recorded link drops and neither endpoint recorded UDP receive-buffer errors in those two runs. The counters cover different intervals, so equality is not per-packet correlation. Crucially, this queue is *before* the encoder: no number of on-wire copies repairs an original rejected there.
4. The hub now gives the negotiated destination up to **1 ms per shared TUN read batch**, only when its existing native AWG queue is already full. Queue depth is unchanged. The original still undergoes the same route and ACL checks. At the deadline, the ordinary visible drop remains rather than indefinitely blocking other transports.

## Scope

`BTAWG_AWG_REPAIR=wide` on **both** updated processes authorizes the higher bandwidth cost. The client needs its existing `awg_inner_repair` import capability. A session-key authenticated request and acknowledgement renew a five-second lease. Only eligible unfragmented IPv4 TCP data is replicated; at most two extra, independently encrypted packets per original, subject to bounded queue space. Original delivery does not wait for a block or feedback. The receiver checks exact bytes in a bounded per-session cache; collision eviction can allow a redundant copy through to TCP but cannot discard different bytes. UDP and ACK-only packets are not triplicated. The original adaptive parity policy is the default. Wide is deliberately **not** an automatic deployment-wide change: budget for three data transmissions plus headers/ACKs, and do not assume independent losses on a congested or bursty real link.

## Reproduction

[Receiver goodput and path counters](measurements.tsv) · [Sender TCP and both endpoint traces](trace.txt) · [Allowlisted raw counters, binary digests, runner and reducers](raw.tar.gz). `reserve` is existing adaptive parity; `wide` is the new explicit profile. Download means hub → client. In the final test the endpoint qdiscs do not clone traffic. The `tc-control`, `pilot-one` and `pre-admission` subdirectories of the archive are diagnostic controls, not additional final-build trials. Seeds identify configured loss sequences, not identical dropped data packets after the traffic pattern changes.

Extract the archive into a fresh scratch directory. `python3 -I final-reduce.py final.log` and `python3 -I trace.py final.log` reproduce the published tables. To rerun: rename the archived `final.log`, then `LAB_RUNS="$(cat profiles.txt)" bash final.sh /path/to/matching/checkout` on a Linux host with CPUs 0–7, real TUN and unprivileged user/network namespaces. Sender counters are sampled at about 250 ms; their deltas use the first and last samples **inside** the receiver window. Netem, TUN, UDP, queue and replica deltas span the longer before/after snapshot interval including warmup and teardown. Netem egress bytes exclude dropped packets and are not divided by useful bytes from a shorter interval. No generated private keys, databases, panel credentials or packet payloads are included.
