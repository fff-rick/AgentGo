package main

import (
	"testing"

	"github.com/enterprise/ai-agent-go/internal/config"
)

func TestStubConfigResolvesLocalModelEndpoint(t *testing.T) {
	t.Setenv("BENCH_STUB_BASE_URL", "http://localhost:18088")
	cfg, err := config.Load("../../benchmarks/config.stub.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.LLM.Models) != 2 || cfg.LLM.Models[0].BaseURL != "http://localhost:18088" {
		t.Fatalf("models=%+v", cfg.LLM.Models)
	}
}

func TestCalibrationExcludesErrors(t *testing.T) {
	brier, ece, samples := calibration([]caseResult{
		{Expected: "chat", Actual: "chat", Confidence: .8},
		{Expected: "chat", Actual: "tool_use", Confidence: .6},
		{Expected: "chat", Error: "timeout"},
	})
	if samples != 2 || brier < .199 || brier > .201 || ece < .399 || ece > .401 {
		t.Fatalf("samples=%d brier=%f ece=%f", samples, brier, ece)
	}
}
