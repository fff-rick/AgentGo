package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/tool"
)

const KnowledgeSearchToolName = "knowledge_search"

type knowledgePipeline interface {
	Search(context.Context, string, int) ([]model.Reference, error)
}

type KnowledgeSearchTool struct {
	pipeline    knowledgePipeline
	defaultTopK int
}

func NewKnowledgeSearchTool(pipeline knowledgePipeline, topK int) *KnowledgeSearchTool {
	if topK <= 0 {
		topK = 5
	}
	return &KnowledgeSearchTool{pipeline: pipeline, defaultTopK: topK}
}

func (*KnowledgeSearchTool) Name() string { return KnowledgeSearchToolName }
func (*KnowledgeSearchTool) Description() string {
	return "检索 AgentGo 内部知识库；适合文档、内部资料、项目知识和需要引用来源的问题"
}
func (*KnowledgeSearchTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{"type": "string", "description": "结合对话上下文改写后的完整检索问题"},
			"top_k": map[string]interface{}{"type": "integer", "description": "返回的知识片段数量，范围 1-20", "minimum": 1, "maximum": 20},
		},
		"required": []string{"query"},
	}
}

func (t *KnowledgeSearchTool) Execute(ctx context.Context, input string) (*tool.ToolResult, error) {
	var params struct {
		Query string `json:"query"`
		TopK  int    `json:"top_k"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return tool.NewErrorResult("参数解析失败: " + err.Error()), nil
	}
	params.Query = strings.TrimSpace(params.Query)
	if params.Query == "" {
		return tool.NewErrorResult("检索问题不能为空"), nil
	}
	if len([]rune(params.Query)) > 2000 {
		return tool.NewErrorResult("检索问题不能超过 2000 个字符"), nil
	}
	if params.TopK <= 0 {
		params.TopK = t.defaultTopK
	} else if params.TopK > 20 {
		params.TopK = 20
	}
	refs, err := t.pipeline.Search(ctx, params.Query, params.TopK)
	if err != nil {
		return nil, fmt.Errorf("知识库检索失败: %w", err)
	}
	encoded, err := json.Marshal(map[string]interface{}{"query": params.Query, "top_k": params.TopK, "references": refs})
	if err != nil {
		return nil, fmt.Errorf("序列化知识库结果失败: %w", err)
	}
	return &tool.ToolResult{Success: true, Output: string(encoded), References: refs}, nil
}
