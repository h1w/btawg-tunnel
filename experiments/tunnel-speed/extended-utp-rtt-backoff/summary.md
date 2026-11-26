# Extended/µTP WAN: RTT-aware loss backoff

Fast retransmit now cuts the congestion window at most once per smoothed RTT, with the existing 100 ms minimum when RTT is short or unknown. This follows the loss-response cadence in [RFC 6817 §3.3.2](https://www.rfc-editor.org/rfc/rfc6817#section-3.3.2). It does not defer eligible retransmissions or change the queue-delay controller, RTO, framing, or pacing defaults. The regression test reproduced a second cut at 199 ms into a 200 ms round on the old controller; the new controller still cuts on the next round. The focused RTT, RTO, and paced-window tests passed 20 times each under `-race`.

## Matched WAN comparison

Fresh Extended/µTP hub and client per run, +200 ms RTT, eight TCP download streams, no AWG batching, and the [same TCP receiver counter method](../extended-utp-wan-cwnd/summary.md) over seconds 4–16. Independent random loss in each direction when enabled. Same selective offloads and calibrated per-datagram loss model. A = fixed 100 ms loss-cut spacing; Q = RTT-aware spacing. Both include the earlier RTO fix. Unpaced rows use A/Q/Q/A with two paired seeds per loss profile. Each bottleneck arm has one sample: a 10 Mbit/s `netem` downstream rate with a 160-packet queue, no injected random loss. The paced control uses the existing `BTAWG_UTP_PACE_MBPS=200` on both endpoints, not a new implementation.

| Profile | A unpaced, Mbit/s | Q unpaced, Mbit/s | Q paced at 200 Mbit/s, Mbit/s |
|---|---:|---:|---:|
| 0% random loss | 218.540, 219.515 | 207.312, 189.073 | 78.506 |
| 1% random loss | 0.530, 0.621 | 0.595, 0.590 | 12.232, 12.789 |
| 10 Mbit/s bottleneck | 2.632 | 2.523 | 2.977 |

The RTT gate corrects the loss-response cadence, but **does not fix the unpaced throughput collapse**: at 1% the two pairs moved in opposite directions. The median sender window remained about 19 KiB, and the number of cuts moved from 12 to 11 in each repeat. Most cuts were already spaced far enough apart that the RTT gate could not suppress them. The two clean candidate samples were slower than their paired controls and had more hub engine drops; one also reported a fast retransmit cut despite no injected underlay loss. These short runs do not establish the cause of that clean difference or a clean-path speed benefit.

In the bounded-queue control Q still cut the window: five fast-loss cuts, versus three with A, and 212 versus 207 qdisc drops across the whole run. This is evidence of a retained reaction to loss, not a general coexistence/fairness result.

The opt-in paced arm increased 1% goodput roughly 21× relative to Q unpaced, but lowered clean throughput and caused **16,070 qdisc drops** on the 10 Mbit/s control. It combines pacing, faster regrowth, a low-delay window floor and existing recovery settings, so the result does not isolate pacing alone. Do not use the lab's 200 Mbit/s cap as a default on unknown paths. No automatic cap or window floor was enabled.

[Measurements](measurements.tsv) · [Raw counters, RED/GREEN logs, source and runners](raw.tar.gz). The TSV reproduces byte for byte from the archive with `python3 -I reduce.py bench.log paced.log`.
