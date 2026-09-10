// Package builtin 提供 AI Agent 的内置工具实现。
package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/tool"
)

const (
	defaultSearchResults = 5
	maxSearchResults     = 10
	maxSearchResponse    = 4 << 20
)

// SearchTool 通过私有 SearXNG 实例执行真实网络搜索。
type SearchTool struct {
	baseURL    string
	language   string
	safeSearch int
	httpClient *http.Client
	logger     *zap.Logger
}

// NewSearchTool 创建网络搜索工具
func NewSearchTool(cfg config.SearchConfig, logger *zap.Logger) *SearchTool {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &SearchTool{
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		language:   cfg.Language,
		safeSearch: cfg.SafeSearch,
		httpClient: &http.Client{Timeout: timeout},
		logger:     logger,
	}
}

func (t *SearchTool) Name() string { return "web_search" }

func (t *SearchTool) Description() string {
	return "通过 SearXNG 搜索互联网获取实时信息；结果包含标题、URL、摘要和来源，回答时应标注来源链接"
}

func (t *SearchTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{
				"type":        "string",
				"description": "准确、简洁的搜索关键词",
			},
			"max_results": map[string]interface{}{
				"type":        "integer",
				"description": "返回结果数，范围 1-10",
				"default":     defaultSearchResults,
			},
			"time_range": map[string]interface{}{
				"type":        "string",
				"description": "可选时间范围",
				"enum":        []string{"day", "month", "year"},
			},
		},
		"required": []string{"query"},
	}
}

type searchParams struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
	TimeRange  string `json:"time_range"`
}

type searxResponse struct {
	Results []struct {
		Title         string   `json:"title"`
		URL           string   `json:"url"`
		Content       string   `json:"content"`
		Engine        string   `json:"engine"`
		Engines       []string `json:"engines"`
		PublishedDate string   `json:"publishedDate"`
	} `json:"results"`
}

type searchOutput struct {
	Query   string         `json:"query"`
	Results []searchResult `json:"results"`
}

type searchResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Snippet     string `json:"snippet,omitempty"`
	Source      string `json:"source,omitempty"`
	PublishedAt string `json:"published_at,omitempty"`
}

// Execute 执行真实搜索并返回适合模型消费的结构化结果。
func (t *SearchTool) Execute(ctx context.Context, input string) (*tool.ToolResult, error) {
	var params searchParams
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return tool.NewErrorResult("参数解析失败: " + err.Error()), nil
	}
	params.Query = strings.TrimSpace(params.Query)
	if params.Query == "" {
		return tool.NewErrorResult("搜索关键词不能为空"), nil
	}
	if len([]rune(params.Query)) > 500 {
		return tool.NewErrorResult("搜索关键词不能超过 500 个字符"), nil
	}
	if params.MaxResults <= 0 {
		params.MaxResults = defaultSearchResults
	} else if params.MaxResults > maxSearchResults {
		params.MaxResults = maxSearchResults
	}
	if params.TimeRange != "" && params.TimeRange != "day" && params.TimeRange != "month" && params.TimeRange != "year" {
		return tool.NewErrorResult("time_range 仅支持 day、month 或 year"), nil
	}
	if t.baseURL == "" {
		return tool.NewErrorResult("搜索服务地址未配置，请设置 APP_SEARCH_BASE_URL"), nil
	}

	endpoint, err := url.Parse(t.baseURL + "/search")
	if err != nil {
		return nil, fmt.Errorf("无效的搜索服务地址: %w", err)
	}
	query := endpoint.Query()
	query.Set("q", params.Query)
	query.Set("format", "json")
	if t.language != "" {
		query.Set("language", t.language)
	}
	query.Set("safesearch", fmt.Sprintf("%d", t.safeSearch))
	if params.TimeRange != "" {
		query.Set("time_range", params.TimeRange)
	}
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("创建搜索请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "AgentGo/1.0")

	t.logger.Info("执行真实网络搜索", zap.String("query", params.Query), zap.String("backend", t.baseURL))
	resp, err := t.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("搜索服务请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("搜索服务返回 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(message)))
	}

	var payload searxResponse
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxSearchResponse))
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("解析搜索结果失败: %w", err)
	}

	output := searchOutput{Query: params.Query, Results: make([]searchResult, 0, params.MaxResults)}
	for _, item := range payload.Results {
		if len(output.Results) >= params.MaxResults {
			break
		}
		if strings.TrimSpace(item.Title) == "" || strings.TrimSpace(item.URL) == "" {
			continue
		}
		source := item.Engine
		if len(item.Engines) > 0 {
			source = strings.Join(item.Engines, ",")
		}
		output.Results = append(output.Results, searchResult{
			Title:       strings.TrimSpace(item.Title),
			URL:         strings.TrimSpace(item.URL),
			Snippet:     strings.TrimSpace(item.Content),
			Source:      source,
			PublishedAt: item.PublishedDate,
		})
	}

	var encoded bytes.Buffer
	if err := json.NewEncoder(&encoded).Encode(output); err != nil {
		return nil, fmt.Errorf("序列化搜索结果失败: %w", err)
	}
	return tool.NewSuccessResult(strings.TrimSpace(encoded.String())), nil
}
