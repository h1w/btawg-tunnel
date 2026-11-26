# Loss recovery over the integrated SOCKS relay at +200 ms RTT

Current real Linux TUN client and hub with automatic BBR and pacing, fixed MTU 1280 and selective veth offloads. One 128 MiB verified HTTP download per cell through real SOCKS5. CUBIC origin outside the impaired hop. Transit adds 100 ms in each direction and caps downlink at 300 Mbit/s. `integrated` is the existing direct native UDP path; `shim` passes WG/AWG datagrams through the same lab processes without parity; `fec16x2` adds two parity shards per 16 encrypted datagrams in both directions; `fecdown` protects only hub → client. WG and AWG use different seeds and opposite trial orders. Do not treat the two protocols as repetitions of the same cell.

Shims operate only in private user/network namespaces. The C lab protocol forwards data immediately, with no retransmission loop or wait for an entire group before sending data. A reconstructed ciphertext must still pass native WG/AWG authentication. The lab does not implement authenticated negotiation, protected control feedback, loss-aware enablement, or parity pacing. It does not alter the native protocol in the product or work against an unmodified peer on the wire.

## Independent random loss

| Profile | Integrated Mbit/s | Shim without parity | FEC 16+2 both ways | Loaded p95 integrated → FEC |
|---|---:|---:|---:|---:|
| WG, 1% | 47.75 | 35.89 | 107.05 | 201 → 213 ms |
| AWG, 1% | 49.53 | 54.48 | 100.10 | 201 → 204 ms |
| WG, 0% | 126.41 | 126.33 | 119.08 | 206 → 216 ms |
| AWG, 0% | 116.52 | 117.31 | 115.24 | 206 → 216 ms |

At 1%, reconstructed downlink datagrams: **1077 WG / 1151 AWG**. Cumulative TCP retransmits at the final sampled TCP_INFO snapshot: **1110 → 12 WG**, **1159 → 17 AWG**. Observed downlink random loss was 1.00–1.05%; no additional WAN queue drops, TUN drops, hub userspace queue drops, UDP receive buffer errors or local shim send errors in these cells. Both parity cases added about 13.35% downlink UDP payload bytes (parity and framing, excluding IP/Ethernet); reverse overhead was 23–24% relative to the small ACK-direction payload. At 0%, the AWG shim reconstructed one late original that was subsequently classified as redundant: a reconstruction is not necessarily a wire drop.

## Shared capacity stress

WG, independent 1% and one UDP flow offering 130 Mbit/s for six seconds starting four seconds into the download. Both flows use the same downlink netem queue. Per-phase p95 is from ping probes while the competitor is active; speed is the entire completed file, not a per-phase send rate.

| Variant | TCP payload Mbit/s | Competing UDP received Mbit/s | p95 during competition | Extra WAN queue drops |
|---|---:|---:|---:|---:|
| Integrated | 56.51 | 128.59 | 201 ms | 0 |
| Shim without parity | 55.09 | 128.65 | 202 ms | 0 |
| FEC downlink only | 89.21 | 128.57 | 298 ms | 52 |
| FEC both ways | 71.88 | 128.56 | 355 ms | 184 |

One-way FEC saved all reverse parity bytes and beat two-way parity on this trace, but still added 97 ms to the loaded tail versus direct integrated. These samples reject enabling parity unconditionally or assuming existing inner TCP pacing accounts for the outer parity bytes.

## Burst loss and interleaved parity

WG. The Gilbert–Elliott model enters a fully losing state with probability 0.25% per good-state packet and leaves it with probability 25% per bad-state packet (stationary loss about 1%, mean bad-state run four packets). Actual downlink drops were 0.90–0.96%. `fecstripe` keeps the 16+2 parity ratio and uses four round-robin encoder blocks in the downlink. Adjacent original datagrams are transmitted immediately and assigned to different blocks; parity is emitted when its own block fills. This trades a longer recovery horizon for burst tolerance.

| Burst-loss variant | Payload Mbit/s | Loaded p95 | Reconstructed downlink | TCP retransmits at final sample |
|---|---:|---:|---:|---:|
| Integrated | 62.04 | 202 ms | — | 995 |
| Shim without parity | 62.60 | 202 ms | 0 | 991 |
| Ordinary FEC both ways | 53.96 | 202 ms | 196 | 827 |
| Ordinary FEC downlink only | 52.48 | 202 ms | 196 | 826 |
| Four interleaved blocks, downlink only | 77.83 | 203 ms | 736 | 281 |

On the independent-loss competition profile the interleaved variant delivered **87.34 Mbit/s**; p95 during competition **224 ms**, zero queue drops. Compared with ordinary downlink-only FEC, it preserved nearly all goodput (87.34 vs 89.21), reduced tail RTT by 74 ms and dropped reverse parity entirely. It still added 23 ms of contention tail versus integrated. Compared with direct on burst loss it improved goodput 25%; ordinary parity regressed. The interleaving experiment used the same netem model and seeds as its control, but ran in a fresh namespace. This is one sample per cell, not a confidence interval.

## Decision and replay

Do not enable this lab shim or parity everywhere. **Interleaved downlink-only FEC is the candidate** for automatic use where both managed peers support it: start uncoded, negotiate capability within the authenticated session, enable parity only when feedback indicates useful recovery, and charge parity to the sending rate budget. Verify clean bypass, contention between managed peers, burst cases, transition behavior and rollback before selecting it as a default. These runs neither implement negotiation nor show that a fixed loss threshold can distinguish congestion from random loss. Stock WireGuard/AWG peers keep the current path.

[Measurements](measurements.tsv), [binary digests](build.sha256), [raw traces and lab source](raw.tar.gz). Archive replay verified **22 complete bodies by SHA-256** and the deterministic one/two-erasure and late-copy checks. No product source, host sysctl, native wire format or production qdisc changed. From the extracted archive:

```sh
mapfile -t profiles < profiles.txt
python3 verify.py "${profiles[@]}"
cc -std=gnu17 -Wall -Wextra -Werror -O3 -o fec-test fec-test.c
./fec-test
```

Fresh lab runs use the current client/server, the archived origin/receiver and shims in `bin/`; build `fec-stripe` from `fec.c`. The original one-lane executable used before the interleaving experiment uses the same block codec and layout. `./reproduce.sh wg`, `confirm`, or `burst` reproduces the indicated profiles; `fecstripe` is the four-block candidate and uses the private bench runner's `LAB_VARIANTS` selector only.
