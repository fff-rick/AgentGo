package intent

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/llm"
	"github.com/enterprise/ai-agent-go/internal/model"
)

type intentClient struct {
	request *model.LLMRequest
	result  string
}

func (c *intentClient) Chat(_ context.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
	c.request = req
	return &model.LLMResponse{Content: c.result}, nil
}
func (*intentClient) ChatStream(context.Context, *model.LLMRequest) (<-chan llm.StreamEvent, error) {
	panic("intent recognizer must use Chat")
}
func (*intentClient) Name() string                 { return "intent" }
func (*intentClient) Healthy(context.Context) bool { return true }

func TestDecodeIntentAcceptsMarkdownCodeFence(t *testing.T) {
	var result model.IntentResult
	err := decodeIntent("```json\n{\"intent\":\"tool_use\",\"confidence\":0.99}\n```", &result)
	if err != nil || result.Intent != IntentToolUse {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestFallbackRoutesRealtimeQueriesToTools(t *testing.T) {
	recognizer := &Recognizer{logger: zap.NewNop()}
	for _, query := range []string{
		"今天成都的天气怎么样？",
		"请搜索后附上来源链接",
		"查询一下最新股价",
	} {
		result := recognizer.fallback(query)
		if result.Intent != IntentToolUse || len(result.RequiredTools) != 1 || result.RequiredTools[0] != "web_search" {
			t.Fatalf("fallback(%q) = %+v", query, result)
		}
	}
}

func TestRecognizeUsesContextAndLetsModelDecide(t *testing.T) {
	client := &intentClient{result: `{"intent":"rag_query","confidence":0.96,"entities":{}}`}
	router := llm.NewRouter(
		map[string]llm.Client{"intent": client},
		[]config.ModelConfig{{Name: "intent"}},
		config.CBConfig{FailureThreshold: 3, SuccessThreshold: 1},
	)
	recognizer := NewRecognizer(router, zap.NewNop())
	history := []model.LLMMessage{
		{Role: "user", Content: "请根据内部文档介绍部署方式"},
		{Role: "assistant", Content: "你想继续了解哪一部分？"},
	}
	result, err := recognizer.Recognize(context.Background(), "最新版本支持什么？", history)
	if err != nil || result.Intent != IntentRAGQuery {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if client.request == nil || len(client.request.Messages) != 4 {
		t.Fatalf("request messages = %+v", client.request)
	}
	if client.request.Messages[1].Content != history[0].Content || client.request.Messages[3].Content != "最新版本支持什么？" {
		t.Fatalf("history or current input missing: %+v", client.request.Messages)
	}
	if strings.Contains(client.request.Messages[0].Content, "最新版本支持什么") {
		t.Fatal("current input should be a user message, not interpolated into the system prompt")
	}
}

func TestFallbackPrefersKnowledgeBaseIntent(t *testing.T) {
	recognizer := &Recognizer{logger: zap.NewNop()}
	result := recognizer.fallback("请根据知识库中的最新文档回答")
	if result.Intent != IntentRAGQuery {
		t.Fatalf("fallback intent = %s", result.Intent)
	}
}
