// Command benchmark runs black-box quality and performance checks against AgentGo.
// It intentionally uses only the public HTTP API, so the same dataset can compare
// local builds, staging deployments, models, prompts, and infrastructure versions.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/enterprise/ai-agent-go/internal/benchmarkmeta"
	"github.com/enterprise/ai-agent-go/internal/etl"
)

type assertion struct {
	KeywordsAll         []string       `json:"keywords_all,omitempty"`
	KeywordsAny         []string       `json:"keywords_any,omitempty"`
	Forbidden           []string       `json:"forbidden,omitempty"`
	Tool                string         `json:"tool,omitempty"`
	ToolsExact          []string       `json:"tools_exact,omitempty"`
	ReferenceDocIDs     []string       `json:"reference_doc_ids,omitempty"`
	ReferenceTitles     []string       `json:"reference_titles,omitempty"`
	MaxLatencyMS        int64          `json:"max_latency_ms,omitempty"`
	ToolArgs            map[string]any `json:"tool_args,omitempty"`
	MaxToolCalls        *int           `json:"max_tool_calls,omitempty"`
	MinPlannerSteps     int            `json:"min_planner_steps,omitempty"`
	PlannerStepKeywords []string       `json:"planner_step_keywords,omitempty"`
	MaxPlannerSteps     int            `json:"max_planner_steps,omitempty"`
	MaxIterations       int            `json:"max_iterations,omitempty"`
	ReferenceOrder      []string       `json:"reference_order,omitempty"`
	ExpectedStatus      int            `json:"expected_status,omitempty"`
	IsolationProbe      string         `json:"isolation_probe,omitempty"`
	IsolationForbidden  string         `json:"isolation_forbidden,omitempty"`
}

type testCase struct {
	ID           string            `json:"id"`
	Category     string            `json:"category"`
	SessionID    string            `json:"session_id,omitempty"`
	Message      string            `json:"message"`
	Turns        []string          `json:"turns,omitempty"`
	NewSessionAt int               `json:"new_session_at,omitempty"`
	TurnDelayMS  int               `json:"turn_delay_ms,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	Mode         string            `json:"mode,omitempty"`
	Expected     assertion         `json:"expected"`
}

type chatRequest struct {
	SessionID string            `json:"session_id"`
	Message   string            `json:"message"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Options   map[string]any    `json:"options,omitempty"`
}

type toolCall struct {
	ToolName string `json:"tool_name"`
	Input    string `json:"input"`
}

type reference struct {
	DocID string `json:"doc_id"`
}
type usageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type chatResponse struct {
	Content    string      `json:"content"`
	ToolCalls  []toolCall  `json:"tool_calls"`
	References []reference `json:"references"`
	Usage      *usageInfo  `json:"usage"`
	Steps      []struct {
		Type      string `json:"type"`
		ToolName  string `json:"tool_name"`
		Content   string `json:"content"`
		StepIndex int    `json:"step_index"`
	} `json:"steps"`
}

type envelope struct {
	Code    int          `json:"code"`
	Message string       `json:"message"`
	Data    chatResponse `json:"data"`
}

type caseResult struct {
	ID                string          `json:"id"`
	Category          string          `json:"category"`
	Run               int             `json:"run"`
	Success           bool            `json:"success"`
	HTTPSuccess       bool            `json:"http_success"`
	Passed            bool            `json:"passed"`
	LatencyMS         float64         `json:"latency_ms"`
	Failures          []string        `json:"failures,omitempty"`
	HTTPStatus        int             `json:"http_status,omitempty"`
	ChatRequests      int             `json:"chat_requests"`
	TraceID           string          `json:"trace_id,omitempty"`
	ToolCalls         []toolCall      `json:"tool_calls,omitempty"`
	ReferenceIDs      []string        `json:"reference_ids,omitempty"`
	StepCount         int             `json:"step_count,omitempty"`
	Retrieval         *rankingMetrics `json:"retrieval,omitempty"`
	Usage             *usageInfo      `json:"usage,omitempty"`
	ToolEvaluated     bool            `json:"-"`
	ToolPassed        bool            `json:"-"`
	ToolArgsEvaluated bool            `json:"-"`
	ToolArgsPassed    bool            `json:"-"`
	RetrievalHits     int             `json:"-"`
	RetrievalTotal    int             `json:"-"`
}

