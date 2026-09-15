package database_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/database"
	"github.com/enterprise/ai-agent-go/internal/model"
	toolbuiltin "github.com/enterprise/ai-agent-go/internal/tool/builtin"
)

func TestPostgresKeywordSearchAndDatabaseToolIntegration(t *testing.T) {
	if os.Getenv("POSTGRES_INTEGRATION") != "1" {
		t.Skip("set POSTGRES_INTEGRATION=1 to run")
	}
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := database.NewPostgresClient(ctx, cfg.Postgres)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	docID, chunkID, secondChunkID, now := uuid.NewString(), uuid.NewString(), uuid.NewString(), time.Now().UTC().Truncate(time.Microsecond)
	marker := "agentgotest" + strings.ReplaceAll(uuid.NewString(), "-", "")
	defer client.DB().ExecContext(context.Background(), "DELETE FROM documents WHERE id = $1", docID)
	doc := &model.Document{ID: docID, Title: "Hybrid Test", ContentType: "text", ContentHash: "hash-v1", CreatedAt: now, UpdatedAt: now}
	chunks := []model.DocumentChunk{
		{ID: chunkID, DocID: docID, Content: "PostgreSQL 提供中文分词和 BM25 混合检索 " + marker + " " + marker + " " + marker, ChunkIndex: 0, CreatedAt: now},
		{ID: secondChunkID, DocID: docID, Content: "PostgreSQL 提供中文分词和 BM25 混合检索 " + marker, ChunkIndex: 1, CreatedAt: now},
	}
	if err := client.IndexDocument(ctx, doc, chunks); err != nil {
		t.Fatal(err)
	}
	var indexedMarker string
	if err := client.DB().QueryRowContext(ctx, `SELECT term FROM document_terms WHERE chunk_id = $1 AND term = $2`, chunkID, marker).Scan(&indexedMarker); err != nil {
		t.Fatalf("BM25 term index missing marker %q: %v", marker, err)
	}
	var matchingTerms int
	if err := client.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM document_terms WHERE term = ANY($1::text[])`, []string{marker}).Scan(&matchingTerms); err != nil || matchingTerms < 2 {
		t.Fatalf("BM25 query term lookup count = %d, error = %v", matchingTerms, err)
	}
	refs, err := client.Search(ctx, marker, 5)
	if err != nil || !containsChunk(refs, chunkID) {
		t.Fatalf("Search() refs = %#v, error = %v", refs, err)
	}
	if len(refs) < 2 || refs[0].ChunkID != chunkID || refs[0].Score <= refs[1].Score {
		t.Fatalf("BM25 ranking refs = %#v, want repeated term chunk first", refs)
	}
	chineseRefs, err := client.Search(ctx, "中文分词", 20)
	if err != nil || !containsChunk(chineseRefs, chunkID) {
		t.Fatalf("Chinese BM25 refs = %#v, error = %v", chineseRefs, err)
	}

	dbTool := toolbuiltin.NewDatabaseTool(client.DB(), cfg.Postgres, zap.NewNop())
	input, _ := json.Marshal(map[string]string{"sql": fmt.Sprintf("SELECT title FROM documents WHERE id = '%s'", docID)})
	result, err := dbTool.Execute(ctx, string(input))
	if err != nil || !result.Success || result.Output == "" {
		t.Fatalf("database tool result = %#v, error = %v", result, err)
	}

	doc.ContentHash = "hash-v2"
	doc.CreatedAt = now.Add(24 * time.Hour)
	doc.UpdatedAt = now.Add(time.Hour)
	if err := client.IndexDocument(ctx, doc, chunks[:1]); err != nil {
		t.Fatal(err)
	}
	var contentHash string
	var chunkCount int
	var createdAt, updatedAt time.Time
	if err := client.DB().QueryRowContext(ctx, `SELECT content_hash, chunk_count, created_at, updated_at FROM documents WHERE id = $1`, docID).
		Scan(&contentHash, &chunkCount, &createdAt, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if contentHash != "hash-v2" || chunkCount != 1 || !createdAt.Equal(now) || !updatedAt.Equal(doc.UpdatedAt) {
		t.Fatalf("updated document hash=%q chunks=%d created_at=%v updated_at=%v", contentHash, chunkCount, createdAt, updatedAt)
	}
}

func containsChunk(refs []model.Reference, chunkID string) bool {
	for _, ref := range refs {
		if ref.ChunkID == chunkID {
			return true
		}
	}
	return false
}
