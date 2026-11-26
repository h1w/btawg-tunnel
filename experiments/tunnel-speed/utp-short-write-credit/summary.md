# µTP: blocked short writes use available receiver credit

`Conn.writeNoWait` previously entered a blocked state whenever the next segment could not be sent. Its ACK and timer wakeups required space for a full segment, regardless of the size of the waiting write. A four-byte write started at zero credit timed out after a peer STATE update gave it four bytes. It now wakes when the requested segment fits, and rechecks the same packet-ring, congestion, peer-window and send-buffer limits before sending. No framing, timer interval, congestion policy or queue capacity change.

`TestShortWriteResumesWithExactReceiveCredit` failed on the original path with `i/o timeout`; it and the related window/backoff regressions passed 10 repetitions under `-race`. `go test -race ./purego ./interop -count=1` passed, and Linux hub/client builds succeeded.

## Managed real-TUN check

Fresh current Linux builds before and after the wakeup change, both endpoints updated together, Extended/µTP and Request/µTP, eight CUBIC flows per direction, selective offloads, 2 s warmup and 5 s receiver interval. One attempt per arm/cell. Loss independently configured per direction.

| Profile | Baseline → fixed valid attempts | Observed bulk result |
|---|---:|---|
| 0%, local | 4/4 → 4/4 | Goodput changes −1.7% to +1.9% |
| 0%, +200 ms | 4/4 → 4/4 | Changes −2.4% to +10.9%; hub/client queue overflow still visible |
| 1%, local | 3/4 → 4/4 | Baseline Extended upload had three zero-byte flows; no causal link to the short-write bug established |
| 1%, +200 ms | 4/4 → 4/4 | Extended 0.644/0.633 → 0.615/0.601; Request 0.385/0.350 → 0.460/0.308 Mbit/s (down/up) |

No hub engine, client TUN, excess transit qdisc or UDP socket receive drops in the 1% +200 ms cells. All four fixed WAN rates remain below 1 Mbit/s. These runs establish integration and expose the remaining ceiling, not a general speedup or a proven improvement in completion rate.

[32 attempts](measurements.tsv) · [Raw counters, binary hashes, source and replay](raw.tar.gz). `bash replay-short-write.sh` in the extracted archive reproduces the TSV byte for byte; passing a matching checkout rebuilds and repeats the comparison in private namespaces.
