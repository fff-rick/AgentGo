// Package rag 提供检索增强生成（Retrieval-Augmented Generation）的核心组件。
package rag

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/cache"
	"github.com/enterprise/ai-agent-go/internal/embedding"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/vectordb"
)

// RetrievalMode 检索模式
type RetrievalMode int

const (
	// ModeVector 向量检索
	ModeVector RetrievalMode = iota
	// ModeKeyword 关键词检索
	ModeKeyword
	// ModeHybrid 混合检索（向量 + 关键词 + 融合排序）
	ModeHybrid
)

// Retriever 多路检索引擎。
// 支持向量检索、关键词检索和混合检索三种模式，通过 RRF 算法融合多路结果。
type Retriever struct {
	vectorDB       vectordb.VectorDB
	embedder       embedding.Client
	keywordStore   KeywordSearcher
	cache          cache.Cache
	scoreThreshold float64
	logger         *zap.Logger
}

// KeywordSearcher is implemented by the PostgreSQL document index.
type KeywordSearcher interface {
	Search(ctx context.Context, query string, topK int) ([]model.Reference, error)
}

// NewRetriever 创建多路检索引擎
func NewRetriever(vectorDB vectordb.VectorDB, embedder embedding.Client, cache cache.Cache, scoreThreshold float64, logger *zap.Logger, keywordStores ...KeywordSearcher) *Retriever {
	r := &Retriever{
		vectorDB:       vectorDB,
		embedder:       embedder,
		cache:          cache,
		scoreThreshold: scoreThreshold,
		logger:         logger,
	}
	if len(keywordStores) > 0 {
		r.keywordStore = keywordStores[0]
	}
	return r
}

// Retrieve 默认执行 Milvus + PostgreSQL 混合检索。
func (r *Retriever) Retrieve(ctx context.Context, query string, topK int) ([]model.Reference, error) {
	return r.RetrieveWithMode(ctx, query, topK, ModeHybrid)
}

// RetrieveWithMode 使用指定模式执行检索
func (r *Retriever) RetrieveWithMode(ctx context.Context, query string, topK int, mode RetrievalMode) ([]model.Reference, error) {
	switch mode {
	case ModeVector:
		return r.vectorSearch(ctx, query, topK)
	case ModeKeyword:
		return r.keywordSearch(ctx, query, topK)
	case ModeHybrid:
		return r.hybridSearch(ctx, query, topK)
	default:
		return r.hybridSearch(ctx, query, topK)
	}
}

// vectorSearch 向量相似度检索
func (r *Retriever) vectorSearch(ctx context.Context, query string, topK int) ([]model.Reference, error) {
	queryVector, err := r.embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("查询向量化失败: %w", err)
	}

	results, err := r.vectorDB.Search(ctx, "", queryVector, topK)
	if err != nil {
		r.logger.Error("向量检索失败", zap.Error(err))
		return nil, err
	}

	refs := make([]model.Reference, 0, len(results))
	var topScore float64
	for i, result := range results {
		if i == 0 || result.Score > topScore {
			topScore = result.Score
		}
		if result.Score < r.scoreThreshold {
			continue
		}
		docID := result.Metadata["doc_id"]
		if docID == "" {
			docID = result.ID
		}
		refs = append(refs, model.Reference{
			ChunkID: result.ID,
			DocID:   docID,
			Title:   result.Metadata["title"],
			Content: result.Content,
			Score:   result.Score,
		})
	}
	r.logger.Info("向量召回完成",
		zap.Int("candidates", len(results)),
		zap.Int("accepted", len(refs)),
		zap.Float64("top_score", topScore),
		zap.Float64("score_threshold", r.scoreThreshold),
	)

	return refs, nil
}

// keywordSearch 关键词全文检索
func (r *Retriever) keywordSearch(ctx context.Context, query string, topK int) ([]model.Reference, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("关键词不能为空")
	}
	if r.keywordStore == nil {
		return nil, fmt.Errorf("PostgreSQL 关键词索引未配置")
	}
	r.logger.Info("关键词检索",
		zap.String("query", query),
		zap.Int("top_k", topK),
	)
	return r.keywordStore.Search(ctx, query, topK)
}

// hybridSearch 混合检索：并发执行向量检索和关键词检索，然后融合排序
func (r *Retriever) hybridSearch(ctx context.Context, query string, topK int) ([]model.Reference, error) {
	var (
		vectorResults  []model.Reference
		keywordResults []model.Reference
		vectorErr      error
		keywordErr     error
		wg             sync.WaitGroup
	)

	// 并发执行两种检索
	wg.Add(2)

	go func() {
		defer wg.Done()
		vectorResults, vectorErr = r.vectorSearch(ctx, query, topK*2)
	}()

	go func() {
		defer wg.Done()
		keywordResults, keywordErr = r.keywordSearch(ctx, query, topK*2)
	}()

	wg.Wait()

	// 容忍单路检索失败
	if vectorErr != nil {
		r.logger.Warn("混合检索中向量检索失败，仅使用关键词结果", zap.Error(vectorErr))
	}
	if keywordErr != nil {
		r.logger.Warn("混合检索中关键词检索失败，仅使用向量结果", zap.Error(keywordErr))
	}
	if len(keywordResults) == 0 {
		return truncateReferences(vectorResults, topK), vectorErr
	}
	if len(vectorResults) == 0 {
		return truncateReferences(keywordResults, topK), keywordErr
	}

	// 使用 RRF 算法融合两路结果
	merged := r.reciprocalRankFusion(vectorResults, keywordResults)

	// 截断到 topK
	if len(merged) > topK {
		merged = merged[:topK]
	}

	return merged, nil
}

func truncateReferences(refs []model.Reference, topK int) []model.Reference {
	if topK >= 0 && len(refs) > topK {
		return refs[:topK]
	}
	return refs
}

// reciprocalRankFusion 倒数排名融合算法（RRF）。
// 将多路检索结果按照 RRF 公式进行分数融合：score = Σ 1/(k + rank_i)。
// k 是平滑常数，通常取 60。
func (r *Retriever) reciprocalRankFusion(lists ...[]model.Reference) []model.Reference {
	const k = 60.0
	scores := make(map[string]float64)
	docs := make(map[string]model.Reference)

	for _, list := range lists {
		for rank, ref := range list {
			id := referenceKey(ref)
			scores[id] += 1.0 / (k + float64(rank+1))
			docs[id] = ref
		}
	}

	// 按融合分数降序排列
	result := make([]model.Reference, 0, len(docs))
	for id, ref := range docs {
		ref.Score = scores[id]
		result = append(result, ref)
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].Score == result[j].Score {
			return referenceKey(result[i]) < referenceKey(result[j])
		}
		return result[i].Score > result[j].Score
	})

	return result
}

func referenceKey(ref model.Reference) string {
	if ref.ChunkID != "" {
		return ref.ChunkID
	}
	return ref.DocID + "\x00" + ref.Content
}
