package embedding

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/enterprise/ai-agent-go/internal/config"
)

func TestOllamaClientEmbedBatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"embeddings":[[1,0,0],[0,1,0]]}`))
	}))
	defer server.Close()

	client, err := NewOllamaClient(config.EmbeddingConfig{BaseURL: server.URL, Model: "bge-m3", Dimension: 3, BatchSize: 2, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	vectors, err := client.EmbedBatch(context.Background(), []string{"one", "two"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 2 || len(vectors[0]) != 3 {
		t.Fatalf("unexpected vectors: %#v", vectors)
	}
}

func TestOllamaClientRejectsWrongDimension(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"embeddings":[[1,0]]}`))
	}))
	defer server.Close()
	client, _ := NewOllamaClient(config.EmbeddingConfig{BaseURL: server.URL, Model: "bge-m3", Dimension: 3})
	if _, err := client.Embed(context.Background(), "text"); err == nil {
		t.Fatal("expected dimension error")
	}
}
