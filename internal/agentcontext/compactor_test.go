package agentcontext

import (
	"context"
	"testing"

	"github.com/enterprise/ai-agent-go/internal/model"
)

type summaryClient struct{ request *model.LLMRequest }

func (c *summaryClient) Chat(_ context.Context, request *model.LLMRequest) (*model.LLMResponse, error) {
	c.request = request
	return &model.LLMResponse{Content: "压缩后的摘要"}, nil
}

func TestCompactorReportsBeforeAfterAndRetainedMessages(t *testing.T) {
	client := &summaryClient{}
	compactor := NewCompactor(client)
	retained := []model.Message{{Sequence: 2, Role: "user", Content: "最近消息"}}
	result, err := compactor.Compact(context.Background(), CompactionInput{
		ExistingSummary: "已有摘要", Messages: []model.Message{{Sequence: 1, Role: "user", Content: "很长的旧消息"}},
		RetainedMessages: retained, MaxTokens: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.request.MaxTokens != 2000 || len(result.RetainedMessages) != 1 || result.TokensBefore <= 0 || result.TokensAfter <= 0 {
		t.Fatalf("request=%+v result=%+v", client.request, result)
	}
	if !compactor.ShouldCompact(ContextBudgetInput{EstimatedTokens: 30001, MaxTokens: 30000}) {
		t.Fatal("超过预算时应触发压缩")
	}
}
