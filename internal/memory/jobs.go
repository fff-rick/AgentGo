package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

type JobRunner struct {
	store     *ManagedStore
	extractor *Extractor
	logger    *zap.Logger
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	stop      chan struct{}
	closed    atomic.Bool
}

func NewJobRunner(store *ManagedStore, extractor *Extractor, logger *zap.Logger) *JobRunner {
	ctx, cancel := context.WithCancel(context.Background())
	return &JobRunner{store: store, extractor: extractor, logger: logger, ctx: ctx, cancel: cancel, stop: make(chan struct{})}
}

func (r *JobRunner) Start() {
	for range 2 {
		r.wg.Add(1)
		go r.worker()
	}
	r.wg.Add(1)
	go r.expiryLoop()
}

func (r *JobRunner) expiryLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-r.ctx.Done():
			return
		case <-ticker.C:
		}
		ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
		rows, err := r.store.db.QueryContext(ctx, `SELECT id,user_id,version,valid_to FROM memories WHERE status='active' AND valid_to<=now() LIMIT 100`)
		if err != nil {
			r.logger.Warn("扫描过期记忆失败", zap.Error(err))
			cancel()
			continue
		}
		type expired struct {
			id, user string
			version  int64
			at       time.Time
		}
		var batch []expired
		for rows.Next() {
			var item expired
			if err = rows.Scan(&item.id, &item.user, &item.version, &item.at); err != nil {
				break
			}
			batch = append(batch, item)
		}
		rows.Close()
		if err == nil {
			err = rows.Err()
		}
		if err != nil {
			r.logger.Warn("读取过期记忆失败", zap.Error(err))
			cancel()
			continue
		}
		for _, item := range batch {
			if err = r.store.SetExpiry(ctx, item.user, item.id, item.version, &item.at); err != nil && !errors.Is(err, ErrMemoryVersion) {
				r.logger.Warn("记忆到期处理失败", zap.Error(err))
			}
		}
		if _, err = r.store.db.ExecContext(ctx, `DELETE FROM memory_jobs WHERE (status='done' AND created_at<now()-interval '7 days') OR (status='failed' AND created_at<now()-interval '30 days')`); err != nil {
			r.logger.Warn("清理记忆任务记录失败", zap.Error(err))
		}
		cancel()
	}
}

func (r *JobRunner) Enqueue(ctx context.Context, userID, sessionID, messageID, question string, sourceAt time.Time) error {
	if r.closed.Load() {
		return errors.New("记忆任务执行器已关闭")
	}
	if userID == "" || sessionID == "" || messageID == "" || question == "" {
		return errors.New("提取任务缺少来源消息")
	}
	_, err := r.store.db.ExecContext(ctx, `INSERT INTO memory_jobs(id,kind,user_id,session_id,message_id,payload,created_at) VALUES($1,'extract',$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, uuid.NewString(), userID, sessionID, messageID, question, sourceAt)
	return err
}

type memoryJob struct {
	id, kind, userID, sessionID, messageID, payload string
	version                                         int64
	attempts                                        int
	createdAt                                       time.Time
}

func (r *JobRunner) claim(ctx context.Context) (memoryJob, error) {
	var job memoryJob
	err := r.store.db.QueryRowContext(ctx, `UPDATE memory_jobs SET status='working',lease_until=now()+interval '75 seconds'
WHERE id=(SELECT id FROM memory_jobs WHERE (status='pending' AND next_at<=now()) OR (status='working' AND lease_until<now()) ORDER BY next_at,id LIMIT 1 FOR UPDATE SKIP LOCKED)
RETURNING id,kind,user_id,session_id,message_id,payload,target_version,attempts,created_at`).Scan(&job.id, &job.kind, &job.userID, &job.sessionID, &job.messageID, &job.payload, &job.version, &job.attempts, &job.createdAt)
	return job, err
}

func (r *JobRunner) worker() {
	defer r.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-r.stop:
			return
		case <-ticker.C:
		}
		select {
		case <-r.stop:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(r.ctx, 60*time.Second)
		job, err := r.claim(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			cancel()
			continue
		}
		if err != nil {
			cancel()
			r.logger.Warn("领取记忆任务失败", zap.Error(err))
			continue
		}
		err = r.run(ctx, job)
		cancel()
		// Persist completion even when the work deadline expired.
		finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err == nil {
			_, err = r.store.db.ExecContext(finishCtx, `UPDATE memory_jobs SET status='done',lease_until=NULL,last_error='',payload=CASE WHEN kind='extract' THEN '' ELSE payload END WHERE id=$1`, job.id)
		} else {
			attempts := job.attempts + 1
			status := "pending"
			if attempts >= 5 {
				status = "failed"
			}
			backoff := []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour}[min(attempts-1, 4)]
			_, updateErr := r.store.db.ExecContext(finishCtx, `UPDATE memory_jobs SET status=$2,attempts=$3,next_at=now()+$4::interval,lease_until=NULL,last_error=$5,payload=CASE WHEN $2='failed' AND kind='extract' THEN '' ELSE payload END WHERE id=$1`, job.id, status, attempts, fmt.Sprintf("%f seconds", backoff.Seconds()), err.Error())
			if updateErr != nil {
				r.logger.Error("更新记忆任务失败", zap.Error(updateErr))
			}
			r.logger.Warn("记忆任务失败", zap.String("job_id", job.id), zap.String("status", status), zap.Error(err))
		}
		finishCancel()
	}
}

func (r *JobRunner) run(ctx context.Context, job memoryJob) error {
	switch job.kind {
	case "extract":
		return r.extractor.extract(ctx, job.userID, job.sessionID, job.messageID, job.createdAt, job.payload)
	case "index":
		var status string
		var version int64
		err := r.store.db.QueryRowContext(ctx, `SELECT status,version FROM memories WHERE id=$1 AND user_id=$2`, job.payload, job.userID).Scan(&status, &version)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if version != job.version {
			return nil
		}
		if status != "active" {
			vectors, ok := r.store.vector.vectors.(interface {
				Delete(context.Context, string, []string) error
			})
			if !ok {
				return errors.New("向量存储不支持删除")
			}
			if err = vectors.Delete(ctx, r.store.vector.collection, []string{job.payload}); err != nil {
				return err
			}
			return r.requeueIfChanged(ctx, job)
		}
		item, err := r.store.Get(ctx, job.userID, job.payload)
		if err != nil {
			return err
		}
		if err = r.store.vector.Save(ctx, item); err != nil {
			return err
		}
		return r.requeueIfChanged(ctx, job)
	default:
		return fmt.Errorf("未知记忆任务类型 %q", job.kind)
	}
}

func (r *JobRunner) requeueIfChanged(ctx context.Context, job memoryJob) error {
	var version int64
	if err := r.store.db.QueryRowContext(ctx, `SELECT version FROM memories WHERE id=$1 AND user_id=$2`, job.payload, job.userID).Scan(&version); err != nil {
		return err
	}
	if version == job.version {
		return nil
	}
	_, err := r.store.db.ExecContext(ctx, `INSERT INTO memory_jobs(id,kind,user_id,payload,target_version) VALUES($1,'index',$2,$3,$4)`, uuid.NewString(), job.userID, job.payload, version)
	return err
}

func (r *JobRunner) Close(ctx context.Context) error {
	if r.closed.CompareAndSwap(false, true) {
		close(r.stop)
	}
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		r.cancel()
		return nil
	case <-ctx.Done():
		r.cancel()
		return ctx.Err()
	}
}
