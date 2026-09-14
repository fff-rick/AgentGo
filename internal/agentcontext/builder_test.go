package agentcontext

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/memory"
	"github.com/enterprise/ai-agent-go/internal/model"
)

type sessionStub struct {
	messages []model.Message
	summary  *memory.SessionSummary
}

func (*sessionStub) CreateSession(context.Context, model.UserInfo) (*model.Session, error) {
	return nil, nil
}
func (*sessionStub) GetSession(context.Context, string) (*model.Session, error) {
	return &model.Session{ID: "session", UserID: "user-1"}, nil
}
func (*sessionStub) GetUser(context.Context, string) (*model.UserInfo, error) {
	return &model.UserInfo{UserID: "user-1", DisplayName: "Xin"}, nil
}
func (*sessionStub) AppendMessage(context.Context, string, model.Message) error { return nil }
func (s *sessionStub) GetMessages(context.Context, string) ([]model.Message, error) {
	return append([]model.Message(nil), s.messages...), nil
}
func (*sessionStub) GetRecentMessages(context.Context, string, int) ([]model.Message, error) {
	return nil, nil
}
func (s *sessionStub) SaveSummary(_ context.Context, _ string, summary memory.SessionSummary) error {
	s.summary = &summary
	return nil
}
func (s *sessionStub) GetSummary(context.Context, string) (*memory.SessionSummary, error) {
	return s.summary, nil
}

type semanticMemoryStub struct{ items []model.MemoryItem }

func (s semanticMemoryStub) Search(context.Context, string, string, int) ([]model.MemoryItem, error) {
	return s.items, nil
}
func (semanticMemoryStub) Save(context.Context, model.MemoryItem) error { return nil }

type compactorStub struct {
	input CompactionInput
	calls int
	err   error
}

func (*compactorStub) ShouldCompact(input ContextBudgetInput) bool {
	return input.EstimatedTokens > input.MaxTokens
}
func (c *compactorStub) Compact(_ context.Context, input CompactionInput) (*CompactionResult, error) {
	c.input = input
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return &CompactionResult{Summary: "旧消息摘要"}, nil
}

func TestBuilderFallsBackWhenCompactionFails(t *testing.T) {
	sessions := &sessionStub{}
	for sequence := int64(1); sequence <= 5; sequence++ {
		sessions.messages = append(sessions.messages, model.Message{Sequence: sequence, Role: "user", Content: strings.Repeat("a", 500)})
	}
	compactor := &compactorStub{err: errors.New("LLM unavailable")}
	builder := NewBuilder(sessions, semanticMemoryStub{}, compactor, config.ContextConfig{MaxInputTokens: 500, RecentMessages: 2, SummaryMaxTokens: 100}, 5, zap.NewNop())
	result, err := builder.Build(context.Background(), BuildInput{SessionID: "session", Query: "继续", SystemPrompt: "system"})
	if err != nil {
		t.Fatal(err)
	}
	if compactor.calls != 1 || sessions.summary != nil || len(result.Messages) >= 6 || result.EstimatedTokens > 500 {
		t.Fatalf("result=%+v summary=%+v compact_calls=%d", result, sessions.summary, compactor.calls)
	}
}

func TestBuilderCompactsOldMessagesAndKeepsRawHistory(t *testing.T) {
	sessions := &sessionStub{}
	for sequence := int64(1); sequence <= 5; sequence++ {
		sessions.messages = append(sessions.messages, model.Message{Sequence: sequence, Role: "user", Content: strings.Repeat("a", 500)})
	}
	compactor := &compactorStub{}
	builder := NewBuilder(sessions, semanticMemoryStub{items: []model.MemoryItem{{UserID: "user-1", Kind: model.MemoryPreference, Content: "喜欢 Go"}}}, compactor,
		config.ContextConfig{MaxInputTokens: 500, RecentMessages: 2, SummaryMaxTokens: 100}, 5, zap.NewNop())
	result, err := builder.Build(context.Background(), BuildInput{SessionID: "session", Query: "继续", SystemPrompt: "system"})
	if err != nil {
		t.Fatal(err)
	}
	if len(compactor.input.Messages) != 3 || sessions.summary == nil || sessions.summary.ThroughSequence != 3 {
		t.Fatalf("compact=%d summary=%+v", len(compactor.input.Messages), sessions.summary)
	}
	if len(result.Messages) != 3 || len(sessions.messages) != 5 || !strings.Contains(result.SystemPrompt, "喜欢 Go") {
		t.Fatalf("context messages=%d raw=%d system=%q", len(result.Messages), len(sessions.messages), result.SystemPrompt)
	}
	if _, err := builder.Build(context.Background(), BuildInput{SessionID: "session", Query: "再次继续", SystemPrompt: "system"}); err != nil {
		t.Fatal(err)
	}
	if compactor.calls != 1 {
		t.Fatalf("摘要水位线未生效，压缩次数=%d", compactor.calls)
	}
}

func TestBuilderRejectsOversizedCurrentInput(t *testing.T) {
	builder := NewBuilder(&sessionStub{}, semanticMemoryStub{}, &compactorStub{}, config.ContextConfig{MaxInputTokens: 20, RecentMessages: 20, SummaryMaxTokens: 10}, 5, zap.NewNop())
	_, err := builder.Build(context.Background(), BuildInput{SessionID: "session", Query: strings.Repeat("界", 100), SystemPrompt: "system"})
	if !errors.Is(err, ErrContextTooLarge) {
		t.Fatalf("err=%v, want context_too_large", err)
	}
}
