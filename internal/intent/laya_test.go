package intent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/laya"
	"github.com/enterprise/ai-agent-go/internal/model"
)

func TestLayaShadowRecognizesIntentWithoutRawTaskInResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request laya.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		state := request.State.(map[string]interface{})
		if request.MinConfidence == nil || *request.MinConfidence != .6 || len(request.Questions) != 2 || state["scenario"] != "assistant: 上一个问题需要计算" {
			t.Fatalf("request=%+v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"execution_mode":{"choice":"agent","answer_confidence":0.96,"abstention":"passed"},"information_source":{"choice":"calculator","answer_confidence":0.94,"abstention":"passed"}},"routing":{"model":"multilingual"}}`))
	}))
	defer server.Close()
	client, err := laya.NewClient(config.LayaConfig{URL: server.URL, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	history := []model.LLMMessage{{Role: "assistant", Content: "上一个问题需要计算"}, {Role: "user", Content: "计算 6 乘以 7"}}
	result, err := NewLayaShadow(client, .6, zap.NewNop()).Recognize(context.Background(), "计算 6 乘以 7", history)
	if err != nil || result.Intent != IntentToolUse || result.Confidence != .94 || len(result.Entities) != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
