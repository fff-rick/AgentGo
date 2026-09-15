package agent

import (
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

const plannerSystemPrompt = `你是一个任务规划专家。请将用户的复杂任务分解为可执行的子步骤。

每个步骤应包含：
1. step: 步骤编号
2. description: 步骤描述
3. tool: 需要使用的工具（如不需要工具则为空）
4. input: 工具输入参数；必须是符合该工具 parameters JSON Schema 的 JSON 对象
5. depends_on: 依赖的前置步骤编号列表

请以 JSON 数组格式返回执行计划：
[{"step": 1, "description": "...", "tool": "...", "input": {"参数名": "参数值"}, "depends_on": []}]

可用工具定义（JSON）：%s`

const toolSelectionPrompt = `你负责为任务选择工具。根据任务、历史上下文和工具目录，选择完成任务可能需要的工具。
最多选择 %d 个，只返回 JSON：{"tools":["tool_name"]}；不需要工具时返回 {"tools":[]}。
工具目录（不含参数 Schema）：%s`

// Plan 执行计划
type Plan struct {
	Steps []PlanStep `json:"steps"`
}

// PlanStep 计划中的单个步骤
type PlanStep struct {
	Step        int             `json:"step"`
	Description string          `json:"description"`
	Tool        string          `json:"tool"`
	Input       json.RawMessage `json:"input"`
	DependsOn   []int           `json:"depends_on"`
}

func (s PlanStep) toolInput() string {
	var legacy string
	if json.Unmarshal(s.Input, &legacy) == nil {
		return legacy
	}
	return string(s.Input)
}

// PlannerAgent 规划型 Agent。
// 先将复杂任务分解为子步骤计划，然后按照依赖关系逐步执行。
type PlannerAgent struct {
	router     *llm.Router
	toolRouter *tool.Router
	logger     *zap.Logger
}

// NewPlannerAgent 创建规划 Agent
func NewPlannerAgent(router *llm.Router, toolRouter *tool.Router, logger *zap.Logger) *PlannerAgent {
	return &PlannerAgent{
		router:     router,
		toolRouter: toolRouter,
		logger:     logger,
	}
}

// Execute 执行复杂任务：生成计划 → 逐步执行 → 汇总结果
func (p *PlannerAgent) Execute(ctx context.Context, task string, history []model.LLMMessage, scope tool.Scope) (*AgentResult, error) {
	definitions, err := p.planToolDefinitions(ctx, task, history, scope)
	if err != nil {
		return nil, fmt.Errorf("选择规划工具失败: %w", err)
	}
	// 阶段一：生成执行计划
	plan, err := p.generatePlan(ctx, task, history, definitions)
	if err != nil {
		return nil, fmt.Errorf("生成执行计划失败: %w", err)
	}

	p.logger.Info("执行计划已生成", zap.Int("steps", len(plan.Steps)))

	// 阶段二：按步骤执行计划
	result := &AgentResult{}
	stepResults := make(map[int]string) // 步骤编号 -> 执行结果
	selected := make(map[string]struct{}, len(definitions))
	for _, definition := range definitions {
		selected[definition.Function.Name] = struct{}{}
	}

	for _, step := range plan.Steps {
		// 检查依赖是否满足
		for _, dep := range step.DependsOn {
			if _, ok := stepResults[dep]; !ok {
				return nil, fmt.Errorf("步骤 %d 的依赖 %d 未执行", step.Step, dep)
			}
		}

		p.logger.Info("执行计划步骤",
			zap.Int("step", step.Step),
			zap.String("description", step.Description),
		)

		input := step.toolInput()
		var output string
		if step.Tool != "" {
			if _, ok := selected[step.Tool]; !ok {
				return nil, fmt.Errorf("计划引用了未选择的工具 %q", step.Tool)
			}
			// 需要使用工具
			observe.Emit(ctx, observe.Event{Type: observe.TypeToolCall, Stage: "tool", Tool: &model.ToolCallInfo{ToolName: step.Tool, Input: input}})
			startTime := time.Now()
			toolResult, err := p.toolRouter.ExecuteScoped(ctx, scope, step.Tool, input)
			elapsed := time.Since(startTime)

			if err != nil {
				output = fmt.Sprintf("错误: %v", err)
			} else {
				output = toolResult.Output
				result.References = append(result.References, toolResult.References...)
			}

			callInfo := model.ToolCallInfo{
				ToolName: step.Tool,
				Input:    input,
				Output:   output,
				Duration: elapsed.Milliseconds(),
			}
			result.ToolCalls = append(result.ToolCalls, callInfo)
			observe.Emit(ctx, observe.Event{Type: observe.TypeToolResult, Stage: "tool", Message: output, Tool: &callInfo})
		} else {
			output = step.Description
		}

		stepResults[step.Step] = output
		result.Steps = append(result.Steps, model.AgentStep{
			StepIndex:  step.Step,
			Type:       "action",
			Content:    step.Description,
			ToolName:   step.Tool,
			ToolInput:  input,
			ToolOutput: output,
			Timestamp:  time.Now(),
		})
	}

	// 阶段三：汇总所有步骤结果，生成最终答案
	answer, err := p.summarize(ctx, task, stepResults)
	if err != nil {
		return nil, err
	}
	result.Answer = answer

	return result, nil
}

// generatePlan 调用 LLM 生成执行计划
func (p *PlannerAgent) generatePlan(ctx context.Context, task string, history []model.LLMMessage, tools []model.ToolDef) (*Plan, error) {
	definitions := make([]model.FunctionDef, 0, len(tools))
	for _, candidate := range tools {
		definitions = append(definitions, candidate.Function)
	}
	toolJSON, err := json.Marshal(definitions)
	if err != nil {
		return nil, fmt.Errorf("序列化工具定义失败: %w", err)
	}
	systemPrompt := fmt.Sprintf(plannerSystemPrompt, toolJSON)

	messages := []model.LLMMessage{{Role: "system", Content: systemPrompt}}
	messages = append(messages, history...)
	messages = append(messages, model.LLMMessage{Role: "user", Content: task})
	req := &model.LLMRequest{Messages: messages, Temperature: 0.2}

	resp, err := p.router.Chat(ctx, req)
	if err != nil {
		return nil, err
	}

	var steps []PlanStep
	if err := json.Unmarshal([]byte(resp.Content), &steps); err != nil {
		return nil, fmt.Errorf("解析执行计划失败: %w", err)
	}

	return &Plan{Steps: steps}, nil
}

func (p *PlannerAgent) planToolDefinitions(ctx context.Context, task string, history []model.LLMMessage, scope tool.Scope) ([]model.ToolDef, error) {
	if !p.toolRouter.LazyLoadingEnabled() {
		return p.toolRouter.InitialToolDefinitions(ctx, scope), nil
	}
	catalog := p.toolRouter.ToolCatalog(ctx, scope)
	if len(catalog) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(catalog)
	if err != nil {
		return nil, err
	}
	messages := []model.LLMMessage{{Role: "system", Content: fmt.Sprintf(toolSelectionPrompt, p.toolRouter.LazyLoadThreshold(), encoded)}}
	messages = append(messages, history...)
	messages = append(messages, model.LLMMessage{Role: "user", Content: task})
	resp, err := p.router.Chat(ctx, &model.LLMRequest{Messages: messages, Temperature: 0.1})
	if err != nil {
		return nil, err
	}
	var selection struct {
		Tools []string `json:"tools"`
	}
	if err := json.Unmarshal([]byte(resp.Content), &selection); err != nil {
		return nil, fmt.Errorf("解析工具选择失败: %w", err)
	}
	if len(selection.Tools) > p.toolRouter.LazyLoadThreshold() {
		return nil, fmt.Errorf("选择工具数量 %d 超过上限 %d", len(selection.Tools), p.toolRouter.LazyLoadThreshold())
	}
	available := make(map[string]struct{}, len(catalog))
	for _, candidate := range catalog {
		available[candidate.Name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(selection.Tools))
	for _, name := range selection.Tools {
		if _, ok := available[name]; !ok {
			return nil, fmt.Errorf("选择了目录外工具 %q", name)
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("重复选择工具 %q", name)
		}
		seen[name] = struct{}{}
	}
	if len(selection.Tools) == 0 {
		return nil, nil
	}
	loaded, err := p.toolRouter.LoadTools(ctx, scope, selection.Tools)
	if err != nil {
		return nil, err
	}
	if !loaded.Success {
		return nil, fmt.Errorf("加载工具失败: %s", loaded.Error)
	}
	selectedDefinitions := make([]model.ToolDef, 0, len(selection.Tools))
	for _, definition := range loaded.ToolDefinitions {
		if _, ok := seen[definition.Function.Name]; ok {
			selectedDefinitions = append(selectedDefinitions, definition)
		}
	}
	if len(selectedDefinitions) != len(selection.Tools) {
		return nil, fmt.Errorf("加载工具不完整: 选择 %d 个，获得 %d 个", len(selection.Tools), len(selectedDefinitions))
	}
	return selectedDefinitions, nil
}

// summarize 汇总步骤执行结果，生成最终答案
func (p *PlannerAgent) summarize(ctx context.Context, task string, stepResults map[int]string) (string, error) {
	var resultSummary strings.Builder
	for step, output := range stepResults {
		resultSummary.WriteString(fmt.Sprintf("步骤 %d 结果: %s\n", step, output))
	}

	req := &model.LLMRequest{
		Messages: []model.LLMMessage{
			{
				Role:    "system",
				Content: "请根据各步骤的执行结果，为用户的原始任务生成一个完整、清晰的最终回答。",
			},
			{
				Role:    "user",
				Content: fmt.Sprintf("原始任务: %s\n\n执行结果:\n%s", task, resultSummary.String()),
			},
		},
	}

	resp, err := p.router.Chat(ctx, req)
	if err != nil {
		return "", fmt.Errorf("汇总结果失败: %w", err)
	}

	return resp.Content, nil
}
