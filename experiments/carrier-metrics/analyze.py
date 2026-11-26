#!/usr/bin/env python3
"""Summarize carrier-metrics JSONL results.

Usage: python3 analyze.py measurements.jsonl [> summary.md]
"""
import json
import sys
from collections import defaultdict
from statistics import mean


def load(path):
    rows = []
    with open(path) as f:
        for line in f:
            line = line.strip()
            if line:
                rows.append(json.loads(line))
    return rows


def key(row):
    return (row.get("mode"), row.get("experiment"), row.get("delay_ms"))


def summarize(rows):
    groups = defaultdict(list)
    for r in rows:
        groups[key(r)].append(r)

    print("# Carrier metrics summary\n")
    print("| mode | experiment | delay_ms | repeats | small_recv | first_ms | p95_ms | p99_ms | conc_p95_ms | bulk_Mbps | error |")
    print("|------|------------|----------|---------|------------|----------|--------|--------|-------------|-----------|-------|")
    for (mode, exp, delay), grp in sorted(groups.items()):
        repeats = len(grp)
        recv = [r["small_received"] for r in grp]
        first = [r["first_echo_ms"] for r in grp if r["first_echo_ms"] > 0]
        p95 = [r["small_p95_ms"] for r in grp if r["small_p95_ms"] > 0]
        p99 = [r["small_p99_ms"] for r in grp if r["small_p99_ms"] > 0]
        conc_p95 = [r["concurrent_small_p95_ms"] for r in grp if r.get("concurrent_small_p95_ms", 0) > 0]
        bulk = [r["bulk_mbps"] for r in grp if r["bulk_mbps"] > 0]
        errors = [r.get("error", "") for r in grp if r.get("error")]
        print(
            f"| {mode} | {exp} | {delay} | {repeats} | "
            f"{mean(recv):.0f} | {mean(first):.3f} | {mean(p95):.3f} | {mean(p99):.3f} | "
            f"{mean(conc_p95):.3f} | {mean(bulk):.3f} | {len(errors)} |"
        )

    print("\n## Baseline vs experiment deltas (per mode/delay)\n")
    print("| mode | delay_ms | experiment | first_delta | p95_delta | p99_delta | conc_p95_delta | bulk_delta |")
    print("|------|----------|------------|-------------|-----------|-----------|----------------|------------|")
    baselines = {k: g for k, g in groups.items() if k[1] == "baseline"}
    for (mode, exp, delay), grp in sorted(groups.items()):
        if exp == "baseline":
            continue
        base = baselines.get((mode, "baseline", delay))
        if not base:
            continue
        def avg(field, g):
            vals = [r[field] for r in g if r.get(field, 0) > 0]
            return mean(vals) if vals else 0.0
        first_d = avg("first_echo_ms", grp) - avg("first_echo_ms", base)
        p95_d = avg("small_p95_ms", grp) - avg("small_p95_ms", base)
        p99_d = avg("small_p99_ms", grp) - avg("small_p99_ms", base)
        conc_p95_d = avg("concurrent_small_p95_ms", grp) - avg("concurrent_small_p95_ms", base)
        bulk_d = avg("bulk_mbps", grp) - avg("bulk_mbps", base)
        print(f"| {mode} | {delay} | {exp} | {first_d:+.3f} | {p95_d:+.3f} | {p99_d:+.3f} | {conc_p95_d:+.3f} | {bulk_d:+.3f} |")


if __name__ == "__main__":
    rows = load(sys.argv[1])
    summarize(rows)
