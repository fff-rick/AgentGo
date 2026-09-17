// Package embedding provides text embedding clients.
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/metrics"
	"github.com/enterprise/ai-agent-go/internal/trace"
)

// Client converts text into dense vectors.
type Client interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
	Healthy(ctx context.Context) bool
}

// OllamaClient calls Ollama's native /api/embed endpoint.
type OllamaClient struct {
	baseURL   string
	model     string
	dimension int
	batchSize int
	http      *http.Client
}

func NewOllamaClient(cfg config.EmbeddingConfig) (*OllamaClient, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("embedding base_url 不能为空")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("embedding model 不能为空")
	}
	if cfg.Dimension <= 0 {
		return nil, fmt.Errorf("embedding dimension 必须大于 0")
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 32
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	return &OllamaClient{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"), model: cfg.Model,
		dimension: cfg.Dimension, batchSize: cfg.BatchSize,
		http: &http.Client{Timeout: cfg.Timeout},
	}, nil
}

func (c *OllamaClient) Embed(ctx context.Context, text string) ([]float32, error) {
	vectors, err := c.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (c *OllamaClient) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	result := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += c.batchSize {
		end := start + c.batchSize
		if end > len(texts) {
			end = len(texts)
		}
		vectors, err := c.embed(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		result = append(result, vectors...)
	}
	return result, nil
}

func (c *OllamaClient) embed(ctx context.Context, texts []string) (vectors [][]float32, embedErr error) {
	ctx, span := trace.StartSpan(ctx, "embedding")
	defer func() { trace.Finish(span, embedErr) }()
	start := time.Now()
	defer func() { metrics.Default.EmbeddingDuration.WithLabelValues("batch").Observe(metrics.Seconds(start)) }()
	for i, text := range texts {
		if strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("embedding input %d 不能为空", i)
		}
	}
	body, err := json.Marshal(struct {
		Model    string   `json:"model"`
		Input    []string `json:"input"`
		Truncate bool     `json:"truncate"`
	}{Model: c.model, Input: texts, Truncate: true})
	if err != nil {
		return nil, fmt.Errorf("序列化 embedding 请求失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("创建 embedding 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("调用 Ollama embedding 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("Ollama embedding 返回状态码 %d: %s", resp.StatusCode, strings.TrimSpace(string(message)))
	}
	var decoded struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("解析 Ollama embedding 响应失败: %w", err)
	}
	if len(decoded.Embeddings) != len(texts) {
		return nil, fmt.Errorf("Ollama 返回 %d 个向量，期望 %d 个", len(decoded.Embeddings), len(texts))
	}
	for i, vector := range decoded.Embeddings {
		if len(vector) != c.dimension {
			return nil, fmt.Errorf("Ollama 向量 %d 维度为 %d，配置期望 %d", i, len(vector), c.dimension)
		}
	}
	return decoded.Embeddings, nil
}

func (c *OllamaClient) Healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := c.Embed(ctx, "health check")
	return err == nil
}
