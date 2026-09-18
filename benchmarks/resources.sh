#!/bin/bash
set -euo pipefail
prom="${BENCH_PROM_URL:-http://localhost:19090}"
container="${BENCH_CONTAINER:-agentgo-benchmark-agentgo-1}"
query() { curl -fsSG --data-urlencode "query=$1" "$prom/api/v1/query" | jq '[.data.result[]?.value[1] | tonumber] | if length == 0 then null else add end'; }
cpu=$(query 'rate(process_cpu_seconds_total[1m])')
rss=$(query 'process_resident_memory_bytes')
goroutines=$(query 'go_goroutines')
gc=$(query 'rate(go_gc_duration_seconds_sum[5m])')
redis=$(query 'histogram_quantile(0.95,sum by(le)(rate(agentgo_dependency_duration_seconds_bucket{dependency="redis"}[5m])))')
milvus=$(query 'histogram_quantile(0.95,sum by(le)(rate(agentgo_dependency_duration_seconds_bucket{dependency="milvus"}[5m])))')
embedding=$(query 'histogram_quantile(0.95,sum by(le)(rate(agentgo_embedding_duration_seconds_bucket[5m])))')
sse=$(query 'agentgo_sse_connections')
network=$(docker stats --no-stream --format '{{json .}}' "$container" | jq -r '.NetIO // empty')
commit=$(git rev-parse HEAD)
dirty=false
if [ -n "$(git status --porcelain)" ]; then dirty=true; fi
jq -cn --arg time "$(date -u +%FT%TZ)" --arg network "$network" --arg commit "$commit" --argjson dirty "$dirty" --arg backend "${BENCH_BACKEND:-stub}" --arg model "${BENCH_MODEL:-benchmark-primary}" --arg machine "$(uname -s)/$(uname -m) $(hostname) ($(nproc) CPUs)" --argjson cpu "$cpu" --argjson rss "$rss" --argjson goroutines "$goroutines" --argjson gc "$gc" --argjson redis "$redis" --argjson milvus "$milvus" --argjson embedding "$embedding" --argjson sse "$sse" '{metadata:{commit:$commit,dirty:$dirty,backend:$backend,scenario:"resource-snapshot",model:$model,machine:$machine},timestamp:$time,cpu_cores:$cpu,rss_bytes:$rss,goroutines:$goroutines,gc_pause_fraction:$gc,redis_p95_seconds:$redis,milvus_p95_seconds:$milvus,embedding_p95_seconds:$embedding,sse_connections:$sse,container_network_io:$network}'
