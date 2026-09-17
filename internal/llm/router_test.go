package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/metrics"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

type routeClient struct {
	name           string
	err            error
	streamEventErr error
	streamEvents   []StreamEvent
	calls          int
}

type blockingTraceClient struct{ events chan StreamEvent }

func (*blockingTraceClient) Chat(context.Context, *model.LLMRequest) (*model.LLMResponse, error) {
	return nil, nil
}
func (c *blockingTraceClient) ChatStream(context.Context, *model.LLMRequest) (<-chan StreamEvent, error) {
	return c.events, nil
}
func (*blockingTraceClient) Name() string                 { return "trace-stream-test" }
func (*blockingTraceClient) Healthy(context.Context) bool { return true }

func TestStreamSpanClosesAfterEventsAndMarksError(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(oteltrace.NewNoopTracerProvider())
	})
	client := &blockingTraceClient{events: make(chan StreamEvent)}
	router := NewRouter(map[string]Client{client.Name(): client}, []config.ModelConfig{{Name: client.Name()}}, config.CBConfig{FailureThreshold: 3, SuccessThreshold: 1, Timeout: time.Hour})
	out, err := router.ChatStream(context.Background(), &model.LLMRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(exporter.GetSpans()) != 0 {
		t.Fatal("stream span ended before stream completion")
	}
	client.events <- StreamEvent{Content: "sensitive answer"}
	client.events <- StreamEvent{Err: errors.New("sensitive provider error")}
	close(client.events)
	for range out {
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 || spans[0].Name != "llm.chat" || spans[0].Status.Code != codes.Error {
		t.Fatalf("spans=%+v", spans)
	}
	for _, attr := range spans[0].Attributes {
		if attr.Value.AsString() == "sensitive answer" || attr.Value.AsString() == "sensitive provider error" {
			t.Fatal("payload leaked into span")
		}
	}
}

func (c *routeClient) Chat(context.Context, *model.LLMRequest) (*model.LLMResponse, error) {
	c.calls++
	return &model.LLMResponse{Content: c.name}, c.err
}

func (c *routeClient) ChatStream(context.Context, *model.LLMRequest) (<-chan StreamEvent, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	stream := make(chan StreamEvent, len(c.streamEvents)+1)
	if c.streamEventErr != nil {
		stream <- StreamEvent{Err: c.streamEventErr}
	} else if len(c.streamEvents) > 0 {
		for _, event := range c.streamEvents {
			stream <- event
		}
	} else {
		stream <- StreamEvent{Done: true}
	}
	close(stream)
	return stream, nil
}

func TestRouterStreamingMetricsUseOnlyReportedUsage(t *testing.T) {
	client := &routeClient{name: "metrics-stream-test", streamEvents: []StreamEvent{{Content: "hello"}, {Done: true}}}
	router := NewRouter(map[string]Client{client.name: client}, []config.ModelConfig{{Name: client.name}}, config.CBConfig{FailureThreshold: 3, SuccessThreshold: 1, Timeout: time.Hour})
	stream, err := router.ChatStream(context.Background(), &model.LLMRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if got := testutil.ToFloat64(metrics.Default.LLMRequests.WithLabelValues(client.name, "true", "success")); got != 1 {
		t.Fatalf("requests=%v", got)
	}
	if got := testutil.ToFloat64(metrics.Default.LLMUsageReports.WithLabelValues(client.name, "true")); got != 0 {
		t.Fatalf("unreported usage=%v", got)
	}
	client.streamEvents = []StreamEvent{{Reasoning: "thinking"}, {Usage: &model.UsageInfo{PromptTokens: 12, CompletionTokens: 3}}, {Done: true}}
	stream, err = router.ChatStream(context.Background(), &model.LLMRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if got := testutil.ToFloat64(metrics.Default.LLMUsageReports.WithLabelValues(client.name, "true")); got != 1 {
		t.Fatalf("reported usage=%v", got)
	}
	if got := testutil.ToFloat64(metrics.Default.LLMPromptTokens.WithLabelValues(client.name)); got != 12 {
		t.Fatalf("prompt tokens=%v", got)
	}
}

func TestRouterRecordsStreamingFailure(t *testing.T) {
	gpt := &routeClient{name: "gpt", streamEventErr: errors.New("stream failed")}
	qwen := &routeClient{name: "qwen"}
	router := NewRouter(
		map[string]Client{"gpt": gpt, "qwen": qwen},
		[]config.ModelConfig{{Name: "gpt", Priority: 1}, {Name: "qwen", Priority: 10}},
		config.CBConfig{FailureThreshold: 1, SuccessThreshold: 1, Timeout: time.Hour},
	)

	stream, err := router.ChatStream(context.Background(), &model.LLMRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	gpt.streamEventErr = nil
	stream, err = router.ChatStream(context.Background(), &model.LLMRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if gpt.calls != 1 || qwen.calls != 1 || router.breakers["gpt"].State() != StateOpen {
		t.Fatalf("calls: gpt=%d qwen=%d, states=%v", gpt.calls, qwen.calls, router.ListModels())
	}
}

func (c *routeClient) Name() string                 { return c.name }
func (c *routeClient) Healthy(context.Context) bool { return c.err == nil }

func TestRouterUsesPriorityExplicitModelAndCircuitFallback(t *testing.T) {
	gpt, qwen := &routeClient{name: "gpt"}, &routeClient{name: "qwen"}
	router := NewRouter(
		map[string]Client{"gpt": gpt, "qwen": qwen},
		[]config.ModelConfig{{Name: "qwen", Priority: 10}, {Name: "gpt", Priority: 1}},
		config.CBConfig{FailureThreshold: 1, SuccessThreshold: 1, Timeout: time.Hour},
	)

	if response, err := router.Chat(context.Background(), &model.LLMRequest{}); err != nil || response.Content != "gpt" {
		t.Fatalf("priority route: response=%+v err=%v", response, err)
	}
	if response, err := router.Chat(context.Background(), &model.LLMRequest{Model: "qwen"}); err != nil || response.Content != "qwen" {
		t.Fatalf("explicit route: response=%+v err=%v", response, err)
	}

	gpt.err = errors.New("gpt unavailable")
	if _, err := router.Chat(context.Background(), &model.LLMRequest{}); err == nil {
		t.Fatal("expected first GPT failure to open its circuit")
	}
	gpt.err = nil
	if response, err := router.Chat(context.Background(), &model.LLMRequest{}); err != nil || response.Content != "qwen" {
		t.Fatalf("circuit fallback: response=%+v err=%v", response, err)
	}
}
