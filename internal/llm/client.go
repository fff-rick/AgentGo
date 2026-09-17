// Package llm 提供大语言模型的客户端抽象、多模型路由和熔断保护。
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/model"
)

// Client 大语言模型客户端接口。
// 所有 LLM 提供方（OpenAI、Anthropic、本地模型等）都应实现此接口。
type Client interface {
	// Chat 发送对话请求并返回完整响应
	Chat(ctx context.Context, req *model.LLMRequest) (*model.LLMResponse, error)

	// ChatStream 发送对话请求并以流式方式返回响应。
	// 调用方通过 channel 接收每个响应块，channel 关闭表示流结束。
	ChatStream(ctx context.Context, req *model.LLMRequest) (<-chan StreamEvent, error)

	// Name 返回客户端标识名称
	Name() string

	// Healthy 检查客户端是否可用
	Healthy(ctx context.Context) bool
}

// StreamEvent 流式响应事件
type StreamEvent struct {
	Content   string // 文本内容增量
	Reasoning string // 模型显式返回的 reasoning_content/reasoning/thinking
	ToolCalls []model.LLMToolCall
	Usage     *model.UsageInfo // 仅后端实际返回时设置
	Done      bool             // 是否结束
	Err       error            // 错误信息
}

// HTTPClient 基于 HTTP 的 LLM 客户端实现（兼容 OpenAI API 格式）
type HTTPClient struct {
	name      string
	modelName string
	client    openai.Client
}

// NewHTTPClient 创建一个新的 HTTP LLM 客户端
func NewHTTPClient(cfg config.ModelConfig, timeout time.Duration) *HTTPClient {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL != "" && !strings.HasSuffix(baseURL, "/v1") {
		baseURL += "/v1"
	}
	opts := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithHeader("User-Agent", "AgentGo/1.0"),
		option.WithHTTPClient(&http.Client{Timeout: timeout}),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &HTTPClient{
		name:      cfg.Name,
		modelName: cfg.Model,
		client:    openai.NewClient(opts...),
	}
}

// Chat 发送同步对话请求
func (c *HTTPClient) Chat(ctx context.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
	params, err := c.toSDKRequest(req)
	if err != nil {
		return nil, err
	}
	completion, err := c.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("LLM 请求失败: %w", err)
	}
	return completionToResponse(completion), nil
}

// ChatStream 发送流式对话请求
func (c *HTTPClient) ChatStream(ctx context.Context, req *model.LLMRequest) (<-chan StreamEvent, error) {
	params, err := c.toSDKRequest(req)
	if err != nil {
		return nil, err
	}
	ch := make(chan StreamEvent, 32)
	go c.readSDKStream(ctx, params, ch)
	return ch, nil
}

func (c *HTTPClient) readSDKStream(ctx context.Context, params openai.ChatCompletionNewParams, ch chan<- StreamEvent) {
	defer close(ch)
	stream := c.client.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()
	acc := openai.ChatCompletionAccumulator{}
	for stream.Next() {
		chunk := stream.Current()
		if chunk.JSON.Usage.Valid() {
			ch <- StreamEvent{Usage: &model.UsageInfo{PromptTokens: int(chunk.Usage.PromptTokens), CompletionTokens: int(chunk.Usage.CompletionTokens), TotalTokens: int(chunk.Usage.TotalTokens)}}
		}
		if !acc.AddChunk(chunk) {
			ch <- StreamEvent{Err: fmt.Errorf("无法合并 LLM 流式响应")}
			return
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		content := chunk.Choices[0].Delta.Content
		reasoning := reasoningFromJSON(chunk.RawJSON())
		if content != "" || reasoning != "" {
			ch <- StreamEvent{Content: content, Reasoning: reasoning}
		}
	}
	if err := stream.Err(); err != nil {
		ch <- StreamEvent{Err: err}
		return
	}
	var calls []model.LLMToolCall
	if len(acc.Choices) > 0 {
		calls = convertToolCalls(acc.Choices[0].Message.ToolCalls)
	}
	ch <- StreamEvent{Done: true, ToolCalls: calls}
}

// Name 返回客户端名称
func (c *HTTPClient) Name() string {
	return c.name
}

// Healthy 通过简单请求检查模型是否可用
func (c *HTTPClient) Healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req := &model.LLMRequest{
		Model: c.modelName,
		Messages: []model.LLMMessage{
			{Role: "user", Content: "ping"},
		},
		MaxTokens: 5,
	}

	_, err := c.Chat(ctx, req)
	return err == nil
}

