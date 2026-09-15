// Package database provides AgentGo's PostgreSQL-backed document index.
package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-ego/gse"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/model"
)

// Client owns the shared PostgreSQL connection pool used by RAG and tools.
type Client struct {
	db        *sql.DB
	segmenter *gse.Segmenter
}

// NewPostgresClient connects to PostgreSQL and creates the small document index schema.
func NewPostgresClient(ctx context.Context, cfg config.PostgresConfig) (*Client, error) {
	segmenter, err := newSegmenter()
	if err != nil {
		return nil, fmt.Errorf("加载中文分词词典失败: %w", err)
	}
	db, err := sql.Open("pgx", cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("打开 PostgreSQL 失败: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	client := &Client{db: db, segmenter: segmenter}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	if err := client.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := client.backfillTerms(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return client, nil
}

func (c *Client) migrate(ctx context.Context) error {
	statements := []string{`CREATE TABLE IF NOT EXISTS documents (
    id text PRIMARY KEY,
    title text NOT NULL,
    content_type text NOT NULL DEFAULT '',
    tags text[] NOT NULL DEFAULT '{}',
    metadata jsonb NOT NULL DEFAULT '{}',
    status text NOT NULL DEFAULT 'completed',
    chunk_count integer NOT NULL DEFAULT 0,
    content_hash text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
)`, `CREATE TABLE IF NOT EXISTS document_chunks (
    id text PRIMARY KEY,
    doc_id text NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
	content text NOT NULL,
	chunk_index integer NOT NULL,
	token_count integer NOT NULL DEFAULT -1,
	created_at timestamptz NOT NULL DEFAULT now(),
	UNIQUE (doc_id, chunk_index)
)`, `ALTER TABLE documents ADD COLUMN IF NOT EXISTS content_hash text NOT NULL DEFAULT ''`,
		`ALTER TABLE document_chunks ADD COLUMN IF NOT EXISTS token_count integer NOT NULL DEFAULT -1`,
		`CREATE TABLE IF NOT EXISTS document_terms (
	chunk_id text NOT NULL REFERENCES document_chunks(id) ON DELETE CASCADE,
	term varchar(128) NOT NULL,
	term_frequency integer NOT NULL CHECK (term_frequency > 0),
	PRIMARY KEY (chunk_id, term)
)`, `CREATE INDEX IF NOT EXISTS document_terms_term_idx ON document_terms(term)`,
		`CREATE INDEX IF NOT EXISTS document_chunks_doc_id_idx ON document_chunks(doc_id)`,
		`DROP INDEX IF EXISTS document_chunks_search_idx`,
		`ALTER TABLE document_chunks DROP COLUMN IF EXISTS search_vector`}
	for _, statement := range statements {
		if _, err := c.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("初始化 PostgreSQL schema 失败: %w", err)
		}
	}
	return nil
}

func (c *Client) backfillTerms(ctx context.Context) error {
	// ponytail: one startup transaction is enough for the current corpus; batch it when migration time becomes material.
	rows, err := c.db.QueryContext(ctx, `SELECT id, content FROM document_chunks WHERE token_count < 0`)
	if err != nil {
		return fmt.Errorf("查询待迁移文档分块失败: %w", err)
	}
	type chunk struct{ id, content string }
	var chunks []chunk
	for rows.Next() {
		var item chunk
		if err := rows.Scan(&item.id, &item.content); err != nil {
			rows.Close()
			return fmt.Errorf("读取待迁移文档分块失败: %w", err)
		}
		chunks = append(chunks, item)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("关闭待迁移文档分块结果失败: %w", err)
	}
	if len(chunks) == 0 {
		return nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始分词索引迁移事务失败: %w", err)
	}
	defer tx.Rollback()
	for _, item := range chunks {
		if err := c.replaceTerms(ctx, tx, item.id, tokenize(c.segmenter, item.content)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交分词索引迁移失败: %w", err)
	}
	return nil
}

// DB exposes the pool to the read-only database tool.
func (c *Client) DB() *sql.DB { return c.db }

func (c *Client) Close() error { return c.db.Close() }

func (c *Client) Healthy(ctx context.Context) bool { return c.db.PingContext(ctx) == nil }

// FindDocument returns the persisted processing status used by the document API.
func (c *Client) FindDocument(ctx context.Context, id string) (*model.DocumentResponse, error) {
	const query = `SELECT id, title, status, chunk_count, created_at, content_hash FROM documents WHERE id = $1`
	var doc model.DocumentResponse
	if err := c.db.QueryRowContext(ctx, query, id).Scan(&doc.DocID, &doc.Title, &doc.Status, &doc.ChunkCount, &doc.CreatedAt, &doc.ContentHash); err != nil {
		return nil, err
	}
	return &doc, nil
}

// IndexDocument atomically replaces one document and all its keyword-search chunks.
func (c *Client) IndexDocument(ctx context.Context, doc *model.Document, chunks []model.DocumentChunk) error {
	documentMetadata := doc.Metadata
	if documentMetadata == nil {
		documentMetadata = map[string]string{}
	}
	metadata, err := json.Marshal(documentMetadata)
	if err != nil {
		return fmt.Errorf("序列化文档 metadata 失败: %w", err)
	}
	tags := doc.Tags
	if tags == nil {
		tags = []string{}
	}
	chunkTerms := make([]map[string]int, len(chunks))
	for i, chunk := range chunks {
		chunkTerms[i] = tokenize(c.segmenter, chunk.Content)
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始 PostgreSQL 事务失败: %w", err)
	}
	defer tx.Rollback()

	const upsert = `INSERT INTO documents
    (id, title, content_type, tags, metadata, status, chunk_count, content_hash, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, 'completed', $6, $7, $8, $9)
ON CONFLICT (id) DO UPDATE SET
    title = EXCLUDED.title, content_type = EXCLUDED.content_type, tags = EXCLUDED.tags,
    metadata = EXCLUDED.metadata, status = EXCLUDED.status,
    chunk_count = EXCLUDED.chunk_count, content_hash = EXCLUDED.content_hash,
    updated_at = EXCLUDED.updated_at`
	if _, err := tx.ExecContext(ctx, upsert, doc.ID, doc.Title, doc.ContentType, tags, string(metadata), len(chunks), doc.ContentHash, doc.CreatedAt, doc.UpdatedAt); err != nil {
		return fmt.Errorf("保存文档失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM document_chunks WHERE doc_id = $1", doc.ID); err != nil {
		return fmt.Errorf("清理旧文档分块失败: %w", err)
	}
	const insertChunk = `INSERT INTO document_chunks (id, doc_id, content, chunk_index, token_count, created_at) VALUES ($1, $2, $3, $4, $5, $6)`
	for i, chunk := range chunks {
		if _, err := tx.ExecContext(ctx, insertChunk, chunk.ID, chunk.DocID, chunk.Content, chunk.ChunkIndex, termCount(chunkTerms[i]), chunk.CreatedAt); err != nil {
			return fmt.Errorf("保存文档分块 %q 失败: %w", chunk.ID, err)
		}
		if err := c.insertTerms(ctx, tx, chunk.ID, chunkTerms[i]); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 PostgreSQL 事务失败: %w", err)
	}
	return nil
}

func (c *Client) replaceTerms(ctx context.Context, tx *sql.Tx, chunkID string, terms map[string]int) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM document_terms WHERE chunk_id = $1`, chunkID); err != nil {
		return fmt.Errorf("清理分块 %q 的旧分词失败: %w", chunkID, err)
	}
	if err := c.insertTerms(ctx, tx, chunkID, terms); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE document_chunks SET token_count = $2 WHERE id = $1`, chunkID, termCount(terms)); err != nil {
		return fmt.Errorf("更新分块 %q 的词数失败: %w", chunkID, err)
	}
	return nil
}

func (*Client) insertTerms(ctx context.Context, tx *sql.Tx, chunkID string, terms map[string]int) error {
	const insert = `INSERT INTO document_terms (chunk_id, term, term_frequency) VALUES ($1, $2, $3)`
	for term, frequency := range terms {
		if _, err := tx.ExecContext(ctx, insert, chunkID, term, frequency); err != nil {
			return fmt.Errorf("保存分块 %q 的分词 %q 失败: %w", chunkID, term, err)
		}
	}
	return nil
}

func termCount(terms map[string]int) int {
	total := 0
	for _, frequency := range terms {
		total += frequency
	}
	return total
}

// Search segments the query and ranks matching chunks with BM25 (k1=1.2, b=0.75).
func (c *Client) Search(ctx context.Context, query string, topK int) ([]model.Reference, error) {
	if topK <= 0 {
		return nil, fmt.Errorf("topK 必须大于 0")
	}
	termFrequency := tokenize(c.segmenter, query)
	if len(termFrequency) == 0 {
		return nil, nil
	}
	terms := make([]string, 0, len(termFrequency))
	for term := range termFrequency {
		terms = append(terms, term)
	}
	// ponytail: corpus statistics are computed on demand; materialize them only after query profiling shows a bottleneck.
	const search = `WITH query_terms AS (
    SELECT DISTINCT unnest($1::text[]) AS term
), corpus AS (
    SELECT COUNT(*)::float8 AS document_count,
           COALESCE(AVG(token_count), 0)::float8 AS average_length
    FROM document_chunks
    WHERE token_count >= 0
), document_frequency AS (
    SELECT dt.term, COUNT(*)::float8 AS document_frequency
    FROM document_terms dt
    JOIN query_terms q ON q.term = dt.term
    GROUP BY dt.term
)
SELECT c.id, c.doc_id, d.title, c.content,
       SUM(
           LN(1.0 + (corpus.document_count - df.document_frequency + 0.5) / (df.document_frequency + 0.5))
           * (dt.term_frequency * 2.2)
           / (dt.term_frequency + 1.2 * (0.25 + 0.75 * c.token_count / NULLIF(corpus.average_length, 0)))
       )::float8 AS score
FROM document_chunks c
JOIN documents d ON d.id = c.doc_id
JOIN document_terms dt ON dt.chunk_id = c.id
JOIN document_frequency df ON df.term = dt.term
CROSS JOIN corpus
WHERE d.status = 'completed' AND c.token_count > 0
GROUP BY c.id, c.doc_id, d.title, c.content, c.chunk_index, corpus.document_count, corpus.average_length
ORDER BY score DESC, c.chunk_index ASC, c.id ASC
LIMIT $2`
	rows, err := c.db.QueryContext(ctx, search, terms, topK)
	if err != nil {
		return nil, fmt.Errorf("PostgreSQL 关键词检索失败: %w", err)
	}
	defer rows.Close()

	refs := make([]model.Reference, 0, topK)
	for rows.Next() {
		var ref model.Reference
		if err := rows.Scan(&ref.ChunkID, &ref.DocID, &ref.Title, &ref.Content, &ref.Score); err != nil {
			return nil, fmt.Errorf("读取关键词检索结果失败: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历关键词检索结果失败: %w", err)
	}
	return refs, nil
}
