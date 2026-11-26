# Loss mitigation experiments: SOCKS TCP split and native UDP redundancy

Two independent interventions on the current native WG/AWG source builds. The hub and client executables have identical digests to [the cause investigation](../native-loss-cause/build.sha256). No production changes. Real Linux TUN, MTU 1280, CPU affinity hub 0–3/client 4–5/load 6–7, selective veth offload settings fixed across variants, independent IPv4 UDP loss injected in both directions on transit. No rate cap. Compare within a test profile only.

## SOCKS5: separate the origin and VPN TCP connections

All clients used the real foreground SOCKS5 listener on `127.0.0.1:1080`. The CUBIC HTTP origin lived in a separate namespace off the impaired path (`203.0.113.9`, lab only). Baseline `nat` uses the shipping hub's scoped masquerade. For the other two variants a lab-only DNAT rule sends the connection to a fixed-destination relay listening on the hub's overlay address. It opens a new connection to the same origin; the only difference between those relay variants is the relay-to-client sender's `TCP_CONGESTION` option. This is not an arbitrary-destination or unauthenticated Internet listener. Socket snapshots verify both algorithms on the expected leg, and forwarding/NAT counters distinguish the paths.

48/48 valid repeated local transfers, loss order 0/1/1/0. One or eight independent HTTP connections, 48 MiB per connection, 216 complete bodies (10.125 GiB), all SHA-256 verified. Speed is aggregate delivered payload divided by wall time, including connection setup. Medians of two, Mbit/s:

| Mode | Streams | Loss | NAT | Relay CUBIC | Relay BBR | BBR vs NAT |
|---|---:|---:|---:|---:|---:|---:|
| WG | 1 | 0% | 863 | 897 | 943 | +9.3% |
| AWG | 1 | 0% | 879 | 892 | 923 | +5.1% |
| WG | 8 | 0% | 980 | 983 | 974 | −0.6% |
| AWG | 8 | 0% | 961 | 926 | 952 | −0.9% |
| WG | 1 | 1% | 98 | 82 | 935 | 9.50× |
| AWG | 1 | 1% | 93 | 82 | 930 | 9.95× |
| WG | 8 | 1% | 467 | 450 | 965 | 2.07× |
| AWG | 8 | 1% | 446 | 437 | 906 | 2.03× |

At 1% and eight streams, median of the per-trial loaded ICMP p95 changed from 1.05 → 19.80 ms (WG) and 1.23 → 15.50 ms (AWG). Transit overhead per received byte did not increase for the relay: eight-flow bidirectional underlay bytes/payload byte were 1.136 → 1.116 (WG) and 1.212 → 1.159 (AWG). Snapshot packet counts were consistent with the configured impairment (0.74–1.18% across both directions of lossy local samples). A separate six-sample WG smoke run also verified all checksums; its 1% single-flow BBR result was only 272 Mbit/s, so the primary local medians are not a guaranteed minimum.

### SOCKS +120 ms RTT, 1% loss

One completed trial per cell, 4 MiB per connection, one or eight simultaneous connections. All 54 bodies passed SHA-256. Mbit/s including setup:

| Mode | Streams | NAT | Relay CUBIC | Relay BBR |
|---|---:|---:|---:|---:|
| WG | 1 | 0.89 | 1.27 | 18.54 |
| AWG | 1 | 1.28 | 1.15 | 19.40 |
| WG | 8 | 8.18 | 6.06 | 140.91 |
| AWG | 8 | 6.77 | 7.57 | 92.67 |

The first runner reached its overall time limit after ten valid trials; the two remaining AWG relay trials ran in a fresh namespace set. Sampled ICMP p95 was 121 ms after rounding in these short transfers; no sustained-load latency guarantee. The payload and timing definition differ from iperf's warm steady-state window, so do not compare directly with native WAN rates below.

## Transparent TUN: duplicate encrypted UDP before the loss point

The sender-side veth qdisc clones outbound packets; the veth splits UDP GSO in software before the downstream transit qdisc applies independent random loss to individual UDP datagrams. There is no new protocol or receiver patch: the native replay filter accepts only one authenticated copy. `dup-down` enables cloning on hub egress only; `dup-both` enables it on both endpoints. This is a transport experiment with stock-format packets in a lab qdisc, not a packaged client setting or an implementation of FEC.

Local real-TUN `iperf3 -R`, CUBIC, 2 s warm-up and 4 s nominal measurement; one trial per cell. Receiver Mbit/s:

| Mode | Streams | At 0%: base → dup-both | At 1%: base → dup-down → dup-both |
|---|---:|---:|---:|
| WG | 1 | 820 → 542 | 123 → 558 → 520 |
| AWG | 1 | 783 → 521 | 83 → 546 → 493 |
| WG | 8 | 823 → 533 | 544 → 558 → 544 |
| AWG | 8 | 776 → 516 | 532 → 546 → 519 |

