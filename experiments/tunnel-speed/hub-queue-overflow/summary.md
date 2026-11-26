# Hub engine queue overflow without injected loss

The [six-mode delay matrix](../zero-loss-delay/summary.md) recorded drops inside the hub with zero transit-queue drops. On the downlink, Linux can hand the shared TUN reader a GSO frame that becomes a burst of IP packets in one `Read`. The dispatcher does non-blocking sends into the destination engine's 256-slot queue, before encryption and the carrier. If the reader does not drain fast enough, a full queue rejects new packets. An earlier [stage profile](../hub-fast-path/summary.md) saw native queue drops around 1% and zero UDP send failures. This is distinct from packet loss on the underlay, and gives no explanation for the separate iperf connection-start stalls.

The shipped source gives **only native WG/AWG** one scheduler yield and one additional non-blocking enqueue on the first native full queue in each shared TUN read. It does not wait for space, increase capacity, or retry in the direct client-to-client injection path. Packets still undergo the same route and ACL checks on dequeue. If the queue remains full, the drop remains visible in `/api/traffic`. Sustained overload still requires either reducing offered load or increasing transport drain capacity; the retry only mitigates drops from transient imbalance. It does not change the protocol, client, socket tuning, or host configuration.

## Native-only A/B

Same client binary in all trials; server baseline SHA-256 matches the six-mode matrix. Real Linux TUN, stock offloads, 0% injected loss and no rate shaping, eight CUBIC download streams, 2 s omitted, 5 s nominal measurement. Each cell is the median of two independent trials per build in B/A/A/B order. Drops count the entire trial including warmup; the percentage columns normalize by the number of packets reported by the hub TUN qdisc over the same snapshot window. The counts are not a measurement of end-to-end loss.

| Mode | Added RTT | Download Mbit/s A → B | Loaded p95 ms A → B | Queue drops A → B | Drops / hub TUN packets A → B |
|---|---:|---:|---:|---:|---:|
| Native WG | 0 ms | 1585 → 1748 | 4.02 → 3.87 | 12400 → 7663 | 1.07% → 0.60% |
| Native AWG | 0 ms | 1562 → 1665 | 4.11 → 4.25 | 11268 → 7952 | 1.01% → 0.68% |
| Native WG | +200 ms | 423 → 403 | 206.5 → 207.5 | 5765 → 5030 | 2.14% → 2.15% |
| Native AWG | +200 ms | 350 → 463 | 208.5 → 206.0 | 4158 → 4240 | 1.81% → 1.46% |

The integrated source passed `go test -race ./internal/server -count=1` and a fresh mixed-engine smoke: WG delivered 1801 Mbit/s with 8000 hub drops; the separate Extended/TCP client received all 28 pings. The final binary digest and smoke are included in the archive.

No qdisc or UDP receive-buffer drops in these 16 trials. Local improvement is supported in both transports. At +200 ms WG results overlap and its normalized drop rate is unchanged; AWG measured more throughput with a similar absolute drop count. Two repeats per build do not establish a universal WAN gain. In a separate concurrent-client smoke, an Extended/TCP client received 28/28 ping replies while native WG was saturating its queue on both builds. WG queue drops were 13187 → 9436; this is an isolation smoke, not a contention benchmark.

## Rejected candidates

- **Yield/retry on every full enqueue:** a local native WG diagnostic went from control rates of 0.97–1.06% to 0.29% drops per hub TUN packet. The narrower production retry avoids one scheduler handoff per rejected packet and does not touch the client-to-client injection path.
- **Enable 16-packet engine reads for all AWG carriers:** one local Extended/TCP trial rose from 653–663 to 973 Mbit/s, but Request/µTP p95 rose from 40–41 to 379 ms. Not a safe shared default.
- **One retry per TUN batch for every engine:** local native drops fell, but Request/µTP at +200 ms showed 2976–6234 drops and p95 296–336 ms versus 1021 and 281 ms in its successful control. The other control timed out and remains in the raw results. No generic carrier retry.
- **512 instead of 256 native queue slots:** local drops roughly halved, but loaded p95 rose from 3.58 to 5.78 ms WG and 3.88 to 6.04 ms AWG. At +200 ms WG drops increased, and one candidate trial also produced 99 UDP receive-buffer errors. Not adopted.

[Measurements](measurements.tsv) · [Raw counters, iperf JSON, candidate overlays and runners](raw.tar.gz). The four comparison series contain 75 successful trials and one timed-out control. Replay the table from the archive with `python3 compare.py scan.log bounded.log headroom.log native-retry.log > measurements.tsv`.
