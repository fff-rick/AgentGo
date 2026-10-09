#!/bin/bash
set -euo pipefail

base_url="${BENCH_URL:-http://localhost:8080}"
dataset="${BENCH_DATASET:-benchmarks/datasets/planner-laya-shadow.seed.jsonl}"
repeat="${BENCH_REPEAT:-50}"
concurrency="${BENCH_CONCURRENCY:-2}"
report_dir="${BENCH_REPORT_DIR:-benchmarks/reports}"
mkdir -p "$report_dir"
e2e_report="$report_dir/laya-shadow-e2e.json"
summary_report="$report_dir/laya-shadow-summary.json"
before_metrics=$(mktemp)
after_metrics=$(mktemp)
trap 'rm -f "$before_metrics" "$after_metrics"' EXIT

metric() {
  awk -v key="$2" '$1 == key { value=$2; found=1 } END { if (found) print value; else print 0 }' "$1"
}
delta() {
  awk -v before="$1" -v after="$2" 'BEGIN { print after-before }'
}

curl -fsS "$base_url/health" >/dev/null
curl -fsS "$base_url/metrics" -o "$before_metrics"
warmup_success=$(metric "$before_metrics" 'agentgo_laya_requests_total{decision="startup_warmup",result="success"}')
if ! awk -v successes="$warmup_success" 'BEGIN { exit !(successes >= 2) }'; then
  echo "Laya shadow is not ready: startup_warmup successes=$warmup_success, want >=2" >&2
  exit 1
fi

go run ./cmd/benchmark \
  -base-url "$base_url" \
  -dataset "$dataset" \
  -repeat "$repeat" \
  -concurrency "$concurrency" \
  -timeout "${BENCH_TIMEOUT:-3m}" \
  -backend real \
  -scenario laya-shadow \
  -model "${BENCH_MODEL:-configured}" \
  -config-version "${BENCH_CONFIG_VERSION:-shadow-0.6}" \
  -output "$e2e_report" >/dev/null

sleep 3
curl -fsS "$base_url/metrics" -o "$after_metrics"

success=$(delta \
  "$(metric "$before_metrics" 'agentgo_laya_requests_total{decision="planner_tool_selection",result="success"}')" \
  "$(metric "$after_metrics" 'agentgo_laya_requests_total{decision="planner_tool_selection",result="success"}')")
errors=$(delta \
  "$(metric "$before_metrics" 'agentgo_laya_requests_total{decision="planner_tool_selection",result="error"}')" \
  "$(metric "$after_metrics" 'agentgo_laya_requests_total{decision="planner_tool_selection",result="error"}')")
low_confidence=$(delta \
  "$(metric "$before_metrics" 'agentgo_laya_fallbacks_total{decision="planner_tool_selection",reason="low_confidence"}')" \
  "$(metric "$after_metrics" 'agentgo_laya_fallbacks_total{decision="planner_tool_selection",reason="low_confidence"}')")
invalid_response=$(delta \
  "$(metric "$before_metrics" 'agentgo_laya_fallbacks_total{decision="planner_tool_selection",reason="invalid_response"}')" \
  "$(metric "$after_metrics" 'agentgo_laya_fallbacks_total{decision="planner_tool_selection",reason="invalid_response"}')")
policy=$(delta \
  "$(metric "$before_metrics" 'agentgo_laya_fallbacks_total{decision="planner_tool_selection",reason="policy"}')" \
  "$(metric "$after_metrics" 'agentgo_laya_fallbacks_total{decision="planner_tool_selection",reason="policy"}')")
no_candidates=$(delta \
  "$(metric "$before_metrics" 'agentgo_laya_fallbacks_total{decision="planner_tool_selection",reason="no_candidates"}')" \
  "$(metric "$after_metrics" 'agentgo_laya_fallbacks_total{decision="planner_tool_selection",reason="no_candidates"}')")
disagreements=$(delta \
  "$(metric "$before_metrics" 'agentgo_laya_disagreements_total{decision="planner_tool_selection"}')" \
  "$(metric "$after_metrics" 'agentgo_laya_disagreements_total{decision="planner_tool_selection"}')")
duration_sum=$(delta \
  "$(metric "$before_metrics" 'agentgo_laya_duration_seconds_sum{decision="planner_tool_selection"}')" \
  "$(metric "$after_metrics" 'agentgo_laya_duration_seconds_sum{decision="planner_tool_selection"}')")
duration_count=$(delta \
  "$(metric "$before_metrics" 'agentgo_laya_duration_seconds_count{decision="planner_tool_selection"}')" \
  "$(metric "$after_metrics" 'agentgo_laya_duration_seconds_count{decision="planner_tool_selection"}')")

jq -n \
  --arg generated_at "$(date -u +%FT%TZ)" \
  --arg dataset "$dataset" \
  --argjson repeat "$repeat" \
  --argjson concurrency "$concurrency" \
  --argjson success "$success" \
  --argjson errors "$errors" \
  --argjson low_confidence "$low_confidence" \
  --argjson invalid_response "$invalid_response" \
  --argjson policy "$policy" \
  --argjson no_candidates "$no_candidates" \
  --argjson disagreements "$disagreements" \
  --argjson duration_sum "$duration_sum" \
  --argjson duration_count "$duration_count" \
  --slurpfile e2e "$e2e_report" '
    ($success + $errors) as $requests |
    ([0, $success - $low_confidence - $invalid_response] | max) as $comparable |
    ([0, $comparable - $disagreements] | max) as $agreements |
    {
      generated_at:$generated_at,
      dataset:$dataset,
      repeat:$repeat,
      concurrency:$concurrency,
      e2e:($e2e[0] | del(.results)),
      case_map:[$e2e[0].results[] | {id,run,task_sha256}],
      failed_results:[$e2e[0].results[] | select(.passed == false)],
      laya_shadow:{
        requests:$requests,
        successes:$success,
        errors:$errors,
        error_rate:(if $requests == 0 then null else $errors / $requests end),
        low_confidence_fallbacks:$low_confidence,
        invalid_response_fallbacks:$invalid_response,
        policy_fallbacks:$policy,
        no_candidate_fallbacks:$no_candidates,
        comparable_decisions:$comparable,
        agreements:$agreements,
        disagreements:$disagreements,
        agreement_rate:(if $comparable == 0 then null else $agreements / $comparable end),
        average_latency_ms:(if $duration_count == 0 then null else 1000 * $duration_sum / $duration_count end)
      }
    }' | tee "$summary_report"
