package embedding

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/enterprise/ai-agent-go/internal/config"
)

func TestOpenAIClientBatchAuthAndOrder(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/ai/v1/embeddings" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("unexpected path or authorization")
		}
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Model != "@cf/qwen/qwen3-embedding-0.6b" || len(req.Input) == 0 || len(req.Input) > 2 {
			t.Errorf("unexpected request: %+v", req)
		}
		if len(req.Input) == 2 {
			fmt.Fprint(w, `{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`)
		} else {
			fmt.Fprint(w, `{"data":[{"index":0,"embedding":[2,0]}]}`)
		}
	}))
	defer server.Close()
	client, err := NewClient(config.EmbeddingConfig{BaseURL: server.URL + "/ai/v1/embeddings", APIKey: "secret", Model: "@cf/qwen/qwen3-embedding-0.6b", Dimension: 2, BatchSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	vectors, err := client.EmbedBatch(context.Background(), []string{"one", "two", "three"})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(vectors) != 3 || vectors[0][0] != 1 || vectors[1][1] != 1 || vectors[2][0] != 2 {
		t.Fatalf("unexpected vectors or batch count: %v, %d", vectors, calls)
	}
}

func TestOpenAIClientRejectsInvalidResponseWithoutLeakingKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"secret"}`)
	}))
	defer server.Close()
	client, err := NewOpenAIClient(config.EmbeddingConfig{BaseURL: server.URL + "/v1", APIKey: "secret", Model: "model", Dimension: 2})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Embed(context.Background(), "text")
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOpenAIClientRejectsWrongDimension(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[{"index":0,"embedding":[1]}]}`)
	}))
	defer server.Close()
	client, err := NewOpenAIClient(config.EmbeddingConfig{BaseURL: server.URL + "/v1", APIKey: "secret", Model: "model", Dimension: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Embed(context.Background(), "text"); err == nil {
		t.Fatal("expected dimension error")
	}
}
