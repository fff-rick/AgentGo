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

// NewClient keeps local Ollama as the default and selects an OpenAI-compatible
// endpoint when an API key is configured.
func NewClient(cfg config.EmbeddingConfig) (Client, error) {
	if strings.TrimSpace(cfg.APIKey) != "" {
		return NewOpenAIClient(cfg)
	}
	return NewOllamaClient(cfg)
}

type OpenAIClient struct {
	endpoint  string
	model     string
	apiKey    string
	dimension int
	batchSize int
	http      *http.Client
}

func NewOpenAIClient(cfg config.EmbeddingConfig) (*OpenAIClient, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.Model) == "" || strings.TrimSpace(cfg.APIKey) == "" || cfg.Dimension <= 0 {
		return nil, fmt.Errorf("OpenAI-compatible embedding 需要 base_url、model、api_key 和正数 dimension")
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 32
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = time.Minute
	}
	endpoint := strings.TrimRight(cfg.BaseURL, "/")
	if !strings.HasSuffix(endpoint, "/embeddings") {
		endpoint += "/embeddings"
	}
	return &OpenAIClient{endpoint: endpoint, model: cfg.Model, apiKey: cfg.APIKey,
		dimension: cfg.Dimension, batchSize: cfg.BatchSize, http: &http.Client{Timeout: cfg.Timeout}}, nil
}

func (c *OpenAIClient) Embed(ctx context.Context, input string) ([]float32, error) {
	vectors, err := c.EmbedBatch(ctx, []string{input})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (c *OpenAIClient) EmbedBatch(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	vectors := make([][]float32, 0, len(inputs))
	for start := 0; start < len(inputs); start += c.batchSize {
		end := min(start+c.batchSize, len(inputs))
		batch, err := c.embed(ctx, inputs[start:end])
		if err != nil {
			return nil, err
		}
		vectors = append(vectors, batch...)
	}
	return vectors, nil
}

func (c *OpenAIClient) embed(ctx context.Context, inputs []string) (vectors [][]float32, embedErr error) {
	ctx, span := trace.StartSpan(ctx, "embedding")
	defer func() { trace.Finish(span, embedErr) }()
	start := time.Now()
	defer func() { metrics.Default.EmbeddingDuration.WithLabelValues("batch").Observe(metrics.Seconds(start)) }()
	for i, input := range inputs {
		if strings.TrimSpace(input) == "" {
			return nil, fmt.Errorf("embedding input %d 不能为空", i)
		}
	}
	body, err := json.Marshal(struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}{Model: c.model, Input: inputs})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("调用 embedding API 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding API 返回状态码 %d", resp.StatusCode)
	}
	var decoded struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("解析 embedding 响应失败: %w", err)
	}
	if len(decoded.Data) != len(inputs) {
		return nil, fmt.Errorf("embedding API 返回 %d 个向量，期望 %d 个", len(decoded.Data), len(inputs))
	}
	vectors = make([][]float32, len(inputs))
	for _, item := range decoded.Data {
		if item.Index < 0 || item.Index >= len(inputs) || vectors[item.Index] != nil {
			return nil, fmt.Errorf("embedding API 返回无效 index %d", item.Index)
		}
		if len(item.Embedding) != c.dimension {
			return nil, fmt.Errorf("embedding API 向量维度为 %d，配置期望 %d", len(item.Embedding), c.dimension)
		}
		vectors[item.Index] = item.Embedding
	}
	return vectors, nil
}

func (c *OpenAIClient) Healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := c.Embed(ctx, "health check")
	return err == nil
}
