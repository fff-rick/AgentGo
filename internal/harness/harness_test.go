package harness

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/agentcontext"
	"github.com/enterprise/ai-agent-go/internal/agentloop"
	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/memory"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/tool"
)

type sessionsStub struct{ saved []model.Message }

func (*sessionsStub) CreateSession(context.Context, model.UserInfo) (*model.Session, error) {
	return nil, nil
}
func (*sessionsStub) GetSession(context.Context, string) (*model.Session, error) {
	panic("AgentHarness must use the preloaded session")
}
func (*sessionsStub) GetUser(context.Context, string) (*model.UserInfo, error) { return nil, nil }
func (s *sessionsStub) AppendMessage(_ context.Context, _ *model.Session, message model.Message) error {
	s.saved = append(s.saved, message)
	return nil
}
func (*sessionsStub) GetMessages(context.Context, *model.Session) ([]model.Message, error) {
	return nil, nil
}
func (*sessionsStub) GetRecentMessages(context.Context, *model.Session, int) ([]model.Message, error) {
	return nil, nil
}
func (*sessionsStub) SaveSummary(context.Context, *model.Session, *memory.SessionSummary, memory.SessionSummary) (bool, error) {
	return true, nil
}
func (*sessionsStub) GetSummary(context.Context, string) (*memory.SessionSummary, error) {
	return nil, nil
}

type contextStub struct{ result *agentcontext.AgentContext }

func (c contextStub) Build(context.Context, agentcontext.BuildInput) (*agentcontext.AgentContext, error) {
	return c.result, nil
}

type extractorStub struct {
	calls   chan extractionCall
	release <-chan struct{}
	err     error
}

type extractionCall struct {
	userID, sessionID, question, answer string
}

func (e *extractorStub) ExtractAndSave(_ context.Context, userID, sessionID, question, answer string) error {
	if e.calls != nil {
		e.calls <- extractionCall{userID: userID, sessionID: sessionID, question: question, answer: answer}
	}
	if e.release != nil {
		<-e.release
	}
	return e.err
}

func TestHarnessDoesNotFailAnswerWhenMemoryExtractionFails(t *testing.T) {
	sessions := &sessionsStub{}
	loop := &loopStub{}
	extractor := &extractorStub{calls: make(chan extractionCall, 1), err: errors.New("embedding unavailable")}
	contexts := contextStub{result: &agentcontext.AgentContext{Messages: []model.LLMMessage{{Role: "user", Content: "current"}}}}
	result, err := New(loop, nil, contexts, sessions, extractor, toolsStub{}, nil, 2, 4, time.Second, zap.NewNop()).Run(context.Background(), &RunRequest{Session: &model.Session{ID: "session", UserID: "user"}, Message: "current"})
	if err != nil || result.Answer != "draft" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	select {
	case <-extractor.calls:
	case <-time.After(time.Second):
		t.Fatal("memory extraction was not started")
	}
}

type loopStub struct{ input agentloop.Input }

func (l *loopStub) Run(_ context.Context, input agentloop.Input) (*agentloop.Result, error) {
	l.input = input
	return &agentloop.Result{Answer: "draft"}, nil
}

type plannerStub struct {
	calls   int
	task    string
	history []model.LLMMessage
}

func (p *plannerStub) Execute(_ context.Context, task string, history []model.LLMMessage, _ tool.Scope) (*agentloop.Result, error) {
	p.calls, p.task, p.history = p.calls+1, task, history
	return &agentloop.Result{Answer: "planned", Steps: []model.AgentStep{{Type: "action"}}}, nil
}

type toolsStub struct{ definitions []model.ToolDef }

func (t toolsStub) InitialToolDefinitions(context.Context, tool.Scope) []model.ToolDef {
	return t.definitions
}
func (toolsStub) ValidateAllowedTools([]string) error { return nil }

type hookStub struct{}

func (hookStub) AfterLoop(_ context.Context, _ *RunRequest, result *RunResult) error {
	result.Answer = "final"
	return nil
}

