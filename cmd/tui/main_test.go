package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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

func TestStreamReceivesAnswerDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, content := range []string{"你", "好"} {
			payload, _ := json.Marshal(observe.Event{Type: observe.TypeAnswerDelta, Message: content})
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", observe.TypeAnswerDelta, payload)
		}
		_, _ = io.WriteString(w, "event: done\ndata: {\"status\":\"completed\"}\n\n")
	}))
	defer server.Close()

	events := make(chan streamEvent, 8)
	stream(context.Background(), server.URL, "session", "query", events)
	var chunks []string
	for raw := range events {
		if raw.name != observe.TypeAnswerDelta {
			continue
		}
		var event observe.Event
		if err := json.Unmarshal(raw.data, &event); err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, event.Message)
	}
	if strings.Join(chunks, "") != "你好" {
		t.Fatalf("answer chunks = %#v, want incremental 你好", chunks)
	}
}

func TestRenderEventMergesAnswerDeltas(t *testing.T) {
	m := &model{}
	for _, content := range []string{"流式", "回答"} {
		payload, _ := json.Marshal(observe.Event{Type: observe.TypeAnswerDelta, Message: content})
		m.renderEvent(streamEvent{name: observe.TypeAnswerDelta, data: payload})
	}
	if len(m.logs) != 1 || !strings.Contains(m.logs[0], "流式回答") {
		t.Fatalf("logs = %#v, want one incrementally updated answer", m.logs)
	}

	payload, _ := json.Marshal(observe.Event{Type: observe.TypeAnswer, Message: "流式回答"})
	m.renderEvent(streamEvent{name: observe.TypeAnswer, data: payload})
	if len(m.logs) != 1 {
		t.Fatalf("final answer duplicated log entries: %#v", m.logs)
	}
}

func TestHistoryScrollingAndSpinner(t *testing.T) {
	m := &model{width: 40, height: 10, busy: true, startedAt: time.Now()}
	for i := 0; i < 12; i++ {
		m.logs = append(m.logs, fmt.Sprintf("message-%02d", i))
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	m = updated.(*model)
	if m.scrollOffset == 0 || strings.Contains(m.View(), "message-11") {
		t.Fatalf("page up did not move history viewport: offset=%d view=%q", m.scrollOffset, m.View())
	}

	updated, cmd := m.Update(spinnerTickMsg(time.Now()))
	m = updated.(*model)
	if m.spinnerFrame != 1 || cmd == nil {
		t.Fatalf("spinner did not advance: frame=%d cmd=%v", m.spinnerFrame, cmd)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	m = updated.(*model)
	if m.scrollOffset != 0 || !strings.Contains(m.View(), "message-11") {
		t.Fatalf("end did not return to latest history: offset=%d view=%q", m.scrollOffset, m.View())
	}
}
