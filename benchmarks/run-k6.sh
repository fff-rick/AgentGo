#!/bin/bash
set -euo pipefail
scenario="${1:?usage: benchmarks/run-k6.sh baseline|load|stream|stress}"
case "$scenario" in baseline|load|stream|stress) ;; *) echo 'unknown scenario' >&2; exit 2;; esac
directory="${BENCH_REPORT_DIR:-benchmarks/reports}"
mkdir -p "$directory"
summary="$directory/k6-$scenario.summary.json"
export BASE_URL="${BASE_URL:-http://localhost:18080}"
k6 run --summary-export "$summary" "benchmarks/load/$scenario.js"
commit=$(git rev-parse HEAD)
dirty=false
if [ -n "$(git status --porcelain)" ]; then dirty=true; fi
jq -n --arg commit "$commit" --argjson dirty "$dirty" --arg backend "${BENCH_BACKEND:-stub}" --arg scenario "$scenario" --arg model "${BENCH_MODEL:-benchmark-primary}" --arg config "${BENCH_CONFIG_VERSION:-benchmarks/config.stub.yaml}" --arg machine "$(uname -s)/$(uname -m) $(hostname) ($(nproc) CPUs)" --slurpfile summary "$summary" '{metadata:{commit:$commit,dirty:$dirty,backend:$backend,scenario:$scenario,model:$model,config_version:$config,machine:$machine},summary:$summary[0]}' > "$directory/k6-$scenario.json"
