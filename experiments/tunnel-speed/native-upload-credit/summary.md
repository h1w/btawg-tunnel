# Native Linux SOCKS upload: send-buffer pressure and pacing

Actual SOCKS CONNECT, native WG/AWG, current overlay TCP relay, and an HTTP PUT receiver outside the impaired path. The lab overlays share one hub binary; the integrated run rebuilt both endpoints with the same hub pacing policy. The receiver verified body length and SHA-256. This client-side policy does not change transparent TUN TCP or any BitTorrent carrier. Qdisc changes were confined to private namespaces.

At +200 ms and 300 Mbit/s uplink with a 4,096-slot delay/rate queue, the stock Linux BBR socket is capped at 2 MiB accounted send memory. The first four measured uploads spent **61%** of busy time send-buffer-limited at 0% random loss and **24–25%** at 1%; receive-window-limited time stayed below 0.4%. No hub engine or transit overflow recorded in the stock arm. Payload size is 128 MiB unless noted.

## A larger send buffer needs a pacing guard

Extended buffer requests 4 MiB on established WAN BBR client sockets (8 MiB accounted on this host). `Paced` adds the existing relay delivery/RTT pacing rule on the same socket. The hub, wire format, server policy and origin stay identical. Two paired seeds for stock and paced at WAN, one for expansion only and LAN.

| Profile | Mode | Stock → paced Mbit/s | Loaded p95 stock → paced | Excess transit drops stock → paced |
|---|---|---:|---:|---:|
| 0%, +200 ms | WG | 51.29 → 127.66 | 201 → 205 ms | 0 → 0 |
| 0%, +200 ms | AWG | 51.35 → 123.84 | 201 → 206.5 ms | 0 → 0 |
| 1%, +200 ms | WG | 26.68 → 55.53 | 201 → 201 ms | 0 → 0 |
| 1%, +200 ms | AWG | 25.28 → 50.73 | 201 → 201 ms | 0 → 0 |
| 0%, local | WG | 921.07 → 925.50 | 4.22 → 4.47 ms | 0 → 0 |
| 0%, local | AWG | 883.92 → 894.58 | 4.75 → 4.59 ms | 0 → 0 |
| 10 Mbit/s, +200 ms | WG | 7.62 → 6.48 | 271 → 256 ms | 1,017 → 264 |
| 10 Mbit/s, +200 ms | AWG | 7.46 → 6.38 | 276 → 251 ms | 1,016 → 266 |

Expansion alone transferred faster at +200 ms, but produced **2,048–11,016** excess transit drops across its four cells, so it is not a safe default. The pacing arm eliminated those excess drops on the 300 Mbit/s profile, with comparable LAN rates and unchanged 2 MiB LAN socket capacity. The narrow clean control uses 8 MiB uploads and a 160-slot uplink queue including propagation; pacing reduced drops and latency but lost **14–15%** delivered goodput. The stock narrow socket never reported send-buffer limitation. Those results reject unconditional expansion plus pacing.

## Integrated pressure gate

The client only upgrades after at least 512 KiB are acknowledged and an RTT-sized sample reports send-buffer-limited time for at least 20% of the interval with queue inflation below 7.5 ms. It installs the pacing cap before requesting the wider buffer. If the buffer cannot grow, it clears the cap. The hub continues to use its previous policy through the shared Linux pacer.

| Profile | Mode | Stock → integrated Mbit/s | Loaded p95 stock → integrated | Send buffer stock → integrated |
|---|---|---:|---:|---:|
| 0%, +200 ms, 300 Mbit/s | WG | 51.29 → 133.35 | 201 → 211 ms | 2 → 8 MiB |
| 0%, +200 ms, 300 Mbit/s | AWG | 51.35 → 134.12 | 201 → 209.5 ms | 2 → 8 MiB |
| 1%, +200 ms, 300 Mbit/s | WG | 26.68 → 69.33 | 201 → 201 ms | 2 → 8 MiB |
| 1%, +200 ms, 300 Mbit/s | AWG | 25.28 → 59.43 | 201 → 201 ms | 2 → 8 MiB |
| 0%, local | WG | 921.07 → 899.87 | 4.22 → 4.24 ms | 2 → 2 MiB |
| 0%, local | AWG | 883.92 → 861.21 | 4.75 → 5.61 ms | 2 → 2 MiB |
| 10 Mbit/s, +200 ms | WG | 7.615 → 7.612 | 271 → 271 ms | 2 → 2 MiB |
| 10 Mbit/s, +200 ms | AWG | 7.463 → 7.466 | 276 → 276 ms | 2 → 2 MiB |

The 300 Mbit/s rows are medians of two seed pairs and did not record hub engine or excess transit drops. The local rows are single pairs; the gate did not activate and the measured 2–3% differences are not a LAN speedup or a proven regression. These results apply to native Linux SOCKS uploads only. Transparent TUN and µTP still have separate loss and congestion constraints.

On the 10 Mbit/s pair the gate did not activate. The narrow queue still overflowed (WG **1,017 → 1,022**; AWG **1,016 → 1,018**); the change avoids the regression of unconditional pacing, not the bottleneck drops.

All **52/52** payloads have verified receipts, including 12 from the integrated build. Client/server race suites and focused BBR/pacing tests passed; the full privileged `tunsetup` run hit two nftables fixture failures (`TestHubRequiresRoot`, `TestHubDoesNotClobberUnrelatedTable`), outside the pacing tests. Linux hub/client and Windows amd64 client builds passed.

[Measurements](measurements.tsv) · [Raw socket traces, payload receipts, binaries' SHA-256, source snapshots and replay](raw.tar.gz). After extraction, `bash reduce.sh` produces the TSV byte for byte. `bash replay.sh <matching-checkout> integrated` rebuilds and replays the production arm.
