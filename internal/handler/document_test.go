package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/auth"
	"github.com/enterprise/ai-agent-go/internal/etl"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/vectordb"
)

func TestDocumentUploadPermissionLimitsAndConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("publisher succeeds", func(t *testing.T) {
		h, parser := newDocumentTestHandler(nil)
		response := uploadDocument(t, h, "publisher", map[string]any{"title": "Guide", "content": "hello"}, "")
		if response.Code != http.StatusOK || response.Header().Get("ETag") == "" || parser.calls != 1 {
			t.Fatalf("status=%d etag=%q parser_calls=%d body=%s", response.Code, response.Header().Get("ETag"), parser.calls, response.Body.String())
		}
	})

	t.Run("non publisher is forbidden", func(t *testing.T) {
		h, parser := newDocumentTestHandler(nil)
		response := uploadDocument(t, h, "reader", map[string]any{"title": "Guide", "content": "hello"}, "")
		if response.Code != http.StatusForbidden || parser.calls != 0 {
			t.Fatalf("status=%d parser_calls=%d", response.Code, parser.calls)
		}
	})

	t.Run("oversized text and metadata stop before ETL", func(t *testing.T) {
		for name, body := range map[string]map[string]any{
			"text":     {"title": "Guide", "content": strings.Repeat("x", maxMarkdownBytes+1)},
			"metadata": {"title": "Guide", "content": "hello", "metadata": map[string]any{"large": strings.Repeat("x", maxDocumentMetadataBytes)}},
		} {
			t.Run(name, func(t *testing.T) {
				h, parser := newDocumentTestHandler(nil)
				response := uploadDocument(t, h, "publisher", body, "")
				if response.Code != http.StatusRequestEntityTooLarge || parser.calls != 0 {
					t.Fatalf("status=%d parser_calls=%d body=%s", response.Code, parser.calls, response.Body.String())
				}
			})
		}
	})

	t.Run("overwrite requires matching hash", func(t *testing.T) {
		currentHash := etl.ContentHash("old")
		repo := &documentRepoStub{doc: &model.DocumentResponse{
			DocID: etl.DocumentID("text", "Guide"), Title: "Guide", Status: "completed",
			ContentHash: currentHash, CreatedAt: time.Unix(1, 0), ChunkCount: 1,
		}}
		h, parser := newDocumentTestHandler(repo)
		body := map[string]any{"title": "Guide", "content": "new"}
		if response := uploadDocument(t, h, "publisher", body, ""); response.Code != http.StatusConflict || parser.calls != 0 {
			t.Fatalf("status=%d parser_calls=%d body=%s", response.Code, parser.calls, response.Body.String())
		}
		if response := uploadDocument(t, h, "publisher", body, `"`+currentHash+`"`); response.Code != http.StatusOK || parser.calls != 1 {
			t.Fatalf("status=%d parser_calls=%d body=%s", response.Code, parser.calls, response.Body.String())
		}
	})
}

func uploadDocument(t *testing.T, h *DocumentHandler, identity string, body any, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/documents", bytes.NewReader(data))
	c.Request.Header.Set("Content-Type", "application/json")
	if ifMatch != "" {
		c.Request.Header.Set("If-Match", ifMatch)
	}
	auth.SetIdentity(c, identity)
	h.Upload(c)
	return response
}

func newDocumentTestHandler(repo *documentRepoStub) (*DocumentHandler, *documentParserStub) {
	if repo == nil {
		repo = &documentRepoStub{}
	}
	parser := &documentParserStub{}
	pipeline := etl.NewPipeline(parser, etl.NewChunker(64, 0), documentVectorStub{}, documentEmbedderStub{}, zap.NewNop(), repo)
	return NewDocumentHandler(pipeline, nil, zap.NewNop(), []string{"publisher"}, repo), parser
}

type documentParserStub struct{ calls int }

func (p *documentParserStub) Parse(_ context.Context, source etl.SourceDocument) (*etl.ParsedDocument, error) {
	p.calls++
	return &etl.ParsedDocument{Content: string(source.Data)}, nil
}

type documentEmbedderStub struct{}

func (documentEmbedderStub) Embed(context.Context, string) ([]float32, error) {
	return []float32{1}, nil
}
func (documentEmbedderStub) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	result := make([][]float32, len(texts))
	for i := range result {
		result[i] = []float32{1}
	}
	return result, nil
}
func (documentEmbedderStub) Healthy(context.Context) bool { return true }

type documentVectorStub struct{}

func (documentVectorStub) Insert(context.Context, string, []vectordb.VectorRecord) error { return nil }
func (documentVectorStub) Search(context.Context, string, []float32, int) ([]vectordb.SearchResult, error) {
	return nil, nil
}
func (documentVectorStub) Delete(context.Context, string, []string) error { return nil }
func (documentVectorStub) Close() error                                   { return nil }
func (documentVectorStub) Healthy(context.Context) bool                   { return true }

type documentRepoStub struct{ doc *model.DocumentResponse }

func (r *documentRepoStub) FindDocument(context.Context, string) (*model.DocumentResponse, error) {
	if r.doc == nil {
		return nil, sql.ErrNoRows
	}
	copy := *r.doc
	return &copy, nil
}

func (r *documentRepoStub) IndexDocument(_ context.Context, doc *model.Document, chunks []model.DocumentChunk) error {
	r.doc = &model.DocumentResponse{
		DocID: doc.ID, Title: doc.Title, Status: "completed", ChunkCount: len(chunks),
		CreatedAt: doc.CreatedAt, ContentHash: doc.ContentHash,
	}
	return nil
}
