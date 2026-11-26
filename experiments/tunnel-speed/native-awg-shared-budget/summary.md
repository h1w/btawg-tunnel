# Native AWG shared budget versus direct CUBIC

Real Linux TUN, native AWG UDP, no SOCKS or BitTorrent. The client receives either one or eight inner CUBIC downloads; a single direct CUBIC download starts first and shares the same 10 Mbit/s downlink and 32,000-byte FIFO. Two paired seeds (`101`, `211`), each at 0% and 1% independent underlay loss per direction and 100 ms propagation per direction. Three separate egress points apply the hub's AWG budget, random loss/propagation, and shared bottleneck in that order. The 12 s receiver-byte window starts 5 s after tunnel workload admission. RTT p95 is reported separately for underlay and inside AWG. Queue and shim counters cover the whole trial, not only the receiver-byte window. Matching seeds match configurations, not packet identities. The TCP-only control for a seed/loss combination is shared between 1:1 and 8:1.

| Arm | AWG forwarding | Hub downlink budget |
|---|---|---|
| `tcp` | No AWG traffic | None |
| `shim` | Pass-through AWG datagrams, no recovery | None |
| `cap` | Same pass-through, no recovery | 2 Mbit/s |
| `repair` | Existing adaptive 16+2 repair, four interleaved groups | Same 2 Mbit/s for originals, repair and feedback |
| `fixed` | Same 16+2 framing and interleave, repair always on; 1% cells only | Same 2 Mbit/s for originals and repair |

The lab uses one IPv4 UDP flow (source and destination port 46001) through an HTB class on the hub's egress with a 1,600-byte burst and a 4,800-byte byte-FIFO. Direct TCP and ICMP bypass that class; they are still subject to the same shared downstream bottleneck as AWG. The cap is only on the tested downlink, not the client's uplink. The existing adaptive feedback is unauthenticated and lab-only. Coded framing differs from pass-through even when no repairs are emitted, so the same-cap comparison measures the whole adaptive arm, not parity in isolation.

TCP protection gate per trial pair: direct CUBIC must keep at least 80% of solo, 90% of uncapped coexistence and 90% of capped coexistence without repair; underlay p95 can rise by at most 10 ms versus either coexistence control. Recovery gate: AWG must gain at least 10% over the same cap at 1%, and retain at least 90% at 0%. Thresholds are lab acceptance criteria, not deployment guarantees.

## Results

Throughput in Mbit/s; ranges are the two observed seeds, not confidence intervals.

| Loss | AWG:direct flows | Direct TCP alone | Direct with uncapped AWG | Direct with cap only | Direct with cap + adaptive | AWG with cap only → cap + adaptive |
|---:|---:|---:|---:|---:|---:|---:|
| 0% | 1:1 | 8.95–8.96 | 2.32–2.66 | 7.53–7.60 | 7.57–7.58 | 1.53 → 1.38–1.48 |
| 0% | 8:1 | 8.95–8.96 | 0.90–1.18 | 7.47–7.55 | 7.36–7.40 | 1.62–1.67 → 1.64–1.66 |
| 1% | 1:1 | 0.73–0.86 | 0.675–0.693 | 0.573–0.889 | 0.639–0.668 | 0.483–0.684 → 0.490–0.553 |
| 1% | 8:1 | 0.73–0.86 | 0.653–0.840 | 0.550–0.688 | 0.565–0.605 | 1.555–1.605 → 1.542–1.592 |

- **0%:** the cap, not repair, restored direct TCP to 82–85% of solo with both flow counts. Adaptive passed the clean gates in all four pairs. This traded away tunnel bandwidth: uncapped AWG delivered 6.22–7.95 Mbit/s versus 1.38–1.66 with the capped adaptive arm. Underlay p95 was 221–223 ms at cap + adaptive versus 222–224 ms uncapped.
- **1% adaptive:** no pair passed both gates. The only AWG gain over cap-only was +14.5% at 1:1 seed 101, where direct TCP fell from 0.889 to 0.668 Mbit/s. At 8:1 both pairs lost ~0.8% AWG goodput and failed the TCP gate. Direct TCP retained 66–87% of solo; underlay p95 was 200 ms in every adaptive pair. The hub sent no repair in three of four cells; in the fourth it sent eight shards and the client reported recovering one datagram. Client-to-hub repair was active in all four; that direction was not capped.
- **1% fixed repair:** confirmed hub transmissions and client recovery (96–352 repair shards sent and 10–50 datagrams recovered per cell), but tunnel goodput decreased **3–37%** versus the same cap without repair; direct TCP failed in three of four cells. At 8:1, the limiter dropped 466–515 datagrams with fixed repair versus 232–233 without repair. The common FIFO dropped only 0–20 in both capped arms; the drops moved to the budget queue, not the shared bottleneck.

**Decision: do not ship this policy.** A known-capacity allocation protects direct TCP on the clean shared link, but this fixed cap plus FEC did not deliver a paired speedup that also protected TCP at 1%. The experiment cannot isolate shaping delay from shared-queue delay in the existing repair feedback, or isolate repair framing from recovery. The next useful comparison is a byte-budgeted scheduler that gives originals precedence and discards repair on insufficient credit, separately measuring local scheduler delay and network queue delay. Retest 1:1 and 8:1 before considering automatic rates. No production or host defaults changed.

[32 measurements](measurements.tsv) · [Paired gates](comparison.tsv) · [Independent smoke](smoke.tsv) · [Raw counters, sources and binary hashes](raw.tar.gz). All three TSVs reproduce byte for byte on extraction:

```sh
python3 -I reduce.py budget.log fixed.log > measurements.tsv
python3 -I compare.py measurements.tsv > comparison.tsv
python3 -I reduce.py smoke.log > smoke.tsv
```

`bash replay.sh <matching-checkout>` rebuilds and repeats all 32 cases in fresh namespaces. The four fixed-repair controls were collected as a separate job that overlapped the end of the main matrix on the same host; cross-job CPU load was not isolated. No production adoption rests on those positive-control results.
