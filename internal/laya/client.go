// Package laya calls the optional Laya structured decision service.
package laya

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/metrics"
	"github.com/enterprise/ai-agent-go/internal/trace"
)

const maxResponseBytes = 4 << 20

const warmupTimeout = 10 * time.Second

type Question struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     any               `json:"criteria,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

type Request struct {
	State         any                 `json:"state"`
	Questions     map[string]Question `json:"questions"`
	Model         string              `json:"model,omitempty"`
	Task          string              `json:"task,omitempty"`
	Lang          string              `json:"lang,omitempty"`
	LangGuess     string              `json:"lang_guess,omitempty"`
	MaxLen        int                 `json:"max_len,omitempty"`
	HeadMaxLen    int                 `json:"head_max_len,omitempty"`
	MinConfidence *float64            `json:"min_confidence,omitempty"`
}

type Answer struct {
	Type                string             `json:"type"`
	Choice              string             `json:"choice,omitempty"`
	Score               *float64           `json:"score,omitempty"`
	Noul                *float64           `json:"noul,omitempty"`
	Probabilities       map[string]float64 `json:"probabilities,omitempty"`
	Legend              map[string]string  `json:"legend,omitempty"`
	Confidence          *float64           `json:"confidence,omitempty"`
	AnswerConfidence    *float64           `json:"answer_confidence,omitempty"`
	Abstention          string             `json:"abstention,omitempty"`
	AbstentionThreshold *float64           `json:"abstention_threshold,omitempty"`
	LowConfidence       *bool              `json:"low_confidence,omitempty"`
}

type Usage struct {
	InputTokens        int      `json:"input_tokens"`
	OutputTokens       int      `json:"output_tokens"`
	StateTokens        int      `json:"state_tokens"`
	StateTokensDropped int      `json:"state_tokens_dropped"`
	Truncated          bool     `json:"truncated"`
	TruncatedQuestions []string `json:"truncated_questions"`
}

type Routing struct {
	Model     string          `json:"model"`
	Repo      string          `json:"repo"`
	Reason    string          `json:"reason"`
	Detection json.RawMessage `json:"detection"`
	Workflow  json.RawMessage `json:"workflow"`
}

type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
	Routing Routing           `json:"routing"`
}

type Client struct {
	endpoint string
	apiKey   string
	http     *http.Client
}

func NewClient(cfg config.LayaConfig) (*Client, error) {
	endpoint := strings.TrimSpace(cfg.URL)
	parsed, err := url.ParseRequestURI(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("Laya URL 必须是有效的 HTTP(S) 地址")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	return &Client{endpoint: endpoint, apiKey: cfg.APIKey, http: &http.Client{Timeout: cfg.Timeout}}, nil
}

// Warmup loads and runs the English and multilingual checkpoints before the
// production client's shorter timeout is exposed to real shadow traffic.
func Warmup(ctx context.Context, cfg config.LayaConfig) error {
	if cfg.Timeout < warmupTimeout {
		cfg.Timeout = warmupTimeout
	}
	client, err := NewClient(cfg)
	if err != nil {
		return err
	}
	criteria := map[string]string{"none": "No external tool is needed.", "calculator": "Perform exact arithmetic."}
	questions := map[string]Question{
		"primary_tool":   {Type: "choice", Instructions: "Choose the primary tool or none.", Criteria: criteria},
		"secondary_tool": {Type: "choice", Instructions: "Choose an additional tool or none.", Criteria: criteria},
	}
	var warmErr error
	for _, request := range []string{"你好，请简单回答。", "Hello, answer briefly."} {
		_, err := client.Predict(ctx, "startup_warmup", Request{State: map[string]string{"request": request}, Questions: questions})
		if err != nil {
			warmErr = errors.Join(warmErr, err)
		}
	}
	return warmErr
}

// Predict asks Laya for a structured decision. decision must be a fixed,
// low-cardinality call-site name because it is used as a Prometheus label.
func (c *Client) Predict(ctx context.Context, decision string, input Request) (response *Response, callErr error) {
	if strings.TrimSpace(decision) == "" {
		return nil, fmt.Errorf("Laya decision 不能为空")
	}
	if input.State == nil {
		return nil, fmt.Errorf("Laya state 不能为空")
	}
	if len(input.Questions) == 0 {
		return nil, fmt.Errorf("Laya questions 不能为空")
	}
	ctx, span := trace.StartSpan(ctx, "laya.predict")
	defer func() { trace.Finish(span, callErr) }()
	start := time.Now()
	defer func() {
		result := "success"
		if callErr != nil {
			result = "error"
		}
		metrics.Default.LayaRequests.WithLabelValues(decision, result).Inc()
		metrics.Default.LayaDuration.WithLabelValues(decision).Observe(metrics.Seconds(start))
	}()

	body, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("序列化 Laya 请求失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("创建 Laya 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("调用 Laya 失败: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取 Laya 响应失败: %w", err)
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("Laya 响应超过 %d 字节", maxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := data
		if len(message) > 4096 {
			message = message[:4096]
		}
		return nil, fmt.Errorf("Laya 返回状态码 %d: %s", resp.StatusCode, strings.TrimSpace(string(message)))
	}
	var output Response
	if err := json.Unmarshal(data, &output); err != nil {
		return nil, fmt.Errorf("解析 Laya 响应失败: %w", err)
	}
	if len(output.Answers) == 0 {
		return nil, fmt.Errorf("Laya 响应缺少 answers")
	}
	for _, answer := range output.Answers {
		if answer.LowConfidence != nil && *answer.LowConfidence {
			metrics.Default.LayaLowConfidence.WithLabelValues(decision).Inc()
		}
	}
	return &output, nil
}
