package etl

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/model"
)

var ErrImportQueueFull = errors.New("文档导入队列已满")

type ImportRepository interface {
	FindDocument(context.Context, string) (*model.DocumentResponse, error)
	MarkDocumentProcessing(context.Context, *model.Document) error
	MarkDocumentFailed(context.Context, string, string) error
	FailInterruptedDocuments(context.Context) error
}

type importJob struct {
	doc  *model.Document
	path string
}

type Importer struct {
	pipeline *Pipeline
	repo     ImportRepository
	logger   *zap.Logger
	dir      string
	jobs     chan importJob
	slots    chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	submitMu sync.Mutex
}

func NewImporter(ctx context.Context, pipeline *Pipeline, repo ImportRepository, logger *zap.Logger) (*Importer, error) {
	dir, err := os.MkdirTemp("", "agentgo-import-")
	if err != nil {
		return nil, err
	}
	if err := repo.FailInterruptedDocuments(ctx); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	i := &Importer{pipeline: pipeline, repo: repo, logger: logger, dir: dir, jobs: make(chan importJob, 8), slots: make(chan struct{}, 8), ctx: workerCtx, cancel: cancel}
	i.wg.Add(1)
	go i.work()
	return i, nil
}

func (i *Importer) Submit(ctx context.Context, doc *model.Document, data []byte) (*model.DocumentResponse, bool, error) {
	i.submitMu.Lock()
	defer i.submitMu.Unlock()
	doc.Title = strings.TrimSpace(doc.Title)
	doc.ContentType = normalizeContentType(doc.ContentType)
	if doc.Tags == nil {
		doc.Tags = []string{}
	}
	if doc.Metadata == nil {
		doc.Metadata = map[string]any{}
	}
	doc.ID = DocumentID(doc.ContentType, doc.Title)
	doc.ContentHash = ContentHashBytes(data)
	if existing, err := i.repo.FindDocument(ctx, doc.ID); err == nil {
		if existing.Status == "processing" || existing.Status == "completed" && existing.ContentHash == doc.ContentHash {
			return existing, false, nil
		}
		doc.CreatedAt = existing.CreatedAt
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	select {
	case i.slots <- struct{}{}:
	default:
		return nil, false, ErrImportQueueFull
	}
	reserved := true
	defer func() {
		if reserved {
			<-i.slots
		}
	}()
	file, err := os.CreateTemp(i.dir, "upload-*")
	if err != nil {
		return nil, false, err
	}
	path := file.Name()
	if _, err = file.Write(data); err == nil {
		err = file.Close()
	} else {
		_ = file.Close()
	}
	if err != nil {
		os.Remove(path)
		return nil, false, err
	}
	if err := i.repo.MarkDocumentProcessing(ctx, doc); err != nil {
		os.Remove(path)
		return nil, false, err
	}
	job := importJob{doc: doc, path: path}
	response := &model.DocumentResponse{DocID: doc.ID, Title: doc.Title, Status: "processing", CreatedAt: doc.CreatedAt, ContentHash: doc.ContentHash}
	select {
	case i.jobs <- job:
		reserved = false
		return response, true, nil
	default:
		os.Remove(path)
		_ = i.repo.MarkDocumentFailed(context.Background(), doc.ID, ErrImportQueueFull.Error())
		return nil, false, ErrImportQueueFull
	}
}

func (i *Importer) work() {
	defer i.wg.Done()
	for {
		select {
		case <-i.ctx.Done():
			i.failPending()
			return
		case job := <-i.jobs:
			i.process(job)
		}
	}
}

func (i *Importer) process(job importJob) {
	defer func() { <-i.slots }()
	defer os.Remove(job.path)
	data, err := os.ReadFile(job.path)
	if err == nil {
		job.doc.RawContent = data
		_, err = i.pipeline.ProcessDocument(i.ctx, job.doc)
		job.doc.RawContent = nil
	}
	if err != nil {
		i.logger.Error("异步文档导入失败", zap.String("doc_id", job.doc.ID), zap.Error(err))
		_ = i.repo.MarkDocumentFailed(context.Background(), job.doc.ID, err.Error())
	}
}

func (i *Importer) failPending() {
	for {
		select {
		case job := <-i.jobs:
			os.Remove(job.path)
			<-i.slots
			_ = i.repo.MarkDocumentFailed(context.Background(), job.doc.ID, "服务关闭中断，请重新上传")
		default:
			return
		}
	}
}

func (i *Importer) Close() error {
	i.cancel()
	done := make(chan struct{})
	go func() { i.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
	}
	return os.RemoveAll(filepath.Clean(i.dir))
}

func (i *Importer) QueueCapacity() int { return cap(i.jobs) }
