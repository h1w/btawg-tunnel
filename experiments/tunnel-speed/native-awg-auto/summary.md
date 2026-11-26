# Native AWG auto: live directional recovery

`BTAWG_AWG_REPAIR=auto` on both endpoints opts an enrolled native AWG peer into an authenticated profile distinct from `coded` and `wide`. No global TCP/sysctl/qdisc changes. Plain originals continue to flow during negotiation. A receiver samples ordinary IPv4 TCP data identities *before* reconstruction, compares them to compact sender lists, estimates delivery rate and relative one-way queue delay, and returns a short-lived grant for its incoming direction. The reverse data direction has its own state. The sender does not send coded parity without a fresh grant. While coding is inactive the endpoints still observe TCP SYN options; changing TCP credit on a live connection does not require reopening it. Lack of measured capacity, insufficient RTT, stale feedback and sustained clean samples are distinct from evidence of random loss. No whole-data replication is used by auto.

## Clean → loss → clean (no reconnect)

| CUBIC flows | Direction | Clean first | At 1% | Clean again | At 1%: TX/payload | At 1%: overlay p95 | On / off after change |
|---:|---|---:|---:|---:|---:|---:|---:|
| 1 | Down | 74.49 | 53.29 | 63.32 | 1.755 | 202 ms | 0.27 / 3.53 s |
| 1 | Up | 114.66 | 43.91 | 122.49 | 1.742 | 202 ms | 2.53 / 3.29 s |
| 8 | Down | 397.11 | 83.37 | 333.21 | 1.734 | 202 ms | 0.85 / 3.13 s |
| 8 | Up | 511.44 | 82.49 | 510.24 | 1.738 | 202 ms | 0.59 / 3.11 s |

Rates are receiver Mbit/s. Four distinct uninterrupted transfers, one seed per workload (see TSV); **3/4** lossy phase windows met all three goals. In each run the observed direction switched on exactly once, off exactly once, and the opposite direction never switched on. No induced loss in either clean phase. Releasing parity did **not** guarantee low RTT on an unshaped saturated clean path: in the eight-flow upload clean p95 was 365 ms before loss and 278 ms after recovery. Refreshing the receiver credit without parity is intentionally possible.

## Cold and shared controls

Do not extrapolate the transition rates to short downloads. When 1% loss existed from the first packet, the 2 s warmup + 10 s window yielded **0.54–3.86 down / 2.04–4.91 up** Mbit/s with one flow and **38.28–41.34 down / 34.57–48.79 up** with eight. No cold lossy cell exceeded 50. Loaded p95 201–204 ms; TX/payload 1.556–1.878. There were no hub engine queue or socket receive-buffer drops in those eight cases. One slow one-flow download never qualified for coding during the measured trial. A matched seed-211 one-flow upload with forced `coded` achieved **73.93 Mbit/s, 1.711×, 201 ms** (auto: 4.91, 1.774×, 201 ms). Repair is not retroactive and does not reset the sender's CUBIC window after a cut.

With loss configured **from the start for the full 64 s window**, four fresh trials (one per workload, one seed each) gave 54.17/73.38 Mbit/s single-flow down/up and 83.60/79.11 Mbit/s eight-flow down/up in the final ten seconds. All four had 201 ms loaded p95, with 1.682–1.722× wire cost. These are *late steady-state* rates, not averages over 64 seconds or guarantees for a new transfer. Constant-profile rows in `transitions.tsv` repeat that **same single interval** in the three phase columns; they are not three repetitions.

0% unshaped, eight flows, one pair per direction: plain → auto down 299.78 → 307.32 Mbit/s (p95 215 → 215 ms); up 577.73 → 431.13 (525 → 352 ms). Auto emitted no downlink parity and the hub never requested uplink parity. The uplink did limit TCP credit under queue pressure: avoiding parity is not the same as guaranteed non-regression in clean-path throughput.

On the shared 10 Mbit/s downlink with eight tunnel streams plus a direct CUBIC flow, auto did not activate parity. Tunnel / competing TCP delivered 4.75 / 0.74 Mbit/s at 201 ms overlay p95 versus 5.51 / 0.65 at 202 ms with the ordinary adaptive default. This single pair does not establish fairness. **Recommendation:** opt-in for variable paths; retain manual `coded` for a known chronically lossy high-RTT path, especially short transfers. Do not make auto the default on this evidence.

## Measurement contract

Same Linux native-AWG TUN stand as [forced coded](../native-awg-coded/summary.md): MTU 1280, CUBIC, 100 ms one way in each direction, loss applied to individual outer datagrams. Download and upload separately. **Continuous:** the same sockets carry data during 12 s clean, 32 s at 1% random loss each direction, and 20 s clean. `tc qdisc change` preserves the propagation queue. In each phase goodput and wire ratio cover the last 10 s, not the transition itself. Loaded RTT is overlay ICMP p95; underlay RTT is reported separately. Switch times are measured from the end of the qdisc update to a hub counter increment polled at 4 Hz. Downlink sees the sender grant, uplink the receiver decision, not an exact timestamp of first recovery. **Cold:** new TCP connections, loss present from the start, 2 s traffic warmup, then 10 s measurement. Slow start and time spent detecting loss count against this result. The 50 Mbit/s, ≤1.8× bytes and ≤230 ms loaded p95 goals must be met together; do not merge steady-phase and cold results.

The wire ratio counts bytes transmitted by **both** endpoint veth devices, including ACKs, AWG overhead, control packets and packets lost downstream, divided by useful receiver bytes for that window. It does not include PHY preamble/IFG/FCS. The shared 10 Mbit/s FIFO control includes a competing direct CUBIC connection: its ratio includes both workloads. `shim` denotes the plain-AWG comparison with repair disabled (no proxy process). `reserve` denotes the existing adaptive single-parity default.

## Evidence and replay

[Cold, clean and contention measurements](measurements.tsv) · [Per-phase measurements and switches](transitions.tsv) · [Allowlisted raw data, scripts and binary digests](raw.tar.gz). Extract in a new private directory. Run `python3 -I final-reduce.py final.log` and `python3 -I transition-reduce.py transition.log` to regenerate both TSVs. To repeat against a matching checkout, move the archived logs aside, run `LAB_RUNS="$(cat final.log.profiles)" bash final.sh /path/to/checkout`, then `LAB_REUSE_BINARIES=1 LAB_RUNS="$(cat transition.log.profiles)" bash transition.sh /path/to/checkout`. Linux user/network namespaces, TUN and cores 0–7 required. No application payloads, generated keys, cookies, panel credentials or client imports in the archive.