func TestHarnessOwnsRunLifecycle(t *testing.T) {
	sessions := &sessionsStub{}
	loop := &loopStub{}
	extractor := &extractorStub{calls: make(chan extractionCall, 1)}
	tools := toolsStub{definitions: []model.ToolDef{{Function: model.FunctionDef{Name: "knowledge_search"}}}}
	contexts := contextStub{result: &agentcontext.AgentContext{
		SystemPrompt: "system", Messages: []model.LLMMessage{{Role: "user", Content: "previous"}, {Role: "user", Content: "current"}}, Tools: tools.definitions,
	}}
	result, err := New(loop, nil, contexts, sessions, extractor, tools, []Hook{hookStub{}}, 5, 4, time.Second, zap.NewNop()).Run(
		context.Background(), &RunRequest{Session: &model.Session{ID: "session", UserID: "user"}, Message: "current"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "final" || result.RunID == "" || len(loop.input.Messages) != 2 || len(loop.input.Tools) != 1 {
		t.Fatalf("result=%+v input=%+v", result, loop.input)
	}
	if loop.input.State == nil || loop.input.State.UserID != "user" || loop.input.State.Task != "current" {
		t.Fatalf("working state=%+v", loop.input.State)
	}
	if len(sessions.saved) != 2 || sessions.saved[1].Content != "final" {
		t.Fatalf("saved=%+v", sessions.saved)
	}
	select {
	case call := <-extractor.calls:
		if call.userID != "user" || call.sessionID != "session" || call.question != "current" || call.answer != "final" {
			t.Fatalf("extraction call=%+v", call)
		}
	case <-time.After(time.Second):
		t.Fatal("memory extraction was not started")
	}
}

func TestHarnessDoesNotWaitForMemoryExtraction(t *testing.T) {
	release := make(chan struct{})
	extractor := &extractorStub{calls: make(chan extractionCall, 1), release: release}
	h := New(&loopStub{}, nil, contextStub{result: &agentcontext.AgentContext{}}, &sessionsStub{}, extractor, toolsStub{}, nil, 2, 4, time.Second, zap.NewNop())
	done := make(chan error, 1)
	go func() {
		_, err := h.Run(context.Background(), &RunRequest{Session: &model.Session{ID: "session", UserID: "user"}, Message: "current"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("Run waited for memory extraction")
	}
	close(release)
}

type precompactHarnessSessions struct {
	*sessionsStub
	failRole string
	loads    atomic.Int32
}

func (s *precompactHarnessSessions) AppendMessage(ctx context.Context, session *model.Session, message model.Message) error {
	if message.Role == s.failRole {
		return errors.New("save failed")
	}
	return s.sessionsStub.AppendMessage(ctx, session, message)
}

func (s *precompactHarnessSessions) GetMessages(context.Context, *model.Session) ([]model.Message, error) {
	s.loads.Add(1)
	messages := make([]model.Message, 25)
	for i := range messages {
		messages[i] = model.Message{Sequence: int64(i + 1), Role: "user", Content: "old"}
	}
	return messages, nil
}

type precompactHarnessCompactor struct {
	started chan struct{}
	release <-chan struct{}
}

func (*precompactHarnessCompactor) ShouldCompact(agentcontext.ContextBudgetInput) bool { return false }
func (c *precompactHarnessCompactor) Compact(ctx context.Context, _ agentcontext.CompactionInput) (*agentcontext.CompactionResult, error) {
	close(c.started)
	select {
	case <-c.release:
		return &agentcontext.CompactionResult{Summary: "summary"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestHarnessPrecompactionDoesNotBlockResponse(t *testing.T) {
	sessions := &precompactHarnessSessions{sessionsStub: &sessionsStub{}}
	release := make(chan struct{})
	compactor := &precompactHarnessCompactor{started: make(chan struct{}), release: release}
	builder := agentcontext.NewBuilder(sessions, nil, compactor, config.ContextConfig{MaxInputTokens: 100, RecentMessages: 20}, 5, zap.NewNop())
	precompactor := agentcontext.NewPrecompactor(builder)
	h := New(&loopStub{}, nil, contextStub{result: &agentcontext.AgentContext{EstimatedTokens: 70, HistoryMessages: 25}}, sessions, nil, toolsStub{}, nil, 2, 4, time.Second, zap.NewNop())
	h.SetPrecompactor(precompactor)
	done := make(chan error, 1)
	go func() {
		_, err := h.Run(context.Background(), &RunRequest{Session: &model.Session{ID: "session", UserID: "user"}, Message: "current"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("response waited for summary compaction")
	}
	select {
	case <-compactor.started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("summary compaction was not started")
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := precompactor.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessSkipsPrecompactionWhenEitherMessageSaveFails(t *testing.T) {
	for _, failedRole := range []string{"user", "assistant"} {
		sessions := &precompactHarnessSessions{sessionsStub: &sessionsStub{}, failRole: failedRole}
		builder := agentcontext.NewBuilder(sessions, nil, nil, config.ContextConfig{MaxInputTokens: 100, RecentMessages: 20}, 5, zap.NewNop())
		precompactor := agentcontext.NewPrecompactor(builder)
		h := New(&loopStub{}, nil, contextStub{result: &agentcontext.AgentContext{EstimatedTokens: 70, HistoryMessages: 25}}, sessions, nil, toolsStub{}, nil, 2, 4, time.Second, zap.NewNop())
		h.SetPrecompactor(precompactor)
		if _, err := h.Run(context.Background(), &RunRequest{Session: &model.Session{ID: "session", UserID: "user"}, Message: "current"}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := precompactor.Close(ctx)
		cancel()
		if err != nil || sessions.loads.Load() != 0 {
			t.Fatalf("failed role=%s close err=%v background loads=%d", failedRole, err, sessions.loads.Load())
		}
	}
}

func TestHarnessUsesPlannerOnlyWhenExplicitlyRequested(t *testing.T) {
	sessions := &sessionsStub{}
	loop := &loopStub{}
	planner := &plannerStub{}
	contexts := contextStub{result: &agentcontext.AgentContext{
		SystemPrompt: "memory context", Messages: []model.LLMMessage{{Role: "assistant", Content: "previous"}, {Role: "user", Content: "complex task"}},
	}}
	result, err := New(loop, planner, contexts, sessions, nil, toolsStub{}, nil, 3, 4, time.Second, zap.NewNop()).Run(
		context.Background(), &RunRequest{Session: &model.Session{ID: "session", UserID: "user"}, Message: "complex task", Mode: model.ExecutionModePlanner},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "planned" || planner.calls != 1 || loop.input.Messages != nil {
		t.Fatalf("result=%+v planner=%+v loop=%+v", result, planner, loop.input)
	}
	if len(planner.history) != 2 || planner.history[0].Content != "memory context" || planner.history[1].Content != "previous" {
		t.Fatalf("planner history=%+v", planner.history)
	}
}
