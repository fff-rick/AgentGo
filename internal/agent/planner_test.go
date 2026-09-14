package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/llm"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/tool"
)

type plannerLLMClient struct {
	request *model.LLMRequest
}

func (c *plannerLLMClient) Chat(_ context.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
	c.request = req
	return &model.LLMResponse{Content: `[]`}, nil
}

func (*plannerLLMClient) ChatStream(context.Context, *model.LLMRequest) (<-chan llm.StreamEvent, error) {
	panic("Planner must use Chat")
}

func (*plannerLLMClient) Name() string                 { return "planner" }
func (*plannerLLMClient) Healthy(context.Context) bool { return true }

type plannerTool struct {
	name        string
	description string
}

func (t plannerTool) Name() string        { return t.name }
func (t plannerTool) Description() string { return t.description }
func (plannerTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{"type": "string"},
		},
		"required": []string{"query"},
	}
}
func (plannerTool) Execute(context.Context, string) (*tool.ToolResult, error) {
	return tool.NewSuccessResult("ok"), nil
}

func TestPlannerProvidesCompleteToolDefinitions(t *testing.T) {
	client := &plannerLLMClient{}
	llmRouter := llm.NewRouter(
		map[string]llm.Client{"planner": client},
		[]config.ModelConfig{{Name: "planner"}},
		config.CBConfig{FailureThreshold: 3, SuccessThreshold: 1},
	)
	registry := tool.NewRegistry()
	registry.MustRegister(plannerTool{name: "zeta", description: "后注册工具"})
	registry.MustRegister(plannerTool{name: "alpha", description: "先展示工具"})
	agent := NewPlannerAgent(llmRouter, tool.NewRouter(registry, zap.NewNop()), zap.NewNop())

	if _, err := agent.generatePlan(context.Background(), "测试任务"); err != nil {
		t.Fatal(err)
	}

	const marker = "可用工具定义（JSON）："
	index := strings.LastIndex(client.request.Messages[0].Content, marker)
	if index < 0 {
		t.Fatalf("system prompt missing tool definitions: %s", client.request.Messages[0].Content)
	}
	var definitions []model.FunctionDef
	if err := json.Unmarshal([]byte(client.request.Messages[0].Content[index+len(marker):]), &definitions); err != nil {
		t.Fatalf("invalid tool definitions JSON: %v", err)
	}
	if len(definitions) != 2 || definitions[0].Name != "alpha" || definitions[1].Name != "zeta" {
		t.Fatalf("tool definitions are not complete and stable: %+v", definitions)
	}
	if definitions[0].Description != "先展示工具" {
		t.Fatalf("tool description missing: %+v", definitions[0])
	}
	parameters, ok := definitions[0].Parameters.(map[string]interface{})
	if !ok || parameters["type"] != "object" || parameters["required"] == nil {
		t.Fatalf("tool parameter schema missing: %+v", definitions[0].Parameters)
	}
}

func TestPlanStepAcceptsObjectAndLegacyStringInput(t *testing.T) {
	var steps []PlanStep
	planJSON := `[
		{"step":1,"tool":"web_search","input":{"query":"AgentGo"}},
		{"step":2,"tool":"web_search","input":"{\"query\":\"legacy\"}"}
	]`
	if err := json.Unmarshal([]byte(planJSON), &steps); err != nil {
		t.Fatal(err)
	}
	if got := steps[0].toolInput(); got != `{"query":"AgentGo"}` {
		t.Fatalf("object input = %q", got)
	}
	if got := steps[1].toolInput(); got != `{"query":"legacy"}` {
		t.Fatalf("legacy string input = %q", got)
	}
}
