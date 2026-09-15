package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/llm"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/tool"
)

type plannerLLMClient struct {
	request   *model.LLMRequest
	requests  []*model.LLMRequest
	responses []string
}

func (c *plannerLLMClient) Chat(_ context.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
	c.request = req
	c.requests = append(c.requests, req)
	if len(c.responses) > 0 {
		return &model.LLMResponse{Content: c.responses[len(c.requests)-1]}, nil
	}
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

type recordingPlannerTool struct {
	plannerTool
	calls int
}

func (t *recordingPlannerTool) Execute(context.Context, string) (*tool.ToolResult, error) {
	t.calls++
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

	if _, err := agent.generatePlan(context.Background(), "测试任务", []model.LLMMessage{{Role: "system", Content: "会话上下文"}}, agent.toolRouter.ListToolDefinitions()); err != nil {
		t.Fatal(err)
	}
	if len(client.request.Messages) != 3 || client.request.Messages[1].Content != "会话上下文" {
		t.Fatalf("planner did not receive built context: %+v", client.request.Messages)
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

func TestPlannerSelectsAndInjectsOnlyChosenLazyTools(t *testing.T) {
	client := &plannerLLMClient{responses: []string{
		`{"tools":["alpha"]}`,
		`[{"step":1,"description":"use alpha","tool":"alpha","input":{},"depends_on":[]}]`,
		`done`,
	}}
	llmRouter := llm.NewRouter(map[string]llm.Client{"planner": client}, []config.ModelConfig{{Name: "planner"}}, config.CBConfig{FailureThreshold: 3, SuccessThreshold: 1})
	registry := tool.NewRegistry()
	alpha := &recordingPlannerTool{plannerTool: plannerTool{name: "alpha", description: "selected"}}
	registry.MustRegister(alpha)
	registry.MustRegister(plannerTool{name: "beta", description: "not selected"})
	manager := tool.NewManager(registry, nil, 1, time.Hour, zap.NewNop())
	planner := NewPlannerAgent(llmRouter, tool.NewRouter(registry, zap.NewNop(), manager), zap.NewNop())

	result, err := planner.Execute(context.Background(), "task", nil, tool.Scope{SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "done" || alpha.calls != 1 || len(client.requests) != 3 {
		t.Fatalf("result=%+v alpha_calls=%d requests=%d", result, alpha.calls, len(client.requests))
	}
	selectionPrompt := client.requests[0].Messages[0].Content
	if strings.Contains(selectionPrompt, `"properties"`) || !strings.Contains(selectionPrompt, "beta") {
		t.Fatalf("selection prompt must contain compact catalog only: %s", selectionPrompt)
	}
	planPrompt := client.requests[1].Messages[0].Content
	if !strings.Contains(planPrompt, `"name":"alpha"`) || strings.Contains(planPrompt, `"name":"beta"`) {
		t.Fatalf("plan prompt did not contain only selected schema: %s", planPrompt)
	}
}

func TestPlannerSkipsSelectionWhenNoToolsAreAllowed(t *testing.T) {
	client := &plannerLLMClient{responses: []string{`[]`, `done`}}
	llmRouter := llm.NewRouter(map[string]llm.Client{"planner": client}, []config.ModelConfig{{Name: "planner"}}, config.CBConfig{FailureThreshold: 3, SuccessThreshold: 1})
	registry := tool.NewRegistry()
	registry.MustRegister(plannerTool{name: "alpha", description: "hidden"})
	manager := tool.NewManager(registry, nil, 1, time.Hour, zap.NewNop())
	planner := NewPlannerAgent(llmRouter, tool.NewRouter(registry, zap.NewNop(), manager), zap.NewNop())

	result, err := planner.Execute(context.Background(), "task", nil, tool.Scope{Restricted: true, Allowed: []string{}})
	if err != nil || result.Answer != "done" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(client.requests) != 2 || strings.Contains(client.requests[0].Messages[0].Content, `"name":"alpha"`) {
		t.Fatalf("empty allowlist did not skip selection or leaked schema: %+v", client.requests)
	}
}

func TestPlannerRejectsPlanUsingUnselectedTool(t *testing.T) {
	client := &plannerLLMClient{responses: []string{
		`{"tools":["alpha"]}`,
		`[{"step":1,"description":"escape","tool":"beta","input":{},"depends_on":[]}]`,
	}}
	llmRouter := llm.NewRouter(map[string]llm.Client{"planner": client}, []config.ModelConfig{{Name: "planner"}}, config.CBConfig{FailureThreshold: 3, SuccessThreshold: 1})
	registry := tool.NewRegistry()
	registry.MustRegister(plannerTool{name: "alpha", description: "selected"})
	registry.MustRegister(plannerTool{name: "beta", description: "not selected"})
	manager := tool.NewManager(registry, nil, 1, time.Hour, zap.NewNop())
	planner := NewPlannerAgent(llmRouter, tool.NewRouter(registry, zap.NewNop(), manager), zap.NewNop())

	if _, err := planner.Execute(context.Background(), "task", nil, tool.Scope{}); err == nil || !strings.Contains(err.Error(), "未选择") {
		t.Fatalf("planner accepted an unselected tool: %v", err)
	}
}
