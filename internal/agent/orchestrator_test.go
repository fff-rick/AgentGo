package agent

import (
	"strings"
	"testing"

	"github.com/enterprise/ai-agent-go/internal/model"
)

func TestContextualRAGQueryIncludesPreviousTopic(t *testing.T) {
	history := []model.LLMMessage{
		{Role: "user", Content: "根据文档介绍 AgentGo 的文档导入"},
		{Role: "assistant", Content: "AgentGo 支持导入 Markdown 文件"},
	}
	query := contextualRAGQuery("它支持多大的文件？", history)
	if !strings.Contains(query, "AgentGo 的文档导入") || !strings.Contains(query, "它支持多大的文件？") {
		t.Fatalf("follow-up context missing: %q", query)
	}
}
