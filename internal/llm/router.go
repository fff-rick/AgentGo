package llm

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/metrics"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/trace"
	"github.com/enterprise/ai-agent-go/pkg/common"
	"go.opentelemetry.io/otel/attribute"
)

// Router 模型路由器。
// 根据请求参数和模型健康状态智能选择最合适的 LLM 客户端，
// 并通过熔断器保护每个模型的调用链路。
type Router struct {
	mu       sync.RWMutex
	clients  map[string]Client          // 模型名称 -> 客户端
	breakers map[string]*CircuitBreaker // 模型名称 -> 熔断器
	priority map[string]int             // 模型名称 -> 优先级，数值越小越优先
	logger   *zap.Logger
}

// NewRouter 创建模型路由器。
// 为每个注册的客户端自动创建独立的熔断器实例。
func NewRouter(clients map[string]Client, models []config.ModelConfig, cbCfg config.CBConfig) *Router {
	breakers := make(map[string]*CircuitBreaker, len(clients))
	priorities := make(map[string]int, len(models))
	for name := range clients {
		cb := NewCircuitBreaker(cbCfg.FailureThreshold, cbCfg.SuccessThreshold, cbCfg.Timeout)
		breakers[name] = cb
	}
	for _, model := range models {
		priorities[model.Name] = model.Priority
	}

	return &Router{
		clients:  clients,
		breakers: breakers,
		priority: priorities,
	}
}

// SetLogger 设置日志记录器
func (r *Router) SetLogger(logger *zap.Logger) {
	r.logger = logger
}

// Chat 通过路由选择模型并发送对话请求。
// 如果请求指定了模型则优先使用指定模型，否则按 priority 选择。
// 当目标模型熔断时按 priority 自动降级到其他可用模型。
func (r *Router) Chat(ctx context.Context, req *model.LLMRequest) (response *model.LLMResponse, callErr error) {
	ctx, span := trace.StartSpan(ctx, "llm.chat", attribute.Bool("llm.stream", false))
	defer func() { trace.Finish(span, callErr) }()
	client, err := r.selectClient(req.Model)
	if err != nil {
		return nil, err
	}

	modelName := client.Name()
	breaker := r.getBreaker(modelName)

	if !breaker.Allow() {
		// 主模型熔断，尝试降级
		fallback, fbErr := r.findFallback(modelName)
		if fbErr != nil {
			return nil, common.ErrCircuitOpen(modelName)
		}
		r.log("模型 %s 熔断，降级到 %s", modelName, fallback.Name())
		client = fallback
		breaker = r.getBreaker(fallback.Name())
	}

	modelName = client.Name()
	span.SetAttributes(attribute.String("llm.model", modelName))
	start := time.Now()
	resp, err := client.Chat(ctx, req)
	result := "success"
	if err != nil {
		result = "error"
	}
	metrics.Default.LLMRequests.WithLabelValues(modelName, "false", result).Inc()
	metrics.Default.LLMDuration.WithLabelValues(modelName, "false").Observe(metrics.Seconds(start))
	if err == nil && resp != nil && resp.Usage != nil {
		recordUsage(modelName, "false", resp.Usage)
	}
	if err != nil {
		metrics.Default.LLMErrors.WithLabelValues(modelName, "false").Inc()
		breaker.RecordFailure()
		return nil, common.WrapError(common.ErrCodeLLMFailed, "LLM 调用失败", err)
	}

	breaker.RecordSuccess()
	return resp, nil
}

