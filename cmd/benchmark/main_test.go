package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestEvaluate(t *testing.T) {
	result := caseResult{}
	failures := evaluate(assertion{
		KeywordsAll:     []string{"42"},
		KeywordsAny:     []string{"answer", "答案"},
		Forbidden:       []string{"不知道"},
		Tool:            "calculator",
		ReferenceDocIDs: []string{"doc-1", "doc-2"},
		MaxLatencyMS:    100,
	}, chatResponse{
		Content:    "答案是 42",
		ToolCalls:  []toolCall{{ToolName: "calculator"}},
		References: []reference{{DocID: "doc-1"}, {DocID: "doc-2"}},
	}, 50, &result)
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %v", failures)
	}
	if !result.ToolEvaluated || !result.ToolPassed || result.RetrievalHits != 2 {
		t.Fatalf("unexpected counters: %+v", result)
	}
}

func TestExecuteMultiTurn(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		turn := calls.Add(1)
		content := "好的"
		if turn == 2 {
			content = "你的代号是青鸟"
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 0,
			"data": map[string]interface{}{"content": content},
		})
	}))
	defer server.Close()

	result, err := execute(context.Background(), server.Client(), server.URL+"/api/v1/chat", testCase{
		ID:       "memory",
		Category: "memory",
		Turns:    []string{"记住代号青鸟", "我的代号？"},
		Expected: assertion{KeywordsAll: []string{"青鸟"}},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || !result.Passed || calls.Load() != 2 {
		t.Fatalf("unexpected result: %+v, calls=%d", result, calls.Load())
	}
}

func TestBuildReport(t *testing.T) {
	rep := buildReport(testTime, testDuration, []caseResult{
		{Category: "tool", Success: true, Passed: true, LatencyMS: 10, ToolEvaluated: true, ToolPassed: true},
		{Category: "rag", Success: true, Passed: false, LatencyMS: 30, RetrievalHits: 1, RetrievalTotal: 2},
	})
	if rep.SuccessRate != 1 || rep.TaskPassRate != 0.5 || rep.LatencyP50MS != 10 || rep.LatencyP95MS != 30 {
		t.Fatalf("unexpected report: %+v", rep)
	}
	if rep.ToolAccuracy == nil || *rep.ToolAccuracy != 1 || rep.RetrievalRecall == nil || *rep.RetrievalRecall != 0.5 {
		t.Fatalf("unexpected optional metrics: %+v", rep)
	}
}

var (
	testTime     = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	testDuration = 2 * time.Second
)
