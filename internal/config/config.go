// Package config 提供应用程序的配置管理能力。
// 支持从配置文件（YAML）和环境变量中加载配置，环境变量优先级更高。
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config 应用程序全局配置结构
type Config struct {
	Server    ServerConfig    `mapstructure:"server"`
	LLM       LLMConfig       `mapstructure:"llm"`
	Redis     RedisConfig     `mapstructure:"redis"`
	Milvus    MilvusConfig    `mapstructure:"milvus"`
	Embedding EmbeddingConfig `mapstructure:"embedding"`
	Postgres  PostgresConfig  `mapstructure:"postgres"`
	Agent     AgentConfig     `mapstructure:"agent"`
	RAG       RAGConfig       `mapstructure:"rag"`
	Search    SearchConfig    `mapstructure:"search"`
	Log       LogConfig       `mapstructure:"log"`
}

// ServerConfig HTTP 服务器配置
type ServerConfig struct {
	Port         int           `mapstructure:"port"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
	Mode         string        `mapstructure:"mode"` // debug / release / test
}

// LLMConfig 大语言模型客户端配置
type LLMConfig struct {
	Models         []ModelConfig `mapstructure:"models"`
	RequestTimeout time.Duration `mapstructure:"request_timeout"`
	CircuitBreaker CBConfig      `mapstructure:"circuit_breaker"`
}

// ModelConfig 单个模型的配置
type ModelConfig struct {
	Name     string `mapstructure:"name"`
	Provider string `mapstructure:"provider"` // openai / anthropic / local
	APIKey   string `mapstructure:"api_key"`
	BaseURL  string `mapstructure:"base_url"`
	Model    string `mapstructure:"model"`
	Priority int    `mapstructure:"priority"` // 优先级，数值越小优先级越高
}

// CBConfig 熔断器配置
type CBConfig struct {
	FailureThreshold int           `mapstructure:"failure_threshold"` // 触发熔断的连续失败次数
	SuccessThreshold int           `mapstructure:"success_threshold"` // 半开状态下恢复所需的连续成功次数
	Timeout          time.Duration `mapstructure:"timeout"`           // 熔断器打开后的冷却时间
}

// RedisConfig Redis 连接配置
type RedisConfig struct {
	Addr         string        `mapstructure:"addr"`
	Password     string        `mapstructure:"password"`
	DB           int           `mapstructure:"db"`
	PoolSize     int           `mapstructure:"pool_size"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
}

// MilvusConfig Milvus 向量数据库连接配置
type MilvusConfig struct {
	Addr           string        `mapstructure:"addr"`
	Username       string        `mapstructure:"username"`
	Password       string        `mapstructure:"password"`
	Database       string        `mapstructure:"database"`
	CollectionName string        `mapstructure:"collection_name"`
	Dimension      int           `mapstructure:"dimension"`
	MetricType     string        `mapstructure:"metric_type"` // L2 / IP / COSINE
	ConnectTimeout time.Duration `mapstructure:"connect_timeout"`
}

// EmbeddingConfig Ollama embedding 服务配置
type EmbeddingConfig struct {
	BaseURL   string        `mapstructure:"base_url"`
	Model     string        `mapstructure:"model"`
	Dimension int           `mapstructure:"dimension"`
	BatchSize int           `mapstructure:"batch_size"`
	Timeout   time.Duration `mapstructure:"timeout"`
}

// PostgresConfig PostgreSQL 数据库连接配置
type PostgresConfig struct {
	Host         string        `mapstructure:"host"`
	Port         int           `mapstructure:"port"`
	User         string        `mapstructure:"user"`
	Password     string        `mapstructure:"password"`
	DBName       string        `mapstructure:"dbname"`
	SSLMode      string        `mapstructure:"ssl_mode"`
	QueryTimeout time.Duration `mapstructure:"query_timeout"`
	MaxRows      int           `mapstructure:"max_rows"`
}

// AgentConfig Agent 编排器配置
type AgentConfig struct {
	MaxIterations    int           `mapstructure:"max_iterations"`    // ReAct 最大迭代次数
	DefaultTimeout   time.Duration `mapstructure:"default_timeout"`   // 单次 Agent 执行超时
	EnableReflection bool          `mapstructure:"enable_reflection"` // 是否启用反思机制
	ToolModel        string        `mapstructure:"tool_model"`        // Function Calling 指定的模型名称
}

// RAGConfig 检索增强生成配置
type RAGConfig struct {
	TopK           int     `mapstructure:"top_k"`           // 检索返回的文档数量
	ScoreThreshold float64 `mapstructure:"score_threshold"` // 相似度阈值
	ChunkSize      int     `mapstructure:"chunk_size"`      // 文档分块大小
	ChunkOverlap   int     `mapstructure:"chunk_overlap"`   // 分块重叠长度
	EnableRerank   bool    `mapstructure:"enable_rerank"`   // 是否启用重排序
}

// SearchConfig SearXNG 搜索服务配置
type SearchConfig struct {
	BaseURL    string        `mapstructure:"base_url"`
	Timeout    time.Duration `mapstructure:"timeout"`
	Language   string        `mapstructure:"language"`
	SafeSearch int           `mapstructure:"safe_search"`
}

// LogConfig 日志配置
type LogConfig struct {
	Level  string `mapstructure:"level"`  // debug / info / warn / error
	Format string `mapstructure:"format"` // json / text
}

