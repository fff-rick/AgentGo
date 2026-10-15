package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/auth"
	"github.com/enterprise/ai-agent-go/internal/etl"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/pkg/common"
)

const (
	maxMarkdownBytes         = 10 << 20
	maxDocumentBytes         = 50 << 20
	maxDocumentMetadataBytes = 64 << 10
	maxDocumentJSONBytes     = maxMarkdownBytes + maxDocumentMetadataBytes + (1 << 20)
)

// DocumentHandler 文档处理器
type DocumentHandler struct {
	pipeline     *etl.Pipeline
	importer     *etl.Importer
	documentRepo documentReader
	logger       *zap.Logger
	publishers   map[string]struct{}
}

type documentReader interface {
	FindDocument(context.Context, string) (*model.DocumentResponse, error)
}

// NewDocumentHandler 创建文档处理器
func NewDocumentHandler(pipeline *etl.Pipeline, importer *etl.Importer, logger *zap.Logger, publisherSubjects []string, repos ...documentReader) *DocumentHandler {
	h := &DocumentHandler{
		pipeline: pipeline, importer: importer,
		logger: logger, publishers: make(map[string]struct{}, len(publisherSubjects)),
	}
	for _, subject := range publisherSubjects {
		if subject = strings.TrimSpace(subject); subject != "" {
			h.publishers[subject] = struct{}{}
		}
	}
	if len(repos) > 0 {
		h.documentRepo = repos[0]
	}
	return h
}

// Upload 上传文档
// POST /api/v1/documents
func (h *DocumentHandler) Upload(c *gin.Context) {
	if !h.canPublish(c) {
		common.FailWithCode(c, http.StatusForbidden, common.ErrCodeForbidden, "当前身份没有共享知识库写入权限")
		return
	}
	var req model.DocumentUploadRequest
	if !bindLimitedJSON(c, &req, maxDocumentJSONBytes) {
		return
	}
	if len([]byte(req.Content)) > maxMarkdownBytes {
		common.FailWithCode(c, http.StatusRequestEntityTooLarge, common.ErrCodeInvalidParam, "文档文本不能超过 10 MiB")
		return
	}
	metadata, err := json.Marshal(req.Metadata)
	if err != nil {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "metadata 不是有效的 JSON")
		return
	}
	if len(metadata) > maxDocumentMetadataBytes {
		common.FailWithCode(c, http.StatusRequestEntityTooLarge, common.ErrCodeInvalidParam, "文档 metadata 不能超过 64 KiB")
		return
	}
	expectedHash, err := expectedContentHash(c, req.ExpectedContentHash)
	if err != nil {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, err.Error())
		return
	}

	doc := &model.Document{
		Title: req.Title, Content: req.Content,
		ContentType: req.ContentType, Tags: req.Tags, Metadata: req.Metadata,
		ExpectedContentHash: expectedHash,
		CreatedAt:           time.Now(), UpdatedAt: time.Now(),
	}
	h.process(c, doc)
}

// ImportDocument asynchronously imports a supported local document.
// POST /api/v1/documents/import，multipart 字段名为 file，title 可选。
func (h *DocumentHandler) ImportDocument(c *gin.Context) {
	if !h.canPublish(c) {
		common.FailWithCode(c, http.StatusForbidden, common.ErrCodeForbidden, "当前身份没有共享知识库写入权限")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxDocumentBytes+(1<<20))
	header, err := c.FormFile("file")
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			common.FailWithCode(c, http.StatusRequestEntityTooLarge, common.ErrCodeInvalidParam, "文件不能超过 50 MiB")
			return
		}
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "请通过 multipart/form-data 的 file 字段上传文件")
		return
	}
	ext := strings.ToLower(filepath.Ext(header.Filename))
	docType, ok := map[string]string{".md": "markdown", ".markdown": "markdown", ".pdf": "pdf", ".docx": "docx", ".xlsx": "xlsx"}[ext]
	if !ok {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "仅支持 Markdown、PDF、DOCX 或 XLSX 文件")
		return
	}
	limit := int64(maxDocumentBytes)
	if docType == "markdown" {
		limit = maxMarkdownBytes
	}
	if header.Size > limit {
		common.FailWithCode(c, http.StatusRequestEntityTooLarge, common.ErrCodeInvalidParam, fmt.Sprintf("文件不能超过 %d MiB", limit>>20))
		return
	}
	file, err := header.Open()
	if err != nil {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "打开上传文件失败")
		return
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "读取上传文件失败")
		return
	}
	if int64(len(content)) > limit {
		common.FailWithCode(c, http.StatusRequestEntityTooLarge, common.ErrCodeInvalidParam, fmt.Sprintf("文件不能超过 %d MiB", limit>>20))
		return
	}
	if len(content) == 0 {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "文件不能为空")
		return
	}
	if docType == "markdown" && !utf8.Valid(content) {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "Markdown 文件必须是 UTF-8 文本")
		return
	}
	if !validDocumentMagic(docType, content) {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "文件内容与扩展名不匹配")
		return
	}
	title := strings.TrimSpace(c.PostForm("title"))
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(header.Filename), ext)
	}
	if strings.TrimSpace(title) == "" {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "文档标题不能为空")
		return
	}
	expectedHash, err := expectedContentHash(c, c.PostForm("expected_content_hash"))
	if err != nil {
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, err.Error())
		return
	}
	doc := &model.Document{
		Title: title, ContentType: docType, Filename: filepath.Base(header.Filename),
		Metadata:            map[string]any{"source_file": filepath.Base(header.Filename)},
		ExpectedContentHash: expectedHash,
		CreatedAt:           time.Now(), UpdatedAt: time.Now(),
	}
	if h.importer == nil {
		common.FailWithCode(c, http.StatusServiceUnavailable, common.ErrCodeInternal, "文档导入服务未配置")
		return
	}
	resp, queued, err := h.importer.Submit(c.Request.Context(), doc, content)
	if errors.Is(err, etl.ErrDocumentConflict) {
		common.FailWithCode(c, http.StatusConflict, common.ErrCodeConflict, err.Error())
		return
	}
	if errors.Is(err, etl.ErrImportQueueFull) {
		common.FailWithCode(c, http.StatusServiceUnavailable, common.ErrCodeInternal, err.Error())
		return
	}
	if err != nil {
		h.logger.Error("提交文档导入失败", zap.Error(err))
		common.Fail(c, http.StatusInternalServerError, common.ErrInternal(err))
		return
	}
	status := http.StatusOK
	if queued || resp.Status == "processing" {
		status = http.StatusAccepted
	}
	setDocumentETag(c, resp.ContentHash)
	c.JSON(status, common.Result{Code: 0, Message: "success", Data: resp, TraceID: c.GetString("trace_id")})
}

