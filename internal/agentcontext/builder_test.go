package agentcontext

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
	panic("ContextBuilder must use the preloaded session")
}
func (*sessionStub) GetUser(context.Context, string) (*model.UserInfo, error) {
	return &model.UserInfo{UserID: "user-1", DisplayName: "Xin"}, nil
}
func (*sessionStub) AppendMessage(context.Context, *model.Session, model.Message) error { return nil }
func (s *sessionStub) GetMessages(context.Context, *model.Session) ([]model.Message, error) {
	return append([]model.Message(nil), s.messages...), nil
}
func (*sessionStub) GetRecentMessages(context.Context, *model.Session, int) ([]model.Message, error) {
	return nil, nil
}
func (s *sessionStub) SaveSummary(_ context.Context, _ *model.Session, expected *memory.SessionSummary, summary memory.SessionSummary) (bool, error) {
	if s.summary != expected {
		return false, nil
	}
	s.summary = &summary
	return true, nil
}
func (s *sessionStub) GetSummary(context.Context, string) (*memory.SessionSummary, error) {
	return s.summary, nil
}

type semanticMemoryStub struct{ items []model.MemoryItem }

func (s semanticMemoryStub) Search(context.Context, string, string, int) ([]model.MemoryItem, error) {
	return s.items, nil
}
func (semanticMemoryStub) Save(context.Context, model.MemoryItem) error { return nil }

type blockingSessionStub struct {
	*sessionStub
	started chan<- string
	release <-chan struct{}
}

func (s *blockingSessionStub) wait(name string) {
	s.started <- name
	<-s.release
}

func (s *blockingSessionStub) GetUser(ctx context.Context, userID string) (*model.UserInfo, error) {
	s.wait("user")
	return s.sessionStub.GetUser(ctx, userID)
}

func (s *blockingSessionStub) GetMessages(ctx context.Context, session *model.Session) ([]model.Message, error) {
	s.wait("messages")
	return s.sessionStub.GetMessages(ctx, session)
}

func (s *blockingSessionStub) GetSummary(ctx context.Context, sessionID string) (*memory.SessionSummary, error) {
	s.wait("summary")
	return s.sessionStub.GetSummary(ctx, sessionID)
}

type blockingSemanticMemory struct {
	started chan<- string
	release <-chan struct{}
}

func (s blockingSemanticMemory) Search(context.Context, string, string, int) ([]model.MemoryItem, error) {
	s.started <- "memories"
	<-s.release
	return nil, nil
}

func (blockingSemanticMemory) Save(context.Context, model.MemoryItem) error { return nil }

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
	result, err := builder.Build(context.Background(), BuildInput{Session: &model.Session{ID: "session", UserID: "user-1"}, Query: "继续", SystemPrompt: "system"})
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
	result, err := builder.Build(context.Background(), BuildInput{Session: &model.Session{ID: "session", UserID: "user-1"}, Query: "继续", SystemPrompt: "system"})
	if err != nil {
		t.Fatal(err)
	}
	if len(compactor.input.Messages) != 3 || sessions.summary == nil || sessions.summary.ThroughSequence != 3 {
		t.Fatalf("compact=%d summary=%+v", len(compactor.input.Messages), sessions.summary)
	}
	if len(result.Messages) != 3 || len(sessions.messages) != 5 || !strings.Contains(result.SystemPrompt, "喜欢 Go") {
		t.Fatalf("context messages=%d raw=%d system=%q", len(result.Messages), len(sessions.messages), result.SystemPrompt)
	}
	if _, err := builder.Build(context.Background(), BuildInput{Session: &model.Session{ID: "session", UserID: "user-1"}, Query: "再次继续", SystemPrompt: "system"}); err != nil {
		t.Fatal(err)
	}
	if compactor.calls != 1 {
		t.Fatalf("摘要水位线未生效，压缩次数=%d", compactor.calls)
	}
}

func TestBuilderRejectsOversizedCurrentInput(t *testing.T) {
	builder := NewBuilder(&sessionStub{}, semanticMemoryStub{}, &compactorStub{}, config.ContextConfig{MaxInputTokens: 20, RecentMessages: 20, SummaryMaxTokens: 10}, 5, zap.NewNop())
	_, err := builder.Build(context.Background(), BuildInput{Session: &model.Session{ID: "session", UserID: "user-1"}, Query: strings.Repeat("界", 100), SystemPrompt: "system"})
	if !errors.Is(err, ErrContextTooLarge) {
		t.Fatalf("err=%v, want context_too_large", err)
	}
}

