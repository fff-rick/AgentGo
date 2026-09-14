package agentloop

import (
	"context"
	"testing"

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
	stream := make(chan llm.StreamEvent, 2)
	if response.Content != "" {
		stream <- llm.StreamEvent{Content: response.Content}
	}
	stream <- llm.StreamEvent{Done: true, ToolCalls: response.ToolCalls}
	close(stream)
	return stream, nil
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
