package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http/httptest"
)

func TestIndependentRegistryAndMetricFamilies(t *testing.T) {
	m := New()
	m.AgentRequests.WithLabelValues("agent", "success").Inc()
	m.ToolCalls.WithLabelValues("calculator", "error").Inc()
	m.LLMTTFT.WithLabelValues("test-model").Observe(.2)
	m.ContextCompressionRatio.Observe(.5)
	m.DependencyDuration.WithLabelValues("redis", "get").Observe(time.Second.Seconds())
	r := httptest.NewRecorder()
	promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}).ServeHTTP(r, httptest.NewRequest("GET", "/metrics", nil))
	if r.Code != 200 {
		t.Fatalf("status=%d: %s", r.Code, r.Body.String())
	}
	for _, want := range []string{
		`agentgo_agent_requests_total{mode="agent",result="success"} 1`,
		`agentgo_tool_calls_total{result="error",tool="calculator"} 1`,
		`agentgo_llm_ttft_seconds_count{model="test-model"} 1`,
		`agentgo_context_compression_ratio_count 1`,
		`agentgo_dependency_duration_seconds_count{dependency="redis",operation="get"} 1`,
		`go_goroutines`, `process_resident_memory_bytes`,
	} {
		if !strings.Contains(r.Body.String(), want) {
			t.Errorf("missing %q", want)
		}
	}
}
