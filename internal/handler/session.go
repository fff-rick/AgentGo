package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/enterprise/ai-agent-go/internal/memory"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/user"
	"github.com/enterprise/ai-agent-go/pkg/common"
)

type SessionHandler struct {
	sessions memory.SessionManager
}

func NewSessionHandler(sessions memory.SessionManager) *SessionHandler {
	return &SessionHandler{sessions: sessions}
}

func (h *SessionHandler) Create(c *gin.Context) {
	var req model.CreateSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "请求参数错误: "+err.Error())
		return
	}
	if err := user.Validate(req.User); err != nil {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, err.Error())
		return
	}
	session, err := h.sessions.CreateSession(c.Request.Context(), req.User)
	if err != nil {
		common.Fail(c, http.StatusInternalServerError, common.ErrInternal(err))
		return
	}
	common.OK(c, model.CreateSessionResponse{SessionID: session.ID, UserID: session.UserID, CreatedAt: session.CreatedAt})
}
