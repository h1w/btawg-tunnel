# Hub IPv4 parsing and AWG header masking A/B

Experimental builds only; neither change adopted. Same-machine real Linux TUN, isolated veth/transit namespaces, no balancer or bitrate cap, zero added delay; eight CUBIC streams, 2 s warmup and 8 s nominal measurement. Stock offloads at 0%, calibrated selective UDP segmentation changes at 1%. CPU affinity held constant. Each loss/candidate pair ran A/B/B/A. Results below are medians of two trials per variant. The control includes the existing constant-time flow LRU.

| Candidate | Mode | Loss | Download Mbit/s A → B | Loaded download p95 ms A → B |
|---|---|---:|---:|---:|
| Cache immutable IPv4 selectors across the queue | WG | 0% | 1094 → 751 | 5.73 → 10.30 |
| Cache immutable IPv4 selectors across the queue | AWG | 0% | 966 → 1036 | 8.24 → 6.27 |
| Cache immutable IPv4 selectors across the queue | WG | 1% | 353 → 309 | 4.00 → 3.75 |
| Cache immutable IPv4 selectors across the queue | AWG | 1% | 338 → 339 | 3.71 → 3.19 |
| Remove heap escape in AWG transport header masking | AWG | 0% | 1067 → 1069 | 6.31 → 6.22 |
| Remove heap escape in AWG transport header masking | AWG | 1% | 319 → 298 | 4.64 → 4.43 |

- The parser variant cached only packet fields, not routing or policy decisions; it still revalidated both at dequeue. Server tests under `-race` passed on the overlay build. WG clean download was lower in both pairs; sampled median CPU frequency was also lower in the candidate's WG trials (2308 MHz vs 2439 MHz). The results do not isolate the cause of the difference; they do not justify deploying the shared hot-path change.
- AWG's scratch change passed a byte-equivalence check for the same key, nonce, and header, and moved from one heap allocation to zero. The existing native WG/AWG client tests passed. End-to-end trials used an enabled AWG header protection key and random trailers; neither algorithm nor wire format changed. Clean AWG throughput was effectively flat, and the 1% pair was mixed. No dependency fork adopted.
- `measurements.tsv` and `raw.tar.gz` contain 40 successful measurements in 16 run groups, including parser upload trials, CPU samples, loaded ping and qdisc evidence. `before/` is unchanged.
