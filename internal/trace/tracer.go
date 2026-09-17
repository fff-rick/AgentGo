// Package trace provides request-scoped OpenTelemetry tracing without payload capture.
package trace

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

type TracerProvider struct{ provider *sdktrace.TracerProvider }

// InitTracer uses standard OTLP environment variables; no endpoint means no exporter.
func InitTracer(serviceName string) (*TracerProvider, error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	ratio := 1.0
	if raw := os.Getenv("OTEL_TRACES_SAMPLER_ARG"); raw != "" {
		var err error
		ratio, err = strconv.ParseFloat(raw, 64)
		if err != nil || ratio < 0 || ratio > 1 {
			return nil, fmt.Errorf("OTEL_TRACES_SAMPLER_ARG 必须在 0 到 1 之间")
		}
	}
	var sampler sdktrace.Sampler
	switch os.Getenv("OTEL_TRACES_SAMPLER") {
	case "", "parentbased_traceidratio":
		sampler = sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
	case "traceidratio":
		sampler = sdktrace.TraceIDRatioBased(ratio)
	case "always_on":
		sampler = sdktrace.AlwaysSample()
	case "always_off":
		sampler = sdktrace.NeverSample()
	case "parentbased_always_on":
		sampler = sdktrace.ParentBased(sdktrace.AlwaysSample())
	case "parentbased_always_off":
		sampler = sdktrace.ParentBased(sdktrace.NeverSample())
	default:
		return nil, fmt.Errorf("不支持的 OTEL_TRACES_SAMPLER")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(serviceName)))
	if err != nil {
		return nil, err
	}
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res), sdktrace.WithSampler(sampler)}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" {
		exporter, err := otlptracegrpc.New(ctx)
		if err != nil {
			return nil, err
		}
		opts = append(opts, sdktrace.WithBatcher(exporter))
	}
	p := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(p)
	return &TracerProvider{provider: p}, nil
}

func (tp *TracerProvider) Shutdown(ctx context.Context) error {
	if tp == nil || tp.provider == nil {
		return nil
	}
	return tp.provider.Shutdown(ctx)
}

func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, oteltrace.Span) {
	return otel.Tracer("agentgo").Start(ctx, name, oteltrace.WithAttributes(attrs...))
}

func StartServerSpan(ctx context.Context, name string) (context.Context, oteltrace.Span) {
	return otel.Tracer("agentgo").Start(ctx, name, oteltrace.WithSpanKind(oteltrace.SpanKindServer))
}

func StartLinked(ctx context.Context, name, traceparent string) (context.Context, oteltrace.Span) {
	options := []oteltrace.SpanStartOption{oteltrace.WithNewRoot()}
	if parent := SpanContextFromTraceParent(traceparent); parent.IsValid() {
		options = append(options, oteltrace.WithLinks(oteltrace.Link{SpanContext: parent}))
	}
	return otel.Tracer("agentgo").Start(ctx, name, options...)
}

// Finish marks failures without adding exception messages that may contain user data.
func Finish(span oteltrace.Span, err error) {
	if err != nil {
		span.SetStatus(codes.Error, "operation failed")
	}
	span.End()
}

func SetError(ctx context.Context, err error) {
	if err != nil {
		oteltrace.SpanFromContext(ctx).SetStatus(codes.Error, "operation failed")
	}
}

func AddEvent(ctx context.Context, name string, attrs ...attribute.KeyValue) {
	span := oteltrace.SpanFromContext(ctx)
	if span.IsRecording() {
		span.AddEvent(name, oteltrace.WithAttributes(attrs...))
	}
}

func TraceParent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

func SpanContextFromTraceParent(value string) oteltrace.SpanContext {
	if strings.TrimSpace(value) == "" {
		return oteltrace.SpanContext{}
	}
	ctx := otel.GetTextMapPropagator().Extract(context.Background(), propagation.MapCarrier{"traceparent": value})
	return oteltrace.SpanContextFromContext(ctx)
}

func TraceID(ctx context.Context) string {
	sc := oteltrace.SpanFromContext(ctx).SpanContext()
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

func SpanID(ctx context.Context) string {
	sc := oteltrace.SpanFromContext(ctx).SpanContext()
	if !sc.IsValid() {
		return ""
	}
	return sc.SpanID().String()
}

func WrapError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if id := TraceID(ctx); id != "" {
		return fmt.Errorf("[trace=%s] %w", id, err)
	}
	return err
}
