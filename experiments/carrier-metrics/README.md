# carrier-metrics

Isolated measurement harness for the btawg BitTorrent carrier path. It
reuses the real `btawg` server/client code with registered keys, drives
two clients through the local overlay network, and records round-trip
latency, bulk throughput, outer bytes, and (optionally) pcaps.

## What it measures

- Small overlay TCP round trips (default 100 × 64 B) between two clients.
- One bulk echo (default 1 MiB) on the same connection.
- Time to first echoed byte, p50/p95/p99 small RTT.
- Hub inner-traffic counters (outer bytes require pcap analysis).
- Impact of `BTAWG_REQ_EXPERIMENT` and `BTAWG_EXT_EXPERIMENT`.

## Quick start

```bash
cd experiments/carrier-metrics
./run.sh
```

This builds `./cmd/measure` and writes results to `results/`:

- `measurements.jsonl` — one JSON object per run
- `pcaps/` — tcpdump captures (root only)
- `summary.md` — Python summary

The default matrix covers:

- Modes: `ext-tcp`, `ext-utp`, `req-tcp`, `req-utp`,
  `tls-ext-tcp`, `tls-ext-utp`, `tls-req-tcp`, `tls-req-utp`
- Experiments: `baseline`, `req-window`, `req-varied`,
  `ext-protected`, `ext-packed`, `ext-padded`
- Delays: 0, 40, 200 ms one-way
- Repeats: 3

## Manual invocation

```bash
go run ./cmd/measure \
  -modes ext-tcp,ext-utp \
  -experiments baseline,ext-protected \
  -delays 0,40 \
  -repeats 3 \
  -capture /tmp/cap \
  -capture-iface lo \
  -out results.jsonl
```

Disable concurrent small-during-bulk with `-concurrent-small 0`, or tune
it with `-concurrent-small-size`. By default 100 concurrent 64-byte round
trips run on a second overlay connection while the 1 MiB bulk echo runs.

Optional lab timing knobs (only effective if the btawg implementation
reads these variables):

```bash
go run ./cmd/measure \
  -pace-mbps 10 \
  -idle-keepalives 12 \
  ...
```

These set `BTAWG_CARRIER_PACE_MBPS=10` (integer Mbps) and
`BTAWG_CARRIER_IDLE_KEEPALIVES=12`.

## Environment knobs

The harness sets the following variables before creating the hub/clients:

| Variable | Values |
|----------|--------|
| `BTAWG_REQ_EXPERIMENT` | `baseline`, `window`, `varied` |
| `BTAWG_EXT_EXPERIMENT` | `baseline`, `protected`, `packed`, `padded` |
| `BTAWG_CARRIER_PACE_MBPS` | integer Mbps, e.g. `10` |
| `BTAWG_CARRIER_IDLE_KEEPALIVES` | 1–12 |

Baseline runs explicitly select `baseline` for both carrier families and
clear the timing options. They do not use production auto-negotiation.

## Limitations

- **Loopback only.** The harness runs server and both clients in one process
  on `127.0.0.1`. Network namespaces, veth pairs, and tc shaping require
  root and are out of scope for this harness; they are handled by the
  controller/integration harness.
- **tc delay requires root.** Without root the requested delay is ignored
  and the run proceeds on the loopback path.
- **tcpdump requires root + a usable binary.** The harness looks in
  `/tmp/btawg-dpi-harness.tjayWT8n/tools/usr/bin` and then `/usr/bin`.
- **SOCKS5 egress is not directly measured.** Local loopback and overlay
  addresses are rejected by the btawg SOCKS5/egress policy, so the workload
  uses overlay sockets. A real-TUN + public-IP setup can drive SOCKS5
  CONNECT/UDP ASSOCIATE directly in the integration harness.
- **µTP measurement uses TCP payload.** The carrier underneath is µTP, but
  the application workload is TCP over the overlay netstack.

## Related captures

The following existing tunnel captures can be used for visual/control
comparison, but they are **not** genuine public BitTorrent swarms:

- `btawg-dpi-capture/` — `ext-tcp.pcap` and `req-tcp.pcap` from the
  existing btawg carrier path.
- `experiments/awg-bittorrent/results/ndpi-pcaps/` — prior btawg variant
  captures, mostly from the anacrolix/go-utp test path rather than a live
  swarm.

## Tests

`go test ./internal/harness` contains contract tests that verify the
harness sets the experiment environment variables correctly and can bring
up a real hub + two registered clients.

## Output schema

```json
{
  "mode": "socks5-awg-ext-tcp",
  "experiment": "baseline",
  "delay_ms": 40,
  "repeat": 0,
  "connected_s": 0.123,
  "small_sent": 100,
  "small_received": 100,
  "small_bad": 0,
  "first_echo_ms": 45.2,
  "small_p50_ms": 82.1,
  "small_p95_ms": 95.4,
  "small_p99_ms": 98.7,
  "small_max_ms": 102.3,
  "concurrent_small_sent": 100,
  "concurrent_small_received": 100,
  "concurrent_small_p95_ms": 105.4,
  "concurrent_small_p99_ms": 110.2,
  "bulk_bytes": 1048576,
  "bulk_duration_s": 1.234,
  "bulk_mbps": 6.789,
  "hub_up_bytes": 1234,
  "hub_down_bytes": 1234,
  "pcap": "pcaps/lo-1234567890.pcap"
}
```
