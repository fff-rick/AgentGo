package etl

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

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
	mu       sync.Mutex
}

// DocumentIndexer persists chunks for PostgreSQL keyword search.
type DocumentIndexer interface {
	FindDocument(ctx context.Context, id string) (*model.DocumentResponse, error)
	IndexDocument(ctx context.Context, doc *model.Document, chunks []model.DocumentChunk) error
}

// NewPipeline 创建 ETL 流水线
func NewPipeline(parser Parser, chunker *Chunker, vectorDB vectordb.VectorDB, embedder embedding.Client, logger *zap.Logger, indexer DocumentIndexer) *Pipeline {
	return &Pipeline{
		parser:   parser,
		chunker:  chunker,
		vectorDB: vectorDB,
		embedder: embedder,
		logger:   logger,
		indexer:  indexer,
	}
}

// ProcessDocument 处理单个文档：解析 → 分块 → 向量化 → 入库
func (p *Pipeline) ProcessDocument(ctx context.Context, doc *model.Document) (*model.DocumentResponse, error) {
	// ponytail: a global lock is sufficient for the current single-instance importer;
	// use per-document/distributed locks when concurrent import throughput requires it.
	p.mu.Lock()
	defer p.mu.Unlock()

	startTime := time.Now()
	if p.indexer == nil {
		return nil, fmt.Errorf("文档索引存储未配置")
	}
	doc.Title = strings.TrimSpace(doc.Title)
	if doc.Title == "" {
		return nil, fmt.Errorf("文档标题不能为空")
	}
	doc.ContentType = normalizeContentType(doc.ContentType)
	doc.ID = DocumentID(doc.ContentType, doc.Title)
	doc.ContentHash = ContentHash(doc.Content)

	existing, err := p.indexer.FindDocument(ctx, doc.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("查询现有文档失败: %w", err)
	}
	if err == nil {
		if existing.ContentHash == doc.ContentHash {
			return existing, nil
		}
		doc.CreatedAt = existing.CreatedAt
	}
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
	for i, chunk := range parsedChunks {
		chunkID := stableChunkID(doc.ID, chunk.ChunkIndex)
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
		keywordChunks = append(keywordChunks, model.DocumentChunk{
			ID: chunkID, DocID: doc.ID, Content: chunk.Content,
			ChunkIndex: chunk.ChunkIndex, CreatedAt: time.Now(),
		})
	}

	if err := p.vectorDB.Insert(ctx, "", records); err != nil {
		p.logger.Error("向量入库失败，可重复导入修复", zap.Error(err))
		return nil, fmt.Errorf("向量入库失败: %w", err)
	}
	if existing != nil && existing.ChunkCount > len(records) {
		staleIDs := make([]string, 0, existing.ChunkCount-len(records))
		for i := len(records); i < existing.ChunkCount; i++ {
			staleIDs = append(staleIDs, stableChunkID(doc.ID, i))
		}
		if err := p.vectorDB.Delete(ctx, "", staleIDs); err != nil {
			p.logger.Error("清理旧向量失败，可重复导入修复", zap.Error(err))
			return nil, fmt.Errorf("清理旧向量失败: %w", err)
		}
	}
	if err := p.indexer.IndexDocument(ctx, doc, keywordChunks); err != nil {
		p.logger.Error("关键词索引入库失败，可重复导入修复", zap.Error(err))
		return nil, fmt.Errorf("关键词索引入库失败: %w", err)
	}

	elapsed := time.Since(startTime)
	p.logger.Info("文档处理完成",
		zap.String("doc_id", doc.ID),
		zap.Int("chunks", len(parsedChunks)),
		zap.Duration("elapsed", elapsed),
	)

	return &model.DocumentResponse{
		DocID:       doc.ID,
		Title:       doc.Title,
		Status:      "completed",
		ChunkCount:  len(parsedChunks),
		CreatedAt:   doc.CreatedAt,
		ContentHash: doc.ContentHash,
	}, nil
}

// DocumentID identifies one logical document by normalized type and trimmed title.
func DocumentID(contentType, title string) string {
	sum := sha256.Sum256([]byte(normalizeContentType(contentType) + "\x00" + strings.TrimSpace(title)))
	return fmt.Sprintf("doc-%x", sum)
}

// ContentHash identifies the exact raw content version of a document.
func ContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", sum)
}

func normalizeContentType(contentType string) string {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	if contentType == "" {
		return string(DocTypeText)
	}
	return contentType
}

func stableChunkID(docID string, index int) string {
	sum := sha256.Sum256([]byte(docID + "\x00" + strconv.Itoa(index)))
	return fmt.Sprintf("chunk-%x", sum)
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
