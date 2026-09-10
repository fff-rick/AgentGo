package etl

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/embedding"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/vectordb"
)

// Pipeline 文档 ETL 流水线。
// 完整流程：原始文档 → 解析 → 分块 → 向量化 → 存入向量数据库。
type Pipeline struct {
	parser   Parser
	chunker  *Chunker
	vectorDB vectordb.VectorDB
	embedder embedding.Client
	logger   *zap.Logger
}

// NewPipeline 创建 ETL 流水线
func NewPipeline(parser Parser, chunker *Chunker, vectorDB vectordb.VectorDB, embedder embedding.Client, logger *zap.Logger) *Pipeline {
	return &Pipeline{
		parser:   parser,
		chunker:  chunker,
		vectorDB: vectorDB,
		embedder: embedder,
		logger:   logger,
	}
}

// ProcessDocument 处理单个文档：解析 → 分块 → 向量化 → 入库
func (p *Pipeline) ProcessDocument(ctx context.Context, doc *model.Document) (*model.DocumentResponse, error) {
	startTime := time.Now()
	p.logger.Info("开始处理文档",
		zap.String("doc_id", doc.ID),
		zap.String("title", doc.Title),
	)

	// 阶段一：解析文档
	parsed, err := p.parser.Parse(ctx, doc.Content, DocumentType(doc.ContentType))
	if err != nil {
		return nil, fmt.Errorf("文档解析失败: %w", err)
	}

	// 阶段二：文档分块
	chunks := p.chunker.Split(parsed.Content, StrategySentence)
	if len(chunks) == 0 {
		return nil, fmt.Errorf("文档分块结果为空")
	}

	p.logger.Info("文档分块完成",
		zap.String("doc_id", doc.ID),
		zap.Int("chunk_count", len(chunks)),
	)

	// 阶段三：向量化并入库
	texts := make([]string, len(chunks))
	for i, chunk := range chunks {
		texts[i] = chunk.Content
	}
	embeddings, err := p.embedder.EmbedBatch(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("文档向量化失败: %w", err)
	}

	records := make([]vectordb.VectorRecord, 0, len(chunks))
	for i, chunk := range chunks {
		record := vectordb.VectorRecord{
			ID:        uuid.New().String(),
			Content:   chunk.Content,
			Embedding: embeddings[i],
			Metadata: map[string]string{
				"doc_id":      doc.ID,
				"title":       doc.Title,
				"chunk_index": fmt.Sprintf("%d", chunk.ChunkIndex),
			},
		}
		records = append(records, record)
	}

	if err := p.vectorDB.Insert(ctx, "", records); err != nil {
		p.logger.Error("向量入库失败", zap.Error(err))
		return nil, fmt.Errorf("向量入库失败: %w", err)
	}

	elapsed := time.Since(startTime)
	p.logger.Info("文档处理完成",
		zap.String("doc_id", doc.ID),
		zap.Int("chunks", len(chunks)),
		zap.Duration("elapsed", elapsed),
	)

	return &model.DocumentResponse{
		DocID:      doc.ID,
		Title:      doc.Title,
		Status:     "completed",
		ChunkCount: len(chunks),
		CreatedAt:  time.Now(),
	}, nil
}

// ProcessBatch 批量处理文档
func (p *Pipeline) ProcessBatch(ctx context.Context, docs []*model.Document) ([]*model.DocumentResponse, error) {
	results := make([]*model.DocumentResponse, 0, len(docs))

	for _, doc := range docs {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}

		resp, err := p.ProcessDocument(ctx, doc)
		if err != nil {
			p.logger.Error("批量处理中单个文档失败",
				zap.String("doc_id", doc.ID),
				zap.Error(err),
			)
			results = append(results, &model.DocumentResponse{
				DocID:  doc.ID,
				Title:  doc.Title,
				Status: "failed",
			})
			continue
		}
		results = append(results, resp)
	}

	return results, nil
}
