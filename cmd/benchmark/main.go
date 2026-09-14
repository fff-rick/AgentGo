// Command benchmark runs black-box quality and performance checks against AgentGo.
// It intentionally uses only the public HTTP API, so the same dataset can compare
// local builds, staging deployments, models, prompts, and infrastructure versions.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type assertion struct {
	KeywordsAll     []string `json:"keywords_all,omitempty"`
	KeywordsAny     []string `json:"keywords_any,omitempty"`
	Forbidden       []string `json:"forbidden,omitempty"`
	Tool            string   `json:"tool,omitempty"`
	ReferenceDocIDs []string `json:"reference_doc_ids,omitempty"`
	MaxLatencyMS    int64    `json:"max_latency_ms,omitempty"`
}

type testCase struct {
	ID        string            `json:"id"`
	Category  string            `json:"category"`
	SessionID string            `json:"session_id,omitempty"`
	Message   string            `json:"message"`
	Turns     []string          `json:"turns,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Expected  assertion         `json:"expected"`
}

type chatRequest struct {
	SessionID string            `json:"session_id"`
	Message   string            `json:"message"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type toolCall struct {
	ToolName string `json:"tool_name"`
}

type reference struct {
	DocID string `json:"doc_id"`
}

type chatResponse struct {
	Content    string      `json:"content"`
	ToolCalls  []toolCall  `json:"tool_calls"`
	References []reference `json:"references"`
}

type envelope struct {
	Code    int          `json:"code"`
	Message string       `json:"message"`
	Data    chatResponse `json:"data"`
}

type caseResult struct {
	ID             string   `json:"id"`
	Category       string   `json:"category"`
	Run            int      `json:"run"`
	Success        bool     `json:"success"`
	Passed         bool     `json:"passed"`
	LatencyMS      float64  `json:"latency_ms"`
	Failures       []string `json:"failures,omitempty"`
	ToolEvaluated  bool     `json:"-"`
	ToolPassed     bool     `json:"-"`
	RetrievalHits  int      `json:"-"`
	RetrievalTotal int      `json:"-"`
}

type categoryReport struct {
	Cases    int     `json:"cases"`
	Passed   int     `json:"passed"`
	PassRate float64 `json:"pass_rate"`
}

type report struct {
	StartedAt       time.Time                 `json:"started_at"`
	DurationSeconds float64                   `json:"duration_seconds"`
	Total           int                       `json:"total"`
	Successful      int                       `json:"successful"`
	Passed          int                       `json:"passed"`
	SuccessRate     float64                   `json:"success_rate"`
	TaskPassRate    float64                   `json:"task_pass_rate"`
	ToolAccuracy    *float64                  `json:"tool_accuracy,omitempty"`
	RetrievalRecall *float64                  `json:"retrieval_recall,omitempty"`
	LatencyP50MS    float64                   `json:"latency_p50_ms"`
	LatencyP95MS    float64                   `json:"latency_p95_ms"`
	LatencyP99MS    float64                   `json:"latency_p99_ms"`
	ThroughputRPS   float64                   `json:"throughput_rps"`
	ByCategory      map[string]categoryReport `json:"by_category"`
	Results         []caseResult              `json:"results"`
}

func main() {
	var (
		baseURL     = flag.String("base-url", "http://localhost:8080", "AgentGo base URL")
		datasetPath = flag.String("dataset", "benchmarks/datasets/smoke.example.jsonl", "JSONL dataset")
		concurrency = flag.Int("concurrency", 1, "number of concurrent requests")
		repeat      = flag.Int("repeat", 1, "number of runs per test case")
		warmup      = flag.Int("warmup", 0, "unreported warm-up requests")
		timeout     = flag.Duration("timeout", 2*time.Minute, "per-request timeout")
		output      = flag.String("output", "", "optional JSON report path")
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
	encoded, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fatalf("encode report: %v", err)
	}
	fmt.Println(string(encoded))
	if *output != "" {
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
	for turn, message := range messages {
		var err error
		env, err = sendChat(ctx, client, endpoint, chatRequest{SessionID: sessionID, Message: message, Metadata: tc.Metadata})
		if err != nil {
			result.LatencyMS = float64(time.Since(started).Microseconds()) / 1000
			result.Failures = []string{fmt.Sprintf("turn %d: %v", turn+1, err)}
			return result, err
		}
	}
	result.LatencyMS = float64(time.Since(started).Microseconds()) / 1000
	result.Success = true
	result.Failures = evaluate(tc.Expected, env.Data, result.LatencyMS, &result)
	result.Passed = len(result.Failures) == 0
	return result, nil
}

func createBenchmarkSession(ctx context.Context, client *http.Client, endpoint, userID string) (string, error) {
	body, _ := json.Marshal(map[string]any{"user": map[string]string{"user_id": userID, "display_name": userID}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var payload struct {
		Data struct {
			SessionID string `json:"session_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK || payload.Data.SessionID == "" {
		return "", fmt.Errorf("create session: HTTP %d", resp.StatusCode)
	}
	return payload.Data.SessionID, nil
}

func sendChat(ctx context.Context, client *http.Client, endpoint string, payload chatRequest) (envelope, error) {
	var env envelope
	body, err := json.Marshal(payload)
	if err != nil {
		return env, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return env, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return env, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return env, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return env, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return env, fmt.Errorf("decode response: %w", err)
	}
	if env.Code != 0 {
		return env, fmt.Errorf("business code %d: %s", env.Code, env.Message)
	}
	return env, nil
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
		for _, call := range got.ToolCalls {
			if call.ToolName == want.Tool {
				result.ToolPassed = true
				break
			}
		}
		if !result.ToolPassed {
			failures = append(failures, fmt.Sprintf("expected tool %q was not called", want.Tool))
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
	}
	if want.MaxLatencyMS > 0 && latencyMS > float64(want.MaxLatencyMS) {
		failures = append(failures, fmt.Sprintf("latency %.2fms exceeds %dms", latencyMS, want.MaxLatencyMS))
	}
	return failures
}

func buildReport(started time.Time, duration time.Duration, results []caseResult) report {
	rep := report{StartedAt: started, DurationSeconds: duration.Seconds(), Total: len(results), ByCategory: make(map[string]categoryReport), Results: results}
	latencies := make([]float64, 0, len(results))
	toolTotal, toolPassed, retrievalHits, retrievalTotal := 0, 0, 0, 0
	for _, result := range results {
		latencies = append(latencies, result.LatencyMS)
		if result.Success {
			rep.Successful++
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
		retrievalHits += result.RetrievalHits
		retrievalTotal += result.RetrievalTotal
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
	rep.TaskPassRate = ratio(rep.Passed, rep.Total)
	if toolTotal > 0 {
		value := ratio(toolPassed, toolTotal)
		rep.ToolAccuracy = &value
	}
	if retrievalTotal > 0 {
		value := ratio(retrievalHits, retrievalTotal)
		rep.RetrievalRecall = &value
	}
	sort.Float64s(latencies)
	rep.LatencyP50MS = percentile(latencies, 0.50)
	rep.LatencyP95MS = percentile(latencies, 0.95)
	rep.LatencyP99MS = percentile(latencies, 0.99)
	if duration > 0 {
		rep.ThroughputRPS = float64(rep.Total) / duration.Seconds()
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