type categoryReport struct {
	Cases    int     `json:"cases"`
	Passed   int     `json:"passed"`
	PassRate float64 `json:"pass_rate"`
}

type report struct {
	Metadata                 benchmarkmeta.Metadata    `json:"metadata"`
	StartedAt                time.Time                 `json:"started_at"`
	DurationSeconds          float64                   `json:"duration_seconds"`
	Total                    int                       `json:"total"`
	Successful               int                       `json:"successful"`
	HTTPSuccessful           int                       `json:"http_successful"`
	Passed                   int                       `json:"passed"`
	SuccessRate              float64                   `json:"success_rate"`
	HTTPSuccessRate          float64                   `json:"http_success_rate"`
	Unavailable              []string                  `json:"unavailable,omitempty"`
	TaskPassRate             float64                   `json:"task_pass_rate"`
	ToolAccuracy             *float64                  `json:"tool_accuracy,omitempty"`
	ToolArgsAccuracy         *float64                  `json:"tool_args_accuracy,omitempty"`
	RetrievalRecall          *float64                  `json:"retrieval_recall,omitempty"`
	RetrievalPrecision       *float64                  `json:"retrieval_precision,omitempty"`
	RetrievalMRR             *float64                  `json:"retrieval_mrr,omitempty"`
	RetrievalNDCG            *float64                  `json:"retrieval_ndcg,omitempty"`
	UsageCoverage            *float64                  `json:"usage_coverage,omitempty"`
	PromptTokens             int                       `json:"prompt_tokens"`
	CompletionTokens         int                       `json:"completion_tokens"`
	LatencyP50MS             float64                   `json:"latency_p50_ms"`
	LatencyP95MS             float64                   `json:"latency_p95_ms"`
	LatencyP99MS             float64                   `json:"latency_p99_ms"`
	ThroughputRPS            float64                   `json:"throughput_rps"`
	ThroughputTasksPerSecond float64                   `json:"throughput_tasks_per_second"`
	ByCategory               map[string]categoryReport `json:"by_category"`
	Results                  []caseResult              `json:"results"`
}

func main() {
	var (
		baseURL       = flag.String("base-url", "http://localhost:8080", "AgentGo base URL")
		datasetPath   = flag.String("dataset", "benchmarks/datasets/smoke.example.jsonl", "JSONL dataset")
		corpusPath    = flag.String("corpus", "", "optional corpus snapshot for report metadata")
		concurrency   = flag.Int("concurrency", 1, "number of concurrent requests")
		repeat        = flag.Int("repeat", 1, "number of runs per test case")
		warmup        = flag.Int("warmup", 0, "unreported warm-up requests")
		timeout       = flag.Duration("timeout", 2*time.Minute, "per-request timeout")
		output        = flag.String("output", "", "optional JSON report path")
		backend       = flag.String("backend", "real", "real or stub model backend")
		scenario      = flag.String("scenario", "smoke", "experiment scenario")
		modelName     = flag.String("model", "", "model identifier for report metadata")
		modelParams   = flag.String("model-params", "", "non-secret model parameters for report metadata")
		configVersion = flag.String("config-version", "", "non-secret configuration version or digest")
	)
	flag.Parse()

	if *concurrency < 1 || *repeat < 1 || *warmup < 0 {
		fatalf("concurrency and repeat must be >= 1; warmup must be >= 0")
	}
	cases, err := loadCases(*datasetPath)
	if err != nil {
		fatalf("load dataset: %v", err)
	}
	if len(cases) == 0 {
		fatalf("dataset contains no cases")
	}

	client := &http.Client{Timeout: *timeout}
	endpoint := strings.TrimRight(*baseURL, "/") + "/api/v1/chat"
	for i := 0; i < *warmup; i++ {
		tc := cases[i%len(cases)]
		_, _ = execute(context.Background(), client, endpoint, tc, -1)
	}

	started := time.Now()
	results := runAll(client, endpoint, cases, *repeat, *concurrency)
	rep := buildReport(started, time.Since(started), results)
	rep.Metadata = benchmarkmeta.Collect(*datasetPath, *backend, *scenario, *modelName, *modelParams, *configVersion)
	if *corpusPath != "" {
		rep.Metadata.CorpusSHA256 = benchmarkmeta.HashFile(*corpusPath)
		if rep.Metadata.CorpusSHA256 == "" {
			fatalf("cannot hash corpus %s", *corpusPath)
		}
	}
	encoded, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fatalf("encode report: %v", err)
	}
	fmt.Println(string(encoded))
	if *output != "" {
		if err := os.MkdirAll(filepath.Dir(*output), 0o755); err != nil {
			fatalf("create report directory: %v", err)
		}
		if err := os.WriteFile(*output, append(encoded, '\n'), 0o644); err != nil {
			fatalf("write report: %v", err)
		}
	}
}