func validDocumentMagic(docType string, data []byte) bool {
	switch docType {
	case "pdf":
		return bytes.Contains(data[:min(len(data), 1024)], []byte("%PDF-"))
	case "docx", "xlsx":
		if len(data) < 4 || string(data[:2]) != "PK" {
			return false
		}
		reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return false
		}
		for _, file := range reader.File {
			if file.Name != "[Content_Types].xml" {
				continue
			}
			rc, err := file.Open()
			if err != nil {
				return false
			}
			content, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
			rc.Close()
			if docType == "docx" {
				return bytes.Contains(content, []byte("wordprocessingml.document"))
			}
			return bytes.Contains(content, []byte("spreadsheetml.sheet"))
		}
		return false
	default:
		return true
	}
}

func (h *DocumentHandler) process(c *gin.Context, doc *model.Document) {
	h.logger.Info("收到文档导入请求",
		zap.String("doc_id", doc.ID),
		zap.String("title", doc.Title),
		zap.String("content_type", doc.ContentType),
		zap.Int("content_len", len(doc.Content)),
	)
	resp, err := h.pipeline.ProcessDocument(c.Request.Context(), doc)
	if errors.Is(err, etl.ErrDocumentConflict) {
		common.FailWithCode(c, http.StatusConflict, common.ErrCodeConflict, err.Error())
		return
	}
	if err != nil {
		h.logger.Error("文档处理失败", zap.String("doc_id", doc.ID), zap.Error(err))
		common.Fail(c, http.StatusInternalServerError, common.ErrInternal(err))
		return
	}
	setDocumentETag(c, resp.ContentHash)
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

	if h.documentRepo == nil {
		common.FailWithCode(c, http.StatusServiceUnavailable, common.ErrCodeInternal, "文档存储未配置")
		return
	}
	doc, err := h.documentRepo.FindDocument(c.Request.Context(), docID)
	if errors.Is(err, sql.ErrNoRows) {
		common.FailWithCode(c, http.StatusNotFound, common.ErrCodeNotFound, "文档不存在")
		return
	}
	if err != nil {
		h.logger.Error("查询文档状态失败", zap.String("doc_id", docID), zap.Error(err))
		common.Fail(c, http.StatusInternalServerError, common.ErrInternal(err))
		return
	}
	setDocumentETag(c, doc.ContentHash)
	common.OK(c, doc)
}

func (h *DocumentHandler) canPublish(c *gin.Context) bool {
	_, ok := h.publishers[auth.Principal(c)]
	return ok
}

func bindLimitedJSON(c *gin.Context, target any, limit int64) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	if err := c.ShouldBindJSON(target); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			common.FailWithCode(c, http.StatusRequestEntityTooLarge, common.ErrCodeInvalidParam, "请求体过大")
			return false
		}
		common.FailWithCode(c, http.StatusBadRequest, common.ErrCodeInvalidParam, "请求参数错误: "+err.Error())
		return false
	}
	return true
}

func expectedContentHash(c *gin.Context, bodyValue string) (string, error) {
	headerValue := strings.TrimSpace(c.GetHeader("If-Match"))
	if headerValue != "" {
		if strings.HasPrefix(headerValue, "W/") {
			return "", errors.New("If-Match 必须使用强 ETag")
		}
		headerValue = strings.Trim(headerValue, `"`)
	}
	bodyValue = strings.TrimSpace(bodyValue)
	if headerValue != "" && bodyValue != "" && headerValue != bodyValue {
		return "", errors.New("If-Match 与 expected_content_hash 不一致")
	}
	value := headerValue
	if value == "" {
		value = bodyValue
	}
	if value == "" {
		return "", nil
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return "", errors.New("预期内容哈希必须是 64 位十六进制 SHA-256")
	}
	return strings.ToLower(value), nil
}

func setDocumentETag(c *gin.Context, contentHash string) {
	if contentHash != "" {
		c.Header("ETag", `"`+contentHash+`"`)
	}
}
