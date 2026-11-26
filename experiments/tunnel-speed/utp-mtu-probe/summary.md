# µTP: identify the packet actually sent as an MTU probe

When peer credit opens, `sendPacket` can transmit an older queued DATA packet while `seqNr-1` still refers to a later queued packet. Recording the latter as the probe meant an ACK for the actual full-size datagram did not advance the confirmed MTU floor; the smaller unrelated packet could inherit the probe result. The sender now uses the sequence in the transmitted header for both first-flight timing and MTU probe bookkeeping, retaining the zero-sequence sentinel. No framing, discovery threshold, congestion policy, or queue capacity change.

A deterministic regression queues one 1,402-byte datagram and a short tail at zero credit, then admits just the first. Its ACK left the MTU floor at 1,200 on stock and advanced it to 1,402 after the fix. The focused regression and the related retransmit/window tests passed ten times under `-race`. The full Pure Go race suite passed. Interop initially could not link under the temporary filesystem quota and passed under `-race` with the build directory on workspace storage. Linux hub/client and Windows amd64 client built.

The follow-up closes the corresponding SACK path: a probe delivered after a lost earlier DATA can be retired by a selective ACK before any cumulative ACK reaches it. Previously the packet left the send queue without advancing the floor, so the later cumulative ACK had no probe to inspect. The common `ackPacket` retirement now confirms either kind of ACK. The regression holds back DATA/20, selectively acknowledges the 1,402-byte DATA/21, and checks that the MTU floor advances without waiting for DATA/20 or issuing a retry. It failed before the change and the focused race regressions passed ten times afterward. The throughput comparison below predates this follow-up.

## Managed real-TUN check

Both Extended/µTP and Request/µTP, eight CUBIC flows per direction, independently seeded 1% loss in both directions, same selective offloads and 2 s warmup + 5 s receiver interval. One attempt per arm/cell, only the MTU sequence fix differs. Stock **7/8**, patched **8/8** valid. One stock Request upload failed at +200 ms; no causal link to the probe bug was established.

At 1% +200 ms, patched Extended delivered **0.556 / 0.633 Mbit/s** and Request **0.440 / 0.646 Mbit/s** (down/up), with zero measured endpoint queue or excess transit drops. At 1% local the rates were mixed. This fixes probe accounting, not the outer congestion window's loss response.

[16 attempts](measurements.tsv) · [Raw receiver data, counters, source snapshots, hashes and replay](raw.tar.gz). `python3 -I matrix/summarize.py mtu.log` in the extracted archive reproduced the TSV byte for byte; `bash replay.sh <matching-checkout>` rebuilds and repeats the comparison.

## Follow-up: first-flight timer and current-build smoke

The first-flight RTO had still used the cached socket clock even though the outgoing packet already carried a newer timestamp. The timer now uses the packet timestamp without another clock read. With cached socket time 1,000 ms and a packet stamped 1,100 ms, the regression used to see a retry at 2,099 ms on the same clock. It now checks no retry at 2,099 ms and a retry at 2,100 ms. Five ACK/credit/RTO regressions passed 20 times under `-race`; the full `purego` and libutp `interop` race suites passed. Linux hub/client and Windows amd64 client built.

With both follow-ups applied, all four managed real-TUN intervals completed at 1% bidirectional loss and +200 ms RTT: Extended/µTP **0.617 / 0.568 Mbit/s**, Request/µTP **0.393 / 0.599 Mbit/s** (down/up). Endpoint queue, UDP receive-buffer, and excess transit drops were all zero. This is a current-build smoke, not a matched speedup claim. [Four measurements](final-timer.tsv) · [Raw samples, reducer, source and hashes](final-timer.tar.gz). The extracted `python3 -I matrix/summarize.py final-timer.log` reproduces the table byte for byte.
