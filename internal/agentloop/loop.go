// Package agentloop implements the model/tool execution loop used by every Agent run.
package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/enterprise/ai-agent-go/internal/llm"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/observe"
	"github.com/enterprise/ai-agent-go/internal/tool"
)

const maxToolOutputRunes = 20000

type AgentLoop interface {
	Run(context.Context, Input) (*Result, error)
}

type Input struct {
	Messages      []model.LLMMessage
	Tools         []model.ToolDef
	MaxIterations int
	RequiredTools []string
	SystemPrompt  string
	Model         string
	RequireTool   bool
	State         *RunState
}

// RunState is ephemeral working memory for one Agent run.
type RunState struct {
	RunID       string
	UserID      string
	SessionID   string
	Task        string
	Steps       []model.AgentStep
	ToolResults []tool.ToolResult
}

type Result struct {
	Answer     string
	Steps      []model.AgentStep
	ToolCalls  []model.ToolCallInfo
	References []model.Reference
}

type Loop struct {
	router   *llm.Router
	executor *tool.Router
}

func New(router *llm.Router, executor *tool.Router) *Loop {
	return &Loop{router: router, executor: executor}
}

func (l *Loop) Run(ctx context.Context, input Input) (*Result, error) {
	if input.MaxIterations <= 0 {
		return nil, fmt.Errorf("max iterations 必须大于 0")
	}
	forcedTools, err := validateRequiredTools(input.RequiredTools, input.Tools)
	if err != nil {
		return nil, err
	}
	messages := make([]model.LLMMessage, 0, len(input.Messages)+2+input.MaxIterations*3)
	if input.SystemPrompt != "" {
		messages = append(messages, model.LLMMessage{Role: "system", Content: input.SystemPrompt})
	}
	messages = append(messages, input.Messages...)

	result := &Result{}
	forcedIndex := 0
	for iteration := 0; iteration < input.MaxIterations; iteration++ {
		observe.Emit(ctx, observe.Event{Type: observe.TypeStatus, Stage: "agent_loop", Message: fmt.Sprintf("Agent 决策第 %d/%d 轮", iteration+1, input.MaxIterations)})
		req := &model.LLMRequest{Model: input.Model, Messages: messages, Tools: input.Tools, ToolChoice: "auto", Temperature: 0.3}
		forcedTool := ""
		if forcedIndex < len(forcedTools) {
			forcedTool = forcedTools[forcedIndex]
			req.RequiredTool = forcedTool
		} else if input.RequireTool && len(result.ToolCalls) == 0 && len(input.Tools) > 0 {
			req.ToolChoice = "required"
		}

		resp, err := l.chatStream(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("Agent Loop 第 %d 轮失败: %w", iteration+1, err)
		}
		if len(resp.ToolCalls) == 0 {
			if forcedTool != "" {
				return nil, fmt.Errorf("模型未按要求调用工具 %q", forcedTool)
			}
			answer := cleanFinalAnswer(resp.Content)
			if answer == "" {
				return nil, fmt.Errorf("模型未返回工具调用或最终答案")
			}
			result.Answer = answer
			result.Steps = append(result.Steps, model.AgentStep{StepIndex: iteration + 1, Type: "answer", Content: answer, Timestamp: time.Now()})
			syncState(input.State, result.Steps)
			return result, nil
		}
		if forcedTool != "" {
			if !containsToolCall(resp.ToolCalls, forcedTool) {
				return nil, fmt.Errorf("模型返回的工具调用不包含强制工具 %q", forcedTool)
			}
			forcedIndex++
		}

		messages = append(messages, model.LLMMessage{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls})
		calls := make([]tool.ToolCall, len(resp.ToolCalls))
		for i, call := range resp.ToolCalls {
			calls[i] = tool.ToolCall{Name: call.Function.Name, Input: call.Function.Arguments}
			observe.Emit(ctx, observe.Event{Type: observe.TypeToolCall, Stage: "tool", Tool: &model.ToolCallInfo{ToolName: call.Function.Name, Input: call.Function.Arguments}})
		}
		for i, execution := range l.executor.BatchExecute(ctx, calls) {
			call := resp.ToolCalls[i]
			callInfo, toolMessage := toolExecutionResult(call, execution)
			result.ToolCalls = append(result.ToolCalls, callInfo)
			if execution != nil && execution.Result != nil {
				result.References = append(result.References, execution.Result.References...)
				if input.State != nil {
					input.State.ToolResults = append(input.State.ToolResults, *execution.Result)
				}
			}
			result.Steps = append(result.Steps, model.AgentStep{
				StepIndex: iteration + 1, Type: "action", ToolName: call.Function.Name,
				ToolInput: call.Function.Arguments, ToolOutput: callInfo.Output, Timestamp: time.Now(),
			})
			syncState(input.State, result.Steps)
			observe.Emit(ctx, observe.Event{Type: observe.TypeToolResult, Stage: "tool", Message: callInfo.Output, Tool: &callInfo})
			messages = append(messages, model.LLMMessage{Role: "tool", ToolCallID: call.ID, Content: toolMessage})
		}
	}

	observe.Emit(ctx, observe.Event{Type: observe.TypeStatus, Stage: "agent_loop", Message: "工具调用达到上限，正在生成最终答案"})
	messages = append(messages, model.LLMMessage{Role: "system", Content: "工具调用轮次已结束。禁止继续调用工具；请仅根据已有可信信息直接给出最终答案。"})
	resp, err := l.chatStream(ctx, &model.LLMRequest{Model: input.Model, Messages: messages, Tools: input.Tools, ToolChoice: "none"})
	if err != nil {
		return nil, fmt.Errorf("生成最终答案失败: %w", err)
	}
	result.Answer = cleanFinalAnswer(resp.Content)
	if result.Answer == "" {
		result.Answer = "抱歉，已达到工具调用上限，无法生成有效答案。"
	}
	result.Steps = append(result.Steps, model.AgentStep{StepIndex: input.MaxIterations + 1, Type: "answer", Content: result.Answer, Timestamp: time.Now()})
	syncState(input.State, result.Steps)
	return result, nil
}

