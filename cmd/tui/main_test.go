package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/enterprise/ai-agent-go/internal/observe"
)

func TestImportMarkdown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/documents/import" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		content, _ := io.ReadAll(file)
		if header.Filename != "knowledge.md" || string(content) != "# Knowledge\nAgentGo" {
			t.Fatalf("unexpected upload: %q %q", header.Filename, content)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"doc_id":"doc-1","title":"knowledge","status":"completed","chunk_count":2}}`))
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "knowledge.md")
	if err := os.WriteFile(path, []byte("# Knowledge\nAgentGo"), 0o600); err != nil {
		t.Fatal(err)
	}
	events := make(chan streamEvent, 8)
	importMarkdown(context.Background(), server.URL, path, events)
	var got []observe.Event
	for raw := range events {
		if raw.name == observe.TypeStatus {
			var event observe.Event
			if err := json.Unmarshal(raw.data, &event); err != nil {
				t.Fatal(err)
			}
			got = append(got, event)
		}
	}
	if len(got) != 2 || got[1].Message != "导入完成：knowledge，2 个分块，doc_id=doc-1" {
		t.Fatalf("unexpected events: %+v", got)
	}
}
