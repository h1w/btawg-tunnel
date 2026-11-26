# Loss recovery inside native AWG encryption

The two arms use identical hub and client binaries, MTU 1280, the same native AWG profile, and separate fresh Linux TUN sessions. No external repair proxy, no application proxy and no TCP congestion-control change. `shim` in the raw TSV means the client has disabled the inner extension; `reserve` means it permits the advertised extension. These labels are inherited from the replay runner; neither arm uses a shim process. Every case verifies both overlay addresses with a DF probe at MTU 1280. The target has **100 ms each way**, independent 1% netem loss in both directions, two seconds warmup and a ten-second receiver-byte window. On the shared 10 Mbit/s downlink, a separate CUBIC flow starts before the tunnel workload; loss/propagation and the shared 32 KB FIFO are on separate interfaces. TCP-only controls use the identical shared path. Pair order reverses on seed 211.

Originals, probes, feedback and recovery fragments all pass through the native AWG device. Originals are not held to fill a group. Repair pieces for a full-size original fit the inner MTU; a short tail is packed with its descriptor. The congestion policy uses echoed RTT, packet trains, loss and delay inflation. In-tunnel negotiation and authentication are scoped to an enrolled client's key and its group gateway. A reconstructed uplink must have that peer's source IP; a reconstructed downlink must target the local client; in both cases it goes through the ordinary delivery policy. A session/key change invalidates pending groups and queued controls. Late originals can complete a pending group and a late copy of an already reconstructed packet is suppressed once within a bounded window.

Read `hub_parity_blocks` as groups built, not as proof of delivery; `hub_recovered` counts actual validated reconstructions at the hub (upload). `hub_control_bytes` covers inner packets emitted toward AWG encryption, including probes, and not just parity. These counters span the whole trial; goodput and p95 cover the receiver window. The two lossy seeds are matched configurations, not identical dropped packet identities or confidence intervals. A 0% cell means no induced loss, not necessarily zero queue drops.

## Compatibility and rollout

Refresh the native client's import after upgrading the hub: only a new hub advertises `awg_inner_repair`. With that capability, the new client negotiates automatically unless started with `BTAWG_AWG_REPAIR=0`. Standalone AWG exports and clients which have not negotiated the extension continue to carry ordinary IP. No packet bypasses the normal AWG encryption and header protection. The extra traffic still affects sizes, timing and byte counts; the claim is absence of a separate clear protocol, not immunity to traffic analysis.

Mixed-version Linux checks used a real TUN, not a simulated bind. Both old-client/new-hub and new-client/old-hub pairs delivered all three 1280-byte DF probes in each direction. The old hub import lacked the capability. The new hub emitted zero inner control bytes and zero parity blocks for the old client.

A separate lossy upload smoke with the final binaries observed 25,959 AWG UDP datagrams on the physical-namespace veth while the hub reconstructed 130 inner packets. None of the observed payloads started with the previous clear `AW1` prefix or a plaintext marked inner-control IPv4 header. The archive contains aggregate counts (`wire.json`, `wire-recovery.json`), not the captured payloads. This is a wire-format check, not a DPI-resistance test.

The clean-path bypass suspends bulk hashing/caching/probing when unassisted traffic already sustains 32 Mbit/s; it rechecks on slower traffic and does not discard state in an active repair flight. Focused tests cover one and multiple gaps, short and full-MTU packets, out-of-order fragments and originals, replay, wrong addresses, revocation, a wake during an idle TUN read, Linux direct reads and the portable fallback.

## 1% loss, +200 ms RTT, no bandwidth shaper

Each entry is **disabled → enabled**, Mbit/s delivered to the receiver. The two columns are independent paired seeds.

| CUBIC flows | Direction | Seed 101 | Seed 211 |
|---:|---|---:|---:|
| 1 | Download | 0.911 → 1.265 | 0.569 → 0.798 |
| 1 | Upload | 0.615 → 1.822 | 0.906 → 3.679 |
| 8 | Download | 7.453 → 12.206 | 6.601 → 8.916 |
| 8 | Upload | 7.448 → 19.436 | 7.268 → 15.488 |

All eight lossy unshaped pairs improved. The eight-flow gain was **1.35–1.64× download and 2.13–2.61× upload**, not the result of the older outer-wire prototype. Loaded p95 inside AWG was 201–202 ms, changing by at most 1 ms within a pair. The hub reconstructed 113 and 103 originals in the eight-flow uploads, versus zero with recovery disabled. In the corresponding downloads it generated 1,099 and 775 parity groups. One of the eight-flow download pairs did not reach 1.5×. The 1-flow download remained close to 1 Mbit/s; the feature does not remove the entire WAN penalty.

## Shared bottleneck and clean controls

On the 10 Mbit/s shared downlink, the hub generated **zero parity blocks in all six enabled cases**, including both clean cases. Only small control exchanges remained. At 1% the competing direct CUBIC received 0.671 / 0.825 Mbit/s in the 1:1 / 8:1 seed-101 cases (versus 0.681 / 0.692 disabled), and 0.795 / 0.598 in seed 211 (versus 1.018 / 0.624). Thus probe/measurement mode produced no repeatable shared-link tunnel acceleration, and **one pair was 22% worse for competing TCP** than disabled. All retained at least 86% of their paired TCP-only rates. A short two-seed comparison does not prove fairness or explain that scatter.

At 0% induced loss, no hub parity blocks or hub reconstructions were recorded. On the unshaped clean path, 1-flow down/up changed 78.3/201.4 → 94.4/129.8 Mbit/s and 8-flow 323.5/327.5 → 262.8/305.6. This single clean run does **not** establish clean-path non-regression; hashing, probes and run-to-run variation have not been isolated. A clean 8:1 FIFO left the competing flow 1.34 → 1.56 Mbit/s against 8.34 alone. Use the disable switch for an A/B on a particular connection, especially on a clean high-throughput path. No changes were made to the host congestion controller or queue defaults.

## Evidence

[39 receiver windows](measurements.tsv) · [Matched pairs including direct TCP](comparison.tsv) · [Raw receiver data, counters, scripts and binary digests](raw.tar.gz). After extracting into a new directory, run `python3 -I final-reduce.py final.log > measurements.tsv` and `python3 -I evidence.py measurements.tsv > comparison.tsv`. `bash final.sh <matching-checkout>` builds both endpoints and repeats the suite; use a fresh directory because the runner appends its log.
