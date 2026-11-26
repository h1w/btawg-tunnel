# Current transparent TUN and µTP timer check

Managed Linux client, real TUN, eight inner CUBIC flows in each measured direction, identical selective offloads at both loss levels, 2 s warmup and 5 s receiver-byte interval. Independent 1% underlay loss in each direction where enabled. No bandwidth cap for this matrix. These are transparent IP/TCP measurements, not SOCKS CONNECT, even though the config mode names start with `socks5-`.

## Current source before the unsent-DATA timer fix

| Mode | 0%, local down / up | 1%, local | 0%, +200 ms | 1%, +200 ms |
|---|---:|---:|---:|---:|
| Native WG | 1,045 / 910 | 549 / 542 | 329 / 225 | 8.06 / 8.74 |
| Native AWG | 987 / 852 | 306 / 539 | 383 / 135 | 8.98 / 9.37 |
| Extended/µTP | 322 / 442 | 39.82 / 27.43 | 172 / 77.72 | 0.627 / 0.902 |
| Request/µTP | 279 / 299 | 14.85 / 24.29 | 138 / 159 | 0.629 / 0.409 |

Receiver Mbit/s, one sample per cell. All **32/32** intervals completed. With 0% induced loss, managed endpoint queues still overflowed under load (for example 26,929 client queue drops on native AWG LAN download). With 1% +200 ms, all four native cells had zero endpoint queue drops but remained below 10 Mbit/s. Three µTP WAN cells had zero endpoint queue drops; the Extended downlink had 1,260 client queue drops in that trial. No excess transit or UDP receive-buffer drops in any of these 32 intervals. This distinguishes saturation in the endpoints from the remaining WAN limit; it does not prove queue overflow never contributes under loss.

## Unsent DATA timer change

The fixed build starts the retransmission timer when the oldest outstanding packet is first sent. Previously an unsent short tail could cross a zero-credit window when an old RTO expired. The regression failed on the old send; the fixed path waited for credit, sent the tail, and retried it only after its actual RTO. Pure Go and libutp interop passed under `-race`, and Linux hub/client plus Windows amd64 client built.

All **16/16** real-TUN µTP cells on the fixed build completed. At 0% local, rates stayed within 2% of the matched prior build. At 0% +200 ms rates varied and queues still overflowed. At 1% +200 ms, Extended delivered **0.540 / 0.656 Mbit/s**, Request **0.432 / 0.554 Mbit/s**, with p95 **3.94–6.65 s** and zero measured endpoint queue drops in all four cells. This is evidence of correct send timing and continued data delivery, not a WAN throughput improvement.

## RTT-guarded loss cut: lab only

Extended/µTP download on a fresh foreground tunnel per cell, eight CUBIC flows, receiver counters over seconds 4–16. Both arms already include the unsent-DATA timer fix. The overlay retains 7/8 of the congestion window after fast loss instead of half only with fresh delay feedback, RTT at least 50 ms, one-way inflation under 2.5 ms, smoothed RTT within 3 ms of its base, and RTT variation no more than 3 ms. No timer, framing, or queue setting change. The narrow control has 10 Mbit/s downlink, 160 packet slots including propagation, and no injected random loss.

| +200 ms profile | Receiver Mbit/s stock → guarded | Excess transit drops stock → guarded |
|---|---:|---:|
| 0% unshaped | 188.04 → 181.47 | 0 → 0 |
| 1% unshaped, pair 101 | 0.595 → 1.537 | 0 → 0 |
| 1% unshaped, pair 211 | 0.634 → 1.367 | 0 → 0 |
| 10 Mbit/s, pair 101 | 2.709 → 2.590 | 208 → 193 |
| 10 Mbit/s, pair 211 | 2.734 → 2.620 | 231 → 216 |

All ten intervals completed. The stricter guard gained **2.16–2.58×** on this lossy unshaped path without the earlier candidate's narrow-link overflow increase. It still delivered less than 1.6 Mbit/s, lost 4% goodput on the narrow controls, and increased clean WAN hub drops **4,689 → 7,258** in the only clean pair. It was not checked in Request/µTP, upload, or under contention in this series. Do not ship this overlay; it is evidence for the window limitation, not a general congestion fix.

[48 current/timer checks](measurements.tsv) · [10 guarded-controller intervals](guarded.tsv) · [Raw queue and socket data, source snapshots, hashes and replay](raw.tar.gz). After extracting, run `python3 -I matrix/summarize.py current.log timeout-credit.log` and `python3 -I narrow/summarize.py narrow/guarded.log` to reproduce both TSVs. `bash replay.sh <matching-checkout>` rebuilds the arms and repeats the trials.
