// Command benchmark-intent evaluates the standalone recognizer, which is not in the default Agent path.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/enterprise/ai-agent-go/internal/benchmarkmeta"
	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/intent"
	"github.com/enterprise/ai-agent-go/internal/laya"
	"github.com/enterprise/ai-agent-go/internal/llm"
	"github.com/enterprise/ai-agent-go/internal/model"
	"go.uber.org/zap"
)

type caseItem struct {
	ID      string             `json:"id"`
	Message string             `json:"message"`
	History []model.LLMMessage `json:"history,omitempty"`
	Intent  string             `json:"intent"`
}
type caseResult struct {
	ID         string  `json:"id"`
	Expected   string  `json:"expected"`
	Actual     string  `json:"actual,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	LatencyMS  float64 `json:"latency_ms"`
	Error      string  `json:"error,omitempty"`
}

func main() {
	path := flag.String("dataset", "benchmarks/datasets/intent.example.jsonl", "intent labels")
	configPath := flag.String("config", "", "AgentGo config file; defaults to config.yaml")
	output := flag.String("output", "", "JSON report path")
	backend := flag.String("backend", "real", "real or stub model backend")
	engine := flag.String("engine", "llm", "intent engine: llm or laya")
	layaURL := flag.String("laya-url", "", "Laya /v1/systemone URL; overrides config")
	minConfidence := flag.Float64("min-confidence", -1, "Laya answer-confidence gate; negative uses config")
	configVersion := flag.String("config-version", "", "configuration version")
	flag.Parse()
	cfg, err := config.Load(*configPath)
	must(err)
	clients := map[string]llm.Client{}
	for _, m := range cfg.LLM.Models {
		clients[m.Name] = llm.NewHTTPClient(m, cfg.LLM.RequestTimeout)
	}
	var recognize func(context.Context, string, []model.LLMMessage) (*model.IntentResult, error)
	modelName := ""
	switch *engine {
	case "llm":
		recognizer := intent.NewRecognizer(llm.NewRouter(clients, cfg.LLM.Models, cfg.LLM.CircuitBreaker), zap.NewNop())
		recognize = recognizer.Recognize
		if len(cfg.LLM.Models) > 0 {
			modelName = cfg.LLM.Models[0].Name
		}
	case "laya":
		if *layaURL != "" {
			cfg.Laya.URL = *layaURL
		}
		if *minConfidence < 0 {
			*minConfidence = cfg.Laya.MinConfidence
		}
		if *minConfidence < 0 || *minConfidence > 1 {
			must(fmt.Errorf("min-confidence must be in [0,1]"))
		}
		client, clientErr := laya.NewClient(cfg.Laya)
		must(clientErr)
		recognizer := intent.NewLayaShadow(client, *minConfidence, zap.NewNop())
		recognize = recognizer.Recognize
		modelName = "laya"
	default:
		must(fmt.Errorf("engine must be llm or laya"))
	}
	file, err := os.Open(*path)
	must(err)
	defer file.Close()
	scanner := bufio.NewScanner(file)
	var results []caseResult
	for scanner.Scan() {
		var c caseItem
		must(json.Unmarshal(scanner.Bytes(), &c))
		ctx, cancel := context.WithTimeout(context.Background(), cfg.LLM.RequestTimeout)
		start := time.Now()
		got, err := recognize(ctx, c.Message, c.History)
		cancel()
		item := caseResult{ID: c.ID, Expected: c.Intent, LatencyMS: float64(time.Since(start).Microseconds()) / 1000}
		if err != nil {
			item.Error = err.Error()
		} else {
			item.Actual = got.Intent
			item.Confidence = got.Confidence
		}
		results = append(results, item)
	}
	must(scanner.Err())
	labels := []string{intent.IntentChat, intent.IntentRAGQuery, intent.IntentToolUse, intent.IntentComplexTask}
	scores := map[string]float64{}
	sum := 0.0
	correct := 0
	for _, label := range labels {
		tp, fp, fn := 0, 0, 0
		for _, r := range results {
			if r.Actual == r.Expected && r.Actual == label {
				tp++
			}
			if r.Actual == label && r.Expected != label {
				fp++
			}
			if r.Expected == label && r.Actual != label {
				fn++
			}
		}
		f1 := 0.0
		if 2*tp+fp+fn > 0 {
			f1 = float64(2*tp) / float64(2*tp+fp+fn)
		}
		scores[label] = f1
		sum += f1
	}
	for _, r := range results {
		if r.Actual == r.Expected {
			correct++
		}
	}
	version := *configVersion
	if version == "" {
		version = *configPath
		if version == "" {
			version = "config.yaml"
		}
	}
	brier, ece, calibrated := calibration(results)
	report := map[string]any{
		"metadata": benchmarkmeta.Collect(*path, *backend, "intent", modelName, "", version),
		"accuracy": float64(correct) / float64(max(1, len(results))), "macro_f1": sum / float64(len(labels)),
		"f1_by_intent": scores, "coverage": float64(calibrated) / float64(max(1, len(results))),
		"calibration": map[string]any{"samples": calibrated, "brier_score": brier, "ece_10_bins": ece}, "results": results,
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	must(err)
	fmt.Println(string(encoded))
	if *output != "" {
		must(os.MkdirAll(filepath.Dir(*output), 0o755))
		must(os.WriteFile(*output, append(encoded, '\n'), 0644))
	}
}

func calibration(results []caseResult) (brier, ece float64, samples int) {
	type bin struct {
		count, correct int
		confidence     float64
	}
	bins := make([]bin, 10)
	for _, result := range results {
		if result.Error != "" {
			continue
		}
		confidence := max(0, min(1, result.Confidence))
		correct := 0
		if result.Actual == result.Expected {
			correct = 1
		}
		delta := confidence - float64(correct)
		brier += delta * delta
		index := min(9, int(confidence*10))
		bins[index].count++
		bins[index].correct += correct
		bins[index].confidence += confidence
		samples++
	}
	if samples == 0 {
		return 0, 0, 0
	}
	brier /= float64(samples)
	for _, bin := range bins {
		if bin.count == 0 {
			continue
		}
		accuracy := float64(bin.correct) / float64(bin.count)
		confidence := bin.confidence / float64(bin.count)
		difference := accuracy - confidence
		if difference < 0 {
			difference = -difference
		}
		ece += float64(bin.count) / float64(samples) * difference
	}
	return brier, ece, samples
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, strings.TrimSpace(err.Error()))
		os.Exit(2)
	}
}
