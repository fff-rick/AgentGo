#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
export AGENTGO_PORT=18080 POSTGRES_PORT=15432 MILVUS_PORT=19531 MILVUS_WEBUI_PORT=19122
export SEARXNG_PORT=17070 DOCLING_PORT=15001 PROMETHEUS_PORT=19090 TEMPO_PORT=13200 GRAFANA_PORT=13000
exec docker compose -p agentgo-benchmark -f compose.yaml -f benchmarks/compose.yaml "$@"