func TestDropOldestTurnNeverLeavesOrphanAssistant(t *testing.T) {
	messages := []model.Message{{Role: "user", Content: "order"}, {Role: "assistant", Content: "number?"}, {Role: "user", Content: "A123"}, {Role: "assistant", Content: "done"}}
	remaining := dropOldestTurn(messages)
	if len(remaining) != 2 || remaining[0].Role != "user" || remaining[0].Content != "A123" {
		t.Fatalf("remaining=%+v", remaining)
	}
	if got := dropOldestTurn([]model.Message{{Role: "assistant", Content: "orphan"}, {Role: "user", Content: "next"}}); len(got) != 1 || got[0].Role != "user" {
		t.Fatalf("orphan remains: %+v", got)
	}
}

func TestCompactionDoesNotSplitRetainedTurn(t *testing.T) {
	active := []model.Message{{Sequence: 1, Role: "user", Content: "one"}, {Sequence: 2, Role: "assistant", Content: "reply"}, {Sequence: 3, Role: "user", Content: "two"}, {Sequence: 4, Role: "assistant", Content: "reply"}}
	compactor := &compactorStub{}
	builder := NewBuilder(&sessionStub{}, semanticMemoryStub{}, compactor, config.ContextConfig{MaxInputTokens: 100, RecentMessages: 3, SummaryMaxTokens: 20}, 5, zap.NewNop())
	summary, remaining, err := builder.compact(context.Background(), &model.Session{ID: "session", UserID: "user-1"}, nil, active)
	if err != nil || summary != nil || len(remaining) != len(active) || compactor.calls != 0 {
		t.Fatalf("summary=%+v remaining=%+v err=%v calls=%d", summary, remaining, err, compactor.calls)
	}
}

type conflictingSummarySessions struct{ *sessionStub }

func (s *conflictingSummarySessions) SaveSummary(_ context.Context, _ *model.Session, _ *memory.SessionSummary, _ memory.SessionSummary) (bool, error) {
	s.summary = &memory.SessionSummary{Content: "newer summary", ThroughSequence: 3}
	return false, nil
}

func TestBuilderReloadsSummaryAfterConcurrentUpdate(t *testing.T) {
	sessions := &conflictingSummarySessions{sessionStub: &sessionStub{}}
	for sequence := int64(1); sequence <= 5; sequence++ {
		sessions.messages = append(sessions.messages, model.Message{Sequence: sequence, Role: "user", Content: strings.Repeat("a", 500)})
	}
	compactor := &compactorStub{}
	builder := NewBuilder(sessions, semanticMemoryStub{}, compactor, config.ContextConfig{MaxInputTokens: 500, RecentMessages: 2, SummaryMaxTokens: 100}, 5, zap.NewNop())
	result, err := builder.Build(context.Background(), BuildInput{Session: &model.Session{ID: "session", UserID: "user-1"}, Query: "next", SystemPrompt: "system"})
	if err != nil || compactor.calls != 1 || !strings.Contains(result.SystemPrompt, "newer summary") || len(result.Messages) != 3 {
		t.Fatalf("result=%+v err=%v compact_calls=%d", result, err, compactor.calls)
	}
}

func TestBuilderDoesNotCompactAcrossSequenceGap(t *testing.T) {
	sessions := &sessionStub{}
	for _, sequence := range []int64{1, 2, 4, 5, 6} {
		sessions.messages = append(sessions.messages, model.Message{Sequence: sequence, Role: "user", Content: strings.Repeat("a", 500)})
	}
	compactor := &compactorStub{}
	builder := NewBuilder(sessions, semanticMemoryStub{}, compactor, config.ContextConfig{MaxInputTokens: 500, RecentMessages: 2, SummaryMaxTokens: 100}, 5, zap.NewNop())
	result, err := builder.Build(context.Background(), BuildInput{Session: &model.Session{ID: "session", UserID: "user-1"}, Query: "next", SystemPrompt: "system"})
	if err != nil || compactor.calls != 0 || sessions.summary != nil || result.EstimatedTokens > 500 {
		t.Fatalf("result=%+v err=%v compact_calls=%d summary=%+v", result, err, compactor.calls, sessions.summary)
	}
}

func TestBuilderLoadsIndependentContextConcurrently(t *testing.T) {
	started := make(chan string, 4)
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()

	sessions := &blockingSessionStub{sessionStub: &sessionStub{}, started: started, release: release}
	semantic := blockingSemanticMemory{started: started, release: release}
	builder := NewBuilder(sessions, semantic, &compactorStub{}, config.ContextConfig{MaxInputTokens: 1000}, 5, zap.NewNop())
	done := make(chan error, 1)
	go func() {
		_, err := builder.Build(context.Background(), BuildInput{Session: &model.Session{ID: "session", UserID: "user-1"}, Query: "continue", SystemPrompt: "system"})
		done <- err
	}()

	seen := make(map[string]bool, 4)
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for len(seen) < 4 {
		select {
		case name := <-started:
			seen[name] = true
		case <-timer.C:
			t.Fatalf("context loads did not run concurrently; started=%v", seen)
		}
	}
	close(release)
	released = true
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
