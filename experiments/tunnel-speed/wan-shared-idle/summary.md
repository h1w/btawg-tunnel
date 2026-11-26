# 200 ms WAN: adaptive pacing under contention and idle connection reuse

Linux real TUN WG, one foreground SOCKS5 client, fixed-destination BBR lab relay and CUBIC origin outside the impaired path. +200 ms RTT, same selective offloads and hub TUN `fq`, 32 MiB TCP send autotune ceiling in isolated namespaces. 300 Mbit/s downlink netem, 4096 packet limit, 0% or 1% independent random loss per direction. No production code or global settings changed.

## Shared bottleneck

A 320 MiB SHA-256-verified download overlaps one 130 Mbit/s offered UDP competitor from 5 to 12 seconds after transfer starts. Both enter the exact same impaired downlink qdisc. Compare fixed 270, fixed 150 and a TCP_INFO prototype adjusting the relay socket's `SO_MAX_PACING_RATE` between 60 and 270 Mbit/s. It samples every 250 ms, lowers the ceiling by 18% after two samples over 18 ms above minimum RTT, and increases by 12 Mbit/s after two samples under 8 ms with a cooldown. It is not a general bandwidth estimator. Timing windows and receive counts are verified by the archived scripts; phase rates are TCP bytes acknowledged, not independent completed file measurements.

At **1% loss**, median of two trials per cell (variant order reversed in the second trial). Rates in Mbit/s; phase p95 is loaded overlay ICMP. Queue drops exclude configured random drops.

| Relay pacing | File goodput | Before / during / after competitor | Competing UDP received | p95 during, ms | Queue drops per run, median |
|---|---:|---:|---:|---:|---:|
| Fixed 270 | 125 | 134 / 130 / 156 | 125.1 | 243 | 12,411 |
| RTT adaptive | 132 | 128 / 139 / 151 | 126.0 | 243 | 8,338 |
| Fixed 150 | 123 | 112 / 146 / 142 | 128.7 | 202 | 0 |

At **0% loss**, one trial per cell. Fixed 270 / adaptive / fixed 150 file goodput: 168 / 154 / 132 Mbit/s; queue overflow: 24,757 / 10,656 / 0; cross-phase p95: 244 / 243 / 202 ms. Fixed 150 does not know the path state; it is an intentionally conservative reference chosen for this load, not a portable default. The cross traffic is open-loop UDP and does not yield to congestion. This experiment does not establish fairness against competing TCP or other adaptive flows.

The adaptive loop decreased its cap to 131–148 Mbit/s in the two lossy trials and increased afterward. Relative to fixed 270 it reduced overflow by about 33%, but the loaded p95 did not improve and its full-file result was within the fixed-270 scatter. At +200 ms, the smoothed-RTT trigger plus sampling/cooldown reacts after the queue fills. Fixed 150 prevented overflow and carried more application data during the competing flow; its cost was lower throughput when alone. Do not adopt this delay controller as tested.

First cap reduction came 0.54–1.04 s after cross traffic began, versus a first smoothed-RTT threshold crossing at about 0.29 s in all three runs. The queue was already full by then. Upward recovery to nearly 270 Mbit/s took about 4.5–5 seconds after load removal in the lossy trials. A faster pre-queue signal such as ECN/AQM at an owned bottleneck is a better next control than increasing smoothing or buffers; external ECN support and outer-to-inner signal propagation are not established here.

## Idle HTTP data connection

Separate run with no competitor, same 200 ms / 300 Mbit/s WAN and BBR without extra cap. The same SOCKS data socket is reused for a 64 KiB range after an 8 MiB response, with no traffic intentionally sent during the idle period. Cold opens a fresh socket. Prime and idle time not included in timed request. The verifier checks data-socket identity, exactly one successful SOCKS connect per trial, requested idle duration and response SHA-256. No TLS, longer idle, WAN NAT timeout, or cross-destination pooling in this test.

| Idle before request | 0%: median completion, ms | 1%: median / maximum completion, ms | 1%: median TTFB, ms |
|---|---:|---:|---:|
| Cold | 942 | 940 / 1954 | 405 |
| 0 s | 203 | 204 / 403 | 202 |
| 1 s | 203 | 206 / 406 | 202 |
| 5 s | 203 | 203 / 204 | 202 |
| 15 s | 203 | 303 / 404 | 202 |

Five samples per cell at 0%; four at 1%. The run hit its overall deadline after four complete lossy repetitions (45 timed requests retained, no substituted samples). No warm socket reconnect and no TTFB collapse up to 15 s. The tail after 15 s is not proof of idle window decay: extra ~200 ms also appears at shorter pauses with random loss, and zero-loss samples did not slow down. Report maxima, not meaningful p95s, at this sample count.

## Additional fault: TCP half-close in SOCKS

`btawg/internal/socks/server.go:151–181` returns on the first completed `io.Copy` in both observed and unobserved paths, instead of propagating FIN in that direction and letting the other copy finish. A real loopback SOCKS regression probe calls `CloseWrite()` after writing a request. The origin reads the complete request to EOF and writes a reply successfully, but the SOCKS client sees an empty clean EOF. Reproduced in an isolated namespace. This breaks request-EOF protocols; ordinary HTTP download does not require this half-close, so it does not explain the throughput table. Production code not changed in this measurement task.

Linux foreground and managed SOCKS additionally apply Reno and `SO_SNDBUF=1 MiB` to the overlay socket's **send** side (`btawg/internal/tunsetup/tcp_linux.go:19–25`). This is a concrete upload constraint to verify separately, not a cause of the tested server-to-client download. The relay telemetry in the 1% fixed-270 runs reported cumulative receiver-window-limited busy time of 18–34% at the final sample and zero send-buffer-limited time; simply growing the sender buffer again is not supported by that counter. No UDP receive-buffer errors or TUN `fq` drops in the nine shared runs. The userspace hub engine's separate 256-packet queue can silently reject enqueues (`router.go:345–358`); neither of those kernel counters observes it, so the current run does not estimate its contribution.

## Evidence and next experiment

[Shared results](shared.tsv), [idle results](idle.tsv), [half-close failure](halfclose.txt), [raw logs and lab sources](raw.tar.gz). Archive replay verified 9 completed bulk trials and 45 short responses. From the extraction directory:

```sh
python3 shared-verify.py shared run.csDnkFRf 9
python3 shared-verify.py idle run.u5pRZAta 5,4
```

For a fresh run, build the relay with `go build -o <scratch>/bin/lab-relay <scratch>/relay.go <scratch>/monitor.go` from the btawg module, place the hub/client binaries in `bin/`, and run `./reproduce.sh shared` or `./reproduce.sh idle` in the scratch directory. For the expected-failing half-close probe, copy `halfclose.go` into the btawg module root (Go `internal` import boundary), build it and run with Linux TCP loopback available. These are diagnostic variants, not production components.

Priority: fix half-close independently with a regression check; then instrument the userspace engine queue, and test flow queueing/AQM at a controlled bottleneck before tuning another sender-only RTT feedback loop. Keep application data-channel reuse where possible; do not pay an artificial 8 MiB warm-up for each request.
