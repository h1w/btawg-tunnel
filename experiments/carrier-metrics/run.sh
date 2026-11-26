#!/usr/bin/env bash
# Run the carrier-metrics harness across modes, experiments, and delays.
# Usage: ./run.sh [output-dir]
#
# Requirements:
#   - Go toolchain for the btawg module.
#   - Root privileges if you want tc delay and tcpdump capture.
#   - tcpdump in /usr/bin or in $TOOLS_DIR.
#
# The harness itself does not need root, but without it the delay is zero
# and no pcaps are captured.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
OUTDIR="${1:-$ROOT/results}"
mkdir -p "$OUTDIR"

TOOLS_DIR="${TOOLS_DIR:-/tmp/btawg-dpi-harness.tjayWT8n/tools/usr/bin}"
CAPTURE_DIR="$OUTDIR/pcaps"

# Default matrix. Override with env vars to narrow runs.
MODES="${MODES:-ext-tcp,ext-utp,req-tcp,req-utp,tls-ext-tcp,tls-ext-utp,tls-req-tcp,tls-req-utp}"
EXPERIMENTS="${EXPERIMENTS:-baseline,req-window,req-varied,ext-protected,ext-packed,ext-padded}"
DELAYS="${DELAYS:-0,40,200}"
REPEATS="${REPEATS:-3}"

# Optional lab timing knobs.
PACE_MBPS="${PACE_MBPS:-0}"
IDLE_KEEPALIVES="${IDLE_KEEPALIVES:-0}"

echo "Building carrier-metrics..."
cd "$ROOT"
go build -o "$OUTDIR/measure" ./cmd/measure

OUT="$OUTDIR/measurements.jsonl"
: > "$OUT"

echo "Running matrix: modes=$MODES experiments=$EXPERIMENTS delays=$DELAYS repeats=$REPEATS"
if [ "$PACE_MBPS" != "0" ]; then
  echo "  pace=$PACE_MBPS Mbps"
fi
if [ "$IDLE_KEEPALIVES" != "0" ]; then
  echo "  idle-keepalives=$IDLE_KEEPALIVES"
fi

# Prompt for root if tc/tcpdump are wanted but not available.
if [ "$(id -u)" -ne 0 ]; then
  echo "WARNING: not root; tc delay and tcpdump capture will be skipped." >&2
fi

"$OUTDIR/measure" \
  -modes "$MODES" \
  -experiments "$EXPERIMENTS" \
  -delays "$DELAYS" \
  -repeats "$REPEATS" \
  -capture "$CAPTURE_DIR" \
  -tools "$TOOLS_DIR" \
  -out "$OUT" \
  -pace-mbps "$PACE_MBPS" \
  -idle-keepalives "$IDLE_KEEPALIVES" \
  2>"$OUTDIR/run.log"

echo "Wrote $OUT"

if [ -f "$ROOT/analyze.py" ]; then
  python3 "$ROOT/analyze.py" "$OUT" > "$OUTDIR/summary.md"
  echo "Wrote $OUTDIR/summary.md"
fi
