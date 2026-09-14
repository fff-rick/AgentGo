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

	docID, chunkID, now := uuid.NewString(), uuid.NewString(), time.Now()
	marker := "agentgotest" + strings.ReplaceAll(uuid.NewString(), "-", "")
	defer client.DB().ExecContext(context.Background(), "DELETE FROM documents WHERE id = $1", docID)
	doc := &model.Document{ID: docID, Title: "Hybrid Test", ContentType: "text", CreatedAt: now, UpdatedAt: now}
	chunks := []model.DocumentChunk{{ID: chunkID, DocID: docID, Content: "PostgreSQL 提供混合检索关键词召回 " + marker, ChunkIndex: 0, CreatedAt: now}}
	if err := client.IndexDocument(ctx, doc, chunks); err != nil {
		t.Fatal(err)
	}
	refs, err := client.Search(ctx, marker, 5)
	found := false
	for _, ref := range refs {
		if ref.ChunkID == chunkID {
			found = true
			break
		}
	}
	if err != nil || !found {
		t.Fatalf("Search() refs = %#v, error = %v", refs, err)
	}

	dbTool := toolbuiltin.NewDatabaseTool(client.DB(), cfg.Postgres, zap.NewNop())
	input, _ := json.Marshal(map[string]string{"sql": fmt.Sprintf("SELECT title FROM documents WHERE id = '%s'", docID)})
	result, err := dbTool.Execute(ctx, string(input))
	if err != nil || !result.Success || result.Output == "" {
		t.Fatalf("database tool result = %#v, error = %v", result, err)
	}
}