func syncState(state *RunState, steps []model.AgentStep) {
	if state != nil {
		state.Steps = append(state.Steps[:0], steps...)
	}
}

func (l *Loop) chatStream(ctx context.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
	stream, err := l.router.ChatStream(ctx, req)
	if err != nil {
		return nil, err
	}
	response := &model.LLMResponse{}
	var content strings.Builder
	for event := range stream {
		if event.Err != nil {
			return nil, event.Err
		}
		if event.Reasoning != "" {
			observe.Emit(ctx, observe.Event{Type: observe.TypeReasoningDelta, Stage: "agent_loop", Message: event.Reasoning})
		}
		if event.Content != "" {
			content.WriteString(event.Content)
			observe.Emit(ctx, observe.Event{Type: observe.TypeAnswerDelta, Stage: "answer", Message: event.Content})
		}
		if len(event.ToolCalls) > 0 {
			response.ToolCalls = event.ToolCalls
		}
	}
	response.Content = content.String()
	return response, nil
}

func validateRequiredTools(required []string, definitions []model.ToolDef) ([]string, error) {
	available := make(map[string]struct{}, len(definitions))
	for _, definition := range definitions {
		available[definition.Function.Name] = struct{}{}
	}
	result := make([]string, 0, len(required))
	seen := make(map[string]struct{}, len(required))
	for _, name := range required {
		if _, ok := available[name]; !ok {
			return nil, fmt.Errorf("强制工具 %q 未注册", name)
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	return result, nil
}

func containsToolCall(calls []model.LLMToolCall, name string) bool {
	for _, call := range calls {
		if call.Function.Name == name {
			return true
		}
	}
	return false
}

func toolExecutionResult(call model.LLMToolCall, execution *tool.ToolCallResult) (model.ToolCallInfo, string) {
	success := execution != nil && execution.Err == nil && execution.Result != nil && execution.Result.Success
	output := "工具未返回结果"
	if execution != nil {
		switch {
		case execution.Err != nil:
			output = "工具执行错误: " + execution.Err.Error()
		case execution.Result == nil:
		case execution.Result.Success:
			output = execution.Result.Output
		default:
			output = "工具返回错误: " + execution.Result.Error
		}
	}
	duration := int64(0)
	if execution != nil {
		duration = execution.Duration.Milliseconds()
	}
	info := model.ToolCallInfo{ToolName: call.Function.Name, Input: call.Function.Arguments, Output: output, Duration: duration}
	wrapped, _ := json.Marshal(map[string]interface{}{
		"source": "tool", "trusted": false, "success": success, "output": truncateRunes(output, maxToolOutputRunes),
	})
	return info, string(wrapped)
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…[truncated]"
}

func cleanFinalAnswer(content string) string {
	content = strings.TrimSpace(content)
	for _, marker := range []string{"Final Answer:", "Final Answer：", "最终答案:", "最终答案："} {
		if index := strings.LastIndex(content, marker); index >= 0 {
			content = strings.TrimSpace(content[index+len(marker):])
		}
	}
	return content
}
