package handler

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/etl"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/pkg/common"
)

const maxMarkdownBytes = 10 << 20

// DocumentHandler 文档处理器
type DocumentHandler struct {
	pipeline *etl.Pipeline
	logger   *zap.Logger
}

// NewDocumentHandler 创建文档处理器
func NewDocumentHandler(pipeline *etl.Pipeline, logger *zap.Logger) *DocumentHandler {
	return &DocumentHandler{
		pipeline: pipeline,
		logger:   logger,
	}
}

// Upload 上传文档
// POST /api/v1/documents
func (h *DocumentHandler) Upload(c *gin.Context) {
	var req model.DocumentUploadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "请求参数错误: "+err.Error())
		return
	}

	doc := &model.Document{
		ID: uuid.NewString(), Title: req.Title, Content: req.Content,
		ContentType: req.ContentType, Tags: req.Tags, Metadata: req.Metadata,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	h.process(c, doc)
}

// ImportMarkdown 上传本地 Markdown 文件并写入知识库。
// POST /api/v1/documents/import，multipart 字段名为 file，title 可选。
func (h *DocumentHandler) ImportMarkdown(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxMarkdownBytes+(1<<20))
	header, err := c.FormFile("file")
	if err != nil {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "请通过 multipart/form-data 的 file 字段上传 Markdown 文件")
		return
	}
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext != ".md" && ext != ".markdown" {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "仅支持 .md 或 .markdown 文件")
		return
	}
	if header.Size > maxMarkdownBytes {
		common.FailWithCode(c, http.StatusRequestEntityTooLarge, common.ErrCodeInvalidParam, "Markdown 文件不能超过 10 MiB")
		return
	}
	file, err := header.Open()
	if err != nil {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "打开上传文件失败")
		return
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxMarkdownBytes+1))
	if err != nil {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "读取上传文件失败")
		return
	}
	if len(content) > maxMarkdownBytes {
		common.FailWithCode(c, http.StatusRequestEntityTooLarge, common.ErrCodeInvalidParam, "Markdown 文件不能超过 10 MiB")
		return
	}
	if len(content) == 0 || !utf8.Valid(content) {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "Markdown 文件必须是非空 UTF-8 文本")
		return
	}
	title := strings.TrimSpace(c.PostForm("title"))
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(header.Filename), ext)
	}
	doc := &model.Document{
		ID: uuid.NewString(), Title: title, Content: string(content), ContentType: "markdown",
		Metadata:  map[string]string{"source_file": filepath.Base(header.Filename)},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	h.process(c, doc)
}

func (h *DocumentHandler) process(c *gin.Context, doc *model.Document) {
	h.logger.Info("收到文档导入请求",
		zap.String("doc_id", doc.ID),
		zap.String("title", doc.Title),
		zap.String("content_type", doc.ContentType),
		zap.Int("content_len", len(doc.Content)),
	)
	resp, err := h.pipeline.ProcessDocument(c.Request.Context(), doc)
	if err != nil {
		h.logger.Error("文档处理失败", zap.String("doc_id", doc.ID), zap.Error(err))
		common.Fail(c, http.StatusInternalServerError, common.ErrInternal(err))
		return
	}
	common.OK(c, resp)
}

// GetStatus 查询文档处理状态
// GET /api/v1/documents/:id
func (h *DocumentHandler) GetStatus(c *gin.Context) {
	docID := c.Param("id")
	if docID == "" {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "文档 ID 不能为空")
		return
	}

	// 实际项目中从数据库查询文档状态
	// doc, err := docRepo.FindByID(ctx, docID)

	resp := &model.DocumentResponse{
		DocID:      docID,
		Title:      "示例文档",
		Status:     "completed",
		ChunkCount: 10,
		CreatedAt:  time.Now(),
	}

	common.OK(c, resp)
}
