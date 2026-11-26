# Extended/µTP window policy versus direct TCP CUBIC

**Decision: lab only.** The candidate improved solo lossy goodput in both pairs but failed one of four CUBIC coexistence gates. It did not improve µTP goodput in either lossy shared-link pair. No production source, wire format or defaults changed.

## Policy and test

The baseline halves cwnd on fast retransmit at most once per smoothed RTT, with a 100 ms floor. The overlay keeps 7/8 of cwnd only at smoothed RTT ≥50 ms after at least two RTTs of quiet feedback: any missing one-way delay sample or newest sample ≥2.5 ms above its base resets the quiet interval. On these WAN paths it also caps the LEDBAT target at 5 ms instead of the stock 100 ms. RTT gate, timers, pacing (off), receive credit and wire format are unchanged. This is a comparison of the two changes together, not attribution to either knob.

Fresh Linux real TUN, Extended/µTP download, eight inner CUBIC flows. For coexistence, one separate direct CUBIC connection starts first and shares a 10 Mbit/s downlink with a 32,000-byte FIFO; propagation delay is imposed separately so it does not occupy that FIFO. TCP-only controls run on the same profile without tunnel traffic. Both loss levels use +200 ms RTT and the same per-packet offload settings. The two 1% netem directions have independent seeds; matching seeds reproduce the configuration, not packet identities across different traffic mixes. Throughput is receiver `TCP_INFO` byte deltas over seconds 5–17 after tunnel workload start (equivalent timestamp in TCP-only controls). RTT is direct underlay ICMP p95 over that window, not latency inside the tunnel. Queue and loss counters span the whole trial.

The gates were set before the full matrix: ≥1.5× solo lossy µTP goodput in each pair; in each coexistence pair CUBIC must retain ≥90% of stock coexistence and ≥80% of TCP-only goodput, with no more than +10 ms underlay p95. A pass is evidence only for the tested profile, not equivalence or a deployment guarantee.

## Solo throughput

| Loss | Seed | µTP stock → candidate, Mbit/s | Loaded p95 RTT, ms |
|---|---:|---:|---:|
| 0%, unshaped | 101 | 142.651 → 136.909 | 200 → 200 |
| 1%, unshaped | 101 | 0.584 → 1.326 (2.27×) | 200 → 200 |
| 1%, unshaped | 211 | 0.576 → 1.812 (3.15×) | 200 → 200 |

## Shared bottleneck

| Loss | Seed | µTP stock → candidate, Mbit/s | TCP only / with stock / with candidate, Mbit/s | Loaded p95 stock → candidate, ms | FIFO drops stock → candidate |
|---|---:|---:|---:|---:|---:|
| 0% | 101 | 2.333 → 0.332 | 8.965 / 6.058 / 8.874 | 223 → 222 | 127 → 51 |
| 0% | 211 | 2.084 → 0.309 | 8.949 / 6.251 / 8.900 | 223 → 223 | 126 → 53 |
| 1% | 101 | 0.679 → 0.641 | 1.212 / 0.872 / 0.696 | 200 → 200 | 39 → 40 |
| 1% | 211 | 0.650 → 0.621 | 0.912 / 0.710 / 1.016 | 200 → 200 | 0 → 0 |

Without injected loss the candidate yielded to CUBIC, which recovered 99.0–99.5% of its solo throughput. With loss, CUBIC was already below 1.22 Mbit/s when alone. In the failed pair it retained 79.8% of its stock coexistence rate and 57.4% of its TCP-only rate. The second pair moved the other way. The loss profiles had no persistent queue inflation in the p95 and the first pair had some FIFO drops; neither result proves that the soft-cut branch caused the TCP difference. There were no excess netem drops or UDP receive-buffer errors. There were 138 hub engine drops in one guarded solo lossy run and none in the other three solo lossy trials.

Next discriminator: hold the 5 ms target fixed and compare half-cut against guarded 7/8 with longer receiver windows and cut counters, on both lossy shared profiles. That separates the loss policy from the delay target and distinguishes a repeatable TCP penalty from seed scatter. This batch covers download, one carrier variant, one shared rate/queue and one competing CUBIC flow; it does not qualify a default.

[18 receiver windows](measurements.tsv) · [Paired results and gates](comparison.tsv) · [Raw counters, scripts, source overlay and binary hashes](raw.tar.gz). Both TSVs reproduce byte for byte after extracting the archive:

```sh
python3 -I reduce.py pilot.log matrix.log > measurements.tsv
python3 -I compare.py measurements.tsv > comparison.tsv
```

`bash replay.sh <matching-checkout>` rebuilds both arms and repeats the Linux namespace experiment.
