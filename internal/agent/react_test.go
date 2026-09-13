package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/llm"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/observe"
	"github.com/enterprise/ai-agent-go/internal/tool"
)

type sequenceLLMClient struct {
	responses []model.LLMResponse
	requests  []*model.LLMRequest
	next      int
}

func (c *sequenceLLMClient) Chat(context.Context, *model.LLMRequest) (*model.LLMResponse, error) {
	panic("ReAct must use ChatStream")
}

func (c *sequenceLLMClient) ChatStream(_ context.Context, req *model.LLMRequest) (<-chan llm.StreamEvent, error) {
	c.requests = append(c.requests, req)
	response := c.responses[c.next]
	c.next++
	stream := make(chan llm.StreamEvent, 2)
	if response.Content != "" {
		stream <- llm.StreamEvent{Content: response.Content}
	}
	stream <- llm.StreamEvent{Done: true, ToolCalls: response.ToolCalls}
	close(stream)
	return stream, nil
}

func (*sequenceLLMClient) Name() string                 { return "sequence" }
func (*sequenceLLMClient) Healthy(context.Context) bool { return true }

type recordingTool struct {
	name   string
	mu     sync.Mutex
	inputs []string
}

func (t *recordingTool) Name() string        { return t.name }
func (t *recordingTool) Description() string { return "test tool" }
func (t *recordingTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{"value": map[string]interface{}{"type": "string"}}}
}
func (t *recordingTool) Execute(_ context.Context, input string) (*tool.ToolResult, error) {
	t.mu.Lock()
	t.inputs = append(t.inputs, input)
	t.mu.Unlock()
	return tool.NewSuccessResult(t.name + " result"), nil
}

func nativeCall(id, name, arguments string) model.LLMToolCall {
	call := model.LLMToolCall{ID: id, Type: "function"}
	call.Function.Name, call.Function.Arguments = name, arguments
	return call
}

func newTestAgent(t *testing.T, client *sequenceLLMClient, maxIterations int, tools ...tool.Tool) *ReActAgent {
	t.Helper()
	router := llm.NewRouter(map[string]llm.Client{"sequence": client}, []config.ModelConfig{{Name: "sequence"}}, config.CBConfig{FailureThreshold: 3, SuccessThreshold: 1})
	registry := tool.NewRegistry()
	for _, candidate := range tools {
		registry.MustRegister(candidate)
	}
	return NewReActAgent(router, tool.NewRouter(registry, zap.NewNop()), maxIterations, "", zap.NewNop())
}

func TestReActRoutesToolCallingToConfiguredModel(t *testing.T) {
	calculator := &recordingTool{name: "calculator"}
	qwen := &sequenceLLMClient{responses: []model.LLMResponse{
		{ToolCalls: []model.LLMToolCall{nativeCall("call-1", "calculator", `{"value":"1+1"}`)}},
		{Content: "2"},
	}}
	gpt := &sequenceLLMClient{}
	router := llm.NewRouter(map[string]llm.Client{"gpt": gpt, "qwen-tools": qwen}, []config.ModelConfig{{Name: "gpt", Priority: 1}, {Name: "qwen-tools", Priority: 10}}, config.CBConfig{FailureThreshold: 3, SuccessThreshold: 1})
	registry := tool.NewRegistry()
	registry.MustRegister(calculator)
	agent := NewReActAgent(router, tool.NewRouter(registry, zap.NewNop()), 3, "qwen-tools", zap.NewNop())

	if _, err := agent.Run(context.Background(), "计算", nil); err != nil {
		t.Fatal(err)
	}
	if len(qwen.requests) != 2 || len(gpt.requests) != 0 {
		t.Fatalf("qwen requests=%d, gpt requests=%d", len(qwen.requests), len(gpt.requests))
	}
}

func TestReActUsesNativeParallelToolCallsAndStreamsFinalAnswer(t *testing.T) {
	search, calculator := &recordingTool{name: "web_search"}, &recordingTool{name: "calculator"}
	client := &sequenceLLMClient{responses: []model.LLMResponse{
		{ToolCalls: []model.LLMToolCall{nativeCall("call-1", "web_search", `{"value":"weather"}`), nativeCall("call-2", "calculator", `{"value":"2+2"}`)}},
		{Content: "最终结果"},
	}}
	agent := newTestAgent(t, client, 3, search, calculator)
	var deltas string
	ctx := observe.WithEmitter(context.Background(), func(event observe.Event) {
		if event.Type == observe.TypeAnswerDelta {
			deltas += event.Message
		}
	})
	result, err := agent.Run(ctx, "查天气并计算", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "最终结果" || deltas != "最终结果" || len(result.ToolCalls) != 2 || len(result.Steps) != 3 {
		t.Fatalf("result=%+v deltas=%q", result, deltas)
	}
	if client.requests[0].ToolChoice != "required" || len(client.requests[0].Tools) != 2 {
		t.Fatalf("first request did not require native tools: %+v", client.requests[0])
	}
	secondMessages := client.requests[1].Messages
	if len(secondMessages) < 3 || len(secondMessages[len(secondMessages)-3].ToolCalls) != 2 {
		t.Fatalf("assistant tool calls missing from history: %+v", secondMessages)
	}
	for _, message := range secondMessages[len(secondMessages)-2:] {
		var wrapped map[string]interface{}
		if message.Role != "tool" || message.ToolCallID == "" || json.Unmarshal([]byte(message.Content), &wrapped) != nil || wrapped["trusted"] != false {
			t.Fatalf("tool output is not isolated: %+v", message)
		}
	}
}

func TestReActCanForceAnyRegisteredTool(t *testing.T) {
	calculator := &recordingTool{name: "calculator"}
	client := &sequenceLLMClient{responses: []model.LLMResponse{
		{ToolCalls: []model.LLMToolCall{nativeCall("call-1", "calculator", `{"value":"1+1"}`)}},
		{Content: "2"},
	}}
	agent := newTestAgent(t, client, 3, calculator)
	if _, err := agent.RunWithRequiredTools(context.Background(), "计算", nil, []string{"calculator"}); err != nil {
		t.Fatal(err)
	}
	if client.requests[0].RequiredTool != "calculator" {
		t.Fatalf("required tool = %q", client.requests[0].RequiredTool)
	}
}

func TestReActCleansMaxIterationAnswer(t *testing.T) {
	search := &recordingTool{name: "web_search"}
	client := &sequenceLLMClient{responses: []model.LLMResponse{
		{ToolCalls: []model.LLMToolCall{nativeCall("call-1", "web_search", `{}`)}},
		{Content: "Thought: hidden\nFinal Answer: cleaned"},
	}}
	agent := newTestAgent(t, client, 1, search)
	result, err := agent.Run(context.Background(), "search", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "cleaned" || client.requests[1].ToolChoice != "none" {
		t.Fatalf("answer=%q request=%+v", result.Answer, client.requests[1])
	}
}
