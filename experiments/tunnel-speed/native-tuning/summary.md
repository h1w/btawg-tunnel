# Native WG/AWG tuning

Local Linux namespaces, real TUN, no balancer or rate limit, zero added RTT. Userspace: eight TCP streams, two seconds warmup, six seconds nominal measurement. Kernel WG control: eight seconds. Each direction measured separately. CPU pinning in the userspace matrix: two physical cores for hub, one for client, one for iperf3. Host CPU settings were not changed. Loaded RTT is ICMP p95 during the measured window; minimum idle RTT is in the raw ping files. All 68 userspace and 4 kernel trials were valid.

| Final userspace build | WG down / up, Mbit/s | AWG down / up, Mbit/s | WG p95 down / up, ms | AWG p95 down / up, ms |
|---|---:|---:|---:|---:|
| 0%, Linux offload defaults, CUBIC | 1082 / 1942 | 1161 / 1885 | 4.90 / 34.60 | 4.26 / 31.90 |
| 1%, selective offload changes, CUBIC | 246 / 337 | 412 / 217 | 7.08 / 4.71 | 3.05 / 10.00 |
| 1%, selective offload changes, BBR + TUN fq (lab only) | 827 / 769 | 833 / 749 | 11.40 / 11.80 | 22.00 / 13.20 |

## Decisions

1. **TUN error recovery shipped in source:** the hub forwards valid segments from an `ErrTooManySegments` read and continues. Previously it stopped the shared router. Targeted regression reproduced the stop before the fix; server, client and e2e tests passed after it, with focused server tests under `-race`.
2. **Native batching:** retained existing reads up to 16. In alternating A1/B1/A2/B2, adding enqueue batching increased WG download from 1397/1302 to 1601/1347 Mbit/s, but lowered AWG from 1332/1244 to 1257/1175 Mbit/s. No shared production win. Counters show ~7.8–8.8 packets per TUN read and ~11–13 per successful handoff to crypto/UDP. Native queue drops were 1.1–1.5% of accepted plus dropped in instrumented runs, with zero reported UDP send failures. UDP stage counts are socket batch calls and datagrams handed to the bind, not wire-time latency.
3. **Offloads:** left production defaults alone. The selective lab profile disables UDP TX segmentation on sender-side veths and any UDP forwarding coalescing on transit. TCP segmentation, generic GSO and checksums remain enabled. Calibration in both directions proved loss of individual UDP datagrams rather than whole GSO bursts; the matrix uses 1%, not the 20% calibration rate.
4. **CPU:** with affinity, both native modes exceeded 1 Gbit/s in both directions across all four clean A/B runs. In the unpinned control AWG download was 531 Mbit/s, with 99% host CPU in the AWG upload window. CPU load and temperature were logged rather than assuming a fixed thermal ceiling. Pinning remains lab-only.
5. **MTU:** 1408 produced zero IPv4 fragmentation in all lab namespaces and modest CUBIC gains under loss, but reduced clean AWG throughput. Keep 1280 as the production default; this local result does not establish Internet path MTU.
6. **TCP and queues:** per-socket BBR at 1% with default qdisc raised control throughput from 426–498 to 810–872 Mbit/s, but loaded p95 rose from 1.21–2.12 to 10.90–14.00 ms. Smaller 64-packet native queues reduced clean downlink p95 to 1.37–1.43 ms but reduced downlink to 806–859 Mbit/s. `fq` reduced clean AWG download and increased p95 in its control. Keep CUBIC/system choice, existing queue depth 256 and system qdisc. No global TCP settings were modified.
7. **Kernel reference:** WG handshakes verified on both ends. Measured 764/762 Mbit/s at 0% with default offloads and 724/850 Mbit/s at 1% with offloads disabled (down/up). Different scheduling/run length and offload profiles, not an equal-conditions backend recommendation. No kernel AWG module on this host; AWG 3.1 compatibility cannot be inferred from kernel WG.

The original `before/` remains unchanged. See `measurements.tsv`, `kernel-wg.tsv`, and `raw.tar.gz` for case parameters, counters, and the lab scripts.
