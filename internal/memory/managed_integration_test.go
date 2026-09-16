package memory

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/database"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/vectordb"
	"github.com/google/uuid"
)

func TestManagedMemoryPostgresIntegration(t *testing.T) {
	if os.Getenv("POSTGRES_INTEGRATION") != "1" {
		t.Skip("set POSTGRES_INTEGRATION=1 to run")
	}
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := database.NewPostgresClient(ctx, cfg.Postgres)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	userID := uuid.NewString()
	defer func() {
		db := client.DB()
		db.ExecContext(context.Background(), `DELETE FROM memory_jobs WHERE user_id=$1`, userID)
		db.ExecContext(context.Background(), `DELETE FROM memory_versions WHERE memory_id IN (SELECT id FROM memories WHERE user_id=$1)`, userID)
		db.ExecContext(context.Background(), `DELETE FROM memories WHERE user_id=$1`, userID)
	}()
	vectors := &filteredVectorsStub{}
	store, err := NewManagedStore(ctx, client.DB(), NewSemanticStore(vectors, embeddingStub{}, "managed_memory_test"))
	if err != nil {
		t.Fatal(err)
	}
	runner := NewJobRunner(store, nil, nil)
	defer runner.Close(context.Background())
	messageID := uuid.NewString()
	for range 2 {
		if err := runner.Enqueue(ctx, userID, "s", messageID, "我喜欢浅色主题", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	var queued int
	if err := client.DB().QueryRowContext(ctx, `SELECT count(*) FROM memory_jobs WHERE user_id=$1 AND kind='extract' AND message_id=$2`, userID, messageID).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("queued=%d err=%v", queued, err)
	}
	base := time.Now().Add(-time.Hour)
	newItem := func(content string, at time.Time) model.MemoryItem {
		return model.MemoryItem{UserID: userID, Kind: model.MemoryPreference, Topic: "theme", Content: content, Importance: 0.9, Confidence: 0.9, SourceSessionID: "s", SourceMessageID: uuid.NewString(), CreatedAt: at}
	}
	items := []model.MemoryItem{newItem("喜欢深色主题", base), newItem("现在喜欢浅色主题", base.Add(time.Minute))}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, item := range items {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- store.Save(ctx, item) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	list, err := store.List(ctx, userID, 10, 0)
	if err != nil || len(list) != 1 || list[0].Content != "现在喜欢浅色主题" {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	id := list[0].ID
	if _, err := store.Correct(ctx, userID, id, "只喜欢自动主题", list[0].Version); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, newItem("旧的深色偏好", base.Add(2*time.Minute))); err != nil {
		t.Fatal(err)
	}
	current, err := store.Get(ctx, userID, id)
	if err != nil || current.Content != "只喜欢自动主题" {
		t.Fatalf("correction overwritten: %+v %v", current, err)
	}
	vectors.results = []vectordb.SearchResult{{ID: id, Content: "stale vector", Score: 0.9, Metadata: map[string]any{"user_id": userID, "kind": "preference"}}}
	if err := store.Delete(ctx, userID, id); err != nil {
		t.Fatal(err)
	}
	recalled, err := store.Search(ctx, userID, "主题", 5)
	if err != nil || len(recalled) != 0 {
		t.Fatalf("deleted memory recalled: %+v %v", recalled, err)
	}
	var versions int
	if err := client.DB().QueryRowContext(ctx, `SELECT count(*) FROM memory_versions WHERE memory_id=$1`, id).Scan(&versions); err != nil || versions != 0 {
		t.Fatalf("deleted versions=%d err=%v", versions, err)
	}
}
