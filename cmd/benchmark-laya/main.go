// Command benchmark-laya evaluates Laya tool selection without changing the Agent runtime path.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/enterprise/ai-agent-go/internal/benchmarkmeta"
	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/laya"
)

type caseItem struct {
	ID             string   `json:"id"`
	Request        string   `json:"request"`
	ExpectedTools  []string `json:"expected_tools"`
	ForbiddenTools []string `json:"forbidden_tools,omitempty"`
	Difficulty     string   `json:"difficulty"`
	Source         string   `json:"source"`
}

type catalogEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Selectable  bool   `json:"selectable"`
}

type answerEvidence struct {
	Choice              string             `json:"choice"`
	Probabilities       map[string]float64 `json:"probabilities,omitempty"`
	Confidence          *float64           `json:"confidence,omitempty"`
	AnswerConfidence    *float64           `json:"answer_confidence,omitempty"`
	Abstention          string             `json:"abstention,omitempty"`
	AbstentionThreshold *float64           `json:"abstention_threshold,omitempty"`
	LowConfidence       *bool              `json:"low_confidence,omitempty"`
}

type caseResult struct {
	ID                string                    `json:"id"`
	ExpectedTools     []string                  `json:"expected_tools"`
	ActualTools       []string                  `json:"actual_tools,omitempty"`
	ForbiddenSelected []string                  `json:"forbidden_selected,omitempty"`
	Covered           bool                      `json:"covered"`
	LowConfidence     []string                  `json:"low_confidence,omitempty"`
	Decisions         map[string]answerEvidence `json:"decisions,omitempty"`
	Checkpoint        string                    `json:"checkpoint,omitempty"`
	RoutingReason     string                    `json:"routing_reason,omitempty"`
	LatencyMS         float64                   `json:"latency_ms"`
	Error             string                    `json:"error,omitempty"`
}

type warmupResult struct {
	Language   string  `json:"language"`
	Checkpoint string  `json:"checkpoint,omitempty"`
	LatencyMS  float64 `json:"latency_ms"`
	Error      string  `json:"error,omitempty"`
}

type toolScore struct {
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	F1        float64 `json:"f1"`
	Support   int     `json:"support"`
}

type summary struct {
	Cases                 int                  `json:"cases"`
	Errors                int                  `json:"errors"`
	ExactMatchRate        float64              `json:"exact_match_rate"`
	MicroPrecision        float64              `json:"micro_precision"`
	MicroRecall           float64              `json:"micro_recall"`
	MicroF1               float64              `json:"micro_f1"`
	Coverage              float64              `json:"coverage"`
	CoveredExactMatchRate float64              `json:"covered_exact_match_rate"`
	EffectivePassRate     float64              `json:"effective_pass_rate"`
	ForbiddenSelections   int                  `json:"forbidden_selections"`
	LatencyP50MS          float64              `json:"latency_p50_ms"`
	LatencyP95MS          float64              `json:"latency_p95_ms"`
	LatencyP99MS          float64              `json:"latency_p99_ms"`
	ScoresByTool          map[string]toolScore `json:"scores_by_tool"`
}

type report struct {
	Metadata benchmarkmeta.Metadata `json:"metadata"`
	Config   map[string]any         `json:"config"`
	Warmup   []warmupResult         `json:"warmup,omitempty"`
	Summary  summary                `json:"summary"`
	Results  []caseResult           `json:"results"`
}

