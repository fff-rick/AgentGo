// Package router 负责 HTTP 路由的注册和中间件配置。
package router

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"

	"github.com/enterprise/ai-agent-go/internal/auth"
	"github.com/enterprise/ai-agent-go/internal/handler"
	"github.com/enterprise/ai-agent-go/internal/metrics"
	"github.com/enterprise/ai-agent-go/internal/trace"
)

// Register 注册所有 HTTP 路由和中间件
func Register(engine *gin.Engine, verifier *auth.Verifier, chatH *handler.ChatHandler, sessionH *handler.SessionHandler, memoryH *handler.MemoryHandler, docH *handler.DocumentHandler, healthH *handler.HealthHandler) {
	// 全局中间件
	engine.Use(
		requestIDMiddleware(),
		traceMiddleware(),
		corsMiddleware(),
		metricsMiddleware(),
		loggerMiddleware(),
	)
	engine.GET("/metrics", gin.WrapH(promhttp.HandlerFor(metrics.Default.Registry, promhttp.HandlerOpts{})))

	// 健康检查（不受路由组前缀影响）
	engine.GET("/health", healthH.Check)

	// API v1 路由组
	v1 := engine.Group("/api/v1")
	{
		// 对话接口
		protected := v1.Group("")
		if verifier == nil {
			protected.Use(auth.LocalMiddleware())
		} else {
			protected.Use(verifier.Middleware())
		}
		protected.POST("/chat", chatH.Chat)
		protected.POST("/chat/stream", chatH.ChatStream)
		protected.POST("/sessions", sessionH.Create)
		protected.GET("/memories", memoryH.List)
		protected.GET("/memories/:id", memoryH.Get)
		protected.GET("/memories/:id/versions", memoryH.Versions)
		protected.PUT("/memories/:id", memoryH.Correct)
		protected.PATCH("/memories/:id/expiry", memoryH.SetExpiry)
		protected.DELETE("/memories/:id", memoryH.Delete)

		// 文档接口
		v1.POST("/documents", docH.Upload)
		v1.POST("/documents/import", docH.ImportDocument)
		v1.GET("/documents/:id", docH.GetStatus)
	}
}

func traceMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.Path == "/metrics" {
			c.Next()
			return
		}
		parent := otel.GetTextMapPropagator().Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header))
		ctx, span := trace.StartServerSpan(parent, "HTTP request")
		c.Request = c.Request.WithContext(ctx)
		c.Header("X-Trace-ID", trace.TraceID(ctx))
		defer func() {
			route := c.FullPath()
			if route == "" {
				route = "unmatched"
			}
			method := c.Request.Method
			switch method {
			case "GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD":
			default:
				method = "OTHER"
			}
			span.SetName(method + " " + route)
			span.SetAttributes(attribute.String("http.request.method", method), attribute.String("http.route", route), attribute.Int("http.response.status_code", c.Writer.Status()))
			if c.Writer.Status() >= 500 {
				span.SetStatus(codes.Error, "HTTP server error")
			}
			span.End()
		}()
		c.Next()
	}
}

func metricsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.Path == "/metrics" {
			c.Next()
			return
		}
		start := time.Now()
		metrics.Default.HTTPInflight.Inc()
		defer func() {
			metrics.Default.HTTPInflight.Dec()
			route := c.FullPath()
			if route == "" {
				route = "unmatched"
			}
			method, status := c.Request.Method, strconv.Itoa(c.Writer.Status())
			switch method {
			case "GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD":
			default:
				method = "OTHER"
			}
			metrics.Default.HTTPRequests.WithLabelValues(method, route, status).Inc()
			metrics.Default.HTTPDuration.WithLabelValues(method, route).Observe(metrics.Seconds(start))
			if c.Writer.Status() >= 400 {
				metrics.Default.HTTPErrors.WithLabelValues(method, route, status).Inc()
			}
		}()
		c.Next()
	}
}

// requestIDMiddleware 保留现有的业务请求 ID；OpenTelemetry Trace ID 由 traceMiddleware 单独生成。
func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		traceID := c.GetHeader("X-Request-ID")
		if traceID == "" {
			traceID = uuid.New().String()
		}
		c.Set("trace_id", traceID)
		c.Header("X-Request-ID", traceID)
		c.Next()
	}
}

// corsMiddleware 跨域资源共享中间件
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Request-ID, traceparent")
		c.Header("Access-Control-Expose-Headers", "X-Request-ID, X-Trace-ID")
		c.Header("Access-Control-Max-Age", "86400")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	}
}

// loggerMiddleware 请求日志中间件
func loggerMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path

		c.Next()

		latency := time.Since(start)
		statusCode := c.Writer.Status()

		// Gin 默认 Logger 已输出日志，此处可做额外的结构化日志
		_ = latency
		_ = statusCode
		_ = path
	}
}
