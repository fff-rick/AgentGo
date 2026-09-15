package memory

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/enterprise/ai-agent-go/internal/embedding"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/vectordb"
)

type SemanticMemory interface {
	Search(context.Context, string, string, int) ([]model.MemoryItem, error)
	Save(context.Context, model.MemoryItem) error
}

type filteredVectorDB interface {
	Insert(context.Context, string, []vectordb.VectorRecord) error
	SearchWithFilter(context.Context, string, []float32, int, string) ([]vectordb.SearchResult, error)
}

type SemanticStore struct {
	vectors    filteredVectorDB
	embedder   embedding.Client
	collection string
}

func NewSemanticStore(vectors filteredVectorDB, embedder embedding.Client, collection string) *SemanticStore {
	if collection == "" {
		collection = "semantic_memory_v1"
	}
	return &SemanticStore{vectors: vectors, embedder: embedder, collection: collection}
}

func (s *SemanticStore) Save(ctx context.Context, item model.MemoryItem) error {
	if item.UserID == "" || item.Content == "" || !validMemoryKind(item.Kind) {
		return fmt.Errorf("语义记忆缺少有效的 user_id、kind 或 content")
	}
	vector, err := s.embedder.Embed(ctx, item.Content)
	if err != nil {
		return err
	}
	return s.vectors.Insert(ctx, s.collection, []vectordb.VectorRecord{{
		ID: item.ID, Content: item.Content, Embedding: vector,
		Metadata: map[string]any{
			"user_id": item.UserID, "kind": string(item.Kind),
			"importance":        fmt.Sprintf("%g", item.Importance),
			"source_session_id": item.SourceSessionID,
			"created_at":        item.CreatedAt.UTC().Format(time.RFC3339Nano),
		},
	}})
}

func (s *SemanticStore) Search(ctx context.Context, userID, query string, topK int) ([]model.MemoryItem, error) {
	if userID == "" || query == "" || topK <= 0 {
		return nil, nil
	}
	vector, err := s.embedder.Embed(ctx, query)
	if err != nil {
		return nil, err
	}
	filter := `metadata["user_id"] == ` + strconv.Quote(userID)
	results, err := s.vectors.SearchWithFilter(ctx, s.collection, vector, topK, filter)
	if err != nil {
		return nil, err
	}
	items := make([]model.MemoryItem, 0, len(results))
	for _, result := range results {
		importance, _ := strconv.ParseFloat(fmt.Sprint(result.Metadata["importance"]), 64)
		createdAt, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(result.Metadata["created_at"]))
		items = append(items, model.MemoryItem{
			ID: result.ID, UserID: fmt.Sprint(result.Metadata["user_id"]), Kind: model.MemoryKind(fmt.Sprint(result.Metadata["kind"])),
			Content: result.Content, Importance: importance, SourceSessionID: fmt.Sprint(result.Metadata["source_session_id"]),
			CreatedAt: createdAt, Score: result.Score,
		})
	}
	return items, nil
}

func validMemoryKind(kind model.MemoryKind) bool {
	switch kind {
	case model.MemoryPreference, model.MemoryFact, model.MemoryExperience, model.MemorySolution:
		return true
	default:
		return false
	}
}
