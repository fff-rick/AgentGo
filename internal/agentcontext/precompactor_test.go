package agentcontext

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/memory"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/trace"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

type precompactSessions struct {
	*sessionStub
	mu        sync.Mutex
	summaries map[string]memory.SessionSummary
}

func (s *precompactSessions) GetSummary(_ context.Context, sessionID string) (*memory.SessionSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	summary, ok := s.summaries[sessionID]
	if !ok {
		return nil, nil
	}
	return &summary, nil
}

func (s *precompactSessions) SaveSummary(_ context.Context, session *model.Session, expected *memory.SessionSummary, next memory.SessionSummary) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.summaries[session.ID]
	if exists != (expected != nil) || exists && current != *expected {
		return false, nil
	}
	s.summaries[session.ID] = next
	return true, nil
}

type waitingCompactor struct {
	started chan string
	release <-chan struct{}
	calls   atomic.Int32
}

func (*waitingCompactor) ShouldCompact(input ContextBudgetInput) bool {
	return input.EstimatedTokens > input.MaxTokens
}

func (c *waitingCompactor) Compact(ctx context.Context, input CompactionInput) (*CompactionResult, error) {
	c.calls.Add(1)
	c.started <- input.Messages[0].SessionID
	select {
	case <-c.release:
		return &CompactionResult{Summary: "compressed"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func newPrecompactTestBuilder(compactor Compactor) (*ContextBuilder, *precompactSessions) {
	sessions := &precompactSessions{sessionStub: &sessionStub{}, summaries: make(map[string]memory.SessionSummary)}
	for sequence := int64(1); sequence <= 25; sequence++ {
		sessions.messages = append(sessions.messages, model.Message{SessionID: "session", Sequence: sequence, Role: "user", Content: "message"})
	}
	return NewBuilder(sessions, semanticMemoryStub{}, compactor, config.ContextConfig{MaxInputTokens: 100, RecentMessages: 20, SummaryMaxTokens: 20}, 5, zap.NewNop()), sessions
}

func TestPrecompactorStartsLinkedTrace(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(oteltrace.NewNoopTracerProvider())
	})
	release := make(chan struct{})
	compactor := &waitingCompactor{started: make(chan string, 1), release: release}
	builder, _ := newPrecompactTestBuilder(compactor)
	p := NewPrecompactor(builder)
	requestCtx, requestSpan := trace.StartServerSpan(context.Background(), "chat")
	if !p.SubmitWithContext(requestCtx, &model.Session{ID: "session", UserID: "user-1"}, 70, 25, "answer") {
		t.Fatal("job not accepted")
	}
	requestSpan.End()
	select {
	case <-compactor.started:
	case <-time.After(time.Second):
		t.Fatal("job not started")
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	var linked bool
	for _, span := range exporter.GetSpans() {
		if span.Name == "memory.precompact" {
			linked = len(span.Links) == 1 && span.Links[0].SpanContext.SpanID() == requestSpan.SpanContext().SpanID() && span.SpanContext.TraceID() != requestSpan.SpanContext().TraceID()
		}
	}
	if !linked {
		t.Fatal("precompaction trace is not linked to request")
	}
}

func TestPrecompactorThresholdDedupAndNextBuild(t *testing.T) {
	release := make(chan struct{})
	compactor := &waitingCompactor{started: make(chan string, 20), release: release}
	builder, sessions := newPrecompactTestBuilder(compactor)
	p := NewPrecompactor(builder)
	session := &model.Session{ID: "session", UserID: "user-1"}
	if p.Submit(session, 60, 25, "short") || p.Submit(session, 70, 18, "short") {
		t.Fatal("below threshold or without old messages must not submit")
	}
	if !p.Submit(session, 70, 25, "answer") || p.Submit(session, 70, 25, "answer") {
		t.Fatal("expected one pending job per session")
	}
	select {
	case <-compactor.started:
	case <-time.After(time.Second):
		t.Fatal("background compaction did not start")
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	summary, err := sessions.GetSummary(context.Background(), session.ID)
	if err != nil || summary == nil || summary.ThroughSequence != 5 {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	result, err := builder.Build(context.Background(), BuildInput{Session: session, Query: "next", SystemPrompt: "system"})
	if err != nil || len(result.Messages) == 0 || len(result.Messages) > 21 || summary.Content != "compressed" || result.Messages[len(result.Messages)-1].Content != "next" {
		t.Fatalf("context=%+v err=%v", result, err)
	}
}

func TestPrecompactorQueueFullAndShutdown(t *testing.T) {
	release := make(chan struct{})
	compactor := &waitingCompactor{started: make(chan string, 20), release: release}
	builder, _ := newPrecompactTestBuilder(compactor)
	p := NewPrecompactor(builder)
	for i := range 2 {
		if !p.Submit(&model.Session{ID: fmt.Sprintf("running-%d", i)}, 70, 25, "") {
			t.Fatal("worker task was rejected")
		}
	}
	for range 2 {
		select {
		case <-compactor.started:
		case <-time.After(time.Second):
			t.Fatal("workers did not start")
		}
	}
	for i := range 16 {
		if !p.Submit(&model.Session{ID: fmt.Sprintf("queued-%d", i)}, 70, 25, "") {
			t.Fatalf("queued task %d was rejected", i)
		}
	}
	if p.Submit(&model.Session{ID: "overflow"}, 70, 25, "") {
		t.Fatal("full queue accepted task")
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Close(ctx); err != nil || p.Submit(&model.Session{ID: "after-close"}, 70, 25, "") {
		t.Fatalf("close err=%v", err)
	}
}

func TestPrecompactorCloseCancelsBlockedTask(t *testing.T) {
	compactor := &waitingCompactor{started: make(chan string, 1), release: make(chan struct{})}
	builder, _ := newPrecompactTestBuilder(compactor)
	p := NewPrecompactor(builder)
	if !p.Submit(&model.Session{ID: "session"}, 70, 25, "") {
		t.Fatal("task was rejected")
	}
	select {
	case <-compactor.started:
	case <-time.After(time.Second):
		t.Fatal("task did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Close(ctx); err != context.Canceled {
		t.Fatalf("Close err=%v", err)
	}
	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Fatal("blocked task did not exit after cancellation")
	}
}