func (c *HTTPClient) toSDKRequest(req *model.LLMRequest) (openai.ChatCompletionNewParams, error) {
	params := openai.ChatCompletionNewParams{Model: c.modelName}
	for _, message := range req.Messages {
		sdkMessage, err := toSDKMessage(message)
		if err != nil {
			return params, err
		}
		params.Messages = append(params.Messages, sdkMessage)
	}
	if req.Temperature != 0 {
		params.Temperature = openai.Float(req.Temperature)
	}
	if req.MaxTokens > 0 {
		params.MaxTokens = openai.Int(int64(req.MaxTokens))
	}
	for _, definition := range req.Tools {
		parameters, ok := definition.Function.Parameters.(map[string]interface{})
		if !ok {
			return params, fmt.Errorf("工具 %q 的 parameters 不是 JSON 对象", definition.Function.Name)
		}
		params.Tools = append(params.Tools, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name: definition.Function.Name, Description: openai.String(definition.Function.Description), Parameters: parameters,
		}))
	}
	if len(params.Tools) > 0 {
		params.ParallelToolCalls = openai.Bool(true)
	}
	if req.RequiredTool != "" {
		params.ToolChoice = openai.ToolChoiceOptionFunctionToolChoice(openai.ChatCompletionNamedToolChoiceFunctionParam{Name: req.RequiredTool})
	} else if req.ToolChoice != "" {
		params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: openai.String(req.ToolChoice)}
	}
	return params, nil
}

func toSDKMessage(message model.LLMMessage) (openai.ChatCompletionMessageParamUnion, error) {
	switch message.Role {
	case "system":
		return openai.SystemMessage(message.Content), nil
	case "developer":
		return openai.DeveloperMessage(message.Content), nil
	case "user":
		return openai.UserMessage(message.Content), nil
	case "tool":
		if message.ToolCallID == "" {
			return openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("tool 消息缺少 tool_call_id")
		}
		return openai.ToolMessage(message.Content, message.ToolCallID), nil
	case "assistant":
		result := openai.AssistantMessage(message.Content)
		for _, call := range message.ToolCalls {
			result.OfAssistant.ToolCalls = append(result.OfAssistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
					ID:       call.ID,
					Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{Name: call.Function.Name, Arguments: call.Function.Arguments},
				},
			})
		}
		return result, nil
	default:
		return openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("不支持的消息角色 %q", message.Role)
	}
}

func completionToResponse(completion *openai.ChatCompletion) *model.LLMResponse {
	result := &model.LLMResponse{Reasoning: reasoningFromJSON(completion.RawJSON())}
	if len(completion.Choices) > 0 {
		result.Content = completion.Choices[0].Message.Content
		result.ToolCalls = convertToolCalls(completion.Choices[0].Message.ToolCalls)
	}
	if completion.JSON.Usage.Valid() {
		result.Usage = &model.UsageInfo{
			PromptTokens: int(completion.Usage.PromptTokens), CompletionTokens: int(completion.Usage.CompletionTokens), TotalTokens: int(completion.Usage.TotalTokens),
		}
	}
	return result
}

func convertToolCalls(calls []openai.ChatCompletionMessageToolCallUnion) []model.LLMToolCall {
	result := make([]model.LLMToolCall, 0, len(calls))
	for _, call := range calls {
		if call.Type != "function" {
			continue
		}
		var converted model.LLMToolCall
		converted.ID, converted.Type = call.ID, "function"
		converted.Function.Name, converted.Function.Arguments = call.Function.Name, call.Function.Arguments
		result = append(result, converted)
	}
	return result
}

func reasoningFromJSON(raw string) string {
	var payload struct {
		Choices []struct {
			Message reasoningFields `json:"message"`
			Delta   reasoningFields `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(raw), &payload) != nil || len(payload.Choices) == 0 {
		return ""
	}
	fields := payload.Choices[0].Message
	if fields.empty() {
		fields = payload.Choices[0].Delta
	}
	return fields.value()
}

type reasoningFields struct {
	ReasoningContent string `json:"reasoning_content"`
	Reasoning        string `json:"reasoning"`
	Thinking         string `json:"thinking"`
}

func (r reasoningFields) empty() bool { return r.value() == "" }
func (r reasoningFields) value() string {
	if r.ReasoningContent != "" {
		return r.ReasoningContent
	}
	if r.Reasoning != "" {
		return r.Reasoning
	}
	return r.Thinking
}
