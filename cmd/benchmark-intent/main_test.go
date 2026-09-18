package main

import (
	"github.com/enterprise/ai-agent-go/internal/config"
	"testing"
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
