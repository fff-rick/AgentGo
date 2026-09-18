#!/bin/sh
set -eu
base_url="${BENCH_URL:-http://localhost:18080}"
dataset="${1:-benchmarks/datasets/corpus.example.jsonl}"
while IFS= read -r row; do
  [ -n "$row" ] || continue
  if [ -n "${AGENTGO_ACCESS_TOKEN:-}" ]; then
    response=$(printf '%s' "$row" | curl -fsS -H 'Content-Type: application/json' -H "Authorization: Bearer $AGENTGO_ACCESS_TOKEN" --data-binary @- "$base_url/api/v1/documents")
  else
    response=$(printf '%s' "$row" | curl -fsS -H 'Content-Type: application/json' --data-binary @- "$base_url/api/v1/documents")
  fi
  printf '%s\n' "$response" | jq -e '.code == 0' >/dev/null
  printf '%s\n' "$response" | jq -r '.data.doc_id'
done < "$dataset"
