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
		if r.URL.Path == "/api/v1/sessions" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"code": 0, "data": map[string]string{"session_id": "session-1"}})
			return
		}
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

func TestRankingAndToolArguments(t *testing.T) {
	score := rankScores([]string{"a", "b"}, []string{"x", "b", "a"})
	if score.Recall != 1 || score.Precision != 2.0/3 || score.MRR != .5 || score.NDCG <= 0 || score.NDCG >= 1 {
		t.Fatalf("ranking=%+v", score)
	}
	result := caseResult{}
	failures := evaluate(assertion{Tool: "calculator", ToolArgs: map[string]any{"expression": "6*7"}, ReferenceOrder: []string{"b", "a"}}, chatResponse{ToolCalls: []toolCall{{ToolName: "calculator", Input: `{"expression":"6*7"}`}}, References: []reference{{DocID: "b"}, {DocID: "a"}}}, 10, &result)
	if len(failures) != 0 {
		t.Fatal(failures)
	}
	if !result.ToolArgsEvaluated || !result.ToolArgsPassed {
		t.Fatalf("tool argument evaluation=%+v", result)
	}
}

func TestToolsExactIgnoresDiscoveryCalls(t *testing.T) {
	result := caseResult{}
	failures := evaluate(assertion{ToolsExact: []string{"calculator"}}, chatResponse{ToolCalls: []toolCall{{ToolName: "list_tools"}, {ToolName: "list_tools"}, {ToolName: "calculator"}}}, 1, &result)
	if len(failures) != 0 {
		t.Fatal(failures)
	}
}

func TestTimeoutKeepsRequestTraceID(t *testing.T) {
	traceparent := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceparent <- r.Header.Get("traceparent")
		time.Sleep(50 * time.Millisecond)
	}))
	defer server.Close()
	client := &http.Client{Timeout: 20 * time.Millisecond}
	_, _, traceID, err := sendChat(context.Background(), client, server.URL, chatRequest{})
	if err == nil || len(traceID) != 32 {
		t.Fatalf("trace=%q err=%v", traceID, err)
	}
	select {
	case got := <-traceparent:
		if len(got) < 36 || got[3:35] != traceID {
			t.Fatalf("traceparent=%q trace=%q", got, traceID)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not receive request")
	}
}

func TestExpectedErrorStatusIsNotBusinessSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/sessions" {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]string{"session_id": "s"}})
			return
		}
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	}))
	defer server.Close()
	got, err := execute(context.Background(), server.Client(), server.URL+"/api/v1/chat", testCase{ID: "large", Message: "large", Expected: assertion{ExpectedStatus: 413}}, 1)
	if err != nil || !got.Passed || got.Success || got.HTTPSuccess || got.HTTPStatus != 413 {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func TestIsolationProbeUsesSeparateIdentity(t *testing.T) {
	t.Setenv("AGENTGO_ACCESS_TOKEN", "primary")
	t.Setenv("AGENTGO_ISOLATION_ACCESS_TOKEN", "other")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity := r.Header.Get("Authorization")
		if r.URL.Path == "/api/v1/sessions" {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]string{"session_id": identity}})
			return
		}
		content := "青鸟"
		if identity == "Bearer other" {
			content = "不知道"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]string{"content": content}})
	}))
	defer server.Close()
	got, err := execute(context.Background(), server.Client(), server.URL+"/api/v1/chat", testCase{ID: "isolation", Message: "我的代号？", Expected: assertion{KeywordsAll: []string{"青鸟"}, IsolationProbe: "我的代号？", IsolationForbidden: "青鸟"}}, 1)
	if err != nil || !got.Passed {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

var (
	testTime     = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	testDuration = 2 * time.Second
)
