#!/bin/bash
set -euo pipefail
seconds="${1:-300}"
interval="${BENCH_SAMPLE_INTERVAL:-5}"
output="${BENCH_REPORT_DIR:-benchmarks/reports}/resources.jsonl"
mkdir -p "$(dirname "$output")"
: > "$output"
end=$((SECONDS + seconds))
while (( SECONDS < end )); do
  bash benchmarks/resources.sh >> "$output"
  sleep "$interval"
done
echo "$output"
