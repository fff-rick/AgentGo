package etl

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/vectordb"
)

func TestDocumentIDAndContentHashAreIndependent(t *testing.T) {
	docID := DocumentID(" Markdown ", " AgentGo ")
	if docID != DocumentID("markdown", "AgentGo") || docID != DocumentID("MARKDOWN", "AgentGo") {
		t.Fatal("document identity was not normalized")
	}
	if docID == DocumentID("text", "AgentGo") || docID == DocumentID("markdown", "agentgo") {
		t.Fatal("type and case-sensitive title must participate in document identity")
	}
	if ContentHash("one") != ContentHash("one") || ContentHash("one") == ContentHash("two") {
		t.Fatal("content hash must be stable and content-sensitive")
	}
	if DocumentID("", "AgentGo") != DocumentID("text", "AgentGo") {
		t.Fatal("empty content type should default to text")
	}
	if stableChunkID(docID, 0) == stableChunkID(docID, 1) {
		t.Fatal("chunk index must participate in chunk identity")
	}
}

func TestProcessDocumentSkipsUnchangedContent(t *testing.T) {
	createdAt := time.Unix(100, 0)
	doc := testDocument("Same content.")
	parser, embedder := &parserStub{}, &embedderStub{}
	vectors := &vectorDBStub{}
	indexer := &indexerStub{existing: &model.DocumentResponse{
		DocID: DocumentID(doc.ContentType, doc.Title), Title: "Guide", Status: "completed", ChunkCount: 2,
		CreatedAt: createdAt, ContentHash: ContentHash(doc.Content),
	}}

	resp, err := newTestPipeline(parser, vectors, embedder, indexer).ProcessDocument(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if resp.CreatedAt != createdAt || resp.ChunkCount != 2 {
		t.Fatalf("response = %+v", resp)
	}
	if parser.calls != 0 || embedder.calls != 0 || vectors.insertCalls != 0 || indexer.indexCalls != 0 {
		t.Fatal("unchanged content performed ETL work")
	}
}

func TestProcessDocumentReplacesContentAndDeletesStaleVectors(t *testing.T) {
	createdAt := time.Unix(100, 0)
	doc := testDocument("New.")
	docID := DocumentID(doc.ContentType, doc.Title)
	vectors := &vectorDBStub{}
	indexer := &indexerStub{existing: &model.DocumentResponse{
		DocID: docID, Title: "Guide", Status: "completed", ChunkCount: 3,
		CreatedAt: createdAt, ContentHash: ContentHash("Old. content. here."),
	}}

	resp, err := newTestPipeline(&parserStub{}, vectors, &embedderStub{}, indexer).ProcessDocument(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	wantDeleted := []string{stableChunkID(docID, 1), stableChunkID(docID, 2)}
	if !reflect.DeepEqual(vectors.deleted, wantDeleted) {
		t.Fatalf("deleted IDs = %v, want %v", vectors.deleted, wantDeleted)
	}
	if indexer.indexCalls != 1 || len(indexer.savedChunks) != 1 || indexer.saved.ContentHash != ContentHash(doc.Content) {
		t.Fatalf("saved document = %+v, chunks = %+v", indexer.saved, indexer.savedChunks)
	}
	if resp.CreatedAt != createdAt || resp.ContentHash != ContentHash(doc.Content) {
		t.Fatalf("response = %+v", resp)
	}
}

func TestProcessDocumentGrowthDoesNotDeleteVectors(t *testing.T) {
	doc := testDocument("aa.bb.cc.")
	docID := DocumentID(doc.ContentType, doc.Title)
	vectors := &vectorDBStub{}
	indexer := &indexerStub{existing: &model.DocumentResponse{
		DocID: docID, ChunkCount: 1, ContentHash: ContentHash("old"), CreatedAt: doc.CreatedAt,
	}}

	resp, err := newTestPipeline(&parserStub{}, vectors, &embedderStub{}, indexer).ProcessDocument(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ChunkCount != 3 || vectors.deleteCalls != 0 {
		t.Fatalf("chunk count = %d, delete calls = %d", resp.ChunkCount, vectors.deleteCalls)
	}
}

func TestProcessDocumentFailuresRemainRetryable(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*vectorDBStub, *indexerStub)
		clear     func(*vectorDBStub, *indexerStub)
	}{
		{"vector upsert", func(v *vectorDBStub, _ *indexerStub) { v.insertErr = errors.New("upsert failed") }, func(v *vectorDBStub, _ *indexerStub) { v.insertErr = nil }},
		{"stale vector delete", func(v *vectorDBStub, _ *indexerStub) { v.deleteErr = errors.New("delete failed") }, func(v *vectorDBStub, _ *indexerStub) { v.deleteErr = nil }},
		{"postgres index", func(_ *vectorDBStub, i *indexerStub) { i.indexErr = errors.New("index failed") }, func(_ *vectorDBStub, i *indexerStub) { i.indexErr = nil }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := testDocument("New.")
			indexer := &indexerStub{existing: &model.DocumentResponse{
				DocID: DocumentID(doc.ContentType, doc.Title), ChunkCount: 2,
				ContentHash: ContentHash("Old."), CreatedAt: doc.CreatedAt,
			}}
			vectors := &vectorDBStub{}
			tt.configure(vectors, indexer)
			pipeline := newTestPipeline(&parserStub{}, vectors, &embedderStub{}, indexer)

			if _, err := pipeline.ProcessDocument(context.Background(), doc); err == nil {
				t.Fatal("expected first import to fail")
			}
			if indexer.existing.ContentHash != ContentHash("Old.") {
				t.Fatal("failed import advanced persisted content hash")
			}
			tt.clear(vectors, indexer)
			if _, err := pipeline.ProcessDocument(context.Background(), doc); err != nil {
				t.Fatalf("retry failed: %v", err)
			}
			if indexer.existing.ContentHash != ContentHash(doc.Content) {
				t.Fatal("successful retry did not persist new content hash")
			}
		})
	}
}

