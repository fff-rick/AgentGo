package memory

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/database"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/vectordb"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

func TestManagedMemoryMilvusPostgresIntegration(t *testing.T) {
	if os.Getenv("POSTGRES_INTEGRATION") != "1" || os.Getenv("MILVUS_INTEGRATION") != "1" {
		t.Skip("set POSTGRES_INTEGRATION=1 and MILVUS_INTEGRATION=1 to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.NewPostgresClient(ctx, cfg.Postgres)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	vectors, err := vectordb.NewMilvusClient(config.MilvusConfig{Addr: cfg.Milvus.Addr, Username: cfg.Milvus.Username, Password: cfg.Milvus.Password, Database: cfg.Milvus.Database, CollectionName: "agentgo_managed_memory_integration", Dimension: 2, MetricType: "COSINE", ConnectTimeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer vectors.Close()
	store, err := NewManagedStore(ctx, db.DB(), NewSemanticStore(vectors, embeddingStub{}, "agentgo_managed_memory_integration"))
	if err != nil {
		t.Fatal(err)
	}
	userID := uuid.NewString()
	defer func() {
		db.DB().ExecContext(context.Background(), `DELETE FROM memory_jobs WHERE user_id=$1`, userID)
		db.DB().ExecContext(context.Background(), `DELETE FROM memory_versions WHERE memory_id IN (SELECT id FROM memories WHERE user_id=$1)`, userID)
		db.DB().ExecContext(context.Background(), `DELETE FROM memories WHERE user_id=$1`, userID)
	}()
	item := model.MemoryItem{UserID: userID, Kind: model.MemoryPreference, Topic: "language", Content: "喜欢 Go", Importance: 0.9, Confidence: 0.9, SourceSessionID: "s", SourceMessageID: uuid.NewString(), CreatedAt: time.Now()}
	if err := store.Save(ctx, item); err != nil {
		t.Fatal(err)
	}
	list, err := store.List(ctx, userID, 10, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	id := list[0].ID
	runner := NewJobRunner(store, nil, zap.NewNop())
	defer runner.Close(context.Background())
	if err := runner.run(ctx, memoryJob{kind: "index", userID: userID, payload: id, version: list[0].Version}); err != nil {
		t.Fatal(err)
	}
	recalled, err := store.Search(ctx, userID, "Go", 5)
	if err != nil || len(recalled) != 1 || recalled[0].ID != id {
		t.Fatalf("recall=%+v err=%v", recalled, err)
	}
	if err := store.Delete(ctx, userID, id); err != nil {
		t.Fatal(err)
	}
	if recalled, err = store.Search(ctx, userID, "Go", 5); err != nil || len(recalled) != 0 {
		t.Fatalf("stale vector leaked deleted memory: %+v %v", recalled, err)
	}
	if err := runner.run(ctx, memoryJob{kind: "index", userID: userID, payload: id, version: list[0].Version + 1}); err != nil {
		t.Fatal(err)
	}
}
