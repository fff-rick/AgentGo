package main

import (
	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/tool"
	"go.uber.org/zap"
	"testing"
)

func TestSensitiveToolsAreNotRegisteredByDefault(t *testing.T) {
	registry := tool.NewRegistry()
	closeTools, err := registerBuiltinTools(registry, config.SearchConfig{}, config.DatabaseToolConfig{}, config.FilesystemConfig{}, nil, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer closeTools()
	for _, name := range []string{"database_query", "file_inspect", "file_edit_preview", "file_edit_apply"} {
		if _, ok := registry.Get(name); ok {
			t.Fatalf("%s registered by default", name)
		}
	}
}

func TestFilesystemRequiresValidServerWorkspace(t *testing.T) {
	_, err := registerBuiltinTools(tool.NewRegistry(), config.SearchConfig{}, config.DatabaseToolConfig{}, config.FilesystemConfig{Enabled: true}, nil, zap.NewNop())
	if err == nil {
		t.Fatal("enabled filesystem accepted an empty workspace")
	}
}
