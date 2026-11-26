# 256 MiB SOCKS download: FEC versus a larger TCP send budget

Current Linux hub/client, actual SOCKS5 split-TCP relay, native WG, automatic BBR pacing unchanged, 300 Mbit/s cap in the download direction, 100 ms transit delay each way, MTU 1280, same selective offloads. The CUBIC HTTP origin is outside the impaired hop. Independent random losses are configured in both directions. The FEC variant is the existing four-block interleaved, downlink-only laboratory shim (two parity packets per 16 original datagrams per block); it emits no reverse parity. This does not add FEC to the product.

Each cell is one SHA-256-checked transfer of the same 256 MiB payload. `Normal` is the existing hub policy, which on this host widens the relay's send memory ceiling to 8 MiB. `Wide` changes only `tcp_wmem` maximum to 32 MiB inside the disposable endpoint namespaces; the relay retains kernel autotuning instead of locking a smaller per-socket send buffer. For each loss setting the two paths share delay, rate, seeds and file size; run order reverses between send-budget profiles. This changes the outer WG path to a shim as well as adding FEC; the previous [shim-without-parity controls](../wan-fec-200ms/summary.md) measured that separately.

## Whole file and steady portion

| Loss | Path | Send memory | Whole-file Mbit/s | ACK rate on relay VPN socket, Mbit/s | Loaded p95 RTT | WAN overflow |
|---|---|---:|---:|---:|---:|---:|
| 1% | Native | 8 MiB | 79.39 | 92.43 | 201 ms | 0 |
| 1% | Interleaved FEC | 8 MiB | 126.38 | 166.16 | 212 ms | 0 |
| 1% | Native | 32 MiB | 101.97 | 124.18 | 202 ms | 0 |
| 1% | Interleaved FEC | 32 MiB | 131.01 | 178.10 | 225 ms | 270 |
| 0% | Native | 8 MiB | 154.69 | 206.55 | 207 ms | 0 |
| 0% | Interleaved FEC | 8 MiB | 151.04 | 205.53 | 216 ms | 0 |
| 0% | Native | 32 MiB | 165.57 | 241.67 | 233 ms | 425 |
| 0% | Interleaved FEC | 32 MiB | 154.83 | 212.57 | 227 ms | 471 |

Whole-file goodput starts before connection setup and ends when the body is received. ACK rate is computed from first/last relay `bytes_acked` samples between 5 s after start and 1 s before completion. It is not another independent throughput sample. Rates for old 128 MiB tests must not be compared with rates here as a code A/B.

At 1%, FEC cut the final sampled TCP retransmit counter from 2145 to 25 with the existing socket budget. Raising that budget with FEC increased sampled maximum outstanding payload (`unacked × MSS`, a proxy, not exact flight size) from 7.67 to 13.19 MiB. It made the whole transfer only 3.7% faster and introduced queue overflow while TCP retransmits rose to 262. All wide-buffer rows sampled zero sender-buffer-limited time at the end, so raising the budget further is not justified by those counters. Clean WAN shows the same tradeoff: the larger buffer can deliver more in the middle of a transfer while creating overflow and adding loaded delay.

## Shared downlink

Another seed pair, 1% independent loss, same 300 Mbit/s cap. A separate UDP stream offers 130 Mbit/s for six seconds starting four seconds after the download begins. Report p95 during that overlap separately from the entire-file rate; the competing flow received 128.66–128.75 Mbit/s in these four trials.

| Path | Send memory | Whole-file Mbit/s | p95 during competing UDP | WAN overflow |
|---|---:|---:|---:|---:|
| Native | 8 MiB | 64.08 | 201 ms | 0 |
| Interleaved FEC | 8 MiB | 70.23 | 227 ms | 17 |
| Native | 32 MiB | 76.45 | 201 ms | 0 |
| Interleaved FEC | 32 MiB | 124.47 | 246 ms | 116 |

The widest FEC variant made the whole-file download much faster on this seed, but added 45 ms to loaded p95 and recreated overflow. A six-second competing period is not a TCP fairness test. This does not support increasing global buffer defaults or shipping always-on parity.

## Startup-only diagnostic

An isolated Go overlay changes one constant: the production relay begins limiting BBR probes after 512 KiB have been acknowledged; the lab candidate waits for 16 MiB. Same 8 MiB actual send memory and the same seeds as the regular profiles. No client flags or source changes outside the lab.

| Loss | Path | Whole-file Mbit/s early → late cap | p95 early → late cap | Extra WAN drops early → late cap |
|---|---|---:|---:|---:|
| 1% | Native | 79.39 → 91.75 | 201 → 202 ms | 0 → 0 |
| 1% | Interleaved FEC | 126.38 → 140.06 | 212 → 233 ms | 0 → 0 |
| 0% | Native | 154.69 → 148.21 | 207 → 206 ms | 0 → 180 |
| 0% | Interleaved FEC | 151.04 → 169.13 | 216 → 231 ms | 0 → 0 |

The candidate sped up the lossy file but did not raise the sampled steady FEC ACK rate (166 → 155 Mbit/s). Delaying the cap lets the early sender pace much more aggressively, not necessarily sustain more goodput. The clean native path regressed and overflowed, so 16 MiB is not an acceptable unconditional default.

Under competition the delayed-cap native variant delivered **91.64 Mbit/s**, with **202 ms** overlap p95 and **443** overflow drops; the delayed-cap FEC variant delivered **90.75 Mbit/s**, with **387 ms** overlap p95 and **1850** overflow drops. With the same workload, the current native cap had 64.08 Mbit/s, 201 ms p95 and zero overflow. A fixed larger startup allowance is not a safe way to recover the gain.

## Decision and evidence

Keep the shipped defaults. For another speed gain without switches, the working candidate is negotiated downlink-only interleaved FEC with parity included in the outer pacing budget and automatic bypass on clean/queued paths. It gave a repeatable direction of benefit on cleanly isolated random loss runs, but the throughput gain under contention varied markedly across seeds and queue states. Do not stack a bigger buffer or a later cap on top and claim production improvement. The negotiation, outer rate accounting and automatic transitions are not implemented by this experiment.

[Measurements](measurements.tsv), [raw traces and lab sources](raw.tar.gz), [build digests](build.sha256). Archive replay checked **18 complete 256 MiB payloads by SHA-256**, qdisc and socket samples, and the FEC one/two erasure test. `tcp_wmem` maximum remained 4 MiB on the host. No production defaults, wire format or host settings changed. From the extracted archive:

```sh
mapfile -t profiles < profiles.txt
python3 verify.py "${profiles[@]}"
cc -std=gnu17 -Wall -Wextra -Werror -O3 -o fec-test fec-test.c
./fec-test
```

To reproduce fresh trials: build current client/server into `bin/`, receiver/origin from the included Go sources, shims from `fec.c`, and `bin/startup` from the hub with a Go overlay replacing `internal/server/tcp_egress_pacing.go` with `startup.go`. Run `./reproduce.sh` with the profile label from the measurement table in a private Linux lab.
