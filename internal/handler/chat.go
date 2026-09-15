// Package handler 提供 HTTP 请求处理器。
package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/agent"
	"github.com/enterprise/ai-agent-go/internal/agentcontext"
	"github.com/enterprise/ai-agent-go/internal/memory"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/observe"
	"github.com/enterprise/ai-agent-go/pkg/common"
)

// ChatHandler 对话请求处理器
type ChatHandler struct {
	orchestrator *agent.Orchestrator
	sessions     memory.SessionManager
	logger       *zap.Logger
}

// NewChatHandler 创建对话处理器
func NewChatHandler(orchestrator *agent.Orchestrator, sessions memory.SessionManager, logger *zap.Logger) *ChatHandler {
	return &ChatHandler{
		orchestrator: orchestrator,
		sessions:     sessions,
		logger:       logger,
	}
}

// Chat 处理同步对话请求
// POST /api/v1/chat
func (h *ChatHandler) Chat(c *gin.Context) {
	var req model.ChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "请求参数错误: "+err.Error())
		return
	}
	if !validExecutionMode(req.Options) {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "options.mode 只支持 agent 或 planner")
		return
	}
	if req.Options != nil && req.Options.Tools != nil {
		if err := h.orchestrator.ValidateAllowedTools(req.Options.Tools); err != nil {
			common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "options.tools 无效: "+err.Error())
			return
		}
	}
	session, err := h.sessions.GetSession(c.Request.Context(), req.SessionID)
	if err != nil {
		h.writeChatError(c, err)
		return
	}

	h.logger.Info("收到对话请求",
		zap.String("session_id", req.SessionID),
		zap.Int("message_len", len(req.Message)),
	)

	ctx := c.Request.Context()

	resp, err := h.orchestrator.ProcessMessage(ctx, &req, session)
	if err != nil {
		h.logger.Error("对话处理失败", zap.Error(err))
		h.writeChatError(c, err)
		return
	}

	resp.MessageID = uuid.New().String()
	common.OK(c, resp)
}

// ChatStream 处理 SSE 流式对话请求
// POST /api/v1/chat/stream
func (h *ChatHandler) ChatStream(c *gin.Context) {
	var req model.ChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "请求参数错误: "+err.Error())
		return
	}
	if !validExecutionMode(req.Options) {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "options.mode 只支持 agent 或 planner")
		return
	}
	if req.Options != nil && req.Options.Tools != nil {
		if err := h.orchestrator.ValidateAllowedTools(req.Options.Tools); err != nil {
			common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "options.tools 无效: "+err.Error())
			return
		}
	}
	session, err := h.sessions.GetSession(c.Request.Context(), req.SessionID)
	if err != nil {
		h.writeChatError(c, err)
		return
	}

	h.logger.Info("收到流式对话请求",
		zap.String("session_id", req.SessionID),
	)

	// 设置 SSE 响应头
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	ctx := c.Request.Context()
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		common.FailWithCode(c, http.StatusInternalServerError, common.ErrCodeInternal, "不支持流式响应")
		return
	}

	// 发送会话 ID
	h.writeSSE(c.Writer, "session", map[string]string{"session_id": req.SessionID})
	flusher.Flush()

	ctx = observe.WithEmitter(ctx, func(event observe.Event) {
		h.writeSSE(c.Writer, event.Type, event)
		flusher.Flush()
	})
	resp, err := h.orchestrator.ProcessMessage(ctx, &req, session)
	if err != nil {
		h.writeSSE(c.Writer, "error", map[string]string{"error": err.Error()})
		flusher.Flush()
		return
	}

	// 发送完成事件
	resp.MessageID = uuid.New().String()
	h.writeSSE(c.Writer, "done", resp)
	flusher.Flush()
}

func validExecutionMode(options *model.ChatOptions) bool {
	return options == nil || options.Mode == "" || options.Mode == model.ExecutionModeAgent || options.Mode == model.ExecutionModePlanner
}

func (*ChatHandler) writeChatError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, memory.ErrSessionNotFound):
		common.FailWithCode(c, http.StatusNotFound, common.ErrCodeNotFound, memory.ErrSessionNotFound.Error())
	case errors.Is(err, agentcontext.ErrContextTooLarge):
		common.FailWithCode(c, http.StatusRequestEntityTooLarge, common.ErrCodeInvalidParam, err.Error())
	default:
		common.Fail(c, http.StatusInternalServerError, common.ErrInternal(err))
	}
}

// writeSSE 写入一条 SSE 事件
func (h *ChatHandler) writeSSE(w io.Writer, event string, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		data = []byte(`{"error":"serialize stream event failed"}`)
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
}
