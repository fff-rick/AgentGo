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
	"github.com/enterprise/ai-agent-go/internal/observe"
	"github.com/enterprise/ai-agent-go/internal/tool"
)

type sequenceLLMClient struct {
	responses []string
	next      int
}

func (c *sequenceLLMClient) Chat(context.Context, *model.LLMRequest) (*model.LLMResponse, error) {
	response := c.responses[c.next]
	c.next++
	return &model.LLMResponse{Content: response}, nil
}

func (*sequenceLLMClient) ChatStream(context.Context, *model.LLMRequest) (<-chan llm.StreamEvent, error) {
	stream := make(chan llm.StreamEvent)
	close(stream)
	return stream, nil
}

func (*sequenceLLMClient) Name() string                 { return "sequence" }
func (*sequenceLLMClient) Healthy(context.Context) bool { return true }

type recordingTool struct {
	input string
}

func (*recordingTool) Name() string        { return "web_search" }
func (*recordingTool) Description() string { return "搜索实时信息" }
func (*recordingTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{"type": "string"},
		},
		"required": []string{"query"},
	}
}
func (t *recordingTool) Execute(_ context.Context, input string) (*tool.ToolResult, error) {
	t.input = input
	return tool.NewSuccessResult("成都晴，25°C"), nil
}

type retryingSearchTool struct {
	inputs []string
}

func (*retryingSearchTool) Name() string        { return "web_search" }
func (*retryingSearchTool) Description() string { return "搜索实时信息" }
func (*retryingSearchTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{"type": "string"},
		},
		"required": []string{"query"},
	}
}
func (t *retryingSearchTool) Execute(_ context.Context, input string) (*tool.ToolResult, error) {
	t.inputs = append(t.inputs, input)
	if len(t.inputs) == 1 {
		return tool.NewSuccessResult(`{"query":"成都天气","results":[]}`), nil
	}
	return tool.NewSuccessResult(`{"query":"成都实时天气","results":[{"title":"成都天气","snippet":"晴，25°C"}]}`), nil
}