func main() {
	dataset := flag.String("dataset", "benchmarks/datasets/laya-tool-selection.seed.jsonl", "reviewed tool-selection JSONL")
	catalogPath := flag.String("catalog", "benchmarks/datasets/laya-tool-catalog.json", "candidate tool catalog")
	configPath := flag.String("config", "", "AgentGo config file; defaults to config.yaml")
	endpoint := flag.String("url", "", "Laya /v1/systemone URL; overrides config")
	output := flag.String("output", "", "JSON report path")
	minConfidence := flag.Float64("min-confidence", -1, "Laya answer-confidence gate; negative disables it")
	timeout := flag.Duration("timeout", 10*time.Second, "benchmark-only per-request timeout")
	warm := flag.Bool("warmup", true, "warm English and multilingual checkpoints before measurement")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	must(err)
	if *endpoint != "" {
		cfg.Laya.URL = *endpoint
	}
	if *minConfidence > 1 || *timeout <= 0 {
		must(fmt.Errorf("min-confidence must be in [0,1] or negative, and timeout must be positive"))
	}
	cfg.Laya.Timeout = *timeout
	cases, err := readCases(*dataset)
	must(err)
	catalog, err := readCatalog(*catalogPath)
	must(err)
	must(validateCases(cases, catalog))
	client, err := laya.NewClient(cfg.Laya)
	must(err)

	var warmup []warmupResult
	if *warm {
		warmup = warmCheckpoints(context.Background(), client, catalog)
	}
	results := make([]caseResult, 0, len(cases))
	for _, item := range cases {
		results = append(results, evaluate(context.Background(), client, item, catalog, *minConfidence))
	}
	metadata := benchmarkmeta.Collect(*dataset, "laya", "tool-selection", "laya", fmt.Sprintf("schema=primary-secondary-choice-v1,min_confidence=%.4f,timeout=%s", *minConfidence, *timeout), configName(*configPath))
	metadata.CorpusSHA256 = benchmarkmeta.HashFile(*catalogPath)
	report := report{
		Metadata: metadata,
		Config: map[string]any{
			"catalog": *catalogPath, "question_schema": "primary-secondary-choice-v1",
			"min_confidence": *minConfidence, "timeout": timeout.String(), "warmup": *warm,
			"seed_dataset": strings.Contains(filepath.Base(*dataset), ".seed."),
		},
		Warmup: warmup, Summary: summarize(results, catalog), Results: results,
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	must(err)
	fmt.Println(string(encoded))
	if *output != "" {
		must(os.MkdirAll(filepath.Dir(*output), 0o755))
		must(os.WriteFile(*output, append(encoded, '\n'), 0o644))
	}
}

type predictor interface {
	Predict(context.Context, string, laya.Request) (*laya.Response, error)
}

func evaluate(ctx context.Context, client predictor, item caseItem, catalog []catalogEntry, minConfidence float64) caseResult {
	request := laya.Request{State: map[string]string{"request": item.Request}, Questions: toolQuestions(catalog)}
	if minConfidence >= 0 {
		request.MinConfidence = &minConfidence
	}
	start := time.Now()
	response, err := client.Predict(ctx, "tool_selection", request)
	result := caseResult{ID: item.ID, ExpectedTools: sorted(item.ExpectedTools), Covered: true, Decisions: map[string]answerEvidence{}, LatencyMS: float64(time.Since(start).Microseconds()) / 1000}
	if err != nil {
		result.Error = err.Error()
		result.Covered = false
		return result
	}
	result.Checkpoint, result.RoutingReason = response.Routing.Model, response.Routing.Reason
	allowed := map[string]bool{"none": true}
	for _, candidate := range selectableTools(catalog) {
		allowed[candidate.Name] = true
	}
	for _, questionID := range []string{"primary_tool", "secondary_tool"} {
		answer, ok := response.Answers[questionID]
		if !ok || !allowed[answer.Choice] {
			result.Error = fmt.Sprintf("missing or invalid choice answer for %s", questionID)
			result.Covered = false
			return result
		}
		result.Decisions[questionID] = answerEvidence{
			Choice: answer.Choice, Probabilities: answer.Probabilities, Confidence: answer.Confidence,
			AnswerConfidence: answer.AnswerConfidence, Abstention: answer.Abstention,
			AbstentionThreshold: answer.AbstentionThreshold, LowConfidence: answer.LowConfidence,
		}
		if answer.Choice != "none" {
			result.ActualTools = append(result.ActualTools, answer.Choice)
		}
		if minConfidence >= 0 && (answer.Abstention != "passed" || answer.LowConfidence != nil && *answer.LowConfidence) {
			result.Covered = false
			result.LowConfidence = append(result.LowConfidence, questionID)
		}
	}
	result.ActualTools = sorted(result.ActualTools)
	result.LowConfidence = sorted(result.LowConfidence)
	actual := set(result.ActualTools)
	for _, name := range item.ForbiddenTools {
		if actual[name] {
			result.ForbiddenSelected = append(result.ForbiddenSelected, name)
		}
	}
	result.ForbiddenSelected = sorted(result.ForbiddenSelected)
	return result
}

func toolQuestions(catalog []catalogEntry) map[string]laya.Question {
	criteria := map[string]string{"none": "No external tool is needed for this request."}
	for _, candidate := range selectableTools(catalog) {
		criteria[candidate.Name] = candidate.Description
	}
	return map[string]laya.Question{
		"primary_tool": {
			Type: "choice", Instructions: "Which single tool is most important to complete `request`? Choose none when no tool is needed.", Criteria: criteria,
		},
		"secondary_tool": {
			Type: "choice", Instructions: "Which additional tool is required after the primary tool? Choose none when one or no tool is sufficient.", Criteria: criteria,
		},
	}
}

func warmCheckpoints(ctx context.Context, client predictor, catalog []catalogEntry) []warmupResult {
	inputs := []struct{ language, request string }{{"multilingual", "你好，请简单回答。"}, {"english", "Hello, answer briefly."}}
	results := make([]warmupResult, 0, len(inputs))
	for _, input := range inputs {
		start := time.Now()
		response, err := client.Predict(ctx, "tool_selection_warmup", laya.Request{State: map[string]string{"request": input.request}, Questions: toolQuestions(catalog)})
		result := warmupResult{Language: input.language, LatencyMS: float64(time.Since(start).Microseconds()) / 1000}
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Checkpoint = response.Routing.Model
		}
		results = append(results, result)
	}
	return results
}

