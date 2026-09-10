package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/llm"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/observe"
	"github.com/enterprise/ai-agent-go/internal/tool"
)

// ReAct Agent 的系统 Prompt 模板
const reactSystemPrompt = `你是一个智能助手，使用 ReAct（Reasoning + Acting）方法来解决问题。

在每一步中，你需要：
1. Thought: 分析当前情况，思考下一步应该做什么
2. Action: 如果需要使用工具，指定工具名称和参数
3. Observation: 观察工具返回的结果
4. 重复上述过程直到得出最终答案

可用工具：
%s

响应格式要求：
- 当需要调用工具时，回复格式：
  Thought: <你的思考>
  Action: {"tool": "<工具名>", "input": <工具参数JSON>}

- 当得出最终答案时，回复格式：
  Thought: <你的思考>
  Final Answer: <最终答案>

重要规则：
- 每次只能调用一个工具
- 最多迭代 %d 次
- 如果无法解决问题，直接给出你能给出的最好答案`

// AgentResult Agent 执行结果
type AgentResult struct {
	Answer    string               `json:"answer"`
	Steps     []model.AgentStep    `json:"steps"`
	ToolCalls []model.ToolCallInfo `json:"tool_calls"`
}

// ReActAgent ReAct 推理-行动循环 Agent。
// 实现经典的 Thought → Action → Observation 迭代推理模式。
type ReActAgent struct {
	router        *llm.Router
	toolRouter    *tool.Router
	maxIterations int
	logger        *zap.Logger
}

// NewReActAgent 创建 ReAct Agent
func NewReActAgent(router *llm.Router, toolRouter *tool.Router, maxIterations int, logger *zap.Logger) *ReActAgent {
	return &ReActAgent{
		router:        router,
		toolRouter:    toolRouter,
		maxIterations: maxIterations,
		logger:        logger,
	}
}

// Run 执行 ReAct 推理循环。
// 核心流程：Thought → Action → Observation → ... → Final Answer
func (a *ReActAgent) Run(ctx context.Context, query string, history []model.LLMMessage) (*AgentResult, error) {
	// 构造工具描述
	toolsDesc := a.buildToolsDescription()
	systemPrompt := fmt.Sprintf(reactSystemPrompt, toolsDesc, a.maxIterations)

	// 初始化消息列表
	messages := make([]model.LLMMessage, 0, len(history)+10)
	messages = append(messages, model.LLMMessage{Role: "system", Content: systemPrompt})
	messages = append(messages, history...)
	messages = append(messages, model.LLMMessage{Role: "user", Content: query})

	result := &AgentResult{}

	// ReAct 迭代循环
	for i := 0; i < a.maxIterations; i++ {
		observe.Emit(ctx, observe.Event{Type: observe.TypeStatus, Stage: "react", Message: fmt.Sprintf("ReAct 第 %d/%d 轮", i+1, a.maxIterations)})
		a.logger.Info("ReAct 迭代",
			zap.Int("iteration", i+1),
			zap.Int("max", a.maxIterations),
		)

		// 调用 LLM 获取思考和行动
		req := &model.LLMRequest{
			Messages:    messages,
			Temperature: 0.3,
		}

		resp, err := a.router.Chat(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("ReAct 第 %d 轮 LLM 调用失败: %w", i+1, err)
		}

		content := resp.Content
		if thought := a.extractThought(content); thought != "" {
			observe.Emit(ctx, observe.Event{Type: observe.TypeReasoning, Stage: "react", Message: thought})
		}

		// 检查是否已到达最终答案
		if finalAnswer := a.extractFinalAnswer(content); finalAnswer != "" {
			result.Answer = finalAnswer
			result.Steps = append(result.Steps, model.AgentStep{
				StepIndex: i + 1,
				Type:      "thought",
				Content:   content,
				Timestamp: time.Now(),
			})
			a.logger.Info("ReAct 得出最终答案", zap.Int("total_steps", i+1))
			return result, nil
		}

		// 解析 Action
		action, actionFound, actionErr := a.parseAction(content)
		if actionErr != nil {
			a.logger.Warn("Action 格式错误，要求模型修正",
				zap.Int("iteration", i+1),
				zap.Error(actionErr),
			)
			observe.Emit(ctx, observe.Event{Type: observe.TypeStatus, Stage: "react", Message: "工具调用格式错误，正在要求模型修正"})
			messages = append(messages,
				model.LLMMessage{Role: "assistant", Content: content},
				model.LLMMessage{Role: "user", Content: "你的 Action 无法解析：" + actionErr.Error() + "。请严格按 Action: {\"tool\":\"工具名\",\"input\":{...}} 格式重新输出。"},
			)
			continue
		}
		if !actionFound {
			a.logger.Warn("ReAct 响应缺少 Action 或 Final Answer", zap.Int("iteration", i+1))
			observe.Emit(ctx, observe.Event{Type: observe.TypeStatus, Stage: "react", Message: "模型未输出工具调用或最终答案，正在要求修正"})
			messages = append(messages,
				model.LLMMessage{Role: "assistant", Content: content},
				model.LLMMessage{Role: "user", Content: "你的回复不符合 ReAct 协议。若需工具，输出 Action: {\"tool\":\"工具名\",\"input\":{...}}；若已完成，必须输出 Final Answer: <最终答案>。"},
			)
			continue
		}

		// 记录思考步骤
		result.Steps = append(result.Steps, model.AgentStep{
			StepIndex: i + 1,
			Type:      "thought",
			Content:   a.extractThought(content),
			Timestamp: time.Now(),
		})

		// 执行工具调用
		observe.Emit(ctx, observe.Event{Type: observe.TypeToolCall, Stage: "tool", Tool: &model.ToolCallInfo{ToolName: action.Tool, Input: action.Input}})
		startTime := time.Now()
		toolResult, err := a.toolRouter.Execute(ctx, action.Tool, action.Input)
		elapsed := time.Since(startTime)

		var observation string
		if err != nil {
			observation = fmt.Sprintf("工具执行错误: %v", err)
		} else if !toolResult.Success {
			observation = fmt.Sprintf("工具返回错误: %s", toolResult.Error)
		} else {
			observation = toolResult.Output
		}

		// 记录工具调用
		callInfo := model.ToolCallInfo{
			ToolName: action.Tool,
			Input:    action.Input,
			Output:   observation,
			Duration: elapsed.Milliseconds(),
		}
		result.ToolCalls = append(result.ToolCalls, callInfo)
		observe.Emit(ctx, observe.Event{Type: observe.TypeToolResult, Stage: "tool", Message: observation, Tool: &callInfo})
		result.Steps = append(result.Steps, model.AgentStep{
			StepIndex:  i + 1,
			Type:       "action",
			ToolName:   action.Tool,
			ToolInput:  action.Input,
			ToolOutput: observation,
			Timestamp:  time.Now(),
		})

		// 将 LLM 回复和工具观测结果添加到消息历史
		messages = append(messages, model.LLMMessage{
			Role:    "assistant",
			Content: content,
		})
		messages = append(messages, model.LLMMessage{
			Role:    "user",
			Content: fmt.Sprintf("Observation: %s", observation),
		})
	}

	// 达到最大迭代次数，请求 LLM 给出总结
	messages = append(messages, model.LLMMessage{
		Role:    "user",
		Content: "你已达到最大推理步数，请根据已有信息给出最终答案。",
	})

	req := &model.LLMRequest{Messages: messages}
	resp, err := a.router.Chat(ctx, req)
	if err != nil {
		result.Answer = "抱歉，推理过程中出现错误，无法给出完整答案。"
		return result, nil
	}
	result.Answer = resp.Content

	return result, nil
}

