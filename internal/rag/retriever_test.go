package rag

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/vectordb"
)

type stubEmbedder struct{}

func (stubEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{1, 0}, nil
}

func (stubEmbedder) EmbedBatch(context.Context, []string) ([][]float32, error) {
	return [][]float32{{1, 0}}, nil
}

func (stubEmbedder) Healthy(context.Context) bool { return true }

type stubVectorDB struct {
	results []vectordb.SearchResult
}

func (s stubVectorDB) Insert(context.Context, string, []vectordb.VectorRecord) error { return nil }

func (s stubVectorDB) Search(context.Context, string, []float32, int) ([]vectordb.SearchResult, error) {
	return s.results, nil
}

func (s stubVectorDB) Delete(context.Context, string, []string) error { return nil }
func (s stubVectorDB) Close() error                                   { return nil }
func (s stubVectorDB) Healthy(context.Context) bool                   { return true }

type stubKeywordStore struct {
	results []model.Reference
}

type statusKeywordStore struct {
	stubKeywordStore
	completed bool
}

func (s statusKeywordStore) IsDocumentCompleted(context.Context, string) bool { return s.completed }

func (s stubKeywordStore) Search(context.Context, string, int) ([]model.Reference, error) {
	return s.results, nil
}

func TestRetrieveFiltersLowSimilarityAndPreservesMilvusScore(t *testing.T) {
	db := stubVectorDB{results: []vectordb.SearchResult{
		{ID: "relevant", Content: "relevant content", Score: 0.91, Metadata: map[string]any{"doc_id": "doc-1", "title": "Relevant"}},
		{ID: "irrelevant", Content: "irrelevant content", Score: 0.69, Metadata: map[string]any{"doc_id": "doc-2", "title": "Irrelevant"}},
	}}
	retriever := NewRetriever(db, stubEmbedder{}, nil, 0.7, zap.NewNop())

	refs, err := retriever.Retrieve(context.Background(), "query", 5)
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("Retrieve() returned %d refs, want 1", len(refs))
	}
	if refs[0].DocID != "doc-1" {
		t.Fatalf("DocID = %q, want doc-1", refs[0].DocID)
	}
	if refs[0].Score != 0.91 {
		t.Fatalf("Score = %v, want original cosine score 0.91", refs[0].Score)
	}
}

func TestHybridSearchWithEmptyKeywordResultsPreservesVectorScore(t *testing.T) {
	db := stubVectorDB{results: []vectordb.SearchResult{
		{ID: "relevant", Content: "content", Score: 0.88, Metadata: map[string]any{"doc_id": "doc-1"}},
	}}
	retriever := NewRetriever(db, stubEmbedder{}, nil, 0.7, zap.NewNop())

	refs, err := retriever.RetrieveWithMode(context.Background(), "query", 5, ModeHybrid)
	if err != nil {
		t.Fatalf("RetrieveWithMode() error = %v", err)
	}
	if len(refs) != 1 || refs[0].Score != 0.88 {
		t.Fatalf("refs = %#v, want one ref with original score 0.88", refs)
	}
}

func TestVectorSearchExcludesProcessingDocuments(t *testing.T) {
	db := stubVectorDB{results: []vectordb.SearchResult{{ID: "chunk-1", Content: "stale", Score: .9, Metadata: map[string]any{"doc_id": "doc-1"}}}}
	retriever := NewRetriever(db, stubEmbedder{}, nil, .7, zap.NewNop(), statusKeywordStore{completed: false})
	refs, err := retriever.RetrieveWithMode(context.Background(), "query", 5, ModeVector)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("refs=%+v", refs)
	}
}

func TestRetrieveUsesHybridSearchAndFusesByChunk(t *testing.T) {
	db := stubVectorDB{results: []vectordb.SearchResult{
		{ID: "chunk-1", Content: "hybrid content", Score: 0.9, Metadata: map[string]any{"doc_id": "doc-1"}},
		{ID: "chunk-2", Content: "another chunk", Score: 0.8, Metadata: map[string]any{"doc_id": "doc-1"}},
	}}
	keywords := stubKeywordStore{results: []model.Reference{
		{ChunkID: "chunk-1", DocID: "doc-1", Content: "hybrid content", Score: 1},
	}}
	retriever := NewRetriever(db, stubEmbedder{}, nil, 0.7, zap.NewNop(), keywords)

	refs, err := retriever.Retrieve(context.Background(), "hybrid", 5)
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("Retrieve() returned %d refs, want both chunks", len(refs))
	}
	if refs[0].ChunkID != "chunk-1" {
		t.Fatalf("top chunk = %q, want chunk-1 returned by both retrievers", refs[0].ChunkID)
	}
}