Single-flow retransmits fell from 494 to 24 (WG) and 350 to 29 (AWG) with downlink-only copies. Eight-flow ICMP p95 at 1% rose from 1.05 → 21.9 ms (WG) and 0.85 → 17.3 ms (AWG). At eight flows the traffic-per-byte index for downlink-only copies was 1.95× (WG) and 1.89× (AWG) relative to base; counts include warm-up and both directions, while the denominator is iperf's measured bytes. These are comparable ratios within this fixed-duration profile, not absolute protocol overhead. Always-on duplication is rejected: it consumed nearly twice the wire capacity, gave little local multi-flow improvement, and cut clean speed by about one third.

### Native +120 ms RTT, 1% loss

One downlink trial per cell, 60 ms per direction. Receiver Mbit/s:

| Mode | Streams | Base | Hub-side duplication | Both sides duplicate |
|---|---:|---:|---:|---:|
| WG | 1 | 2.36 | 95.67 | 67.37 |
| AWG | 1 | 1.31 | 89.13 | 96.73 |
| WG | 8 | 10.22 | 365.96 | 411.29 |
| AWG | 8 | 13.37 | 399.51 | 332.14 |

Eight-flow loaded ICMP p95 was 121 ms with base, 151–165 ms with hub-only and 175–202 ms with both endpoints cloning. This shows loss recovery is effective at high RTT, not that a deployment with a real bandwidth bottleneck or correlated loss would see the same gain. Native runs verify delivered bytes/time, CUBIC on the sender, impairment counters and active duplicate counters; full payload SHA-256 verification applies to the SOCKS runs, not iperf.
UDP receive-buffer errors appeared in two clone cells (3396 in the clean local eight-flow AWG `dup-both` run; 466 in the eight-flow WAN WG `dup-down` run). Duplication creates local load as well as wire overhead.

## Engineering decision

1. For managed download, the measured candidate is a selectable SOCKS TCP split at the hub, with BBR scoped to the relay's VPN-facing sockets. Keep NAT for transparent TUN/UDP and stock clients. A product relay must bind to the overlay, authorize the authenticated group's Internet egress, reuse destination filtering (private, local, overlay and special ranges blocked), bound active connections and propagate half-closes. Do not ship the fixed-destination lab listener or the DNAT rules as a generic proxy. Validate upload separately before changing its Reno policy.
2. Do not enable unconditional duplication. For transparent traffic on managed endpoints, compare bounded parity recovery with base and the clone upper-bound: overhead budget below 15%, 0% throughput non-regression, p95 delay and burst-loss tests. Parity is a separate protocol between peers that both opt in, not a compatible setting on stock UDP WG/AWG. A WAN with correlated loss or close to saturation is explicitly outside the evidence here.

Before switching a managed route to the relay by default: repeat with 0% and 1% at both RTTs, complete a long download and upload, and report 1/8 flows separately. Use a clean goodput guard of −5% versus NAT and require an explicit loaded-RTT budget at the target link speed; the short local +15–19 ms eight-flow increment is not free. Also test an impairment caused by an actual rate-limited queue: unlike the random-loss stand, it can punish a sender that keeps more data in flight.

Other candidates: per-socket BBR without a split is actionable only when we own the sender (for example, client-side upload); it cannot retune an arbitrary origin. Selective retransmission on an authenticated datagram carrier avoids parity during clean periods but recovers losses after at least a feedback round trip. Parity can recover some isolated losses without feedback, at a fixed bandwidth cost. Neither was measured here; neither should be claimed as a native protocol fix.

## Reproducing the evidence

- [Local SOCKS](socks-local.tsv), [SOCKS smoke](socks-smoke.tsv), [WAN SOCKS](socks-wan.tsv), [local native TUN](native-local.tsv), [WAN native TUN](native-wan.tsv), [build digests](build.sha256), [environment](environment.txt). Archive replay verified 98/98 recorded trials, including all 66 SOCKS trials; the interrupted WAN attempt is not a speed sample.
- [Raw files and lab sources](raw.tar.gz) contain JSON counters, `ss -tin` samples, per-stream body digests, trial results, relay source, harness, and verifier. No generated credentials, private keys, databases, or payload files are archived. The payload files are random lab input; the recorded digests are sufficient to reverify the archived receiver checks.
- To reproduce a run, extract the archive in a scratch directory, put the server and client in `bin/`, build the lab relay with `go build -o bin/lab-relay relay.go`, and run `./reproduce.sh socks-local`, `socks-wan`, `native-local`, or `native-wan`. The launcher expects private user/network namespaces, Linux TUN, BBR, `tc`, `nft`, `curl`, `iperf3`, and CPUs 0–7. Use only on a lab host.

To reverify the archived samples after extraction:

```sh
H=$(awk '{print $1}' payload.sha256)
W=$(awk '{print $1}' payload-wan.sha256)
python3 verify.py "socks:run.R2Lr0WnR:6:$H,50331648"
python3 verify.py "socks:run.bL8Lnlpw:48:$H,50331648"
python3 verify.py "socks:run.tt2wAgUB:10:$W,4194304" "socks:run.4uz3dCgO:2:$W,4194304"
python3 verify.py native:run.ROTbrU0i:20:0
python3 verify.py native:run.KVO8c2Qe:12:120
```