func loadCases(path string) ([]testCase, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cases []testCase
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	line := 0
	seen := make(map[string]struct{})
	for scanner.Scan() {
		line++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		var tc testCase
		if err := json.Unmarshal([]byte(raw), &tc); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if tc.ID == "" || (tc.Message == "" && len(tc.Turns) == 0) {
			return nil, fmt.Errorf("line %d: id and either message or turns are required", line)
		}
		if tc.Expected.ExpectedStatus > 0 && tc.Expected.ExpectedStatus < 400 {
			return nil, fmt.Errorf("line %d: expected_status must be an error status", line)
		}
		for _, title := range tc.Expected.ReferenceTitles {
			tc.Expected.ReferenceDocIDs = append(tc.Expected.ReferenceDocIDs, etl.DocumentID("text", title))
		}
		if _, ok := seen[tc.ID]; ok {
			return nil, fmt.Errorf("line %d: duplicate id %q", line, tc.ID)
		}
		seen[tc.ID] = struct{}{}
		cases = append(cases, tc)
	}
	return cases, scanner.Err()
}

type job struct {
	tc  testCase
	run int
}

func runAll(client *http.Client, endpoint string, cases []testCase, repeat, concurrency int) []caseResult {
	jobs := make(chan job)
	results := make(chan caseResult, len(cases)*repeat)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				result, _ := execute(context.Background(), client, endpoint, j.tc, j.run)
				results <- result
			}
		}()
	}
	go func() {
		for run := 1; run <= repeat; run++ {
			for _, tc := range cases {
				jobs <- job{tc: tc, run: run}
			}
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	out := make([]caseResult, 0, len(cases)*repeat)
	for result := range results {
		out = append(out, result)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID == out[j].ID {
			return out[i].Run < out[j].Run
		}
		return out[i].ID < out[j].ID
	})
	return out
}

var sessionSequence uint64

func execute(ctx context.Context, client *http.Client, endpoint string, tc testCase, run int) (caseResult, error) {
	result := caseResult{ID: tc.ID, Category: tc.Category, Run: run}
	userID := fmt.Sprintf("benchmark-%d", atomic.AddUint64(&sessionSequence, 1))
	sessionID, err := createBenchmarkSession(ctx, client, strings.TrimSuffix(endpoint, "/chat")+"/sessions", userID)
	if err != nil {
		result.Failures = []string{err.Error()}
		return result, err
	}
	messages := tc.Turns
	if len(messages) == 0 {
		messages = []string{tc.Message}
	}
	started := time.Now()
	var env envelope
	var status int
	for turn, message := range messages {
		if tc.NewSessionAt > 0 && turn+1 == tc.NewSessionAt {
			if tc.TurnDelayMS > 0 {
				time.Sleep(time.Duration(tc.TurnDelayMS) * time.Millisecond)
			}
			sessionID, err = createBenchmarkSession(ctx, client, strings.TrimSuffix(endpoint, "/chat")+"/sessions", userID)
			if err != nil {
				result.LatencyMS = float64(time.Since(started).Microseconds()) / 1000
				result.Failures = []string{"new session: " + err.Error()}
				return result, err
			}
		}
		result.ChatRequests++
		var err error
		payload := chatRequest{SessionID: sessionID, Message: message, Metadata: tc.Metadata}
		if tc.Mode != "" {
			payload.Options = map[string]any{"mode": tc.Mode}
		}
		env, status, result.TraceID, err = sendChat(ctx, client, endpoint, payload)
		result.HTTPStatus = status
		result.HTTPSuccess = status >= 200 && status < 300
		if tc.Expected.ExpectedStatus > 0 && status == tc.Expected.ExpectedStatus {
			result.LatencyMS = float64(time.Since(started).Microseconds()) / 1000
			result.Success = err == nil
			result.Passed = true
			return result, nil
		}
		if err != nil {
			result.LatencyMS = float64(time.Since(started).Microseconds()) / 1000
			result.Failures = []string{fmt.Sprintf("turn %d: %v", turn+1, err)}
			return result, err
		}
	}
	result.LatencyMS = float64(time.Since(started).Microseconds()) / 1000
	result.Success = true
	result.ToolCalls = env.Data.ToolCalls
	result.Usage = env.Data.Usage
	result.StepCount = len(env.Data.Steps)
	for _, ref := range env.Data.References {
		result.ReferenceIDs = append(result.ReferenceIDs, ref.DocID)
	}
	result.Failures = evaluate(tc.Expected, env.Data, result.LatencyMS, &result)
	if tc.Expected.IsolationProbe != "" {
		if tc.TurnDelayMS > 0 {
			time.Sleep(time.Duration(tc.TurnDelayMS) * time.Millisecond)
		}
		otherToken := os.Getenv("AGENTGO_ISOLATION_ACCESS_TOKEN")
		if otherToken == "" {
			result.Failures = append(result.Failures, "isolation probe requires AGENTGO_ISOLATION_ACCESS_TOKEN")
		} else {
			other, err := createBenchmarkSessionWithToken(ctx, client, strings.TrimSuffix(endpoint, "/chat")+"/sessions", userID, otherToken)
			if err != nil {
				result.Failures = append(result.Failures, "isolation setup: "+err.Error())
			} else {
				probe, _, _, err := sendChatWithToken(ctx, client, endpoint, chatRequest{SessionID: other, Message: tc.Expected.IsolationProbe}, otherToken)
				if err != nil {
					result.Failures = append(result.Failures, "isolation probe: "+err.Error())
				} else if tc.Expected.IsolationForbidden != "" && strings.Contains(strings.ToLower(probe.Data.Content), strings.ToLower(tc.Expected.IsolationForbidden)) {
					result.Failures = append(result.Failures, "isolation probe revealed protected fact")
				}
			}
		}
	}
	result.Passed = len(result.Failures) == 0
	return result, nil
}

