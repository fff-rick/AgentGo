package agentcontext

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/trace"
)

type precompactJob struct {
	session     model.Session
	traceparent string
}

// Precompactor performs best-effort summary updates outside the response path.
// ponytail: process-local queue can lose work on crash; use a durable queue if summaries must survive restart.
type Precompactor struct {
	builder *ContextBuilder
	jobs    chan precompactJob
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	pending map[string]struct{}
	closed  bool
	done    chan struct{}
}

func NewPrecompactor(builder *ContextBuilder) *Precompactor {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Precompactor{
		builder: builder, jobs: make(chan precompactJob, 16), ctx: ctx, cancel: cancel,
		pending: make(map[string]struct{}), done: make(chan struct{}),
	}
	var workers sync.WaitGroup
	workers.Add(2)
	for range 2 {
		go func() {
			defer workers.Done()
			for job := range p.jobs {
				func() {
					defer p.finished(job.session.ID)
					ctx, cancel := context.WithTimeout(p.ctx, time.Minute)
					defer cancel()
					ctx, span := trace.StartLinked(ctx, "memory.precompact", job.traceparent)
					err := p.builder.precompact(ctx, &job.session)
					trace.Finish(span, err)
					if err != nil && !errors.Is(err, errSummaryChanged) && !errors.Is(err, errMessageGap) {
						p.builder.logger.Warn("后台会话摘要压缩失败", zap.String("session_id", job.session.ID), zap.Error(err))
					}
				}()
			}
		}()
	}
	go func() { workers.Wait(); close(p.done) }()
	return p
}

func (p *Precompactor) Submit(session *model.Session, estimatedTokens, historyMessages int, answer string) bool {
	return p.SubmitWithContext(context.Background(), session, estimatedTokens, historyMessages, answer)
}

func (p *Precompactor) SubmitWithContext(ctx context.Context, session *model.Session, estimatedTokens, historyMessages int, answer string) bool {
	if session == nil || session.ID == "" || historyMessages+2 <= p.builder.cfg.RecentMessages ||
		(estimatedTokens+estimateText(answer))*10 < p.builder.cfg.MaxInputTokens*7 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	if _, exists := p.pending[session.ID]; exists {
		return false
	}
	copy := *session
	select {
	case p.jobs <- precompactJob{session: copy, traceparent: trace.TraceParent(ctx)}:
		p.pending[session.ID] = struct{}{}
		return true
	default:
		p.builder.logger.Warn("后台摘要队列已满，跳过预压缩", zap.String("session_id", session.ID))
		return false
	}
}

func (p *Precompactor) finished(sessionID string) {
	p.mu.Lock()
	delete(p.pending, sessionID)
	p.mu.Unlock()
}

func (p *Precompactor) Close(ctx context.Context) error {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.jobs)
	}
	p.mu.Unlock()
	select {
	case <-p.done:
		p.cancel()
		return nil
	case <-ctx.Done():
		p.cancel()
		return ctx.Err()
	}
}
