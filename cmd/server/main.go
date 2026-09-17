// Package main 是 AI Agent 服务的程序入口。
// 负责加载配置、初始化依赖、注册路由并启动 HTTP 服务器，
// 同时支持优雅关停（Graceful Shutdown）。
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/agent"
	"github.com/enterprise/ai-agent-go/internal/agentcontext"
	"github.com/enterprise/ai-agent-go/internal/agentloop"
	"github.com/enterprise/ai-agent-go/internal/auth"
	"github.com/enterprise/ai-agent-go/internal/cache"
	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/database"
	"github.com/enterprise/ai-agent-go/internal/embedding"
	"github.com/enterprise/ai-agent-go/internal/etl"
	"github.com/enterprise/ai-agent-go/internal/handler"
	"github.com/enterprise/ai-agent-go/internal/harness"
	"github.com/enterprise/ai-agent-go/internal/llm"
	"github.com/enterprise/ai-agent-go/internal/memory"
	"github.com/enterprise/ai-agent-go/internal/rag"
	"github.com/enterprise/ai-agent-go/internal/router"
	"github.com/enterprise/ai-agent-go/internal/tool"
	toolbuiltin "github.com/enterprise/ai-agent-go/internal/tool/builtin"
	"github.com/enterprise/ai-agent-go/internal/trace"
	"github.com/enterprise/ai-agent-go/internal/user"
	"github.com/enterprise/ai-agent-go/internal/vectordb"
)

var (
	version   = "dev"
	buildTime = "unknown"
)

