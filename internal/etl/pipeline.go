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
// 完整流程：原始文档 → 解析 → 分块 → 向量化 → 写入 Milvus 和 PostgreSQL。
type Pipeline struct {
	parser   Parser
	chunker  *Chunker
	vectorDB vectordb.VectorDB
	embedder embedding.Client
	indexer  DocumentIndexer
	logger   *zap.Logger
}

// DocumentIndexer persists chunks for PostgreSQL keyword search.
type DocumentIndexer interface {
	IndexDocument(ctx context.Context, doc *model.Document, chunks []model.DocumentChunk) error
}

// NewPipeline 创建 ETL 流水线
func NewPipeline(parser Parser, chunker *Chunker, vectorDB vectordb.VectorDB, embedder embedding.Client, logger *zap.Logger, indexers ...DocumentIndexer) *Pipeline {
	p := &Pipeline{
		parser:   parser,
		chunker:  chunker,
		vectorDB: vectorDB,
		embedder: embedder,
		logger:   logger,
	}
	if len(indexers) > 0 {
		p.indexer = indexers[0]
	}
	return p
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
	parsedChunks := p.chunker.Split(parsed.Content, StrategySentence)
	if len(parsedChunks) == 0 {
		return nil, fmt.Errorf("文档分块结果为空")
	}

	p.logger.Info("文档分块完成",
		zap.String("doc_id", doc.ID),
		zap.Int("chunk_count", len(parsedChunks)),
	)

	// 阶段三：向量化并入库
	texts := make([]string, len(parsedChunks))
	for i, chunk := range parsedChunks {
		texts[i] = chunk.Content
	}
	embeddings, err := p.embedder.EmbedBatch(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("文档向量化失败: %w", err)
	}

	records := make([]vectordb.VectorRecord, 0, len(parsedChunks))
	keywordChunks := make([]model.DocumentChunk, 0, len(parsedChunks))
	chunkIDs := make([]string, 0, len(parsedChunks))
	for i, chunk := range parsedChunks {
		chunkID := uuid.New().String()
		record := vectordb.VectorRecord{
			ID:        chunkID,
			Content:   chunk.Content,
			Embedding: embeddings[i],
			Metadata: map[string]string{
				"doc_id":      doc.ID,
				"title":       doc.Title,
				"chunk_index": fmt.Sprintf("%d", chunk.ChunkIndex),
			},
		}
		records = append(records, record)
		chunkIDs = append(chunkIDs, chunkID)
		keywordChunks = append(keywordChunks, model.DocumentChunk{
			ID: chunkID, DocID: doc.ID, Content: chunk.Content,
			ChunkIndex: chunk.ChunkIndex, CreatedAt: time.Now(),
		})
	}

	if err := p.vectorDB.Insert(ctx, "", records); err != nil {
		p.logger.Error("向量入库失败", zap.Error(err))
		return nil, fmt.Errorf("向量入库失败: %w", err)
	}
	if p.indexer != nil {
		if err := p.indexer.IndexDocument(ctx, doc, keywordChunks); err != nil {
			p.logger.Error("关键词索引入库失败", zap.Error(err))
			if cleanupErr := p.vectorDB.Delete(ctx, "", chunkIDs); cleanupErr != nil {
				p.logger.Error("回滚 Milvus 文档分块失败", zap.Error(cleanupErr))
			}
			return nil, fmt.Errorf("关键词索引入库失败: %w", err)
		}
	}

	elapsed := time.Since(startTime)
	p.logger.Info("文档处理完成",
		zap.String("doc_id", doc.ID),
		zap.Int("chunks", len(parsedChunks)),
		zap.Duration("elapsed", elapsed),
	)

	return &model.DocumentResponse{
		DocID:      doc.ID,
		Title:      doc.Title,
		Status:     "completed",
		ChunkCount: len(parsedChunks),
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
