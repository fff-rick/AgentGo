#!/bin/bash
set -euo pipefail
scenario="${1:?usage: benchmarks/fault.sh SCENARIO}"
base="${BENCH_URL:-http://localhost:18080}"
stub="${BENCH_STUB_URL:-http://localhost:18088}"
proxy="${BENCH_PROXY_URL:-http://localhost:18099}"
directory="${BENCH_REPORT_DIR:-benchmarks/reports}"
mkdir -p "$directory"
reset() {
  curl -fsS -H 'Content-Type: application/json' -d '{}' "$stub/admin/fault" >/dev/null || true
  curl -fsS -H 'Content-Type: application/json' -d '{}' "$proxy/fault/redis" >/dev/null || true
  curl -fsS -H 'Content-Type: application/json' -d '{}' "$proxy/fault/milvus" >/dev/null || true
}
trap reset EXIT
reset
dataset=benchmarks/datasets/fault-probe.jsonl
during_dataset=
repeats=1
case "$scenario" in
  llm_429) endpoint="$stub/admin/fault"; payload='{"llm":"429","llm_model":"benchmark-primary"}'; repeats=3 ;;
  llm_500) endpoint="$stub/admin/fault"; payload='{"llm":"500","llm_model":"benchmark-primary"}'; repeats=3 ;;
  llm_timeout) endpoint="$stub/admin/fault"; payload='{"llm":"timeout","llm_model":"benchmark-primary"}'; repeats=3 ;;
  invalid_tool) endpoint="$stub/admin/fault"; payload='{"llm":"invalid_tool"}' ;;
  embedding_timeout) endpoint="$stub/admin/fault"; payload='{"embedding":"timeout"}'; dataset=benchmarks/datasets/fault-rag.jsonl ;;
  search_500) endpoint="$stub/admin/fault"; payload='{"search":"500"}'; dataset=benchmarks/datasets/fault-search.jsonl ;;
  tool_timeout) endpoint="$stub/admin/fault"; payload='{"search":"timeout"}'; dataset=benchmarks/datasets/fault-search.jsonl ;;
  redis_drop) endpoint="$proxy/fault/redis"; payload='{"mode":"drop"}' ;;
  milvus_drop) endpoint="$proxy/fault/milvus"; payload='{"mode":"drop"}'; dataset=benchmarks/datasets/fault-rag.jsonl ;;
  sse_disconnect)
    for attempt in $(seq 1 15); do
      go run ./cmd/benchmark-sse -base-url "$base" -backend stub -scenario "${scenario}-before" -count 3 -concurrency 3 -output "$directory/${scenario}-before.json" >/dev/null
      if jq -e '.completed == 3 and .failed == 0' "$directory/${scenario}-before.json" >/dev/null; then break; fi
      sleep 3
    done
    jq -e '.completed == 3 and .failed == 0' "$directory/${scenario}-before.json" >/dev/null
    curl -fsS -H 'Content-Type: application/json' -d '{"stream_delay_ms":500}' "$stub/admin/fault" >/dev/null
    go run ./cmd/benchmark-sse -base-url "$base" -backend stub -scenario "$scenario" -count 3 -concurrency 3 -disconnect-after-first-delta -output "$directory/$scenario.json" >/dev/null
    reset
    for attempt in $(seq 1 15); do
      go run ./cmd/benchmark-sse -base-url "$base" -backend stub -scenario "${scenario}-recovery" -count 3 -concurrency 3 -output "$directory/${scenario}-recovery.json" >/dev/null
      if jq -e '.completed == 3 and .failed == 0' "$directory/${scenario}-recovery.json" >/dev/null; then break; fi
      sleep 3
    done
    jq -e '.completed == 3 and .failed == 0' "$directory/${scenario}-recovery.json" >/dev/null
    jq -n --slurpfile before "$directory/${scenario}-before.json" --slurpfile during "$directory/$scenario.json" --slurpfile recovery "$directory/${scenario}-recovery.json" '{before:$before[0],during:$during[0],recovery:$recovery[0]}' | tee "$directory/${scenario}-summary.json"
    exit ;;
  context_limit)
    temp=$(mktemp)
    trap 'rm -f "$temp"; reset' EXIT
    python3 -c 'import json;print(json.dumps({"id":"context-limit","category":"fault","message":"请记住："+"长文本"*40000,"expected":{"expected_status":413}},ensure_ascii=False))' > "$temp"
    during_dataset="$temp" ;;
  *) echo "unknown scenario: $scenario" >&2; exit 2 ;;
esac
if [ -z "$during_dataset" ]; then during_dataset="$dataset"; fi
go run ./cmd/benchmark -base-url "$base" -dataset "$dataset" -backend stub -scenario "${scenario}-before" -output "$directory/${scenario}-before.json" >/dev/null
stats_before=$(curl -fsS "$stub/admin/stats")
if [ "${endpoint:-}" != "" ]; then curl -fsS -H 'Content-Type: application/json' -d "$payload" "$endpoint" >/dev/null; fi
go run ./cmd/benchmark -base-url "$base" -dataset "$during_dataset" -backend stub -scenario "$scenario" -repeat "$repeats" -output "$directory/${scenario}.json" >/dev/null
stats_during=$(curl -fsS "$stub/admin/stats")
reset
go run ./cmd/benchmark -base-url "$base" -dataset "$dataset" -backend stub -scenario "${scenario}-recovery" -output "$directory/${scenario}-recovery.json" >/dev/null
stats_recovery=$(curl -fsS "$stub/admin/stats")
jq -n --argjson first "$stats_before" --argjson middle "$stats_during" --argjson last "$stats_recovery" --slurpfile before "$directory/${scenario}-before.json" --slurpfile during "$directory/${scenario}.json" --slurpfile recovery "$directory/${scenario}-recovery.json" '{before:$before[0].results,during:$during[0].results,recovery:$recovery[0].results,upstream_calls_during:{llm:($middle.llm_requests-$first.llm_requests),embedding:($middle.embedding_requests-$first.embedding_requests),search:($middle.search_requests-$first.search_requests)},upstream_calls_recovery:{llm:($last.llm_requests-$middle.llm_requests),embedding:($last.embedding_requests-$middle.embedding_requests),search:($last.search_requests-$middle.search_requests)}}' | tee "$directory/${scenario}-summary.json"
