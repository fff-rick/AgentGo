package laya

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/enterprise/ai-agent-go/internal/config"
)

func TestClientPredict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("request path=%q authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var request Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.State != "hello" || request.MinConfidence == nil || *request.MinConfidence != 0 {
			t.Fatalf("unexpected request: %+v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"model":"laya-rl-agent","answers":{"intent":{"type":"choice","choice":"chat","answer_confidence":0.91,"probabilities":{"chat":0.91}},"urgency":{"type":"score","score":0},"needs_tools":{"type":"noul","noul":0}},"usage":{"input_tokens":8},"routing":{"model":"english"}}`)
	}))
	defer server.Close()

	client, err := NewClient(config.LayaConfig{URL: server.URL + "/v1/systemone", APIKey: "secret", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	threshold := 0.0
	response, err := client.Predict(context.Background(), "intent", Request{
		State: "hello", Questions: map[string]Question{"intent": {Type: "choice", Instructions: "classify", Criteria: map[string]string{"chat": "chat"}}}, MinConfidence: &threshold,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Answers["intent"].Choice != "chat" || response.Answers["intent"].AnswerConfidence == nil || *response.Answers["intent"].AnswerConfidence != .91 {
		t.Fatalf("unexpected response: %+v", response)
	}
	if response.Answers["urgency"].Score == nil || *response.Answers["urgency"].Score != 0 || response.Answers["needs_tools"].Noul == nil || *response.Answers["needs_tools"].Noul != 0 {
		t.Fatalf("zero-valued score/noul was lost: %+v", response.Answers)
	}
}

func TestClientRejectsInvalidInputAndResponse(t *testing.T) {
	if _, err := NewClient(config.LayaConfig{URL: "file:///tmp/laya"}); err == nil {
		t.Fatal("expected invalid URL error")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	client, _ := NewClient(config.LayaConfig{URL: server.URL})
	if _, err := client.Predict(context.Background(), "intent", Request{State: "hello", Questions: map[string]Question{"intent": {Type: "choice", Instructions: "classify"}}}); err == nil || !strings.Contains(err.Error(), "缺少 answers") {
		t.Fatalf("error=%v", err)
	}
	if _, err := client.Predict(context.Background(), "", Request{}); err == nil {
		t.Fatal("expected input validation error")
	}
}

func TestClientRejectsMalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"answers":`)
	}))
	defer server.Close()
	client, _ := NewClient(config.LayaConfig{URL: server.URL})
	if _, err := client.Predict(context.Background(), "intent", validRequest()); err == nil || !strings.Contains(err.Error(), "解析 Laya 响应失败") {
		t.Fatalf("error=%v", err)
	}
}

func TestClientReportsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"detail":"invalid token"}`, http.StatusUnauthorized)
	}))
	defer server.Close()
	client, _ := NewClient(config.LayaConfig{URL: server.URL})
	_, err := client.Predict(context.Background(), "intent", validRequest())
	if err == nil || !strings.Contains(err.Error(), "状态码 401") {
		t.Fatalf("error=%v", err)
	}
}

func TestClientHonorsTimeoutAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()

	client, _ := NewClient(config.LayaConfig{URL: server.URL, Timeout: 20 * time.Millisecond})
	_, err := client.Predict(context.Background(), "intent", validRequest())
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error=%v", err)
	}

	client, _ = NewClient(config.LayaConfig{URL: server.URL, Timeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Predict(ctx, "intent", validRequest())
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}

func TestWarmupCallsBothLanguageCheckpoints(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if len(request.Questions) != 2 {
			t.Errorf("questions=%d", len(request.Questions))
		}
		_, _ = fmt.Fprint(w, `{"answers":{"primary_tool":{"choice":"none"},"secondary_tool":{"choice":"none"}}}`)
	}))
	defer server.Close()
	if err := Warmup(context.Background(), config.LayaConfig{URL: server.URL, Timeout: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2", calls)
	}
}

func validRequest() Request {
	return Request{State: "hello", Questions: map[string]Question{"intent": {Type: "choice", Instructions: "classify"}}}
}
