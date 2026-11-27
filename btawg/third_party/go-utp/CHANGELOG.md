# Changelog

## Unreleased

- btawg vendor patch: when a live route's delay rises in both directions,
  fresh ACKs for non-retransmitted data with a reverse timestamp and a
  matching RTT increase can retire part of the old forward delay baseline
  at a nearly collapsed window. A forward-only delay rise does not qualify;
  ordinary loss backoff and receive credit are unchanged.
- btawg vendor patch: synchronize timeout timer initialization with the callback
  to prevent a race during socket startup.
- btawg vendor patch: selective acknowledgements for later packets no longer
  postpone retransmission of the oldest unacknowledged packet.
- btawg vendor patch: space fast-retransmit congestion-window cuts by at
  least one smoothed RTT, retaining the 100 ms minimum interval.
- btawg vendor patch: reverse DATA carrying a cumulative ACK no longer
  cancels a deferred selective ACK when receive gaps are outstanding.
- btawg vendor patch: arm the deferred ACK timer when reads reopen the
  receive window, even when no inbound datagrams arrive.
- btawg vendor patch: send the current receive window in queued DATA and
  retransmissions instead of advertising obsolete credit.
- btawg vendor patch: duplicate out-of-order DATA schedules another SACK
  in case the previous acknowledgement was lost.
- btawg vendor patch: congestion backoff leaves room for at least one
  segment, while still honoring the peer receive window and send buffer.
- btawg vendor patch: flush queued DATA on peer receive-window growth even
  if the update does not acknowledge new data.
- btawg vendor patch: refresh the socket clock before starting a new DATA
  flight so idle time is not charged to its retransmission timeout.
- btawg vendor patch: release the last queued short segment through the
  normal send path so ACKs cannot bypass the peer receive window.
- btawg vendor patch: wake blocked short writes when the next segment
  fits, without waiting for an entire MTU of receive credit.
- btawg vendor patch: start DATA retransmission timing on first send;
  expiry cannot force previously unsent DATA through a closed receive window.
- btawg vendor patch: record the transmitted packet's sequence for MTU
  probes, including when reopening credit sends older queued DATA.
- btawg vendor patch: confirm an MTU probe when either a cumulative or
  selective ACK retires the transmitted packet.
- btawg vendor patch: derive the first-flight RTO deadline from the
  outgoing packet timestamp rather than an earlier socket clock sample.

- Split out of [go-libutp](https://github.com/anacrolix/go-libutp), where these packages were added
  but never released. The `utp` package is now this module's root, so the import path is
  `github.com/anacrolix/go-utp` rather than `github.com/anacrolix/go-libutp/utp`, and the pure Go
  implementation is `github.com/anacrolix/go-utp/purego`. go-libutp keeps the cgo wrapper around
  the vendored C++ sources, and this module depends on it for `utp.LibUtp`
- `utp` is one interface over both implementations, selected at build time. `utp.Default` is
  libutp; the `purego` build tag or `CGO_ENABLED=0` makes it `utp.PureGo` and leaves the C++
  sources uncompiled. `utp.Socket` and `utp.Implementation` are both interfaces, so code can be
  handed either implementation as a value: `utp.PureGo` is always available, `utp.LibUtp` wherever
  libutp is compiled. An `Implementation` makes a `Socket` over a `net.PacketConn`, and
  `utp.Listen` opens a port for one. `utp.NewSocket` and `utp.NewSocketFromPacketConn` use
  `utp.Default` and match the signatures of the constructors of those names in both underlying
  packages. The shared tunables are `WithLogger`, `WithBufferSizes`, `WithTargetDelay` and
  `Socket.SetLogging`
- `purego` is µTP in pure Go, ported from the libutp sources. It needs no cgo and no C++ compiler,
  and offers the same API shape as the wrapper: `Socket` implements `net.Listener` and
  `net.PacketConn`, and its connections implement `net.Conn`. The state machine, LEDBAT congestion
  control, selective acknowledgements and fast resend, retransmission timers and MTU search follow
  libutp closely, including its constants. It imports nothing outside the standard library, logs
  through `log/slog`, and passes `nettest.TestConn`
- The `purego_utp` build tag selects the pure Go implementation too, for builds that want to say
  that about µTP alone rather than setting the widely answered `purego` tag for everything they
  depend on, or that want cgo out of µTP while leaving it on elsewhere. `CGO_ENABLED=0` and either
  tag are equivalent here
- `interop` tests the two implementations against each other on the wire: transfers in both
  directions, bidirectional transfers, ping-pong, and transfers over a link that drops, delays and
  duplicates packets. Every case runs over each ordered pair of implementations, so each is also
  tested against itself, with libutp against libutp as the control
- `FirewallCallback` is an alias for `func(net.Addr) bool` in both `utp` and `purego`, so a
  `*purego.Socket` satisfies `utp.Socket` outright and is handed back as itself: a caller can
  assert for it to reach the implementation's own API
