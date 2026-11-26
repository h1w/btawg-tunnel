# Extended/µTP: batching only already-ready packets

Follow-up to the [stage profile](../extended-utp-stages/summary.md). The experimental hub takes the first AWG outbound container normally, then takes at most three more already-encrypted packets for the same peer using a nonblocking queue read and `TryLock`. An unfinished container stays next in FIFO; there is no batching timer or wait for a fuller batch. For a TLS write with multiple ready fragments, Extended frames are written together into the existing µTP send path. Protected record and Extended framing formats, queue caps, MTU and congestion control are unchanged. This experiment tests the two changes together, not their individual effects. Neither was applied to product source.

A/B/B/A on real Linux TUN, production Extended protected-record negotiation, identical client binary, 0% injected loss, no rate cap, stock offloads. CUBIC download only, 1 and 8 streams, 2 s omitted and 6 s nominal per trial. Delayed profile adds 100 ms in each direction. Table values are medians of two runs per build, not confidence intervals. Drops and normalized drop counts include warmup; submission and CPU ratios use timestamped samples inside the measurement window. Submission counts include single UDP writes, batch writes and GSO attempts (application-level calls, not a syscall trace). MiB/GiB denominators are UDP payload bytes submitted by the hub; CPU is total hub process user + system time.

| Added RTT | Streams | Goodput Mbit/s A → B | Loaded p95 ms A → B | Hub drops A → B | Submits / UDP MiB A → B | Hub CPU s / UDP GiB A → B |
|---|---:|---:|---:|---:|---:|---:|---:|
| 0 ms | 1 | 323 → 483 | 16.9 → 7.1 | 390 → 108 | 682 → 193 | 38.7 → 28.4 |
| 0 ms | 8 | 318 → 485 | 44.7 → 10.8 | 339 → 4,838 | 683 → 191 | 42.2 → 29.1 |
| +200 ms | 1 | 61.3 → 59.8 | 203 → 204 | 3,363 → 2,861 | 686 → 220 | 52.5 → 39.8 |
| +200 ms | 8 | 178.7 → 167.7 | 279 → 212 | 4,544 → 4,403 | 684 → 204 | 62.1 → 42.6 |

The experiment cut UDP submissions per MiB by **68–72%** and hub CPU per UDP GiB by **24–31%**. Average datagrams per counted submission rose from about **2.0** to **4.0–4.5**. Compared with baseline, local goodput increased at both stream counts, though one-stream B varied from 387 to 579 Mbit/s. Local eight-stream hub drops still rose after normalization (0.13% → 1.23% of hub TUN qdisc packets); inner TCP retransmissions increased from a median 133 to 3,007. At +200 ms and eight streams, the absolute drop count was similar and the normalized rate was 3.26% → 3.24%; p95 fell, but neither B run matched the two A runs' throughput. At +200 ms with one stream, goodput and drop ranges overlapped. Do not enable by default on these measurements: the UDP sender is cheaper, but that did not translate to a general drop fix.

In a separate +200 ms smoke, the unmodified client downloaded four concurrent 8 MiB copies over the experimental tunnel. All four SHA-256 digests matched. The expected digest and received digests are in the archive; to rerun, generate `input.bin` and `input.sha256` next to the lab scripts and source `integrity.sh` via `LAB_CASES` with the B binaries.

All 16 trials completed. No recorded qdisc drops, UDP receive-buffer errors or UDP submission errors. [Throughput and queue counters](measurements.tsv) · [UDP submission and CPU counters](submissions.tsv) · [Raw traces, overlays and runners](raw.tar.gz). Extract the archive and run `python3 compare.py run.log`, `python3 submissions.py run.log` to replay the TSVs. `bash run.sh <repository-root>` rebuilds and runs the matrix in isolated namespaces.
