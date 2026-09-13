// Package database provides AgentGo's PostgreSQL-backed document index.
package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/model"
)

// Client owns the shared PostgreSQL connection pool used by RAG and tools.
type Client struct {
	db *sql.DB
}

// NewPostgresClient connects to PostgreSQL and creates the small document index schema.
func NewPostgresClient(ctx context.Context, cfg config.PostgresConfig) (*Client, error) {
	db, err := sql.Open("pgx", cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("打开 PostgreSQL 失败: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	client := &Client{db: db}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	if err := client.migrate(ctx); err != nil {
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
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
)`, `CREATE TABLE IF NOT EXISTS document_chunks (
    id text PRIMARY KEY,
    doc_id text NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    content text NOT NULL,
    chunk_index integer NOT NULL,
    search_vector tsvector GENERATED ALWAYS AS (to_tsvector('simple', content)) STORED,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (doc_id, chunk_index)
)`, `CREATE INDEX IF NOT EXISTS document_chunks_search_idx ON document_chunks USING gin(search_vector)`,
		`CREATE INDEX IF NOT EXISTS document_chunks_doc_id_idx ON document_chunks(doc_id)`}
	for _, statement := range statements {
		if _, err := c.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("初始化 PostgreSQL schema 失败: %w", err)
		}
	}
	return nil
}

// DB exposes the pool to the read-only database tool.
func (c *Client) DB() *sql.DB { return c.db }

func (c *Client) Close() error { return c.db.Close() }

func (c *Client) Healthy(ctx context.Context) bool { return c.db.PingContext(ctx) == nil }

// FindDocument returns the persisted processing status used by the document API.
func (c *Client) FindDocument(ctx context.Context, id string) (*model.DocumentResponse, error) {
	const query = `SELECT id, title, status, chunk_count, created_at FROM documents WHERE id = $1`
	var doc model.DocumentResponse
	if err := c.db.QueryRowContext(ctx, query, id).Scan(&doc.DocID, &doc.Title, &doc.Status, &doc.ChunkCount, &doc.CreatedAt); err != nil {
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
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始 PostgreSQL 事务失败: %w", err)
	}
	defer tx.Rollback()

	const upsert = `INSERT INTO documents
    (id, title, content_type, tags, metadata, status, chunk_count, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, 'completed', $6, $7, $8)
ON CONFLICT (id) DO UPDATE SET
    title = EXCLUDED.title, content_type = EXCLUDED.content_type, tags = EXCLUDED.tags,
    metadata = EXCLUDED.metadata, status = EXCLUDED.status,
    chunk_count = EXCLUDED.chunk_count, updated_at = EXCLUDED.updated_at`
	if _, err := tx.ExecContext(ctx, upsert, doc.ID, doc.Title, doc.ContentType, tags, string(metadata), len(chunks), doc.CreatedAt, doc.UpdatedAt); err != nil {
		return fmt.Errorf("保存文档失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM document_chunks WHERE doc_id = $1", doc.ID); err != nil {
		return fmt.Errorf("清理旧文档分块失败: %w", err)
	}
	const insertChunk = `INSERT INTO document_chunks (id, doc_id, content, chunk_index, created_at) VALUES ($1, $2, $3, $4, $5)`
	for _, chunk := range chunks {
		if _, err := tx.ExecContext(ctx, insertChunk, chunk.ID, chunk.DocID, chunk.Content, chunk.ChunkIndex, chunk.CreatedAt); err != nil {
			return fmt.Errorf("保存文档分块 %q 失败: %w", chunk.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 PostgreSQL 事务失败: %w", err)
	}
	return nil
}

// Search performs PostgreSQL full-text search, with an exact substring fallback
// so unsegmented Chinese queries also produce useful matches.
func (c *Client) Search(ctx context.Context, query string, topK int) ([]model.Reference, error) {
	if topK <= 0 {
		return nil, fmt.Errorf("topK 必须大于 0")
	}
	const search = `WITH q AS (SELECT websearch_to_tsquery('simple', $1) AS value)
SELECT c.id, c.doc_id, d.title, c.content,
       GREATEST(ts_rank_cd(c.search_vector, q.value),
                CASE WHEN c.content ILIKE '%' || $1 || '%' THEN 1.0 ELSE 0.0 END)::float8 AS score
FROM document_chunks c
JOIN documents d ON d.id = c.doc_id
CROSS JOIN q
WHERE d.status = 'completed'
  AND (c.search_vector @@ q.value OR c.content ILIKE '%' || $1 || '%')
ORDER BY score DESC, c.chunk_index ASC
LIMIT $2`
	rows, err := c.db.QueryContext(ctx, search, query, topK)
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
