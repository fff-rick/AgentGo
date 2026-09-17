package trace

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestChildAndLinkedTracePreserveParentWithoutPayload(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(trace.NewNoopTracerProvider())
	})
	rootCtx, root := StartServerSpan(context.Background(), "POST /api/v1/chat")
	parentHeader := TraceParent(rootCtx)
	childCtx, child := StartSpan(rootCtx, "agent.loop")
	SetError(childCtx, errors.New("secret user content"))
	child.End()
	root.End()
	_, linked := StartLinked(context.Background(), "memory.job.extract", parentHeader)
	linked.End()
	spans := exporter.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("spans=%d", len(spans))
	}
	if spans[0].Parent.SpanID() != root.SpanContext().SpanID() || spans[0].Status.Code != codes.Error || spans[0].Status.Description != "operation failed" {
		t.Fatalf("child=%+v", spans[0])
	}
	if spans[2].SpanContext.TraceID() == root.SpanContext().TraceID() || len(spans[2].Links) != 1 || spans[2].Links[0].SpanContext.SpanID() != root.SpanContext().SpanID() {
		t.Fatalf("linked=%+v", spans[2])
	}
	if parentHeader == "" || !SpanContextFromTraceParent(parentHeader).IsValid() {
		t.Fatal("traceparent not serializable")
	}
}
