#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
export AGENTGO_PORT=18081 POSTGRES_PORT=15433 MILVUS_PORT=19532 MILVUS_WEBUI_PORT=19123
export SEARXNG_PORT=17071 DOCLING_PORT=15002 PROMETHEUS_PORT=19091 TEMPO_PORT=13201 GRAFANA_PORT=13001
exec docker compose -p agentgo-benchmark-real -f compose.yaml -f benchmarks/compose-gpt.yaml "$@"
