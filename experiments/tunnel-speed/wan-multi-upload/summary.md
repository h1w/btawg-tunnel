# Three concurrent SOCKS uploads: Reno versus BBR

Three separately registered real Linux TUN/POSIX SOCKS clients (two WG, one AWG) and one unmodified hub NAT; receiver outside impaired hop. Concurrent HTTP PUT of 32 MiB per client. Two paired rounds per profile; start order reverses across rounds. Fixed `SO_SNDBUF=1 MiB` (Linux reports 2 MiB), identical `fq` TUN qdiscs, selective veth offloads and affinity (hub 0–3/client 4–5/load 6–7). Only the TCP controller differs between builds. BBR uses a lab overlay, not a runtime option or production change.

| Profile | Reno | BBR | Worst per-client p95 per round, Reno → BBR |
|---|---|---|---|
| No added RTT, no rate cap, 0% | 625–646 Mbit/s | 720–731 Mbit/s | 82–124 → 15–23 ms |
| +200 ms RTT, 300 Mbit/s shared uplink, 0% | 127–129 Mbit/s | 124–125 Mbit/s | 203–222 → 203–205 ms |
| +200 ms RTT, 300 Mbit/s shared uplink, 1% bidirectional | 0/6 complete by 58 s | 6/6 complete, 69.0–69.5 Mbit/s aggregate | 201 → 201 ms |

Goodput is all completed payload bytes divided by wall time from first start to last successful receipt for a round, including setup. At 1%, all three BBR transfers completed in 11.58–11.67 s per batch. Reno had 13.30–14.83 MiB of TCP bytes acknowledged across all three flows in the final socket samples, not a zero-byte stall and not a completed speed result. Archive replay verified 30 successful body receipts (length and SHA-256) and six explicit timeouts, three distinct overlay IPs per trial, controller and fixed buffer on every measured socket, and exact delay/rate/loss configuration. All trials had zero non-random uplink drops, hub engine queue drops, and UDP receive-buffer errors. Local selective-offload result is not a comparison to the earlier default-offload clean build A/B.

## Decision

Do not expose an upload mode for users to choose. BBR on Linux native SOCKS upload is a no-switch default candidate: within ~3% of Reno on clean +200 ms and substantially better on the measured lossy link; local profile also improved. These measurements do not establish a transparent TUN/download fix, cross-traffic fairness, all-carrier behavior, or Windows behavior. A product default should change only established native SOCKS outbound TCP sockets we own, automatically retain the existing policy where BBR is unsupported, and preserve the fixed memory bound in the initial change. A full-path design for arbitrary origins and transparent traffic needs a mutually supported loss-aware carrier or split data path; do not imply that client BBR changes TCP congestion control on someone else's sender.

[WAN samples](wan.tsv), [local samples](clean.tsv), [raw data and harness](raw.tar.gz), [binary digests](build.sha256). Extract and run `python3 verify.py run.RJFZ4Nn4 24` and `python3 verify.py run.RJWd0tC1 12 0 0`. For fresh runs, put baseline hub/client in `bin`, build `upload-receiver.go`, and build `bin/candidate` from the identical client tree with Go `-overlay` replacing `internal/tunsetup/tcp_linux.go` by the archived lab source. Then run `./reproduce.sh wan` or `./reproduce.sh clean` in an isolated Linux lab.
