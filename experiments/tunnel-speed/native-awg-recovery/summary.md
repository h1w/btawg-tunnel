# Native AWG UDP: authenticated loss recovery between client and hub

**Historical outer-UDP prototype; not the current recovery protocol.** Its fixed UDP marker and measurements below do not describe the [encrypted inner implementation](../native-awg-inner-recovery/summary.md).

## What changed and what did not

Plain native AWG (`socks5-awg-udp`) forwards inner IP packets as encrypted
AmneziaWG/UDP datagrams and does not repair a datagram lost on the wire. At
1% independent underlay loss and +200 ms RTT the inner TCP senders see every
loss, and the measured CUBIC goodput collapsed to 5–7 Mbit/s even though the
same path carried 340+ Mbit/s for a BBR sender. The chosen fix adds an
optional recovery channel between an updated client and an updated hub.

The recovery layer wraps the native UDP `conn.Bind`, below the AWG device and
above the socket:

- Originals still leave the device unchanged and go straight to the socket.
  A legacy or third-party peer, and an updated peer whose recovery handshake
  has not completed, keeps exactly the existing AWG wire path.
- Both sides derive per-direction authentication keys from their existing
  X25519 key pair. A three-step handshake (hello / challenge / confirm) binds
  the channel to the exact UDP endpoint and proves return reachability before
  either side uses it. Session frames carry a monotonic sequence number and
  are rejected on replay, on the wrong source endpoint, and after key
  revocation.
- When a session is active, every eight transmitted AWG datagrams form a
  group. The sender hashes each datagram and XORs them into one parity
  datagram. The receiver caches recent originals; on exactly one missing
  datagram in a group it reconstructs the ciphertext and hands it to the AWG
  device, which decrypts and delivers it normally. Two or more missing
  datagrams in one group are unrecoverable and are counted as loss, never
  reconstructed from guesses.
- The receiver reports observed loss plus one-way queue inflation back to the
  sender. Repair is sent only when the report shows 0.3–3% loss, RTT at least
  100 ms, no queue buildup (≥8 ms), and the receiver still sees a fast packet
  train (spare path capacity). Queue feedback disables repair immediately and
  holds it off for four RTTs. Repair never enters an output queue in front of
  originals: originals are sent first and parity is sent best-effort after.
- Repair bytes are charged against a per-session credit accrued from original
  bytes, bounding repair to about one parity datagram per eight originals
  (~12.5% wire overhead) and preventing a repair burst from outpacing the
  originals that fund it. Peer state is bounded to 128 live sessions per hub,
  with idle sessions expired before allocation.

Both the client and the hub build this layer behind the same negotiated
protocol; the hub is always ready and the client activates it for native AWG.
No SOCKS, BitTorrent carrier, TCP congestion-control, MTU, AWG wire format or
profile setting was changed.

## Results

All 39 runs completed (0 failed). Throughput is Mbit/s; each cell is one
10 s receiver window after a 2 s warmup, so values are observations, not
confidence intervals.

### Target: 1% loss, +200 ms RTT, unshaped

| Seed | Flows | Direction | Pass-through | With recovery | Change | Overlay p95 |
|---|---:|---|---:|---:|---:|---:|
| 101 | 8 | down | 7.127 | 17.664 | **+148%** | 201 → 201 ms |
| 211 | 8 | down | 7.164 | 35.811 | **+400%** | 201 → 202 ms |
| 101 | 8 | up | 8.096 | 28.959 | **+258%** | 201 → 201 ms |
| 211 | 8 | up | 6.197 | 37.103 | **+499%** | 201 → 202 ms |
| 101 | 1 | down | 0.883 | 1.308 | +48% | 201 → 201 ms |
| 211 | 1 | down | 0.966 | 1.520 | +57% | 201 → 201 ms |
| 101 | 1 | up | 0.631 | 0.635 | +0.6% | 201 → 201 ms |
| 211 | 1 | up | 1.000 | 1.123 | +12% | 201 → 201 ms |

The bulk case the earlier investigation was built around — eight inner CUBIC
flows at 1% / +200 ms — improves 2.5–5× in both directions with **no loaded
latency change**. The gain comes entirely from the tunnel hiding the wire
loss from the inner TCP; the tunnel's own congestion window is unchanged.
Single-flow cells improve less because one CUBIC window at 200 ms RTT
(~18 KiB observed) is itself the ceiling: even with zero wire loss a single
flow needs seconds to grow a window to the bandwidth-delay product. All eight
cells have zero engine drops, zero UDP receive-buffer errors and zero excess
transit drops.

### Clean path (0% loss)

| Flows | Direction | Pass-through | With recovery | Overlay p95 |
|---|---|---:|---:|---:|
| 1 | down | 103.058 | 81.525 | 201 → 202 ms |
| 1 | up | 114.272 | 112.564 | 209 → 210 ms |
| 8 | down | 394.210 | 424.900 | 216 → 239 ms |
| 8 | up | 599.295 | 536.237 | 595 → 606 ms |

At 0% loss the activation gate stays closed, so no repair datagrams are sent;
the only difference is the per-datagram hashing and cache copy on both ends.
These are single-seed runs on an unshaped path where both arms saturate the
hub engine queue (4 245 and 7 118 engine drops in the two downlink cells), and
goodput moves −21% to +8%. That spread is dominated by clean-path saturation
and run order, not by recovery traffic; it does not support a clean-path
speedup claim, nor prove a regression.

### Shared 10 Mbit/s downlink with a direct CUBIC flow

