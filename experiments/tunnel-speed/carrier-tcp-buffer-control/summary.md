# AWG TCP carriers: receive-window and loss-unit controls

Linux real TUN, managed client, both Extended/TCP and Request/TCP, eight inner CUBIC flows, 2 s warmup and 5 s receiver interval. Both outer sockets used Linux BBR, verified in `ss`. Fixed uses the current 4 MiB `SO_RCVBUF` request (8 MiB accounted on this host). Auto omits that one call and leaves the kernel's receive autotune enabled; send buffer, framing, offloads and queue limits are otherwise identical. Both endpoints change together. Transit `netem` applies loss independently in each direction when enabled. This is transparent TUN, not SOCKS CONNECT.

## Receive autotune is not a shared improvement

Goodput in Mbit/s, fixed → auto. Two runs per arm in ABBA order for clean local, clean WAN and lossy WAN; one each for lossy local and the narrow link. The narrow link uses 10 Mbit/s and 160 slots in each direction, including the 100 ms one-way delay; no injected random loss in that control.

| Profile | Extended down / up | Request down / up |
|---|---:|---:|
| 0%, local | 463.51 → 397.20 / 553.47 → 497.33 | 353.92 → 357.65 / 369.82 → 378.43 |
| 0%, +200 ms | 174.91 → 164.36 / 200.35 → 150.82 | 126.15 → 136.79 / 137.15 → 139.47 |
| 1%, local | 298.14 → 255.45 / 274.47 → 346.45 | 256.69 → 243.20 / 275.34 → 337.92 |
| 1%, +200 ms | 15.18 → 12.50 / 13.92 → 19.13 | 13.32 → 16.47 / 13.03 → 17.90 |
| 10 Mbit/s, +200 ms | 7.49 → 6.35 / 7.48 → 7.82 | 5.58 → 5.52 / 3.89 → 5.44 |

- **1% +200 ms:** all 16 paired WAN intervals transferred on all eight flows. Fixed sender sockets reported zero `rwnd_limited` and `sndbuf_limited` deltas despite the slowdown. Only one auto sender had new receive-window-limited time (119 ms of 7,553 ms busy). Removing the fixed receive buffer does not address the measured loss bottleneck; these counters do not establish which of the remaining causes limits it.
- **0% +200 ms:** auto receivers reached the 32 MiB `tcp_rmem` maximum. This removed receive-window-limited time in the measured sender sockets, but send-buffer-limited time and endpoint queue drops increased; e.g. Extended upload had 4,880 → 9,865 client drops and lost about 25% goodput. It did not remove the bottleneck.
- **Narrow link:** Extended upload transit overflow rose 401 → 1,031 with loaded p95 1,092 → 1,290 ms. Request upload moved in the opposite direction (2,481 → 1,874 drops, p95 2,085 → 1,504 ms). Neither result establishes an improvement across modes. No socket buffer limit was recorded in these eight intervals.

**Decision:** retain the current socket configuration. Local goodput regressions and WAN queue growth reject unconditional receive autotuning on these paths. All 72 buffer-control intervals completed; that total includes a redundant lossy WAN pair retained in the raw rows, not used for the WAN medians.

## TCP segmentation changes what netem drops

The separate same-binary control kept the fixed buffers and repeated 1% +200 ms with TCP segmentation offloads and generic GRO disabled on the four private veths in the candidate. TUN offloads were untouched. The successful UDP calibration common to both arms does not calibrate TCP segmentation. `netem` registered **2,377–4,567** delayed work items in the data direction with TCP offload, versus **8,517–12,257** with it disabled. Those are qdisc `delayed` counter deltas, not TCP segment counts. The change in loss-unit and endpoint processing cannot be separated by this experiment.

| Mode | Offloads on → off, down / up Mbit/s | Loaded p95 on → off, down / up ms |
|---|---:|---:|
| Extended/TCP | 16.33 → 12.29 / 13.15 → 12.49 | 606 → 769 / 533 → 773 |
| Request/TCP | 14.99 → 12.40 / 16.45 → 8.71 | 667 → 936 / 657 → 1,072 |

All 16 intervals completed. Turning off TCP offload did not recover goodput and increased loaded p95 in all four cells. Median endpoint queue drops fell, but so did delivered throughput; no added transit overflow or UDP receive-buffer errors were recorded. Report the offload profile when comparing lossy TCP carriers. Do not disable offloads in production from this result.

[88 intervals](measurements.tsv) · [24 profile/mode/direction comparisons](comparison.tsv) · [Outer socket and qdisc deltas](sockets.tsv) · [Raw snapshots, source overlay, hashes and runners](raw.tar.gz). `bash reduce.sh` in the extracted archive reproduces all three TSVs byte for byte. `bash run.sh <matching-checkout> <wan|clean-local|narrow|segments>` reruns one series in private namespaces. The source snapshot is anchored at `base-commit.txt`; apply its patch and untracked archive in a separate checkout.

