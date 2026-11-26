# Parallel ranges on the current SOCKS relay at +200 ms RTT

Linux real TUN native WG, current automatic BBR socket policy and integrated hub relay, 100 ms transit delay per direction, 300 Mbit/s downlink cap, selective veth offloads. One 256 MiB known file from a range-capable HTTP origin outside the impaired hop. All trials verify exact `206` offsets and assemble the entire file before SHA-256 verification. Wall time includes SOCKS connection setup and hashing. Parallel cases use multiple independently established BBR TCP connections in one WG tunnel. FEC is the existing lab-only downlink interleaved 16+2 shim; no protocol or production change.

| Profile | Path | 1 stream, Mbit/s | 2 streams | 4 streams | p95 1 / 2 / 4 |
|---|---|---:|---:|---:|---|
| 1% random, solo | Native | 61.88 | 90.28 | 89.53 | 202 / 202 / 202 ms |
| 1% random, solo | Interleaved FEC | 117.35 | 137.34 | 121.22 | 209 / 221 / 216 ms |
| 0% | Native | 148.46 | — | 164.13 | 205 / — / 233 ms |
| 0% | Interleaved FEC | 143.41 | — | 173.00 | 216 / — / 235 ms |

The same-seed solo lossy pair with two native streams ran at +46% without WAN overflow or increased measured p95. Four did not add throughput. With four static FEC ranges the quickest finished in 8.62 s and the slowest in 17.34 s; the file cannot finish before its last range. Four flows at 0% increase throughput but overflow the WAN queue (395 native / 174 FEC extra drops) and increase p95. So `always use 4` is not supported. One sample per cell; absolute numbers are not comparable to earlier tests at different seeds or with other receive loops.

## Shared bottleneck and persistent scheduling

At 1% with a separate UDP competitor offering 130 Mbit/s for six seconds, the same-file rates were:

| Path | 1 stream | 2 streams | 4 streams | Overlap p95 1 / 2 / 4 | Extra WAN drops 1 / 2 / 4 |
|---|---:|---:|---:|---|---|
| Native | 64.32 | 96.73 | 97.35 | 201 / 201 / 220 ms | 0 / 0 / 0 |
| Interleaved FEC | 114.94 | 95.71 | 115.65 | 222 / 239 / 243 ms | 0 / 1373 / 810 |

Native two streams again improved total file speed by ~50% with no measured overlap RTT or WAN drop penalty. Combining FEC and concurrency did not give a dependable additional gain under competition. One solo single-stream FEC trial also had 160 hub engine queue drops; none of the other cells sampled such drops. All endpoint UDP receive-buffer deltas were zero.

The persistent prototype gives 16 MiB ranges to two reused SOCKS connections in completion order, checks exact ranges, and writes them directly into the temporary output. Compared with two static ranges, it was slower on this file: **77.50 vs 90.28** native and **127.54 vs 137.34** FEC solo; **69.35 vs 96.73** native and **94.37 vs 95.71** FEC with competition. That variant adds serial request RTTs per worker and changes the HTTP receiver implementation; the combined prototype regressed, not a clean estimate of RTT versus copy cost. It is not a candidate for deployment.

## Decision

For a range-aware client downlink, two independent native requests are the simplest measured win without changing the hub or client service. The harness validates one fixed HTTP origin; a real downloader must also keep representation identity stable between requests and fall back if the origin does not support ranges. This is not a transparent proxy feature—SOCKS cannot split an arbitrary HTTPS byte stream without application cooperation. Do not add blanket extra connections, plaintext HTTP parsing to SOCKS, or another manual transport setting. A product-wide improvement still requires the negotiated loss-recovery/data transport already identified in the previous reports; this experiment does not implement it.

[Measurements](measurements.tsv), [raw traces and scripts](raw.tar.gz), [binary digests](build.sha256). Replay after extraction: `mapfile -t profiles < profiles.txt; python3 verify.py "${profiles[@]}"`. It verifies **20 complete assembled SHA-256 payloads**, exact HTTP byte ranges and the number of simultaneously observed relay TCP connections carrying body data. The archive excludes payloads and enrollment secrets. No product source or host configuration changed.