| Loss | Seed | AWG : direct flows | AWG pass-through → recovery | Direct CUBIC alone / with pass-through / with recovery |
|---|---|---|---:|---:|
| 0% | 101 | 1:1 | 5.492 → 5.635 | 8.333 / 3.418 / 3.308 |
| 0% | 101 | 8:1 | 6.918 → 6.746 | 8.333 / 1.995 / 2.100 |
| 1% | 101 | 1:1 | 0.926 → 0.663 | 0.743 / 0.416 / **0.687** |
| 1% | 101 | 8:1 | 5.669 → 4.418 | 0.743 / 0.438 / **0.738** |
| 1% | 211 | 1:1 | 0.861 → 0.761 | 0.628 / 0.665 / 0.563 |
| 1% | 211 | 8:1 | 4.486 → 4.812 | 0.628 / 0.696 / 0.755 |

On the shared link the layer **moves bandwidth toward the direct CUBIC flow**
rather than away from it at 1% loss: direct TCP reaches 92–99% of its
solo rate in the 101 pairs and 120% in the 211 8:1 pair, at a cost of 22–28%
to the AWG tunnel in two of the four lossy cells. One lossy pair (211 1:1)
is worse for both. At 0% the layer is essentially neutral for direct TCP
(−3% / +5%), and it does not fix the tunnel's pre-existing dominance of the
shared link (direct TCP gets 25–40% of solo even with pass-through). Underlay
p95 is unchanged (200–206 ms) throughout.

**Reading this:** the recovery layer is a clear win when AWG is the only
tenant of its path, which is the target case. On a link already shared with
a competing TCP flow it behaves conservatively and often yields to that flow,
which satisfies the "don't crush TCP" constraint but does not make AWG faster
there. The final reducer does not yet capture recovery byte/reconstruction
counters, so this suite measures goodput and path counters only; the earlier
lab runs establish the reconstruction mechanism separately.

[39 runs](measurements.tsv) · [reserve-vs-pass-through comparison](comparison.tsv) ·
[Raw counters, scripts and binary digests](raw.tar.gz). The bundle excludes
client imports, cookies, the hub database and `hub.log`. To reproduce a run,
put a matching `btawg-client`/`btawg-server` build in `.awg-integrated-lab*/current/`
(or let `final.sh` build them) and run `./final.sh <matching-checkout>`, then
`python3 final-reduce.py final.log > final.tsv`. The recovery layer is enabled
on the client with `BTAWG_AWG_REPAIR=1` in these runs; the hub is always ready.

## Compatibility and robustness

- An unmodified AWG datagram is forwarded byte-for-byte before any recovery
  work; the send path never rewrites or reorders originals. A datagram that
  merely collides with the recovery marker and fails authentication is passed
  through to the AWG device untouched.
- Recovery is scoped to the native UDP path. BitTorrent-carrier stream
  endpoints and native WG are unaffected. Client-to-client traffic and SOCKS
  are unaffected.
- A revoked or replaced client key cannot authenticate a new session; a
  confirmed session is bound to the exact UDP source endpoint, so a redirect
  or NAT rebinding requires a fresh handshake. Replayed session frames are
  dropped. Active session state is capped at 128 peers per hub and idle state
  is expired before a new cache is allocated.

Deterministic regression tests cover: one-loss reconstruction and rejection
of two-loss / corrupted parity, per-datagram byte-identity of originals,
mixed-size repair overhead bounded below 1/7 of original wire bytes, ordinary
datagrams passing through with no session and no reply, magic-colliding
legacy datagrams passing through, stream endpoints ignoring recovery frames,
wrong-endpoint and replayed frames being rejected, and the loss/queue
activation gate (including the cooldown after congestion). All of
`internal/awgrepair`, `internal/bind`, `internal/client` and
`internal/server` pass under `-race`; Linux hub/client and the Windows amd64
client build.

## Limitations and rollout

**Wire visibility.** Recovery frames are separate UDP datagrams on the same
socket with a fixed 4-byte marker. Native AWG's own obfuscation (header
protection, random trailers) is not applied to them, so a passive observer
can distinguish them from ordinary AWG traffic. This does not affect the AWG
payload, and a third-party AWG peer ignores them, but it is a new observable
protocol signature on the port. Removing it needs one of two follow-ups:
mask the frames with the deployment's AWG header-protection key (cheap,
keeps this architecture), or carry parity inside the AWG tunnel as inner
IPv4 packets (cleanest, needs a tun.Device wrapper and parity splitting to
fit the 1280-byte inner MTU). Until then the client side is explicit
opt-in (`BTAWG_AWG_REPAIR=1`) while the hub is always ready; flipping the
client to default-on should wait for one of those.

**Fairness on a shared link.** Recovery costs about 12.5% extra wire bytes
when active. On an unshaped link that is free capacity; on a link already
shared with a direct TCP flow it can take bandwidth from that flow. The
activation gate suppresses repair on queue buildup and on paths without a
fast packet train, which is why the unshaped target profile gains while the
shared-link runs show a small, sometimes noisy, direct-TCP change. The
fairness numbers below are single 10 s CUBIC flow measurements and vary
between seeds; they bound the harm observed here, they do not prove
neutrality on an arbitrary bottleneck.

## Measured target profile

Transparent TUN, native AWG/UDP, eight inner CUBIC downloads, 1% independent
random loss per direction, +200 ms propagation RTT, selective offloads,
2 s warmup and a fixed 10 s receiver-byte window, no rate shaping. Two seeds
in matched order. Receiver goodput is the sum of `TCP_INFO` bytes_received
deltas across the eight sockets. Overlay p95 is measured inside the tunnel
during the same window; queue/loss counters span the whole trial.
