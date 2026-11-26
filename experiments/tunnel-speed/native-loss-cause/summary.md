# Native WG/AWG: why TCP slows down at 1% loss

Current source binaries, real Linux TUN, single hub, MTU 1280, selective **identical** veth offload settings at 0% and 1%, no configured rate shaping or balancer. `netem` on both egress interfaces of a transit namespace, with independent seeds per direction; UDP segmentation before `netem` calibrated in both directions. Hub affinity 0–3, client 4–5, iperf 6–7. `iperf3 -R` to the hub's overlay IP, 2 s warm-up and 4 s nominal measurement. **Downlink through TUN, not SOCKS or Internet/NAT egress.** No production code or host configuration changes. Do not compare with the default-offload clean build A/B to estimate a code change.

## Same offloads, same local topology, 0% vs 1%

Two samples per displayed median, loss order 0/1/1/0. Mbit/s delivered by the receiver.

| Mode | CUBIC 1 flow: 0% → 1% | CUBIC 8 flows: 0% → 1% | BBR 1 / 8 flows at 1% | UDP at 1%, offered 750 |
|---|---:|---:|---:|---:|
| WG | 847 → 100 | 860 → 551 | 1002 / 988 | 737 |
| AWG | 812 → 107 | 828 → 534 | 1004 / 981 | 740 |

- Single-flow sender `snd_cwnd` (median of 1 s interval samples) fell from 518 to 2.4 KiB for WG and from 816 to 2.7 KiB for AWG. At 1%, switching **only the iperf sender congestion controller** restored large flight windows and 9.4–10.0× goodput. BBR did not eliminate drops or retransmits. At 1%, eight-flow loaded ICMP p95 ranged from 0.61–0.98 ms for CUBIC vs 11.1–12.9 ms for BBR.
- For local 1% TCP trials netem counted 0.89–1.05% loss across both directions. TCP trials had zero UDP socket `RcvbufErrors`, TUN link errors/drops, TUN qdisc drops and IPv4 fragments. These counters do not instrument the userspace router queue. The independent UDP workload (1200-byte datagrams) did have additional receiver socket drops, and its 1% trials reported 1.11–1.80% application loss. That control establishes available throughput, not absence of local drops under all workloads.
- One clean AWG BBR eight-flow trial failed during connection setup and was excluded: the server accepted six data sockets, no measurement started, and the client hit the timeout. 39/40 valid local trials. The lossy BBR vs CUBIC conclusion does not use this clean cell. The 0% eight-flow CUBIC loaded p95 varied widely (WG 28–196 ms, AWG 33–144 ms); the clean latency numbers are not a stability claim.
- Historical `after-native-batching` all-off versus defaults at **0%**, within that experiment: eight-flow downlink fell 59% WG / 43% AWG even without induced loss. Adding 1% in the same all-off setup reduced the resulting goodput another 27% / 33%. The offload cost and genuine loss cost are different effects.

## 1% and +120 ms RTT

One sample per cell; separate WAN control, not a sustained capacity claim. Mbit/s. The first runner hit its total time limit after seven valid samples; the three remaining WG tests ran in a fresh namespace set.

| Mode | CUBIC 1 / 8 flows | BBR 1 / 8 flows | UDP at 750 offered | Loaded ICMP p95, 8 flows, CUBIC → BBR |
|---|---:|---:|---:|---:|
| WG | 1.05 / 11.80 | 101 / 592 | 738 | 121 → 145 ms |
| AWG | 2.10 / 7.08 | 97 / 717 | 736 | 121 → 158 ms |

The test does not establish why single-flow WAN BBR stopped near 100 Mbit/s or what it can sustain in a longer run. Sparse TCP trials have noisier observed netem loss. The shaped impairment was delay/loss only; no rate cap.

## Decision

Keep `router.go`, `flow_routes.go`, native WG/AWG wire formats and system defaults as they are. For a TCP sender we own, measure per-socket BBR against the actual application's controller before adopting it; add a loaded-RTT guard, because higher goodput came with larger queues at 1%. The Linux SOCKS dialer currently selects **Reno**, not the CUBIC used by this iperf control. In regular SOCKS download the origin sends TCP; the hub does NAT and cannot set congestion control on someone else's socket.

For downloads from arbitrary origins, first do a completed-payload SOCKS control through the real WAN/NAT topology with a controlled origin (same CUBIC origin, 0%/1% on the VPN leg, no loss on origin leg). Compare transparent NAT with a separate TCP relay that terminates the origin leg and runs a per-socket BBR VPN leg. This only applies to managed SOCKS TCP. If the requirement covers transparent TUN/UDP and stock WG/AWG clients, the relay is insufficient; loss recovery requires an additional mutually supported transport, not a change to standard native packets. Success criteria: compare 1 and 8 flows, both directions; hold offloads, MTU, CPU placement and RTT fixed for each A/B; validate entire received payload, retransmits, loaded RTT and 0% non-regression. Repeat longer at WAN RTT before picking a production default.

## Evidence

- [Verified measurements](measurements.txt), [binary digests](build.sha256), [environment](environment.txt), [raw counters, JSON, and runners](raw.tar.gz).
- 49 valid of 50 recorded attempts. Recheck after extracting `raw.tar.gz` from the extraction directory:

```sh
python3 verify-loss.py run.8IJdzURE:40:0 run.ImiQLXGx:7:60 run.z8l5IgaK:3:60
```
