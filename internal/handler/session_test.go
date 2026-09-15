package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/agent"
	"github.com/enterprise/ai-agent-go/internal/agentcontext"
	"github.com/enterprise/ai-agent-go/internal/agentloop"
	"github.com/enterprise/ai-agent-go/internal/harness"
	"github.com/enterprise/ai-agent-go/internal/memory"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/tool"
)

type sessionManagerStub struct {
	created  model.UserInfo
	getErr   error
	getCalls int
}

func (s *sessionManagerStub) CreateSession(_ context.Context, user model.UserInfo) (*model.Session, error) {
	s.created = user
	return &model.Session{ID: "session-1", UserID: user.UserID, CreatedAt: time.Unix(1, 0)}, nil
}
func (s *sessionManagerStub) GetSession(context.Context, string) (*model.Session, error) {
	s.getCalls++
	if s.getErr != nil {
		return nil, s.getErr
	}
	return &model.Session{ID: "session-1", UserID: "user-1"}, nil
}

type chatLoopStub struct{}

func (chatLoopStub) Run(context.Context, agentloop.Input) (*agentloop.Result, error) {
	return &agentloop.Result{Answer: "hello"}, nil
}

type chatContextStub struct{}

func (chatContextStub) Build(_ context.Context, input agentcontext.BuildInput) (*agentcontext.AgentContext, error) {
	return &agentcontext.AgentContext{Messages: []model.LLMMessage{{Role: "user", Content: input.Query}}}, nil
}

type recordingChatContext struct{ input agentcontext.BuildInput }

func (c *recordingChatContext) Build(_ context.Context, input agentcontext.BuildInput) (*agentcontext.AgentContext, error) {
	c.input = input
	return &agentcontext.AgentContext{Messages: []model.LLMMessage{{Role: "user", Content: input.Query}}, Tools: input.Tools}, nil
}

type chatToolsStub struct{}

func (chatToolsStub) InitialToolDefinitions(context.Context, tool.Scope) []model.ToolDef { return nil }
func (chatToolsStub) ValidateAllowedTools([]string) error                                { return nil }

func TestChatEndpointsRejectUnknownSessionBeforeRunningAgent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := &sessionManagerStub{getErr: memory.ErrSessionNotFound}
	handler := NewChatHandler(nil, manager, zap.NewNop())
	for _, path := range []string{"/api/v1/chat", "/api/v1/chat/stream"} {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{"session_id":"missing","message":"hello"}`))
		req.Header.Set("Content-Type", "application/json")
		context, _ := gin.CreateTestContext(recorder)
		context.Request = req
		if path == "/api/v1/chat" {
			handler.Chat(context)
		} else {
			handler.ChatStream(context)
		}
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("path=%s status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewBufferString(`{"session_id":"session","message":"hello","options":{"mode":"auto-detect"}}`))
	req.Header.Set("Content-Type", "application/json")
	context, _ := gin.CreateTestContext(recorder)
	context.Request = req
	handler.Chat(context)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestChatEndpointsReadSessionOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"/api/v1/chat", "/api/v1/chat/stream"} {
		manager := &sessionManagerStub{}
		agentHarness := harness.New(chatLoopStub{}, nil, chatContextStub{}, manager, nil, chatToolsStub{}, nil, 2, 4, time.Second, zap.NewNop())
		handler := NewChatHandler(agent.NewOrchestrator(agentHarness), manager, zap.NewNop())
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{"session_id":"session-1","message":"hello"}`))
		req.Header.Set("Content-Type", "application/json")
		context, _ := gin.CreateTestContext(recorder)
		context.Request = req
		if path == "/api/v1/chat" {
			handler.Chat(context)
		} else {
			handler.ChatStream(context)
		}
		if recorder.Code != http.StatusOK || manager.getCalls != 1 {
			t.Fatalf("path=%s status=%d GetSession calls=%d body=%s", path, recorder.Code, manager.getCalls, recorder.Body.String())
		}
	}
}

func TestChatToolsAllowlistValidationAndExplicitEmptyList(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := tool.NewRegistry()
	manager := tool.NewManager(registry, nil, 3, time.Hour, zap.NewNop())
	toolRouter := tool.NewRouter(registry, zap.NewNop(), manager)
	sessions := &sessionManagerStub{}
	contexts := &recordingChatContext{}
	agentHarness := harness.New(chatLoopStub{}, nil, contexts, sessions, nil, toolRouter, nil, 2, 4, time.Second, zap.NewNop())
	handler := NewChatHandler(agent.NewOrchestrator(agentHarness), sessions, zap.NewNop())

	for _, toolsJSON := range []string{`["missing"]`, `["list_tools"]`} {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewBufferString(`{"session_id":"session-1","message":"hello","options":{"tools":`+toolsJSON+`}}`))
		req.Header.Set("Content-Type", "application/json")
		context, _ := gin.CreateTestContext(recorder)
		context.Request = req
		handler.Chat(context)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("tools=%s status=%d body=%s", toolsJSON, recorder.Code, recorder.Body.String())
		}
	}
	if sessions.getCalls != 0 {
		t.Fatalf("invalid tools reached session lookup: %d", sessions.getCalls)
	}

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewBufferString(`{"session_id":"session-1","message":"hello","options":{"tools":[]}}`))
	req.Header.Set("Content-Type", "application/json")
	context, _ := gin.CreateTestContext(recorder)
	context.Request = req
	handler.Chat(context)
	if recorder.Code != http.StatusOK {
		t.Fatalf("empty allowlist status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if len(contexts.input.Tools) != 1 || contexts.input.Tools[0].Function.Name != tool.ListToolsName {
		t.Fatalf("empty allowlist initial tools=%+v", contexts.input.Tools)
	}
}
func (*sessionManagerStub) GetUser(context.Context, string) (*model.UserInfo, error) { return nil, nil }
func (*sessionManagerStub) AppendMessage(context.Context, *model.Session, model.Message) error {
	return nil
}
func (*sessionManagerStub) GetMessages(context.Context, *model.Session) ([]model.Message, error) {
	return nil, nil
}
func (*sessionManagerStub) GetRecentMessages(context.Context, *model.Session, int) ([]model.Message, error) {
	return nil, nil
}
func (*sessionManagerStub) SaveSummary(context.Context, *model.Session, memory.SessionSummary) error {
	return nil
}
func (*sessionManagerStub) GetSummary(context.Context, string) (*memory.SessionSummary, error) {
	return nil, nil
}

func TestCreateSessionValidatesAndReturnsBinding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := &sessionManagerStub{}
	router := gin.New()
	router.POST("/api/v1/sessions", NewSessionHandler(manager).Create)

	body := []byte(`{"user":{"user_id":"user-1","display_name":"Xin","metadata":{"language":"zh-CN"}}}`)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || manager.created.UserID != "user-1" {
		t.Fatalf("status=%d created=%+v body=%s", recorder.Code, manager.created, recorder.Body.String())
	}
	var result struct {
		Data model.CreateSessionResponse `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil || result.Data.SessionID != "session-1" {
		t.Fatalf("result=%+v err=%v", result, err)
	}

	recorder = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/sessions", bytes.NewBufferString(`{"user":{"user_id":"bad id"}}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid user status=%d", recorder.Code)
	}
}
