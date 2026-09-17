package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/enterprise/ai-agent-go/internal/metrics"
	"github.com/enterprise/ai-agent-go/internal/trace"
	"github.com/enterprise/ai-agent-go/pkg/common"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestHTTPMetricsUseRouteTemplateAndSkipScrapes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(metricsMiddleware())
	engine.GET("/items/:id", func(c *gin.Context) { c.Status(http.StatusTeapot) })
	engine.GET("/metrics", func(c *gin.Context) { c.Status(http.StatusOK) })
	before := testutil.ToFloat64(metrics.Default.HTTPRequests.WithLabelValues("GET", "/items/:id", "418"))
	for _, path := range []string{"/items/one", "/items/two", "/metrics"} {
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	}
	if got := testutil.ToFloat64(metrics.Default.HTTPRequests.WithLabelValues("GET", "/items/:id", "418")); got-before != 2 {
		t.Fatalf("templated requests increment=%v", got-before)
	}
	if got := testutil.ToFloat64(metrics.Default.HTTPErrors.WithLabelValues("GET", "/items/:id", "418")); got < 2 {
		t.Fatalf("HTTP errors=%v", got)
	}
	if got := testutil.ToFloat64(metrics.Default.HTTPRequests.WithLabelValues("GET", "/metrics", "200")); got != 0 {
		t.Fatalf("metrics scrape counted as business request: %v", got)
	}
}

func TestTraceMiddlewareKeepsRequestIDAndMarksSSEError(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(oteltrace.NewNoopTracerProvider())
	})
	engine := gin.New()
	engine.Use(requestIDMiddleware(), traceMiddleware())
	engine.GET("/api/v1/test", func(c *gin.Context) { common.OK(c, "ok") })
	engine.GET("/api/v1/chat/stream", func(c *gin.Context) { c.Status(200); trace.SetError(c.Request.Context(), context.Canceled) })
	engine.GET("/metrics", func(c *gin.Context) { c.Status(200) })
	request := httptest.NewRequest("GET", "/api/v1/test", nil)
	request.Header.Set("X-Request-ID", "legacy-id")
	request.Header.Set("traceparent", "00-12345678901234567890123456789012-1234567890123456-01")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, request)
	var body common.Result
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Header().Get("X-Request-ID") != "legacy-id" || body.TraceID != "legacy-id" || w.Header().Get("X-Trace-ID") != "12345678901234567890123456789012" {
		t.Fatalf("headers=%v body=%+v", w.Header(), body)
	}
	for _, path := range []string{"/api/v1/chat/stream", "/metrics"} {
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	spans := exporter.GetSpans()
	if len(spans) != 2 || spans[0].Name != "GET /api/v1/test" || spans[0].Parent.SpanID().String() != "1234567890123456" || spans[1].Status.Code != codes.Error {
		t.Fatalf("spans=%+v", spans)
	}
}
