package agentloop

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/llm"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/tool"
)

type loopClient struct {
	responses []model.LLMResponse
	requests  []*model.LLMRequest
}

func (*loopClient) Chat(context.Context, *model.LLMRequest) (*model.LLMResponse, error) {
	panic("AgentLoop must use ChatStream")
}
func (c *loopClient) ChatStream(_ context.Context, req *model.LLMRequest) (<-chan llm.StreamEvent, error) {
	c.requests = append(c.requests, req)
	response := c.responses[len(c.requests)-1]
	stream := make(chan llm.StreamEvent, 3)
	if response.Content != "" {
		stream <- llm.StreamEvent{Content: response.Content}
	}
	if response.Usage != nil {
		stream <- llm.StreamEvent{Usage: response.Usage}
	}
	stream <- llm.StreamEvent{Done: true, ToolCalls: response.ToolCalls}
	close(stream)
	return stream, nil
}

func TestLoopReportsOnlyCompleteProviderUsage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage *model.UsageInfo
		want  bool
	}{{"reported", &model.UsageInfo{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}, true}, {"missing", nil, false}} {
		t.Run(tc.name, func(t *testing.T) {
			client := &loopClient{responses: []model.LLMResponse{{Content: "你好", Usage: tc.usage}}}
			router := llm.NewRouter(map[string]llm.Client{"loop": client}, []config.ModelConfig{{Name: "loop"}}, config.CBConfig{FailureThreshold: 3, SuccessThreshold: 1})
			registry := tool.NewRegistry()
			result, err := New(router, tool.NewRouter(registry, zap.NewNop())).Run(context.Background(), Input{Messages: []model.LLMMessage{{Role: "user", Content: "你好"}}, MaxIterations: 1})
			if err != nil {
				t.Fatal(err)
			}
			if (result.Usage != nil) != tc.want {
				t.Fatalf("usage=%+v want reported=%t", result.Usage, tc.want)
			}
		})
	}
}
func (*loopClient) Name() string                 { return "loop" }
func (*loopClient) Healthy(context.Context) bool { return true }

type loopTool struct {
	name       string
	references []model.Reference
	calls      int
}

func (t *loopTool) Name() string      { return t.name }
func (*loopTool) Description() string { return "test tool" }
func (*loopTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object"}
}
func (t *loopTool) Execute(context.Context, string) (*tool.ToolResult, error) {
	t.calls++
	return &tool.ToolResult{Success: true, Output: t.name + " result", References: t.references}, nil
}

func toolCall(id, name string) model.LLMToolCall {
	call := model.LLMToolCall{ID: id, Type: "function"}
	call.Function.Name, call.Function.Arguments = name, `{}`
	return call
}

func toolCallWithArgs(id, name string, args interface{}) model.LLMToolCall {
	call := toolCall(id, name)
	encoded, _ := json.Marshal(args)
	call.Function.Arguments = string(encoded)
	return call
}

