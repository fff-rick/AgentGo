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
	"github.com/enterprise/ai-agent-go/internal/llm"
	"go.uber.org/zap"
)

type caseItem struct {
	ID      string `json:"id"`
	Message string `json:"message"`
	Intent  string `json:"intent"`
}
type caseResult struct {
	ID        string  `json:"id"`
	Expected  string  `json:"expected"`
	Actual    string  `json:"actual,omitempty"`
	LatencyMS float64 `json:"latency_ms"`
	Error     string  `json:"error,omitempty"`
}

func main() {
	path := flag.String("dataset", "benchmarks/datasets/intent.example.jsonl", "intent labels")
	configPath := flag.String("config", "", "AgentGo config file; defaults to config.yaml")
	output := flag.String("output", "", "JSON report path")
	backend := flag.String("backend", "real", "real or stub model backend")
	configVersion := flag.String("config-version", "", "configuration version")
	flag.Parse()
	cfg, err := config.Load(*configPath)
	must(err)
	clients := map[string]llm.Client{}
	for _, m := range cfg.LLM.Models {
		clients[m.Name] = llm.NewHTTPClient(m, cfg.LLM.RequestTimeout)
	}
	recognizer := intent.NewRecognizer(llm.NewRouter(clients, cfg.LLM.Models, cfg.LLM.CircuitBreaker), zap.NewNop())
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
		got, err := recognizer.Recognize(ctx, c.Message, nil)
		cancel()
		item := caseResult{ID: c.ID, Expected: c.Intent, LatencyMS: float64(time.Since(start).Microseconds()) / 1000}
		if err != nil {
			item.Error = err.Error()
		} else {
			item.Actual = got.Intent
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
	modelName := ""
	if len(cfg.LLM.Models) > 0 {
		modelName = cfg.LLM.Models[0].Name
	}
	version := *configVersion
	if version == "" {
		version = *configPath
		if version == "" {
			version = "config.yaml"
		}
	}
	report := map[string]any{"metadata": benchmarkmeta.Collect(*path, *backend, "intent", modelName, "", version), "accuracy": float64(correct) / float64(max(1, len(results))), "macro_f1": sum / float64(len(labels)), "f1_by_intent": scores, "results": results}
	encoded, err := json.MarshalIndent(report, "", "  ")
	must(err)
	fmt.Println(string(encoded))
	if *output != "" {
		must(os.MkdirAll(filepath.Dir(*output), 0o755))
		must(os.WriteFile(*output, append(encoded, '\n'), 0644))
	}
}
func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, strings.TrimSpace(err.Error()))
		os.Exit(2)
	}
}