func main() {
	// ======================== 1. 加载配置 ========================
	cfg, err := config.Load("")
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// ======================== 2. 初始化日志 ========================
	logger := initLogger(cfg.Log)
	defer logger.Sync()
	logger.Info("AI Agent 服务启动中",
		zap.String("version", version),
		zap.String("build_time", buildTime),
	)
	var verifier *auth.Verifier
	if cfg.Auth.Enabled {
		verifier, err = auth.NewVerifier(context.Background(), cfg.Auth.Issuer, cfg.Auth.Audience)
		if err != nil {
			logger.Fatal("初始化 OIDC 验证器失败", zap.Error(err))
		}
	} else {
		logger.Warn("OIDC 鉴权已关闭，所有请求共享本地用户；请勿将服务暴露到不可信网络")
	}

	// ======================== 3. 初始化基础设施 ========================
	// Redis 缓存
	redisCache, err := cache.NewRedisCache(cfg.Redis)
	if err != nil {
		logger.Fatal("初始化 Redis 失败", zap.Error(err))
	}
	defer redisCache.Close()

	// Milvus 向量数据库
	milvusClient, err := vectordb.NewMilvusClient(cfg.Milvus)
	if err != nil {
		logger.Fatal("初始化 Milvus 失败", zap.Error(err))
	}
	defer milvusClient.Close()
	if cfg.Embedding.Dimension != cfg.Milvus.Dimension {
		logger.Fatal("Embedding 与 Milvus 向量维度不一致",
			zap.Int("embedding_dimension", cfg.Embedding.Dimension),
			zap.Int("milvus_dimension", cfg.Milvus.Dimension))
	}
	embeddingClient, err := embedding.NewClient(cfg.Embedding)
	if err != nil {
		logger.Fatal("初始化 embedding 失败", zap.Error(err))
	}

	postgresCtx, postgresCancel := context.WithTimeout(context.Background(), 15*time.Second)
	postgresClient, err := database.NewPostgresClient(postgresCtx, cfg.Postgres)
	postgresCancel()
	if err != nil {
		logger.Fatal("初始化 PostgreSQL 失败", zap.Error(err))
	}
	defer postgresClient.Close()

	// 链路追踪
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { logger.Warn("Trace 导出失败", zap.Error(err)) }))
	tp, err := trace.InitTracer("ai-agent-go")
	if err != nil {
		logger.Warn("初始化链路追踪失败，将降级运行", zap.Error(err))
	}
	if tp != nil {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := tp.Shutdown(shutdownCtx); err != nil {
				logger.Warn("Trace 关闭失败", zap.Error(err))
			}
		}()
	}

	// ======================== 4. 初始化核心组件 ========================
	// LLM 客户端和模型路由
	llmClients := make(map[string]llm.Client)
	for _, modelCfg := range cfg.LLM.Models {
		client := llm.NewHTTPClient(modelCfg, cfg.LLM.RequestTimeout)
		llmClients[modelCfg.Name] = client
	}
	modelRouter := llm.NewRouter(llmClients, cfg.LLM.Models, cfg.LLM.CircuitBreaker)

	// 用户、会话与长期语义记忆
	userManager := user.NewManager(redisCache, cfg.Memory.SessionTTL)
	sessionManager := memory.NewSessionManager(redisCache, userManager, cfg.Memory.SessionTTL)
	vectorMemory := memory.NewSemanticStore(milvusClient, embeddingClient, cfg.Memory.SemanticCollection)
	semanticMemory, err := memory.NewManagedStore(context.Background(), postgresClient.DB(), vectorMemory)
	if err != nil {
		logger.Fatal("初始化长期记忆失败", zap.Error(err))
	}
	memoryExtractor := memory.NewExtractor(modelRouter, semanticMemory, cfg.Memory.ExtractionTimeout, cfg.Memory.ExtractionMinImportance, cfg.Memory.ExtractionMaxItems)
	memoryExtractor.RequireEvidence()
	memoryJobs := memory.NewJobRunner(semanticMemory, memoryExtractor, logger)
	memoryJobs.Start()
	compactor := agentcontext.NewCompactor(modelRouter)
	contextBuilder := agentcontext.NewBuilder(sessionManager, semanticMemory, compactor, cfg.Context, cfg.Memory.SemanticTopK, logger)
	precompactor := agentcontext.NewPrecompactor(contextBuilder)

	// 工具系统
	toolRegistry := tool.NewRegistry()
	registerBuiltinTools(toolRegistry, cfg.Search, cfg.Postgres, postgresClient, logger)
	toolManager := tool.NewManager(toolRegistry, redisCache, cfg.Tools.LazyLoadThreshold, cfg.Memory.SessionTTL, logger)
	toolRouter := tool.NewRouter(toolRegistry, logger, toolManager)

	// 知识检索作为普通工具加入统一 Agent Loop
	retriever := rag.NewRetriever(milvusClient, embeddingClient, redisCache, cfg.RAG.ScoreThreshold, logger, postgresClient)
	reranker := rag.NewReranker(modelRouter, cfg.RAG.ScoreThreshold, logger)
	ragPipeline := rag.NewPipeline(retriever, reranker, cfg.RAG.EnableRerank)
	toolRegistry.MustRegister(toolbuiltin.NewKnowledgeSearchTool(ragPipeline, cfg.RAG.TopK))

	// ======================== 5. 初始化统一 Agent Harness ========================
	var hooks []harness.Hook
	if cfg.Agent.EnableReflection {
		hooks = append(hooks, agent.NewReflectionAgent(modelRouter, logger))
	}
	loop := agentloop.New(modelRouter, toolRouter)
	planner := agent.NewPlannerAgent(modelRouter, toolRouter, logger)
	agentHarness := harness.New(loop, planner, contextBuilder, sessionManager, memoryExtractor, toolRouter, hooks, cfg.Agent.MaxIterations, cfg.Tools.MaxDiscoveryCalls, cfg.Agent.DefaultTimeout, logger)
	agentHarness.SetMemoryJobs(memoryJobs)
	agentHarness.SetPrecompactor(precompactor)
	orchestrator := agent.NewOrchestrator(agentHarness)

	// ======================== 6. 初始化 HTTP 处理器 ========================
	chatHandler := handler.NewChatHandler(orchestrator, sessionManager, logger)
	sessionHandler := handler.NewSessionHandler(sessionManager)
	memoryHandler := handler.NewMemoryHandler(semanticMemory)
	documentParser := etl.NewDocumentParser(cfg.Document.DoclingURL, cfg.Document.ParseTimeout)
	etlPipeline := etl.NewPipeline(documentParser, etl.NewChunker(cfg.RAG.ChunkSize, cfg.RAG.ChunkOverlap), milvusClient, embeddingClient, logger, postgresClient)
	importer, err := etl.NewImporter(context.Background(), etlPipeline, postgresClient, logger)
	if err != nil {
		logger.Fatal("初始化文档导入器失败", zap.Error(err))
	}
	defer importer.Close()
	docHandler := handler.NewDocumentHandler(etlPipeline, importer, logger, postgresClient)
	healthHandler := handler.NewHealthHandler(redisCache, milvusClient, postgresClient, documentParser)

	// ======================== 7. 配置路由并启动服务器 ========================
	gin.SetMode(cfg.Server.Mode)
	engine := gin.New()
	engine.Use(gin.Recovery())

	router.Register(engine, verifier, chatHandler, sessionHandler, memoryHandler, docHandler, healthHandler)

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:      engine,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	// 在 goroutine 中启动服务器
	go func() {
		logger.Info("HTTP 服务器已启动", zap.Int("port", cfg.Server.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("HTTP 服务器异常退出", zap.Error(err))
		}
	}()

	// ======================== 8. 优雅关停 ========================
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	logger.Info("收到关停信号，开始优雅关停...", zap.String("signal", sig.String()))

	// 给予 15 秒的关停超时
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("HTTP 服务器关停失败", zap.Error(err))
	}
	if err := precompactor.Close(ctx); err != nil {
		logger.Warn("后台摘要任务关停超时", zap.Error(err))
	}
	if err := memoryJobs.Close(ctx); err != nil {
		logger.Warn("长期记忆任务关停超时", zap.Error(err))
	}

	logger.Info("服务已安全退出")
}

// registerBuiltinTools 注册所有内置工具
func registerBuiltinTools(registry *tool.Registry, searchCfg config.SearchConfig, postgresCfg config.PostgresConfig, postgresClient *database.Client, logger *zap.Logger) {
	tools := []tool.Tool{
		toolbuiltin.NewSearchTool(searchCfg, logger),
		toolbuiltin.NewCalculatorTool(logger),
		toolbuiltin.NewDatabaseTool(postgresClient.DB(), postgresCfg, logger),
	}
	for _, t := range tools {
		if err := registry.Register(t); err != nil {
			logger.Warn("注册工具失败", zap.String("tool", t.Name()), zap.Error(err))
		}
	}
}

// initLogger 根据配置初始化 zap 日志
func initLogger(cfg config.LogConfig) *zap.Logger {
	var zapCfg zap.Config
	if cfg.Format == "json" {
		zapCfg = zap.NewProductionConfig()
	} else {
		zapCfg = zap.NewDevelopmentConfig()
	}

	level, err := zap.ParseAtomicLevel(cfg.Level)
	if err == nil {
		zapCfg.Level = level
	}

	logger, err := zapCfg.Build()
	if err != nil {
		log.Fatalf("初始化日志失败: %v", err)
	}
	return logger
}
