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

	"github.com/enterprise/ai-agent-go/internal/memory"
	"github.com/enterprise/ai-agent-go/internal/model"
)

type sessionManagerStub struct {
	created model.UserInfo
	getErr  error
}

func (s *sessionManagerStub) CreateSession(_ context.Context, user model.UserInfo) (*model.Session, error) {
	s.created = user
	return &model.Session{ID: "session-1", UserID: user.UserID, CreatedAt: time.Unix(1, 0)}, nil
}
func (s *sessionManagerStub) GetSession(context.Context, string) (*model.Session, error) {
	return nil, s.getErr
}

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
func (*sessionManagerStub) GetUser(context.Context, string) (*model.UserInfo, error)   { return nil, nil }
func (*sessionManagerStub) AppendMessage(context.Context, string, model.Message) error { return nil }
func (*sessionManagerStub) GetMessages(context.Context, string) ([]model.Message, error) {
	return nil, nil
}
func (*sessionManagerStub) GetRecentMessages(context.Context, string, int) ([]model.Message, error) {
	return nil, nil
}
func (*sessionManagerStub) SaveSummary(context.Context, string, memory.SessionSummary) error {
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
