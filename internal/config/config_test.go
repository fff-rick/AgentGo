package config

import (
	"testing"
	"time"
)

func TestApplyModelEnv(t *testing.T) {
	t.Setenv("APP_LLM_BASE_URL", "http://localhost:11434/")
	t.Setenv("APP_LLM_MODEL", "test-model")
	t.Setenv("APP_LLM_NAME", "local")
	t.Setenv("APP_LLM_API_KEY", "test-key")

	cfg := &Config{}
	applySingleModelEnv(cfg)
	if len(cfg.LLM.Models) != 1 {
		t.Fatalf("models = %d, want 1", len(cfg.LLM.Models))
	}
	got := cfg.LLM.Models[0]
	if got.Name != "local" || got.Model != "test-model" || got.BaseURL != "http://localhost:11434" || got.APIKey != "test-key" {
		t.Fatalf("unexpected model config: %+v", got)
	}
}

func TestApplyModelEnvPreservesYAMLModels(t *testing.T) {
	t.Setenv("APP_LLM_BASE_URL", "http://should-not-win")
	cfg := &Config{LLM: LLMConfig{Models: []ModelConfig{{Name: "configured"}}}}
	applySingleModelEnv(cfg)
	if len(cfg.LLM.Models) != 1 || cfg.LLM.Models[0].Name != "configured" {
		t.Fatalf("configured models were overwritten: %+v", cfg.LLM.Models)
	}
}

func TestExpandModelEnv(t *testing.T) {
	t.Setenv("TEST_MODEL_URL", "http://localhost:11434")
	t.Setenv("TEST_MODEL_KEY", "secret")
	models := []ModelConfig{{BaseURL: "${TEST_MODEL_URL}", APIKey: "${TEST_MODEL_KEY}", Model: "qwen2.5:7b"}}
	expandModelEnv(models)
	if models[0].BaseURL != "http://localhost:11434" || models[0].APIKey != "secret" {
		t.Fatalf("environment was not expanded: %+v", models[0])
	}
}

func TestLoadMultiModelRoutingConfig(t *testing.T) {
	t.Setenv("APP_AUTH_ENABLED", "true")
	t.Setenv("APP_AUTH_ISSUER", "https://issuer.example")
	t.Setenv("APP_AUTH_AUDIENCE", "agentgo")
	t.Setenv("APP_LLM_BASE_URL", "https://example.com")
	t.Setenv("APP_LLM_API_KEY", "gpt-key")
	t.Setenv("APP_QWEN_BASE_URL", "http://localhost:11434")
	t.Setenv("APP_QWEN_API_KEY", "ollama")
	cfg, err := Load("../../config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.LLM.Models) != 2 || cfg.LLM.Models[0].Priority != 1 || cfg.LLM.Models[1].Priority != 10 || cfg.Agent.ToolModel != "qwen-tools" {
		t.Fatalf("unexpected routing config: models=%+v agent=%+v", cfg.LLM.Models, cfg.Agent)
	}
	if cfg.Memory.SessionTTL != 720*time.Hour || cfg.Memory.SemanticCollection != "semantic_memory_v2" || cfg.Context.MaxInputTokens != 30000 || cfg.Context.RecentMessages != 20 {
		t.Fatalf("unexpected memory/context config: memory=%+v context=%+v", cfg.Memory, cfg.Context)
	}
	if cfg.Tools.LazyLoadThreshold != 3 || cfg.Tools.MaxDiscoveryCalls != 4 {
		t.Fatalf("unexpected tool config: %+v", cfg.Tools)
	}
	if !cfg.Auth.Enabled || cfg.Auth.Issuer != "https://issuer.example" || cfg.Auth.Audience != "agentgo" {
		t.Fatalf("auth config=%+v", cfg.Auth)
	}
}