func TestReActExecutesActionWithObjectInput(t *testing.T) {
	client := &sequenceLLMClient{responses: []string{
		`Thought: 需要查询实时天气。
Action: {"tool":"web_search","input":{"query":"成都今日天气"}}`,
		`Thought: 已获得天气信息。
Final Answer: 成都今天晴，气温 25°C。`,
	}}
	modelRouter := llm.NewRouter(map[string]llm.Client{"sequence": client}, "sequence", config.CBConfig{
		FailureThreshold: 3,
		SuccessThreshold: 1,
	})
	registry := tool.NewRegistry()
	search := &recordingTool{}
	registry.MustRegister(search)
	agent := NewReActAgent(modelRouter, tool.NewRouter(registry, zap.NewNop()), 3, zap.NewNop())

	var eventTypes []string
	ctx := observe.WithEmitter(context.Background(), func(event observe.Event) {
		eventTypes = append(eventTypes, event.Type)
	})
	result, err := agent.Run(ctx, "今天成都的天气怎么样？", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "成都今天晴，气温 25°C。" {
		t.Fatalf("answer = %q", result.Answer)
	}
	var input map[string]string
	if err := json.Unmarshal([]byte(search.input), &input); err != nil {
		t.Fatalf("tool input %q is not JSON: %v", search.input, err)
	}
	if input["query"] != "成都今日天气" || len(result.ToolCalls) != 1 {
		t.Fatalf("tool input/result = %#v / %+v", input, result.ToolCalls)
	}
	joined := strings.Join(eventTypes, ",")
	callIndex := strings.Index(joined, observe.TypeToolCall)
	resultIndex := strings.Index(joined, observe.TypeToolResult)
	if callIndex < 0 || resultIndex <= callIndex {
		t.Fatalf("tool events missing or out of order: %s", joined)
	}
}

func TestReActRetriesResponseWithoutActionOrFinalAnswer(t *testing.T) {
	client := &sequenceLLMClient{responses: []string{
		`Thought: 我应该继续处理。`,
		`Thought: 已完成。
Final Answer: 已按协议完成。`,
	}}
	modelRouter := llm.NewRouter(map[string]llm.Client{"sequence": client}, "sequence", config.CBConfig{
		FailureThreshold: 3,
		SuccessThreshold: 1,
	})
	agent := NewReActAgent(modelRouter, tool.NewRouter(tool.NewRegistry(), zap.NewNop()), 3, zap.NewNop())

	result, err := agent.Run(context.Background(), "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if client.next != 2 || result.Answer != "已按协议完成。" {
		t.Fatalf("calls=%d answer=%q", client.next, result.Answer)
	}
}

func TestReActRejectsFinalAnswerBeforeToolExecution(t *testing.T) {
	client := &sequenceLLMClient{responses: []string{
		`Thought: 我可以直接回答。
Final Answer: 成都今天晴。`,
		`Thought: 必须先查询。
Action: {"tool":"web_search","input":{"query":"成都今日天气"}}`,
		`Thought: 已完成查询。
Final Answer: 成都今天晴，气温 25°C。`,
	}}
	modelRouter := llm.NewRouter(map[string]llm.Client{"sequence": client}, "sequence", config.CBConfig{
		FailureThreshold: 3,
		SuccessThreshold: 1,
	})
	registry := tool.NewRegistry()
	search := &recordingTool{}
	registry.MustRegister(search)
	agent := NewReActAgent(modelRouter, tool.NewRouter(registry, zap.NewNop()), 4, zap.NewNop())

	result, err := agent.Run(context.Background(), "今天成都天气？", nil)
	if err != nil {
		t.Fatal(err)
	}
	if client.next != 3 || len(result.ToolCalls) != 1 || result.Answer != "成都今天晴，气温 25°C。" {
		t.Fatalf("calls=%d tool_calls=%d answer=%q", client.next, len(result.ToolCalls), result.Answer)
	}
}

func TestReActExecutesRequiredSearchBeforeFirstModelCall(t *testing.T) {
	client := &sequenceLLMClient{responses: []string{
		`Thought: 已获得搜索结果。
Final Answer: 成都今天晴，气温 25°C。`,
	}}
	modelRouter := llm.NewRouter(map[string]llm.Client{"sequence": client}, "sequence", config.CBConfig{
		FailureThreshold: 3,
		SuccessThreshold: 1,
	})
	registry := tool.NewRegistry()
	search := &recordingTool{}
	registry.MustRegister(search)
	agent := NewReActAgent(modelRouter, tool.NewRouter(registry, zap.NewNop()), 3, zap.NewNop())

	var statusMessages []string
	ctx := observe.WithEmitter(context.Background(), func(event observe.Event) {
		if event.Type == observe.TypeStatus {
			statusMessages = append(statusMessages, event.Message)
		}
	})
	result, err := agent.RunWithRequiredTools(ctx, "今天成都天气？", nil, []string{"web_search"})
	if err != nil {
		t.Fatal(err)
	}
	if client.next != 1 || len(result.ToolCalls) != 1 || result.Answer != "成都今天晴，气温 25°C。" {
		t.Fatalf("calls=%d tool_calls=%d answer=%q", client.next, len(result.ToolCalls), result.Answer)
	}
	if len(result.Steps) == 0 || result.Steps[0].StepIndex != 1 || result.Steps[0].Type != "action" {
		t.Fatalf("required tool should be the first ReAct action: %+v", result.Steps)
	}
	var input map[string]interface{}
	if err := json.Unmarshal([]byte(search.input), &input); err != nil || !strings.Contains(input["query"].(string), "今天成都天气？ 天气预报") {
		t.Fatalf("required tool input=%q parsed=%+v err=%v", search.input, input, err)
	}
	if input["time_range"] != "day" {
		t.Fatalf("required realtime search should use day range: %+v", input)
	}
	joined := strings.Join(statusMessages, "|")
	roundIndex := strings.Index(joined, "ReAct 第 1/3 轮")
	requiredToolIndex := strings.Index(joined, "正在执行意图识别要求的工具 web_search")
	if roundIndex < 0 || requiredToolIndex <= roundIndex {
		t.Fatalf("required tool events are out of order: %s", joined)
	}
}

func TestReActRejectsFinalAnswerWhenThoughtSaysRetry(t *testing.T) {
	client := &sequenceLLMClient{responses: []string{
		`Thought: 首次搜索没有返回结果，我换用更简洁的实时天气关键词再次查询。
Final Answer: 暂时没有查询到天气信息。`,
		`Thought: 使用更简洁的关键词重新查询。
Action: {"tool":"web_search","input":{"query":"成都实时天气"}}`,
		`Thought: 新的搜索结果有效，可以回答。
Final Answer: 成都当前晴，气温 25°C。`,
	}}
	modelRouter := llm.NewRouter(map[string]llm.Client{"sequence": client}, "sequence", config.CBConfig{
		FailureThreshold: 3,
		SuccessThreshold: 1,
	})
	registry := tool.NewRegistry()
	search := &retryingSearchTool{}
	registry.MustRegister(search)
	agent := NewReActAgent(modelRouter, tool.NewRouter(registry, zap.NewNop()), 5, zap.NewNop())

	result, err := agent.RunWithRequiredTools(context.Background(), "今天成都天气？", nil, []string{"web_search"})
	if err != nil {
		t.Fatal(err)
	}
	if client.next != 3 || len(search.inputs) != 2 || len(result.ToolCalls) != 2 {
		t.Fatalf("model_calls=%d search_calls=%d tool_calls=%d", client.next, len(search.inputs), len(result.ToolCalls))
	}
	if result.Answer != "成都当前晴，气温 25°C。" {
		t.Fatalf("answer=%q", result.Answer)
	}
}

func TestParseActionAcceptsStringInputAndRejectsMalformedAction(t *testing.T) {
	agent := &ReActAgent{logger: zap.NewNop()}
	action, found, err := agent.parseAction(`Action: {"tool":"calculator","input":"{\"a\":1}"}`)
	if err != nil || !found || action.Input != `{"a":1}` {
		t.Fatalf("string input action = %+v, found=%v, err=%v", action, found, err)
	}
	if _, found, err := agent.parseAction(`Thought: query
Action: {"tool":"web_search","input":`); !found || err == nil {
		t.Fatalf("malformed action should be found with an error: found=%v err=%v", found, err)
	}
}

func TestBuildToolsDescriptionIncludesSchema(t *testing.T) {
	registry := tool.NewRegistry()
	registry.MustRegister(&recordingTool{})
	agent := &ReActAgent{toolRouter: tool.NewRouter(registry, zap.NewNop())}
	description := agent.buildToolsDescription()
	if !strings.Contains(description, "搜索实时信息") || !strings.Contains(description, `"required":["query"]`) {
		t.Fatalf("tool description is incomplete: %s", description)
	}
}