func createBenchmarkSession(ctx context.Context, client *http.Client, endpoint, userID string) (string, error) {
	return createBenchmarkSessionWithToken(ctx, client, endpoint, userID, os.Getenv("AGENTGO_ACCESS_TOKEN"))
}
func createBenchmarkSessionWithToken(ctx context.Context, client *http.Client, endpoint, userID, token string) (string, error) {
	body, _ := json.Marshal(map[string]any{"user": map[string]string{"user_id": userID, "display_name": userID}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var payload struct {
		Code int `json:"code"`
		Data struct {
			SessionID string `json:"session_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK || payload.Code != 0 || payload.Data.SessionID == "" {
		return "", fmt.Errorf("create session: HTTP %d", resp.StatusCode)
	}
	return payload.Data.SessionID, nil
}

func sendChat(ctx context.Context, client *http.Client, endpoint string, payload chatRequest) (envelope, int, string, error) {
	return sendChatWithToken(ctx, client, endpoint, payload, os.Getenv("AGENTGO_ACCESS_TOKEN"))
}
func sendChatWithToken(ctx context.Context, client *http.Client, endpoint string, payload chatRequest, token string) (envelope, int, string, error) {
	var env envelope
	var traceBytes [16]byte
	var spanBytes [8]byte
	if _, err := rand.Read(traceBytes[:]); err != nil {
		return env, 0, "", fmt.Errorf("generate trace ID: %w", err)
	}
	if _, err := rand.Read(spanBytes[:]); err != nil {
		return env, 0, "", fmt.Errorf("generate span ID: %w", err)
	}
	traceID := fmt.Sprintf("%x", traceBytes)
	body, err := json.Marshal(payload)
	if err != nil {
		return env, 0, traceID, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return env, 0, traceID, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("traceparent", fmt.Sprintf("00-%s-%x-01", traceID, spanBytes))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return env, 0, traceID, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	status := resp.StatusCode
	if reported := resp.Header.Get("X-Trace-ID"); reported != "" {
		traceID = reported
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return env, status, traceID, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return env, status, traceID, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return env, status, traceID, fmt.Errorf("decode response: %w", err)
	}
	if env.Code != 0 {
		return env, status, traceID, fmt.Errorf("business code %d: %s", env.Code, env.Message)
	}
	return env, status, traceID, nil
}

func evaluate(want assertion, got chatResponse, latencyMS float64, result *caseResult) []string {
	content := strings.ToLower(got.Content)
	var failures []string
	for _, keyword := range want.KeywordsAll {
		if !strings.Contains(content, strings.ToLower(keyword)) {
			failures = append(failures, fmt.Sprintf("missing keyword %q", keyword))
		}
	}
	if len(want.KeywordsAny) > 0 {
		found := false
		for _, keyword := range want.KeywordsAny {
			found = found || strings.Contains(content, strings.ToLower(keyword))
		}
		if !found {
			failures = append(failures, "none of keywords_any found")
		}
	}
	for _, forbidden := range want.Forbidden {
		if strings.Contains(content, strings.ToLower(forbidden)) {
			failures = append(failures, fmt.Sprintf("forbidden text %q found", forbidden))
		}
	}
	if want.Tool != "" {
		result.ToolEvaluated = true
		result.ToolArgsEvaluated = len(want.ToolArgs) > 0
		for _, call := range got.ToolCalls {
			if call.ToolName == want.Tool {
				result.ToolPassed = true
				break
			}
		}
		if !result.ToolPassed {
			failures = append(failures, fmt.Sprintf("expected tool %q was not called", want.Tool))
		} else if len(want.ToolArgs) > 0 {
			matched := false
			for _, call := range got.ToolCalls {
				if call.ToolName != want.Tool {
					continue
				}
				var args map[string]any
				if json.Unmarshal([]byte(call.Input), &args) != nil {
					continue
				}
				matched = true
				for key, value := range want.ToolArgs {
					if !jsonEqual(args[key], value) {
						matched = false
						break
					}
				}
				if matched {
					break
				}
			}
			if !matched {
				failures = append(failures, "tool arguments do not match expected subset")
			}
			result.ToolArgsPassed = matched
		}
	}
	if want.MaxToolCalls != nil && len(got.ToolCalls) > *want.MaxToolCalls {
		failures = append(failures, fmt.Sprintf("tool calls %d exceed %d", len(got.ToolCalls), *want.MaxToolCalls))
	}
	if want.ToolsExact != nil {
		actual := make([]string, 0, len(got.ToolCalls))
		for _, call := range got.ToolCalls {
			if call.ToolName != "list_tools" {
				actual = append(actual, call.ToolName)
			}
		}
		expected := append([]string{}, want.ToolsExact...)
		sort.Strings(actual)
		sort.Strings(expected)
		if !jsonEqual(actual, expected) {
			failures = append(failures, "tool call set mismatch")
		}
	}
	plannerSteps := 0
	iterationSet := map[int]bool{}
	stepText := ""
	for _, step := range got.Steps {
		if step.Type == "action" {
			plannerSteps++
			stepText += " " + step.ToolName + " " + step.Content
			if step.ToolName != "" {
				iterationSet[step.StepIndex] = true
			}
		}
	}
	for _, keyword := range want.PlannerStepKeywords {
		if !strings.Contains(strings.ToLower(stepText), strings.ToLower(keyword)) {
			failures = append(failures, "planner step keyword missing: "+keyword)
		}
	}
	if want.MinPlannerSteps > 0 && plannerSteps < want.MinPlannerSteps {
		failures = append(failures, "too few planner steps")
	}
	if want.MaxPlannerSteps > 0 && plannerSteps > want.MaxPlannerSteps {
		failures = append(failures, "too many planner steps")
	}
	if want.MaxIterations > 0 && len(iterationSet) > want.MaxIterations {
		failures = append(failures, "too many tool actions")
	}
	if len(want.ReferenceOrder) > 0 {
		for i, id := range want.ReferenceOrder {
			if i >= len(got.References) || got.References[i].DocID != id {
				failures = append(failures, "reference order mismatch")
				break
			}
		}
	}
	if len(want.ReferenceDocIDs) > 0 {
		actual := make(map[string]struct{}, len(got.References))
		for _, ref := range got.References {
			actual[ref.DocID] = struct{}{}
		}
		result.RetrievalTotal = len(want.ReferenceDocIDs)
		for _, id := range want.ReferenceDocIDs {
			if _, ok := actual[id]; ok {
				result.RetrievalHits++
			}
		}
		if result.RetrievalHits < result.RetrievalTotal {
			failures = append(failures, fmt.Sprintf("retrieval recall %d/%d", result.RetrievalHits, result.RetrievalTotal))
		}
		ranked := make([]string, 0, len(got.References))
		for _, ref := range got.References {
			ranked = append(ranked, ref.DocID)
		}
		result.Retrieval = rankScores(want.ReferenceDocIDs, ranked)
	}
	if want.MaxLatencyMS > 0 && latencyMS > float64(want.MaxLatencyMS) {
		failures = append(failures, fmt.Sprintf("latency %.2fms exceeds %dms", latencyMS, want.MaxLatencyMS))
	}
	return failures
}

func jsonEqual(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

func buildReport(started time.Time, duration time.Duration, results []caseResult) report {
	rep := report{StartedAt: started, DurationSeconds: duration.Seconds(), Total: len(results), ByCategory: make(map[string]categoryReport), Results: results}
	latencies := make([]float64, 0, len(results))
	toolTotal, toolPassed, retrievalHits, retrievalTotal := 0, 0, 0, 0
	toolArgsTotal, toolArgsPassed := 0, 0
	usageCount := 0
	chatRequests := 0
	rankCount := 0
	var precision, mrr, ndcg float64
	for _, result := range results {
		chatRequests += result.ChatRequests
		latencies = append(latencies, result.LatencyMS)
		if result.Success {
			rep.Successful++
		}
		if result.HTTPSuccess {
			rep.HTTPSuccessful++
		}
		if result.Passed {
			rep.Passed++
		}
		if result.ToolEvaluated {
			toolTotal++
			if result.ToolPassed {
				toolPassed++
			}
		}
		if result.ToolArgsEvaluated {
			toolArgsTotal++
			if result.ToolArgsPassed {
				toolArgsPassed++
			}
		}
		retrievalHits += result.RetrievalHits
		retrievalTotal += result.RetrievalTotal
		if result.Retrieval != nil {
			rankCount++
			precision += result.Retrieval.Precision
			mrr += result.Retrieval.MRR
			ndcg += result.Retrieval.NDCG
		}
		if result.Usage != nil {
			usageCount++
			rep.PromptTokens += result.Usage.PromptTokens
			rep.CompletionTokens += result.Usage.CompletionTokens
		}
		cat := rep.ByCategory[result.Category]
		cat.Cases++
		if result.Passed {
			cat.Passed++
		}
		rep.ByCategory[result.Category] = cat
	}
	for name, cat := range rep.ByCategory {
		cat.PassRate = ratio(cat.Passed, cat.Cases)
		rep.ByCategory[name] = cat
	}
	rep.SuccessRate = ratio(rep.Successful, rep.Total)
	rep.HTTPSuccessRate = ratio(rep.HTTPSuccessful, rep.Total)
	rep.TaskPassRate = ratio(rep.Passed, rep.Total)
	if toolTotal > 0 {
		value := ratio(toolPassed, toolTotal)
		rep.ToolAccuracy = &value
	}
	if toolArgsTotal > 0 {
		v := ratio(toolArgsPassed, toolArgsTotal)
		rep.ToolArgsAccuracy = &v
	}
	if retrievalTotal > 0 {
		value := ratio(retrievalHits, retrievalTotal)
		rep.RetrievalRecall = &value
	}
	if toolTotal == 0 {
		rep.Unavailable = append(rep.Unavailable, "tool_accuracy: no labeled tool cases")
	}
	if retrievalTotal == 0 {
		rep.Unavailable = append(rep.Unavailable, "retrieval_metrics: no labeled references")
	}
	if rep.Total > 0 {
		v := ratio(usageCount, rep.Total)
		rep.UsageCoverage = &v
	}
	if usageCount == 0 {
		rep.Unavailable = append(rep.Unavailable, "token_usage: provider did not report complete usage")
	}
	rep.Unavailable = append(rep.Unavailable, "monetary_cost: deferred")
	if rankCount > 0 {
		p := precision / float64(rankCount)
		m := mrr / float64(rankCount)
		n := ndcg / float64(rankCount)
		rep.RetrievalPrecision = &p
		rep.RetrievalMRR = &m
		rep.RetrievalNDCG = &n
	}
	sort.Float64s(latencies)
	rep.LatencyP50MS = percentile(latencies, 0.50)
	rep.LatencyP95MS = percentile(latencies, 0.95)
	rep.LatencyP99MS = percentile(latencies, 0.99)
	if duration > 0 {
		rep.ThroughputRPS = float64(chatRequests) / duration.Seconds()
		rep.ThroughputTasksPerSecond = float64(rep.Total) / duration.Seconds()
	}
	return rep
}

func ratio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(q*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "benchmark: "+format+"\n", args...)
	os.Exit(2)
}
