package config

import "testing"

func TestApplySingleModelEnv(t *testing.T) {
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
	if cfg.LLM.DefaultModel != "local" {
		t.Fatalf("default model = %q, want local", cfg.LLM.DefaultModel)
	}
}

func TestApplySingleModelEnvPreservesYAMLModels(t *testing.T) {
	t.Setenv("APP_LLM_BASE_URL", "http://should-not-win")
	cfg := &Config{LLM: LLMConfig{Models: []ModelConfig{{Name: "configured"}}}}
	applySingleModelEnv(cfg)
	if len(cfg.LLM.Models) != 1 || cfg.LLM.Models[0].Name != "configured" {
		t.Fatalf("configured models were overwritten: %+v", cfg.LLM.Models)
	}
}