func testDocument(content string) *model.Document {
	now := time.Unix(200, 0)
	return &model.Document{Title: " Guide ", ContentType: " MARKDOWN ", Content: content, CreatedAt: now, UpdatedAt: now}
}

func newTestPipeline(parser Parser, vectors vectordb.VectorDB, embedder *embedderStub, indexer DocumentIndexer) *Pipeline {
	return NewPipeline(parser, NewChunker(3, 0), vectors, embedder, zap.NewNop(), indexer)
}

type parserStub struct{ calls int }

func (p *parserStub) Parse(_ context.Context, content string, _ DocumentType) (*ParsedDocument, error) {
	p.calls++
	return &ParsedDocument{Content: content}, nil
}

type embedderStub struct{ calls int }

func (*embedderStub) Embed(context.Context, string) ([]float32, error) { return []float32{1}, nil }
func (e *embedderStub) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	e.calls++
	result := make([][]float32, len(texts))
	for i := range result {
		result[i] = []float32{1}
	}
	return result, nil
}
func (*embedderStub) Healthy(context.Context) bool { return true }

type vectorDBStub struct {
	insertErr, deleteErr     error
	insertCalls, deleteCalls int
	deleted                  []string
}

func (v *vectorDBStub) Insert(context.Context, string, []vectordb.VectorRecord) error {
	v.insertCalls++
	return v.insertErr
}
func (*vectorDBStub) Search(context.Context, string, []float32, int) ([]vectordb.SearchResult, error) {
	return nil, nil
}
func (v *vectorDBStub) Delete(_ context.Context, _ string, ids []string) error {
	v.deleteCalls++
	v.deleted = append([]string(nil), ids...)
	return v.deleteErr
}
func (*vectorDBStub) Close() error                 { return nil }
func (*vectorDBStub) Healthy(context.Context) bool { return true }

type indexerStub struct {
	existing    *model.DocumentResponse
	indexErr    error
	indexCalls  int
	saved       *model.Document
	savedChunks []model.DocumentChunk
}

func (i *indexerStub) FindDocument(context.Context, string) (*model.DocumentResponse, error) {
	if i.existing == nil {
		return nil, sql.ErrNoRows
	}
	copy := *i.existing
	return &copy, nil
}

func (i *indexerStub) IndexDocument(_ context.Context, doc *model.Document, chunks []model.DocumentChunk) error {
	i.indexCalls++
	if i.indexErr != nil {
		return i.indexErr
	}
	docCopy := *doc
	i.saved = &docCopy
	i.savedChunks = append([]model.DocumentChunk(nil), chunks...)
	i.existing = &model.DocumentResponse{
		DocID: doc.ID, Title: doc.Title, Status: "completed", ChunkCount: len(chunks),
		CreatedAt: doc.CreatedAt, ContentHash: doc.ContentHash,
	}
	return nil
}
