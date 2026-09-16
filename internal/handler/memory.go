package handler

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/enterprise/ai-agent-go/internal/auth"
	"github.com/enterprise/ai-agent-go/internal/memory"
	"github.com/enterprise/ai-agent-go/pkg/common"
	"github.com/gin-gonic/gin"
)

type MemoryHandler struct{ store *memory.ManagedStore }

func NewMemoryHandler(store *memory.ManagedStore) *MemoryHandler { return &MemoryHandler{store: store} }

func (h *MemoryHandler) List(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	items, err := h.store.List(c.Request.Context(), auth.Identity(c), limit, offset)
	if err != nil {
		common.Fail(c, 500, common.ErrInternal(err))
		return
	}
	common.OK(c, items)
}

func (h *MemoryHandler) Get(c *gin.Context) {
	item, err := h.store.Get(c.Request.Context(), auth.Identity(c), c.Param("id"))
	if err != nil {
		writeMemoryError(c, err)
		return
	}
	common.OK(c, item)
}

func (h *MemoryHandler) Versions(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	items, err := h.store.Versions(c.Request.Context(), auth.Identity(c), c.Param("id"), limit, offset)
	if err != nil {
		writeMemoryError(c, err)
		return
	}
	common.OK(c, items)
}

func (h *MemoryHandler) Correct(c *gin.Context) {
	var req struct {
		Content string `json:"content" binding:"required"`
		Version int64  `json:"version" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.FailWithCode(c, 400, common.ErrCodeInvalidParam, err.Error())
		return
	}
	item, err := h.store.Correct(c.Request.Context(), auth.Identity(c), c.Param("id"), req.Content, req.Version)
	if err != nil {
		writeMemoryError(c, err)
		return
	}
	common.OK(c, item)
}

func (h *MemoryHandler) SetExpiry(c *gin.Context) {
	var req struct {
		ValidTo *time.Time `json:"valid_to"`
		Version int64      `json:"version" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.FailWithCode(c, 400, common.ErrCodeInvalidParam, err.Error())
		return
	}
	if err := h.store.SetExpiry(c.Request.Context(), auth.Identity(c), c.Param("id"), req.Version, req.ValidTo); err != nil {
		writeMemoryError(c, err)
		return
	}
	common.OK(c, gin.H{"updated": true})
}

func (h *MemoryHandler) Delete(c *gin.Context) {
	if err := h.store.Delete(c.Request.Context(), auth.Identity(c), c.Param("id")); err != nil {
		writeMemoryError(c, err)
		return
	}
	common.OK(c, gin.H{"deleted": true})
}

func writeMemoryError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, memory.ErrMemoryNotFound):
		common.FailWithCode(c, http.StatusNotFound, common.ErrCodeNotFound, err.Error())
	case errors.Is(err, memory.ErrMemoryVersion):
		common.FailWithCode(c, http.StatusConflict, common.ErrCodeInvalidParam, err.Error())
	case errors.Is(err, memory.ErrInvalidMemory):
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, err.Error())
	default:
		common.Fail(c, http.StatusInternalServerError, common.ErrInternal(err))
	}
}
