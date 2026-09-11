package intent

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/model"
)

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

func TestRecognizeUsesRulesBeforeLLMForExplicitSearch(t *testing.T) {
	recognizer := &Recognizer{logger: zap.NewNop()}
	result, err := recognizer.Recognize(context.Background(), "今天成都天气如何？请搜索后回答")
	if err != nil || result.Intent != IntentToolUse {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestFallbackPrefersKnowledgeBaseIntent(t *testing.T) {
	recognizer := &Recognizer{logger: zap.NewNop()}
	result := recognizer.fallback("请根据知识库中的最新文档回答")
	if result.Intent != IntentRAGQuery {
		t.Fatalf("fallback intent = %s", result.Intent)
	}
}