// ChatStream 通过路由选择模型并发送流式对话请求
func (r *Router) ChatStream(ctx context.Context, req *model.LLMRequest) (<-chan StreamEvent, error) {
	ctx, span := trace.StartSpan(ctx, "llm.chat", attribute.Bool("llm.stream", true))
	client, err := r.selectClient(req.Model)
	if err != nil {
		trace.Finish(span, err)
		return nil, err
	}

	modelName := client.Name()
	breaker := r.getBreaker(modelName)

	if !breaker.Allow() {
		fallback, fbErr := r.findFallback(modelName)
		if fbErr != nil {
			trace.Finish(span, fbErr)
			return nil, common.ErrCircuitOpen(modelName)
		}
		r.log("模型 %s 熔断，降级到 %s", modelName, fallback.Name())
		client = fallback
		breaker = r.getBreaker(fallback.Name())
	}

	modelName = client.Name()
	span.SetAttributes(attribute.String("llm.model", modelName))
	start := time.Now()
	source, err := client.ChatStream(ctx, req)
	if err != nil {
		trace.Finish(span, err)
		metrics.Default.LLMRequests.WithLabelValues(modelName, "true", "error").Inc()
		metrics.Default.LLMDuration.WithLabelValues(modelName, "true").Observe(metrics.Seconds(start))
		metrics.Default.LLMErrors.WithLabelValues(modelName, "true").Inc()
		breaker.RecordFailure()
		return nil, common.WrapError(common.ErrCodeLLMFailed, "LLM 流式调用失败", err)
	}

	ch := make(chan StreamEvent, 32)
	go func() {
		defer close(ch)
		var streamErr error
		defer func() { trace.Finish(span, streamErr) }()
		failed := false
		first := false
		var usage *model.UsageInfo
		for event := range source {
			if !first && (event.Content != "" || event.Reasoning != "") {
				metrics.Default.LLMTTFT.WithLabelValues(modelName).Observe(metrics.Seconds(start))
				first = true
			}
			if event.Usage != nil {
				usage = event.Usage
			}
			if event.Err != nil && !failed {
				streamErr = event.Err
				breaker.RecordFailure()
				failed = true
			}
			ch <- event
		}
		result := "success"
		if failed {
			result = "error"
			metrics.Default.LLMErrors.WithLabelValues(modelName, "true").Inc()
		}
		metrics.Default.LLMRequests.WithLabelValues(modelName, "true", result).Inc()
		metrics.Default.LLMDuration.WithLabelValues(modelName, "true").Observe(metrics.Seconds(start))
		if usage != nil {
			span.SetAttributes(attribute.Int("llm.prompt_tokens", usage.PromptTokens), attribute.Int("llm.completion_tokens", usage.CompletionTokens))
			recordUsage(modelName, "true", usage)
		}
		if !failed {
			breaker.RecordSuccess()
		}
	}()
	return ch, nil
}

func recordUsage(modelName, stream string, usage *model.UsageInfo) {
	metrics.Default.LLMUsageReports.WithLabelValues(modelName, stream).Inc()
	metrics.Default.LLMPromptTokens.WithLabelValues(modelName).Add(float64(usage.PromptTokens))
	metrics.Default.LLMCompletionTokens.WithLabelValues(modelName).Add(float64(usage.CompletionTokens))
}

// ListModels 返回所有已注册模型的名称和健康状态
func (r *Router) ListModels() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[string]string, len(r.clients))
	for name := range r.clients {
		if breaker, ok := r.breakers[name]; ok {
			result[name] = breaker.State().String()
		} else {
			result[name] = "UNKNOWN"
		}
	}
	return result
}

// selectClient 根据模型名称选择客户端
func (r *Router) selectClient(modelName string) (Client, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if modelName != "" {
		client, ok := r.clients[modelName]
		if !ok {
			return nil, fmt.Errorf("未注册的模型: %s", modelName)
		}
		return client, nil
	}
	for _, name := range r.orderedModels("") {
		return r.clients[name], nil
	}
	return nil, fmt.Errorf("未注册任何模型")
}

// findFallback 在主模型不可用时寻找备选模型。
// 按 priority（同优先级按名称）选择第一个未熔断的模型。
func (r *Router) findFallback(excludeModel string) (Client, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, name := range r.orderedModels(excludeModel) {
		if breaker, ok := r.breakers[name]; ok && breaker.Allow() {
			return r.clients[name], nil
		}
	}

	return nil, fmt.Errorf("所有模型均不可用")
}

func (r *Router) orderedModels(excludeModel string) []string {
	names := make([]string, 0, len(r.clients))
	for name := range r.clients {
		if name != excludeModel {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		left, right := r.priority[names[i]], r.priority[names[j]]
		if left == right {
			return names[i] < names[j]
		}
		return left < right
	})
	return names
}

// getBreaker 获取指定模型的熔断器
func (r *Router) getBreaker(name string) *CircuitBreaker {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if b, ok := r.breakers[name]; ok {
		return b
	}
	// 兜底：返回一个始终允许的默认熔断器
	return NewCircuitBreaker(100, 1, 0)
}

// log 输出日志（如果 logger 已设置）
func (r *Router) log(format string, args ...interface{}) {
	if r.logger != nil {
		r.logger.Info(fmt.Sprintf(format, args...))
	}
}
