// Package metrics owns AgentGo's Prometheus collectors. Labels are deliberately bounded.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type Metrics struct {
	Registry                *prometheus.Registry
	HTTPRequests            *prometheus.CounterVec
	HTTPDuration            *prometheus.HistogramVec
	HTTPInflight            prometheus.Gauge
	HTTPErrors              *prometheus.CounterVec
	AgentRequests           *prometheus.CounterVec
	AgentDuration           *prometheus.HistogramVec
	AgentIterations         *prometheus.HistogramVec
	ToolCalls               *prometheus.CounterVec
	ToolDuration            *prometheus.HistogramVec
	ToolErrors              *prometheus.CounterVec
	PlannerSteps            prometheus.Histogram
	PlannerDuration         prometheus.Histogram
	LLMRequests             *prometheus.CounterVec
	LLMDuration             *prometheus.HistogramVec
	LLMTTFT                 *prometheus.HistogramVec
	LLMPromptTokens         *prometheus.CounterVec
	LLMCompletionTokens     *prometheus.CounterVec
	LLMUsageReports         *prometheus.CounterVec
	LLMErrors               *prometheus.CounterVec
	RAGRequests             prometheus.Counter
	RAGRetrievalDuration    prometheus.Histogram
	RAGResults              prometheus.Histogram
	RAGRerankDuration       prometheus.Histogram
	EmbeddingDuration       *prometheus.HistogramVec
	MemoryLoadDuration      *prometheus.HistogramVec
	MemoryWriteDuration     *prometheus.HistogramVec
	MemoryRetrieval         prometheus.Counter
	MemoryRetrievalHits     prometheus.Counter
	ContextTokens           prometheus.Histogram
	ContextCompressionRatio prometheus.Histogram
	DependencyDuration      *prometheus.HistogramVec
}

func New() *Metrics {
	r := prometheus.NewRegistry()
	r.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	f := promauto.With(r)
	seconds := []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 180, 300}
	hist := func(name, help string, labels ...string) *prometheus.HistogramVec {
		return f.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: seconds}, labels)
	}
	count := func(name, help string, labels ...string) *prometheus.CounterVec {
		return f.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
	}
	return &Metrics{
		Registry:                r,
		HTTPRequests:            count("agentgo_http_requests_total", "HTTP requests", "method", "route", "status"),
		HTTPDuration:            hist("agentgo_http_request_duration_seconds", "HTTP request duration", "method", "route"),
		HTTPInflight:            f.NewGauge(prometheus.GaugeOpts{Name: "agentgo_http_requests_inflight", Help: "HTTP requests in flight"}),
		HTTPErrors:              count("agentgo_http_errors_total", "HTTP 4xx and 5xx responses", "method", "route", "status"),
		AgentRequests:           count("agentgo_agent_requests_total", "Agent runs", "mode", "result"),
		AgentDuration:           hist("agentgo_agent_duration_seconds", "Agent run duration", "mode"),
		AgentIterations:         f.NewHistogramVec(prometheus.HistogramOpts{Name: "agentgo_agent_iterations", Help: "Business tool rounds per agent run", Buckets: prometheus.LinearBuckets(0, 1, 16)}, []string{"mode"}),
		ToolCalls:               count("agentgo_tool_calls_total", "Tool calls", "tool", "result"),
		ToolDuration:            hist("agentgo_tool_duration_seconds", "Tool call duration", "tool"),
		ToolErrors:              count("agentgo_tool_errors_total", "Failed tool calls", "tool"),
		PlannerSteps:            f.NewHistogram(prometheus.HistogramOpts{Name: "agentgo_planner_steps", Help: "Executed planner steps", Buckets: prometheus.LinearBuckets(0, 1, 16)}),
		PlannerDuration:         f.NewHistogram(prometheus.HistogramOpts{Name: "agentgo_planner_duration_seconds", Help: "Planner execution duration", Buckets: seconds}),
		LLMRequests:             count("agentgo_llm_requests_total", "LLM calls", "model", "stream", "result"),
		LLMDuration:             hist("agentgo_llm_duration_seconds", "LLM call duration", "model", "stream"),
		LLMTTFT:                 hist("agentgo_llm_ttft_seconds", "Time to first streamed content or reasoning", "model"),
		LLMPromptTokens:         count("agentgo_llm_prompt_tokens_total", "Provider reported prompt tokens", "model"),
		LLMCompletionTokens:     count("agentgo_llm_completion_tokens_total", "Provider reported completion tokens", "model"),
		LLMUsageReports:         count("agentgo_llm_usage_reports_total", "LLM calls with provider reported usage", "model", "stream"),
		LLMErrors:               count("agentgo_llm_errors_total", "Failed LLM calls", "model", "stream"),
		RAGRequests:             f.NewCounter(prometheus.CounterOpts{Name: "agentgo_rag_requests_total", Help: "RAG search requests"}),
		RAGRetrievalDuration:    f.NewHistogram(prometheus.HistogramOpts{Name: "agentgo_rag_retrieval_duration_seconds", Help: "RAG retrieval duration", Buckets: seconds}),
		RAGResults:              f.NewHistogram(prometheus.HistogramOpts{Name: "agentgo_rag_results_count", Help: "Final RAG result count", Buckets: prometheus.LinearBuckets(0, 1, 21)}),
		RAGRerankDuration:       f.NewHistogram(prometheus.HistogramOpts{Name: "agentgo_rag_rerank_duration_seconds", Help: "RAG rerank duration", Buckets: seconds}),
		EmbeddingDuration:       hist("agentgo_embedding_duration_seconds", "Embedding request duration", "operation"),
		MemoryLoadDuration:      hist("agentgo_memory_load_duration_seconds", "Memory load duration", "operation"),
		MemoryWriteDuration:     hist("agentgo_memory_write_duration_seconds", "Memory write duration", "operation"),
		MemoryRetrieval:         f.NewCounter(prometheus.CounterOpts{Name: "agentgo_memory_retrieval_total", Help: "Semantic memory searches"}),
		MemoryRetrievalHits:     f.NewCounter(prometheus.CounterOpts{Name: "agentgo_memory_retrieval_hits_total", Help: "Semantic memory searches with results"}),
		ContextTokens:           f.NewHistogram(prometheus.HistogramOpts{Name: "agentgo_context_tokens", Help: "Estimated context tokens", Buckets: []float64{500, 1000, 2000, 4000, 8000, 12000, 16000, 24000, 32000, 64000}}),
		ContextCompressionRatio: f.NewHistogram(prometheus.HistogramOpts{Name: "agentgo_context_compression_ratio", Help: "Estimated tokens after divided by before compression", Buckets: prometheus.LinearBuckets(0, .1, 11)}),
		DependencyDuration:      hist("agentgo_dependency_duration_seconds", "Key dependency call duration", "dependency", "operation"),
	}
}

var Default = New()

func Seconds(start time.Time) float64 { return time.Since(start).Seconds() }
