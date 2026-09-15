package tool

import (
	"context"
	"fmt"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/pkg/common"
)

// Router 工具路由器。
// 根据工具名称从注册中心获取工具并执行，提供超时控制和错误处理。
type Router struct {
	registry *Registry
	manager  *Manager
	logger   *zap.Logger
	timeout  time.Duration // 默认工具执行超时
}

// NewRouter 创建工具路由器
func NewRouter(registry *Registry, logger *zap.Logger, managers ...*Manager) *Router {
	router := &Router{
		registry: registry,
		logger:   logger,
		timeout:  30 * time.Second,
	}
	if len(managers) > 0 {
		router.manager = managers[0]
	}
	return router
}

// Execute 根据工具名称路由并执行工具调用。
// 自动添加超时控制，记录执行耗时。
func (r *Router) Execute(ctx context.Context, toolName, input string) (*ToolResult, error) {
	return r.ExecuteScoped(ctx, Scope{}, toolName, input)
}

func (r *Router) ExecuteScoped(ctx context.Context, scope Scope, toolName, input string) (*ToolResult, error) {
	if toolName == ListToolsName {
		if r.manager == nil {
			return nil, common.ErrToolNotFound(toolName)
		}
		return r.manager.Handle(ctx, scope, input)
	}
	if !scope.Allows(toolName) {
		return nil, common.ErrToolNotFound(toolName)
	}
	t, ok := r.registry.Get(toolName)
	if !ok {
		return nil, common.ErrToolNotFound(toolName)
	}

	// 使用带超时的子 Context
	execCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	start := time.Now()
	if r.manager != nil {
		r.manager.RecordUse(ctx, scope, toolName)
	}
	r.logger.Info("开始执行工具",
		zap.String("tool", toolName),
		zap.String("input", truncate(input, 200)),
	)

	result, err := t.Execute(execCtx, input)
	elapsed := time.Since(start)

	if err != nil {
		r.logger.Error("工具执行失败",
			zap.String("tool", toolName),
			zap.Duration("elapsed", elapsed),
			zap.Error(err),
		)
		return nil, common.WrapError(common.ErrCodeToolFailed,
			fmt.Sprintf("工具 %s 执行失败", toolName), err)
	}

	r.logger.Info("工具执行完成",
		zap.String("tool", toolName),
		zap.Duration("elapsed", elapsed),
		zap.Bool("success", result.Success),
	)

	return result, nil
}

// BatchExecute 并发执行多个工具调用。
// 使用 goroutine 并发执行，通过 channel 收集结果。
func (r *Router) BatchExecute(ctx context.Context, calls []ToolCall) []*ToolCallResult {
	return r.BatchExecuteScoped(ctx, Scope{}, calls)
}

func (r *Router) BatchExecuteScoped(ctx context.Context, scope Scope, calls []ToolCall) []*ToolCallResult {
	results := make([]*ToolCallResult, len(calls))
	ch := make(chan indexedResult, len(calls))

	for i, call := range calls {
		go func(idx int, c ToolCall) {
			start := time.Now()
			result, err := r.ExecuteScoped(ctx, scope, c.Name, c.Input)
			ch <- indexedResult{Index: idx, Result: result, Err: err, Duration: time.Since(start)}
		}(i, call)
	}

	for range calls {
		ir := <-ch
		results[ir.Index] = &ToolCallResult{
			ToolName: calls[ir.Index].Name,
			Result:   ir.Result,
			Err:      ir.Err,
			Duration: ir.Duration,
		}
	}

	return results
}

func (r *Router) ValidateAllowedTools(names []string) error {
	if r.manager != nil {
		return r.manager.ValidateAllowed(names)
	}
	for _, name := range names {
		if name == ListToolsName {
			return fmt.Errorf("%s 是保留工具名", ListToolsName)
		}
		if _, ok := r.registry.Get(name); !ok {
			return fmt.Errorf("工具 %q 未注册", name)
		}
	}
	return nil
}

func (r *Router) InitialToolDefinitions(ctx context.Context, scope Scope) []model.ToolDef {
	if r.manager != nil {
		return r.manager.InitialDefinitions(ctx, scope)
	}
	definitions := r.ListToolDefinitions()
	if !scope.Restricted {
		return definitions
	}
	filtered := definitions[:0]
	for _, candidate := range definitions {
		if scope.Allows(candidate.Function.Name) {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

func (r *Router) ToolCatalog(ctx context.Context, scope Scope) []CatalogEntry {
	if r.manager == nil {
		return nil
	}
	return r.manager.Catalog(ctx, scope)
}

func (r *Router) LoadTools(ctx context.Context, scope Scope, names []string) (*ToolResult, error) {
	if r.manager == nil {
		return nil, fmt.Errorf("Lazy Tool Manager 未配置")
	}
	return r.manager.Load(ctx, scope, names)
}

func (r *Router) LazyLoadingEnabled() bool { return r.manager != nil }

func (r *Router) LazyLoadThreshold() int {
	if r.manager == nil {
		return len(r.registry.List())
	}
	return r.manager.Threshold()
}

// ListAvailableTools 返回注册中心中所有可用工具的名称
func (r *Router) ListAvailableTools() []string {
	names := r.registry.List()
	sort.Strings(names)
	return names
}

// ListAvailableToolDetails 返回按名称排序的工具定义，供 Agent 构造稳定且
// 包含参数 Schema 的工具提示词。
func (r *Router) ListAvailableToolDetails() []Tool {
	tools := r.registry.ListTools()
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name() < tools[j].Name() })
	return tools
}

// ListToolDefinitions returns the stable function definitions supplied to an AgentLoop.
func (r *Router) ListToolDefinitions() []model.ToolDef {
	available := r.ListAvailableToolDetails()
	definitions := make([]model.ToolDef, 0, len(available))
	for _, candidate := range available {
		definitions = append(definitions, model.ToolDef{Type: "function", Function: model.FunctionDef{
			Name: candidate.Name(), Description: candidate.Description(), Parameters: candidate.Parameters(),
		}})
	}
	return definitions
}

// ToolCall 工具调用请求
type ToolCall struct {
	Name  string
	Input string
}

// ToolCallResult 工具调用结果（含工具名）
type ToolCallResult struct {
	ToolName string
	Result   *ToolResult
	Err      error
	Duration time.Duration
}

type indexedResult struct {
	Index    int
	Result   *ToolResult
	Err      error
	Duration time.Duration
}

// truncate 截断字符串，超过 maxLen 的部分用省略号代替
func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}