// actionPayload Action 的 JSON 结构
type actionPayload struct {
	Tool  string
	Input string
}

// extractAction 从 LLM 响应中提取 Action（工具调用）
func (a *ReActAgent) extractAction(content string) *actionPayload {
	action, found, err := a.parseAction(content)
	if err != nil {
		a.logger.Warn("解析 Action JSON 失败", zap.Error(err))
		return nil
	}
	if !found {
		return nil
	}
	return action
}

// parseAction 区分“没有 Action”和“Action 格式错误”，防止格式错误时把
// Thought/Action 协议文本误当作最终答案返回给用户。
func (a *ReActAgent) parseAction(content string) (*actionPayload, bool, error) {
	// 查找 "Action:" 后的 JSON
	idx := strings.Index(content, "Action:")
	if idx == -1 {
		return nil, false, nil
	}

	actionStr := strings.TrimSpace(content[idx+len("Action:"):])
	start := strings.Index(actionStr, "{")
	if start == -1 {
		return nil, true, fmt.Errorf("Action 后缺少 JSON 对象")
	}

	var raw struct {
		Tool  string          `json:"tool"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.NewDecoder(strings.NewReader(actionStr[start:])).Decode(&raw); err != nil {
		return nil, true, fmt.Errorf("解析 Action JSON: %w", err)
	}
	if strings.TrimSpace(raw.Tool) == "" {
		return nil, true, fmt.Errorf("Action 缺少 tool")
	}

	input := strings.TrimSpace(string(raw.Input))
	if input == "" || input == "null" {
		input = "{}"
	} else if raw.Input[0] == '"' {
		if err := json.Unmarshal(raw.Input, &input); err != nil {
			return nil, true, fmt.Errorf("解析 Action input 字符串: %w", err)
		}
	} else {
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw.Input); err != nil {
			return nil, true, fmt.Errorf("解析 Action input: %w", err)
		}
		input = compact.String()
	}

	return &actionPayload{Tool: raw.Tool, Input: input}, true, nil
}

// extractFinalAnswer 从 LLM 响应中提取最终答案
func (a *ReActAgent) extractFinalAnswer(content string) string {
	idx := strings.Index(content, "Final Answer:")
	if idx == -1 {
		return ""
	}
	return strings.TrimSpace(content[idx+len("Final Answer:"):])
}

// extractThought 从 LLM 响应中提取思考内容
func (a *ReActAgent) extractThought(content string) string {
	idx := strings.Index(content, "Thought:")
	if idx == -1 {
		return content
	}

	thought := content[idx+len("Thought:"):]
	// 截断到 Action 之前
	if actionIdx := strings.Index(thought, "Action:"); actionIdx > 0 {
		thought = thought[:actionIdx]
	}
	return strings.TrimSpace(thought)
}

// buildToolsDescription 构建工具描述文本，嵌入到系统 Prompt 中
func (a *ReActAgent) buildToolsDescription() string {
	tools := a.toolRouter.ListAvailableToolDetails()
	if len(tools) == 0 {
		return "（无可用工具）"
	}

	var sb strings.Builder
	for _, availableTool := range tools {
		parameters, err := json.Marshal(availableTool.Parameters())
		if err != nil {
			parameters = []byte(`{"type":"object"}`)
		}
		sb.WriteString(fmt.Sprintf("- %s: %s\n  input JSON Schema: %s\n",
			availableTool.Name(), availableTool.Description(), parameters))
	}
	return sb.String()
}
