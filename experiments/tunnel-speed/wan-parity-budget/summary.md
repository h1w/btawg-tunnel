# Parity pacing and bottleneck queue at +200 ms RTT

Linux real TUN / native WG, current automatic BBR relay, one 256 MiB SHA-256 checked HTTP download per cell. CUBIC origin outside the impaired leg, 100 ms one-way transit delay, 300 Mbit/s downlink, same selective veth offloads and 8 MiB accounted relay TCP send memory. Interleaved FEC is the existing lab-only 16+2 downlink shim, four round-robin blocks, uncoded return path. No product or host configuration changes.

## Constant parity reservation in the inner pacing loop

Paired lab A/B with FEC in both builds. The candidate applies `100/114` to every finite computed `SO_MAX_PACING_RATE`, reserving nominal room for the approximately 13.4% extra UDP payload. It does not change initial BBR startup, enforce a wire scheduler, or negotiate FEC. Rates here include connection startup.

| Profile | FEC control Mbit/s | FEC with constant reservation | Loaded p95 control → reservation |
|---|---:|---:|---:|
| 1% independent loss | 132.64 | 103.90 | 217 → 213 ms |
| 0% | 151.37 | 146.97 | 217 → 215 ms |
| 1% plus 130 Mbit/s UDP competitor | 112.06 | 105.10 | 239 → 235 ms during competition |

No hard WAN queue overflow in these six trials. Competitor delivery was 128.69 → 128.67 Mbit/s. Holding ACK-based controller rules fixed while continuously shaving its output slowed its own upward rate discovery, especially on the lossy path; this is a rejected heuristic, not evidence that actual parity scheduling is ineffective. No manual connection mode is added.

During the competing window the sampled relay TCP ACK rate was 96.73 → 109.93 Mbit/s with the reservation. It did not carry that gain through the rest of the file. The ACK rate uses two snapshots inside the window, from 0.7 s after the competitor starts until it ends; it is not the file's verified speed.

## Isolating the outer bottleneck queue

A separate matched 2×2: native versus FEC, and FIFO versus FQ-CoDel as the 1024-packet child of the same 300 Mbit/s TBF after the same delay/loss injection setup. The TCP parity reservation was **off** in all four cells. The competitor offers 130 Mbit/s UDP for six seconds starting four seconds into the transfer. TCP ACK rate and p95 refer to the competing window; whole-file rates include the rest of the transfer.

| Queue / path | Whole-file Mbit/s | TCP ACK Mbit/s under competition | Loaded p95 during competition | UDP competitor delivered | Hard-limit drops |
|---|---:|---:|---:|---:|---:|
| FIFO, native | 68.30 | 29.28 | 201 ms | 128.60 Mbit/s | 0 |
| FIFO, FEC | 99.81 | 72.45 | 285 ms | 126.37 Mbit/s | 1888 |
| FQ-CoDel, native | 65.56 | 23.77 | 202 ms | 128.61 Mbit/s | 0 |
| FQ-CoDel, FEC | 88.57 | 46.08 | 324 ms | 119.55 Mbit/s | 6016 |

FQ-CoDel did not rescue this FEC trace. It added only 30 non-hard-limit drops versus 6016 hard-limit drops. Both selectors see a single encrypted FEC/UDP flow and cannot separate useful packets from parity. Do not generalize this single test to AQM without FEC, and do not compare the TBF rates against the previous netem-rate controls as if only one queue parameter changed. No observed hub engine queue or endpoint UDP-buffer drops.

## Decision and evidence

Do not ship a fixed parity reservation or a register-wide qdisc change. The best observed **whole-file** throughput in this batch remains unreserved FEC (133 Mbit/s solo, 112 with competing UDP), but this alone does not establish an automatic default. A next prototype needs to meter original and parity datagrams at the shim/transport output, prioritize useful data, and discard or withhold parity when there is no credit or the delivery deadline has passed. This batch does not implement that scheduler or negotiate capabilities. One trial per cell with different order of the queue variants; no statistical or public WAN guarantee.

[Measurements](measurements.tsv), [binary digests](build.sha256), [raw traces, verifier and lab sources](raw.tar.gz). Replayed all **10 downloads** from the saved SHA-256 and counter evidence. After extraction:

```sh
mapfile -t profiles < profiles.txt
python3 verify.py "${profiles[@]}"
```

No production source, defaults, or host settings changed. The host `tcp_wmem` maximum was still 4 MiB.
