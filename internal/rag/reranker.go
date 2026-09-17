package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/llm"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/trace"
)

// Reranker 重排序器。
// 使用 LLM 对初检结果进行精排，提高 Top-K 结果的相关性。
type Reranker struct {
	router         *llm.Router
	scoreThreshold float64
	logger         *zap.Logger
}

// NewReranker 创建重排序器
func NewReranker(router *llm.Router, scoreThreshold float64, logger *zap.Logger) *Reranker {
	return &Reranker{
		router:         router,
		scoreThreshold: scoreThreshold,
		logger:         logger,
	}
}

const rerankPrompt = `请对以下文档片段与查询的相关性进行评分（0-10分）。

查询：%s

文档片段：
%s

请以 JSON 数组格式返回每个文档的分数：[{"index": 0, "score": 8.5}, ...]`

type rerankScore struct {
	Index int     `json:"index"`
	Score float64 `json:"score"`
}

// Rerank 对检索结果进行重排序。
// 将查询和所有候选文档一起发送给 LLM，由 LLM 评估相关性并重新排序。
func (r *Reranker) Rerank(ctx context.Context, query string, refs []model.Reference) ([]model.Reference, error) {
	ctx, span := trace.StartSpan(ctx, "rag.rerank")
	defer span.End()
	if len(refs) <= 1 {
		return refs, nil
	}

	// 构造文档摘要列表
	var docsText string
	for i, ref := range refs {
		content := ref.Content
		if len([]rune(content)) > 200 {
			content = string([]rune(content)[:200]) + "..."
		}
		docsText += fmt.Sprintf("[%d] %s\n", i, content)
	}

	prompt := fmt.Sprintf(rerankPrompt, query, docsText)

	req := &model.LLMRequest{
		Messages: []model.LLMMessage{
			{Role: "user", Content: prompt},
		},
		Temperature: 0.1,
		MaxTokens:   500,
	}

	resp, err := r.router.Chat(ctx, req)
	if err != nil {
		trace.SetError(ctx, err)
		r.logger.Warn("Rerank LLM 调用失败，返回原始排序", zap.Error(err))
		return refs, nil
	}

	// 解析 LLM 返回的评分
	var scores []rerankScore
	if err := json.Unmarshal([]byte(resp.Content), &scores); err != nil {
		trace.SetError(ctx, err)
		r.logger.Warn("Rerank 结果解析失败，返回原始排序", zap.Error(err))
		return refs, nil
	}

	refs = applyRerankScores(refs, scores, r.scoreThreshold)

	r.logger.Info("Rerank 完成", zap.Int("doc_count", len(refs)))
	return refs, nil
}

func applyRerankScores(refs []model.Reference, scores []rerankScore, threshold float64) []model.Reference {
	filtered := make([]model.Reference, 0, len(scores))
	seen := make([]bool, len(refs))
	for _, score := range scores {
		if score.Index < 0 || score.Index >= len(refs) || seen[score.Index] {
			continue
		}
		seen[score.Index] = true
		normalized := score.Score / 10
		if normalized < 0 {
			normalized = 0
		} else if normalized > 1 {
			normalized = 1
		}
		if normalized >= threshold {
			ref := refs[score.Index]
			ref.Score = normalized
			filtered = append(filtered, ref)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].Score > filtered[j].Score
	})
	return filtered
}
