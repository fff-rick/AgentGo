package etl

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/model"
)

func TestImporterTransitionsToCompleted(t *testing.T) {
	repo := &importRepositoryStub{}
	pipeline := newTestPipeline(&parserStub{}, &vectorDBStub{}, &embedderStub{}, repo)
	importer, err := NewImporter(context.Background(), pipeline, repo, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer importer.Close()
	now := time.Now()
	doc := &model.Document{Title: "Guide", ContentType: "markdown", Filename: "guide.md", CreatedAt: now, UpdatedAt: now}
	response, queued, err := importer.Submit(context.Background(), doc, []byte("# Guide\ncontent"))
	if err != nil {
		t.Fatal(err)
	}
	if !queued || response.Status != "processing" {
		t.Fatalf("response=%+v queued=%v", response, queued)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		current, err := repo.FindDocument(context.Background(), response.DocID)
		if err == nil && current.Status == "completed" {
			if current.ChunkCount == 0 {
				t.Fatal("completed without chunks")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("import did not complete: %+v", repo.current())
}

type importRepositoryStub struct {
	mu  sync.Mutex
	doc *model.DocumentResponse
}

func (r *importRepositoryStub) FindDocument(context.Context, string) (*model.DocumentResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.doc == nil {
		return nil, sql.ErrNoRows
	}
	copy := *r.doc
	return &copy, nil
}
func (r *importRepositoryStub) MarkDocumentProcessing(_ context.Context, doc *model.Document) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.doc = &model.DocumentResponse{DocID: doc.ID, Title: doc.Title, Status: "processing", CreatedAt: doc.CreatedAt, ContentHash: doc.ContentHash}
	return nil
}
func (r *importRepositoryStub) MarkDocumentFailed(_ context.Context, id, message string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.doc = &model.DocumentResponse{DocID: id, Status: "failed", Error: message}
	return nil
}
func (*importRepositoryStub) FailInterruptedDocuments(context.Context) error { return nil }
func (r *importRepositoryStub) IndexDocument(_ context.Context, doc *model.Document, chunks []model.DocumentChunk) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.doc = &model.DocumentResponse{DocID: doc.ID, Title: doc.Title, Status: "completed", ChunkCount: len(chunks), CreatedAt: doc.CreatedAt, ContentHash: doc.ContentHash}
	return nil
}
func (r *importRepositoryStub) current() *model.DocumentResponse {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.doc == nil {
		return nil
	}
	copy := *r.doc
	return &copy
}
