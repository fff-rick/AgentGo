package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/skill"
)

func TestSkillToolIsTrustedControlTool(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "review")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: review\ndescription: Review code\n---\nFollow the checklist."), 0o600); err != nil {
		t.Fatal(err)
	}
	skills, diagnostics := skill.Load([]string{root})
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	registry := NewRegistry()
	registry.MustRegister(NewSkillTool(skills))
	manager := NewManager(registry, nil, 1, time.Hour, zap.NewNop())
	scope := Scope{Restricted: true, SkillsEnabled: true}

	definitions := manager.InitialDefinitions(context.Background(), scope)
	if len(definitions) != 2 || definitions[1].Function.Name != LoadSkillName || len(manager.Catalog(context.Background(), scope)) != 0 {
		t.Fatalf("definitions=%+v catalog=%+v", definitions, manager.Catalog(context.Background(), scope))
	}
	result, err := NewRouter(registry, zap.NewNop(), manager).ExecuteScoped(context.Background(), scope, LoadSkillName, `{"name":"review"}`)
	if err != nil || !result.Success || !result.Trusted || !strings.Contains(result.Output, "Follow the checklist") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	unknown, _ := NewSkillTool(skills).Execute(context.Background(), `{"name":"../secret"}`)
	if unknown.Success {
		t.Fatal("path-like unknown skill was accepted")
	}
}
