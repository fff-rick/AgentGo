#!/bin/bash
set -euo pipefail
base="${BENCH_URL:-http://localhost:18080}"
stub="${BENCH_STUB_URL:-http://localhost:18088}"
directory="${BENCH_REPORT_DIR:-benchmarks/reports}"
mkdir -p "$directory"
trap 'curl -fsS -H "Content-Type: application/json" -d "{}" "$stub/admin/fault" >/dev/null || true' EXIT
curl -fsS -H 'Content-Type: application/json' -d '{"stream_chunks":300}' "$stub/admin/fault" >/dev/null
go run ./cmd/benchmark-sse -base-url "$base" -backend stub -scenario slow-client-baseline -count 10 -concurrency 5 -output "$directory/slow-client-baseline.json" >/dev/null
go run ./cmd/benchmark-sse -base-url "$base" -backend stub -scenario slow-client -count 10 -concurrency 5 -slow-read 20ms -output "$directory/slow-client.json" >/dev/null
jq -n --slurpfile baseline "$directory/slow-client-baseline.json" --slurpfile slow "$directory/slow-client.json" '{baseline:{p95_duration_ms:$baseline[0].p95_duration_ms,p95_ttft_ms:$baseline[0].p95_ttft_ms,completed:$baseline[0].completed},slow:{p95_duration_ms:$slow[0].p95_duration_ms,p95_ttft_ms:$slow[0].p95_ttft_ms,completed:$slow[0].completed}}' | tee "$directory/slow-client-summary.json"