// DSN 返回 PostgreSQL 连接字符串
func (p PostgresConfig) DSN() string {
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(p.User, p.Password),
		Host:   net.JoinHostPort(p.Host, strconv.Itoa(p.Port)),
		Path:   p.DBName,
	}
	q := u.Query()
	q.Set("sslmode", p.SSLMode)
	u.RawQuery = q.Encode()
	return u.String()
}

// Load 从配置文件和环境变量加载配置。
// 优先级：环境变量 > 配置文件 > 默认值。
// 环境变量前缀为 APP_，使用下划线分隔层级，例如 APP_SERVER_PORT=8080。
func Load(path string) (*Config, error) {
	v := viper.New()

	// 设置默认值
	setDefaults(v)

	// 配置文件
	if path != "" {
		v.SetConfigFile(path)
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		v.AddConfigPath("./config")
		v.AddConfigPath("/etc/ai-agent/")
	}

	// 环境变量绑定：APP_SERVER_PORT -> server.port
	v.SetEnvPrefix("APP")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// 尝试读取配置文件（不存在时使用默认值 + 环境变量）
	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("读取配置文件失败: %w", err)
		}
	}

	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}

	expandModelEnv(cfg.LLM.Models)
	applySingleModelEnv(cfg)

	return cfg, nil
}

func applySingleModelEnv(cfg *Config) {
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("APP_LLM_BASE_URL")), "/")
	if baseURL == "" || len(cfg.LLM.Models) > 0 {
		return
	}
	modelName := envOrDefault("APP_LLM_MODEL", "qwen2.5:7b")
	clientName := envOrDefault("APP_LLM_NAME", "default")
	cfg.LLM.Models = []ModelConfig{{
		Name:     clientName,
		Provider: envOrDefault("APP_LLM_PROVIDER", "openai-compatible"),
		APIKey:   os.Getenv("APP_LLM_API_KEY"),
		BaseURL:  baseURL,
		Model:    modelName,
	}}
}

func expandModelEnv(models []ModelConfig) {
	for i := range models {
		models[i].APIKey = os.ExpandEnv(models[i].APIKey)
		models[i].BaseURL = os.ExpandEnv(models[i].BaseURL)
		models[i].Model = os.ExpandEnv(models[i].Model)
	}
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// setDefaults 设置所有配置项的默认值
func setDefaults(v *viper.Viper) {
	// 服务器默认配置
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.read_timeout", "30s")
	v.SetDefault("server.write_timeout", "60s")
	v.SetDefault("server.mode", "debug")

	// LLM 默认配置
	v.SetDefault("llm.request_timeout", "60s")
	v.SetDefault("llm.circuit_breaker.failure_threshold", 5)
	v.SetDefault("llm.circuit_breaker.success_threshold", 3)
	v.SetDefault("llm.circuit_breaker.timeout", "30s")

	// Redis 默认配置
	v.SetDefault("redis.addr", "localhost:6379")
	v.SetDefault("redis.db", 0)
	v.SetDefault("redis.pool_size", 20)
	v.SetDefault("redis.read_timeout", "3s")
	v.SetDefault("redis.write_timeout", "3s")

	// Milvus 默认配置
	v.SetDefault("milvus.addr", "localhost:19530")
	v.SetDefault("milvus.username", "")
	v.SetDefault("milvus.password", "")
	v.SetDefault("milvus.database", "default")
	v.SetDefault("milvus.collection_name", "documents_bge_m3")
	v.SetDefault("milvus.dimension", 1024)
	v.SetDefault("milvus.metric_type", "COSINE")
	v.SetDefault("milvus.connect_timeout", "30s")

	// Ollama embedding 默认配置（bge-m3 输出 1024 维向量）
	v.SetDefault("embedding.base_url", "http://localhost:11434")
	v.SetDefault("embedding.model", "bge-m3:latest")
	v.SetDefault("embedding.dimension", 1024)
	v.SetDefault("embedding.batch_size", 32)
	v.SetDefault("embedding.timeout", "60s")

	// PostgreSQL 默认配置
	v.SetDefault("postgres.host", "localhost")
	v.SetDefault("postgres.port", 5432)
	v.SetDefault("postgres.user", "postgres")
	v.SetDefault("postgres.password", "postgres")
	v.SetDefault("postgres.dbname", "ai_agent")
	v.SetDefault("postgres.ssl_mode", "disable")
	v.SetDefault("postgres.query_timeout", "10s")
	v.SetDefault("postgres.max_rows", 100)

	// Agent 默认配置
	v.SetDefault("agent.max_iterations", 10)
	v.SetDefault("agent.default_timeout", "120s")
	v.SetDefault("agent.enable_reflection", true)
	v.SetDefault("agent.tool_model", "")

	// RAG 默认配置
	v.SetDefault("rag.top_k", 5)
	v.SetDefault("rag.score_threshold", 0.5)
	v.SetDefault("rag.chunk_size", 512)
	v.SetDefault("rag.chunk_overlap", 64)
	v.SetDefault("rag.enable_rerank", true)

	// SearXNG 搜索配置
	v.SetDefault("search.base_url", "http://localhost:7070")
	v.SetDefault("search.timeout", "20s")
	v.SetDefault("search.language", "zh-CN")
	v.SetDefault("search.safe_search", 1)

	// 日志默认配置
	v.SetDefault("log.level", "info")
	v.SetDefault("log.format", "json")
}