func summarize(results []caseResult, catalog []catalogEntry) summary {
	selectable := selectableTools(catalog)
	result := summary{Cases: len(results), ScoresByTool: make(map[string]toolScore, len(selectable))}
	latencies := make([]float64, 0, len(results))
	tp, fp, fn, exact, covered, coveredExact := 0, 0, 0, 0, 0, 0
	for _, item := range results {
		latencies = append(latencies, item.LatencyMS)
		if item.Error != "" {
			result.Errors++
		}
		expected, actual := set(item.ExpectedTools), set(item.ActualTools)
		isExact := item.Error == "" && equalSets(expected, actual)
		if isExact {
			exact++
		}
		if item.Covered && item.Error == "" {
			covered++
			if isExact {
				coveredExact++
			}
		}
		result.ForbiddenSelections += len(item.ForbiddenSelected)
		for _, candidate := range selectable {
			want, got := expected[candidate.Name], actual[candidate.Name]
			switch {
			case want && got:
				tp++
			case !want && got:
				fp++
			case want && !got:
				fn++
			}
		}
	}
	for _, candidate := range selectable {
		localTP, localFP, localFN, support := 0, 0, 0, 0
		for _, item := range results {
			want, got := set(item.ExpectedTools)[candidate.Name], set(item.ActualTools)[candidate.Name]
			if want {
				support++
			}
			if want && got {
				localTP++
			} else if got {
				localFP++
			} else if want {
				localFN++
			}
		}
		precision, recall, f1 := rates(localTP, localFP, localFN)
		result.ScoresByTool[candidate.Name] = toolScore{Precision: precision, Recall: recall, F1: f1, Support: support}
	}
	result.ExactMatchRate = ratio(exact, len(results))
	result.MicroPrecision, result.MicroRecall, result.MicroF1 = rates(tp, fp, fn)
	result.Coverage = ratio(covered, len(results))
	result.CoveredExactMatchRate = ratio(coveredExact, covered)
	result.EffectivePassRate = ratio(coveredExact, len(results))
	sort.Float64s(latencies)
	result.LatencyP50MS, result.LatencyP95MS, result.LatencyP99MS = percentile(latencies, .50), percentile(latencies, .95), percentile(latencies, .99)
	return result
}

func readCases(path string) ([]caseItem, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var result []caseItem
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		var item caseItem
		if err := json.Unmarshal(scanner.Bytes(), &item); err != nil {
			return nil, fmt.Errorf("dataset line %d: %w", line, err)
		}
		result = append(result, item)
	}
	return result, scanner.Err()
}

func readCatalog(path string) ([]catalogEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var result []catalogEntry
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func validateCases(cases []caseItem, catalog []catalogEntry) error {
	if len(cases) == 0 || len(catalog) == 0 {
		return fmt.Errorf("dataset and catalog must not be empty")
	}
	tools, selectable, ids := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, candidate := range catalog {
		if candidate.Name == "" || candidate.Description == "" || tools[candidate.Name] {
			return fmt.Errorf("catalog contains an empty or duplicate tool %q", candidate.Name)
		}
		tools[candidate.Name] = true
		selectable[candidate.Name] = candidate.Selectable
	}
	for _, item := range cases {
		if item.ID == "" || strings.TrimSpace(item.Request) == "" || item.Difficulty == "" || item.Source == "" || ids[item.ID] {
			return fmt.Errorf("case %q has missing fields or a duplicate id", item.ID)
		}
		ids[item.ID] = true
		expected := set(item.ExpectedTools)
		if len(expected) > 2 {
			return fmt.Errorf("case %q expects more than two tools", item.ID)
		}
		for name := range expected {
			if !selectable[name] {
				return fmt.Errorf("case %q expects non-selectable tool %q", item.ID, name)
			}
		}
		for _, name := range append(append([]string(nil), item.ExpectedTools...), item.ForbiddenTools...) {
			if !tools[name] {
				return fmt.Errorf("case %q references unknown tool %q", item.ID, name)
			}
		}
		for _, name := range item.ForbiddenTools {
			if expected[name] {
				return fmt.Errorf("case %q both expects and forbids %q", item.ID, name)
			}
		}
	}
	return nil
}

func selectableTools(catalog []catalogEntry) []catalogEntry {
	result := make([]catalogEntry, 0, len(catalog))
	for _, candidate := range catalog {
		if candidate.Selectable {
			result = append(result, candidate)
		}
	}
	return result
}

func configName(path string) string {
	if path == "" {
		return "config.yaml"
	}
	return path
}

func set(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func sorted(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func equalSets(left, right map[string]bool) bool {
	if len(left) != len(right) {
		return false
	}
	for key := range left {
		if !right[key] {
			return false
		}
	}
	return true
}

func rates(tp, fp, fn int) (precision, recall, f1 float64) {
	precision = ratio(tp, tp+fp)
	recall = ratio(tp, tp+fn)
	if precision+recall > 0 {
		f1 = 2 * precision * recall / (precision + recall)
	}
	return
}

func ratio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

func percentile(sortedValues []float64, quantile float64) float64 {
	if len(sortedValues) == 0 {
		return 0
	}
	index := int(float64(len(sortedValues)-1)*quantile + .5)
	return sortedValues[index]
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, strings.TrimSpace(err.Error()))
		os.Exit(2)
	}
}
