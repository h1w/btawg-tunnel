# Transparent native AWG recovery and TCP coexistence

## Method

Fresh Linux namespaces and a real TUN session for each trial, eight inner CUBIC downloads through native AmneziaWG/UDP, no SOCKS or BitTorrent. The lab shim forwards encrypted datagrams between a fixed client/hub pair. `shim` is pass-through; `fec` always sends two repair shards per 16 originals across four interleaved groups; `adaptive` uses the same code and wire framing but starts with repair off. It enables repair at 0.3–3% raw missing originals in a report with at least 128 finalized originals. A report with at least 8 ms of maximum one-way delay above the observed minimum disables repair, even without enough loss samples. Three ordinary out-of-range loss reports disable it as well. Repair checks feedback freshness at new group boundaries (1.5 s TTL). The shim does not delay originals to accumulate a group, pace traffic or reserve bandwidth. Clock comparison, receiver reports and fixed addressing are for the isolated lab and not a production protocol; reports are not authenticated.

Independent 1% random loss in each direction where enabled; always 100 ms propagation per direction. FIFO cases put the delay/loss and bandwidth limiter on separate interfaces. A direct CUBIC flow and forward tunnel packets share the same 10 Mbit/s downlink with a 32,000-byte FIFO; the direct flow starts first. TCP-only controls omit tunnel traffic and keep the same impairment. Goodput is measured from receiver TCP byte counters at seconds 5–17 of the transfer. RTT p95 is sampled separately over the underlay and inside AWG during that window; shim, FIFO and other drop counters span the whole trial. A paired seed matches the configuration, not particular dropped datagrams across traffic mixes.

The speed gate on the unshaped lossy path is ≥1.5× stock goodput with no more than +35 ms loaded overlay p95. The clean solo control requires ≥90% stock goodput, no more than +10 ms overlay p95 and zero repair datagrams. On the shared link direct CUBIC must retain ≥90% of its goodput with the stock shim and ≥80% of its TCP-only goodput, with no more than +10 ms underlay p95. Lossy shared-link cells also require ≥10% more tunnel goodput. These are lab acceptance criteria, not general guarantees.

## 1% loss, unshaped downlink

| Seed | Pass-through, Mbit/s | Always-on repair, Mbit/s | Adaptive repair, Mbit/s | Overlay p95 pass-through / always / adaptive, ms |
|---:|---:|---:|---:|---:|
| 101 | 6.650 | 198.417 | 76.780 | 202 / 214 / 212 |
| 211 | 5.295 | 165.524 | 55.854 | 202 / 227 / 211 |

Adaptive goodput rose **10.55–11.55×**, with **11.94–12.36%** more UDP payload than the originals alone across both directions (whole trial). The client recovered **613–1,042** datagrams; the hub recovered **309–442**. Always-on was faster but used **14.63–14.65%** extra payload and raised overlay p95 by **12–25 ms**, versus **9–10 ms** for adaptive. Underlay ICMP p95 was unchanged; the extra delay is inside the tunnel path covered by overlay pings. None of these six shims reported an invalid frame.

## Shared 10 Mbit/s bottleneck

| Loss / seed | Pass-through tunnel / direct TCP | Always-on tunnel / direct TCP | Adaptive tunnel / direct TCP | Direct TCP alone | Underlay p95 pass-through / always / adaptive, ms |
|---|---:|---:|---:|---:|---:|
| 0% / 101 | 7.699 / 1.176 | 7.545 / 0.410 | 7.448 / 1.242 | 8.961 | 223 / 224 / 224 |
| 0% / 211 | 7.946 / 0.883 | — | 7.649 / 1.160 | 8.966 | 224 / — / 223 |
| 1% / 101 | 5.179 / 0.848 | 7.659 / 0.297 | 6.362 / 1.160 | 1.047 | 202 / 224 / 212 |
| 1% / 211 | 5.025 / 0.845 | 7.811 / 0.296 | 6.268 / 0.843 | 0.987 | 205 / 224 / 213 |

Throughput is Mbit/s. At 1%, always-on repair cut direct TCP by about **65%** versus its coexistence baseline. FIFO drops went from **145 / 70 to 467 / 464**. Adaptive passed both lossy coexistence gates: +23–25% tunnel goodput while direct CUBIC retained 100–137% of baseline coexistence and 85–111% of TCP-only. FIFO drops still increased by **11 / 21**, underlay p95 by **10 / 8 ms**, and overlay p95 by **17 / 9 ms**. Queue feedback suspended repair eight and nine times, respectively. These two pairs do not establish general fairness.

The clean FIFO result is a distinct failure: even without repair, eight tunneled flows left the one direct TCP flow only **10–13% of its solo rate**. The adaptive branch did not make that baseline worse, but it did not bring back coexistence either: CUBIC reached **13–14%** of solo. It also emitted 40 repair packets in one clean FIFO run, despite no injected loss; the queue itself can lose packets. On the unshaped clean pair, adaptive emitted no repair and achieved 357 vs 315 Mbit/s, but overlay p95 rose **294 → 357 ms** and UDP receive-buffer errors rose **0 → 803**. This single clean pair does not isolate a causal contribution from the framing versus run-to-run load scatter, but it fails the clean acceptance gate.

## Decision

**Do not ship either variant as a default.** Keep the adaptive branch as evidence that loss recovery can help regular TUN traffic and meet the chosen competition gate under lossy conditions. A next lab arm should use a real native bypass when repair is off and charge original plus repair bytes in a common, delay-sensitive output budget that can also yield tunnel traffic under contention. Merely suppressing repair cannot restore clean coexistence if pass-through itself already crowds the direct flow. Benchmark 1:1 as well as 8:1 flow counts, multiple link rates, and both transfer directions before generalizing. No production code, TCP defaults, SOCKS behavior or AWG format changed.

[23 receiver windows](measurements.tsv) · [Paired comparisons and gates](comparison.tsv) · [Raw counters, codec, scripts and binary hashes](raw.tar.gz). Both TSVs reproduce byte for byte after extracting the archive:

```sh
python3 -I reduce.py recovery.log > measurements.tsv
python3 -I compare.py measurements.tsv > comparison.tsv
```

`bash replay.sh <matching-checkout>` rebuilds the endpoints, runs the codec and feedback policy assertions, and repeats the namespace matrix.