func TestLoopLetsModelCombineKnowledgeAndOtherTools(t *testing.T) {
	client := &loopClient{responses: []model.LLMResponse{
		{ToolCalls: []model.LLMToolCall{toolCall("1", "knowledge_search")}},
		{ToolCalls: []model.LLMToolCall{toolCall("2", "web_search")}},
		{Content: "综合回答"},
	}}
	router := llm.NewRouter(map[string]llm.Client{"loop": client}, []config.ModelConfig{{Name: "loop"}}, config.CBConfig{FailureThreshold: 3, SuccessThreshold: 1})
	knowledge := &loopTool{name: "knowledge_search", references: []model.Reference{{DocID: "doc-1"}}}
	search := &loopTool{name: "web_search"}
	registry := tool.NewRegistry()
	registry.MustRegister(knowledge)
	registry.MustRegister(search)
	toolRouter := tool.NewRouter(registry, zap.NewNop())
	state := &RunState{RunID: "run-1", Task: "test"}

	result, err := New(router, toolRouter).Run(context.Background(), Input{
		Messages: []model.LLMMessage{{Role: "user", Content: "结合内部文档和最新搜索回答"}},
		Tools:    toolRouter.ListToolDefinitions(), MaxIterations: 4, SystemPrompt: "decide", State: state,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "综合回答" || knowledge.calls != 1 || search.calls != 1 || len(result.References) != 1 {
		t.Fatalf("result=%+v knowledge_calls=%d search_calls=%d", result, knowledge.calls, search.calls)
	}
	if client.requests[0].ToolChoice != "auto" {
		t.Fatalf("first turn must let the model decide, request=%+v", client.requests[0])
	}
	if len(state.ToolResults) != 2 || len(state.Steps) != 3 {
		t.Fatalf("working memory=%+v", state)
	}
}

func TestLoopAllowsDirectAnswerWithToolsAvailable(t *testing.T) {
	client := &loopClient{responses: []model.LLMResponse{{Content: "直接回答"}}}
	router := llm.NewRouter(map[string]llm.Client{"loop": client}, []config.ModelConfig{{Name: "loop"}}, config.CBConfig{FailureThreshold: 3, SuccessThreshold: 1})
	candidate := &loopTool{name: "web_search"}
	registry := tool.NewRegistry()
	registry.MustRegister(candidate)
	toolRouter := tool.NewRouter(registry, zap.NewNop())

	result, err := New(router, toolRouter).Run(context.Background(), Input{
		Messages: []model.LLMMessage{{Role: "user", Content: "你好"}},
		Tools:    toolRouter.ListToolDefinitions(), MaxIterations: 2,
	})
	if err != nil || result.Answer != "直接回答" || candidate.calls != 0 {
		t.Fatalf("result=%+v calls=%d err=%v", result, candidate.calls, err)
	}
}

func TestLoopLoadsColdToolWithoutConsumingBusinessIteration(t *testing.T) {
	client := &loopClient{responses: []model.LLMResponse{
		{ToolCalls: []model.LLMToolCall{toolCallWithArgs("catalog", tool.ListToolsName, map[string]interface{}{"action": "catalog"})}},
		{ToolCalls: []model.LLMToolCall{toolCallWithArgs("load", tool.ListToolsName, map[string]interface{}{"action": "load", "names": []string{"web_search"}})}},
		{ToolCalls: []model.LLMToolCall{toolCall("search", "web_search")}},
		{Content: "搜索完成"},
	}}
	llmRouter := llm.NewRouter(map[string]llm.Client{"loop": client}, []config.ModelConfig{{Name: "loop"}}, config.CBConfig{FailureThreshold: 3, SuccessThreshold: 1})
	search := &loopTool{name: "web_search"}
	registry := tool.NewRegistry()
	registry.MustRegister(search)
	manager := tool.NewManager(registry, nil, 1, time.Hour, zap.NewNop())
	toolRouter := tool.NewRouter(registry, zap.NewNop(), manager)
	scope := tool.Scope{SessionID: "session"}

	result, err := New(llmRouter, toolRouter).Run(context.Background(), Input{
		Messages: []model.LLMMessage{{Role: "user", Content: "搜索"}},
		Tools:    toolRouter.InitialToolDefinitions(context.Background(), scope), MaxIterations: 1,
		MaxDiscoveryCalls: 4, ToolScope: scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "搜索完成" || search.calls != 1 || len(client.requests) != 4 {
		t.Fatalf("result=%+v search_calls=%d requests=%d", result, search.calls, len(client.requests))
	}
	if len(client.requests[0].Tools) != 1 || client.requests[0].Tools[0].Function.Name != tool.ListToolsName {
		t.Fatalf("cold initial tools = %+v", client.requests[0].Tools)
	}
	if !hasToolDefinition(client.requests[2].Tools, "web_search") {
		t.Fatalf("loaded tool missing from next request: %+v", client.requests[2].Tools)
	}
	if client.requests[3].ToolChoice != "none" || len(client.requests[3].Tools) != 0 {
		t.Fatalf("forced final request retained schemas: %+v", client.requests[3])
	}
}

func TestLoopRemovesDiscoveryToolAtIndependentLimit(t *testing.T) {
	responses := make([]model.LLMResponse, 0, 5)
	for i := 0; i < 4; i++ {
		responses = append(responses, model.LLMResponse{ToolCalls: []model.LLMToolCall{
			toolCallWithArgs(string(rune('a'+i)), tool.ListToolsName, map[string]interface{}{"action": "catalog"}),
		}})
	}
	responses = append(responses, model.LLMResponse{Content: "停止发现"})
	client := &loopClient{responses: responses}
	llmRouter := llm.NewRouter(map[string]llm.Client{"loop": client}, []config.ModelConfig{{Name: "loop"}}, config.CBConfig{FailureThreshold: 3, SuccessThreshold: 1})
	registry := tool.NewRegistry()
	manager := tool.NewManager(registry, nil, 3, time.Hour, zap.NewNop())
	toolRouter := tool.NewRouter(registry, zap.NewNop(), manager)
	scope := tool.Scope{SessionID: "session"}

	result, err := New(llmRouter, toolRouter).Run(context.Background(), Input{
		Messages: []model.LLMMessage{{Role: "user", Content: "hello"}},
		Tools:    toolRouter.InitialToolDefinitions(context.Background(), scope), MaxIterations: 1,
		MaxDiscoveryCalls: 4, ToolScope: scope,
	})
	if err != nil || result.Answer != "停止发现" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(client.requests) != 5 || len(client.requests[4].Tools) != 0 {
		t.Fatalf("discovery limit requests=%d final tools=%+v", len(client.requests), client.requests[4].Tools)
	}
}

func TestLoopCountsMixedDiscoveryAndParallelBusinessCallsSeparately(t *testing.T) {
	client := &loopClient{responses: []model.LLMResponse{
		{ToolCalls: []model.LLMToolCall{
			toolCallWithArgs("catalog", tool.ListToolsName, map[string]interface{}{"action": "catalog"}),
			toolCall("alpha", "alpha"),
			toolCall("beta", "beta"),
		}},
		{Content: "完成"},
	}}
	llmRouter := llm.NewRouter(map[string]llm.Client{"loop": client}, []config.ModelConfig{{Name: "loop"}}, config.CBConfig{FailureThreshold: 3, SuccessThreshold: 1})
	alpha, beta := &loopTool{name: "alpha"}, &loopTool{name: "beta"}
	registry := tool.NewRegistry()
	registry.MustRegister(alpha)
	registry.MustRegister(beta)
	manager := tool.NewManager(registry, nil, 2, time.Hour, zap.NewNop())
	toolRouter := tool.NewRouter(registry, zap.NewNop(), manager)
	scope := tool.Scope{SessionID: "session"}
	loaded, err := manager.Load(context.Background(), scope, []string{"alpha", "beta"})
	if err != nil || !loaded.Success {
		t.Fatalf("preload result=%+v err=%v", loaded, err)
	}
	initial := append([]model.ToolDef{toolRouter.InitialToolDefinitions(context.Background(), scope)[0]}, loaded.ToolDefinitions...)

	result, err := New(llmRouter, toolRouter).Run(context.Background(), Input{
		Messages: []model.LLMMessage{{Role: "user", Content: "run both"}},
		Tools:    initial, MaxIterations: 1, MaxDiscoveryCalls: 1, ToolScope: scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "完成" || alpha.calls != 1 || beta.calls != 1 {
		t.Fatalf("result=%+v alpha_calls=%d beta_calls=%d", result, alpha.calls, beta.calls)
	}
	if len(client.requests) != 2 || client.requests[1].ToolChoice != "none" || len(client.requests[1].Tools) != 0 {
		t.Fatalf("mixed call budgets produced requests=%+v", client.requests)
	}
}

func hasToolDefinition(definitions []model.ToolDef, name string) bool {
	for _, definition := range definitions {
		if definition.Function.Name == name {
			return true
		}
	}
	return false
}
