# 200 ms WAN: bottleneck AQM and SOCKS upload

Real foreground SOCKS5 and Linux TUN MTU 1280 with selective offloads held fixed. Added 100 ms each way. 300 Mbit/s rate limit in the measured direction. Applications ran in private network/user namespaces; no host TCP or qdisc changes. Binaries include the half-close fix and the new authenticated `GET /api/traffic` field `engine_queue_drops`. Counts below are before/after deltas; the metric covers only full userspace engine queues, not UDP socket, policy, WAN or TUN drops.

## Download: FIFO versus FQ-CoDel under competition

WG/UDP with the same BBR lab relay, 270 Mbit/s socket pacing ceiling and 32 MiB send autotune max. The same 300 Mbit/s TBF feeds a 1024-packet `pfifo` or `fq_codel` (5 ms target, 100 ms interval, no ECN). A separate netem layer provides delay and random loss. Each trial was a verified 320 MiB download; a 130 Mbit/s unfriendly open-loop UDP competitor shared the bottleneck for seven seconds. It is independent of the encrypted WG aggregate; this does not show isolation of multiple inner streams inside one WG flow.

| Configured random loss | Queue | Whole-file Mbit/s | TCP Mbit/s during competitor | Loaded p95 RTT during competitor | Bottleneck drops / hard limit drops |
|---|---|---:|---:|---:|---:|
| 1% | FIFO | 114.10 | 99.36 | 236.5 ms | 13,043 / 13,043 |
| 1% | FQ-CoDel | 112.50 | 109.53 | 215.5 ms | 9569 / 1728 |
| 0% | FIFO | 141.74 | 146.03 | 237 ms | 16,223 / 16,223 |
| 0% | FQ-CoDel | 166.18 | 141.55 | 234 ms | 25,161 / 6720 |

Two 1% trials per queue with reversed order, one 0% trial. Values are medians where repeated. Drops exclude netem random loss. At 1%, FQ-CoDel reduced tail latency by 21 ms and raised throughput during the competitor by 10%, but did not improve whole-file goodput. At 0%, it dropped more packets overall in this one run, so do not claim a universal drop reduction. All six had zero hub engine queue drops, zero UDP receive-buffer errors, and zero TUN `fq` drops. Observed bottleneck drops were at the transit qdisc, not the userspace hub queues. AQM remains an operator choice for an owned bottleneck, not a hub default.

## Upload: real SOCKS CONNECT, NAT and verified receiver

One 8 MiB HTTP PUT per cell through the shipped client and hub NAT, not the download relay. The receiver reads every byte and returns its length and SHA-256; goodput includes setup and the receipt. `reno` is the production socket policy; `bbr-fixed` changes only congestion control, keeping `SO_SNDBUF=1 MiB` (Linux reports 2 MiB); `bbr-auto` additionally removes the manual socket cap and uses the isolated 32 MiB autotune ceiling. Confirmed controller and buffer in `ss -tim`. These are one-shot controls, not sustained bulk speeds.

| Loss | Mode | Reno / fixed | BBR / fixed | BBR / autotune |
|---|---|---:|---:|---:|
| 0% | WG | 24.71 Mbit/s | 23.56 Mbit/s | 28.01 Mbit/s |
| 0% | AWG | 24.69 Mbit/s | 23.53 Mbit/s | 28.06 Mbit/s |
| 1% | WG | 100 s timeout | 13.62 Mbit/s (4.93 s) | 15.16 Mbit/s (4.43 s) |
| 1% | AWG | 100 s timeout | 12.48 Mbit/s (5.38 s) | 17.04 Mbit/s (3.94 s) |

Both lossy Reno trials were still transferring at the deadline (WG had acknowledged ~6.8 MB in its last socket sample), not zero-byte stalls. Do not use them as completed goodput samples. At 1%, changing the controller alone was sufficient to complete the same upload quickly; autotune saved an additional 10% of completion time on WG, 27% on AWG. Sampled send buffers with BBR autotune reached 8.8–10.0 MiB. Loaded p95 was 201 ms in all lossy cases. There were no hub engine queue, uplink non-random netem or UDP receive-buffer drops in any upload trial.

Keep the production Reno setting for now: this establishes a native-mode WAN upload candidate, not a reason to replace Reno for all carriers. Repeat longer uploads with several clients, shared capacity, and a bounded memory policy before changing the default.

## Evidence

[AQM results](aqm.tsv), [upload results](upload.tsv), [digests](build.sha256), [raw data and lab sources](raw.tar.gz). Extract and run:

```sh
python3 shared-verify.py shared run.BpK8WSJx 6
python3 upload-verify.py run.B8Zvp3hS 12
```

Six verified downloads and ten complete uploads; two Reno attempts timed out with no receiver digest. The [launcher](raw.tar.gz) expects current client/server, `lab-relay` and `upload-receiver` in its `bin/` directory. Build relay from `relay.go` and `monitor.go` in the Go module context, receiver from `upload-receiver.go`; build the BBR comparison client with `go build -overlay` replacing the absolute source path `btawg/internal/tunsetup/tcp_linux.go` with the archived lab `tcp_linux.go`. Run `./reproduce.sh aqm` or `./reproduce.sh upload` in an isolated Linux lab.
