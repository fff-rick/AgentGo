package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/google/uuid"
)

var ErrMemoryNotFound = errors.New("记忆不存在")
var ErrMemoryVersion = errors.New("记忆版本已变化")
var ErrInvalidMemory = errors.New("无效的记忆内容")

// ManagedStore keeps PostgreSQL authoritative. Milvus is only a rebuildable index.
type ManagedStore struct {
	db     *sql.DB
	vector *SemanticStore
}

func NewManagedStore(ctx context.Context, db *sql.DB, vector *SemanticStore) (*ManagedStore, error) {
	if db == nil || vector == nil || vector.collection == "semantic_memory_v1" {
		return nil, errors.New("长期记忆管理需要 PostgreSQL 和新的 Milvus collection")
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS memories (
 id text PRIMARY KEY, user_id text NOT NULL, kind text NOT NULL, topic text NOT NULL,
 content text NOT NULL, importance double precision NOT NULL, confidence double precision NOT NULL,
 source_session_id text NOT NULL, source_message_id text NOT NULL, source_at timestamptz NOT NULL,
 status text NOT NULL, version bigint NOT NULL, created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL, valid_from timestamptz NOT NULL, valid_to timestamptz,
 use_count bigint NOT NULL DEFAULT 0, last_used_at timestamptz,
 UNIQUE(user_id,kind,topic))`,
		`CREATE INDEX IF NOT EXISTS memories_user_idx ON memories(user_id,status,updated_at DESC)`,
		`CREATE TABLE IF NOT EXISTS memory_versions (
 memory_id text NOT NULL REFERENCES memories(id) ON DELETE CASCADE, version bigint NOT NULL, content text NOT NULL,
 status text NOT NULL, source_session_id text NOT NULL, source_message_id text NOT NULL,
 source_at timestamptz NOT NULL, created_at timestamptz NOT NULL, valid_to timestamptz,
 PRIMARY KEY(memory_id,version))`,
		`ALTER TABLE memory_versions ADD COLUMN IF NOT EXISTS valid_to timestamptz`,
		`CREATE TABLE IF NOT EXISTS memory_jobs (
 id text PRIMARY KEY, kind text NOT NULL, user_id text NOT NULL, session_id text NOT NULL DEFAULT '',
 message_id text NOT NULL DEFAULT '', payload text NOT NULL, target_version bigint NOT NULL DEFAULT 0,
 attempts integer NOT NULL DEFAULT 0, next_at timestamptz NOT NULL DEFAULT now(),
 status text NOT NULL DEFAULT 'pending', last_error text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now())`,
		`ALTER TABLE memory_jobs ADD COLUMN IF NOT EXISTS lease_until timestamptz`,
		`CREATE UNIQUE INDEX IF NOT EXISTS memory_extract_once_idx ON memory_jobs(message_id) WHERE kind='extract'`,
		`CREATE INDEX IF NOT EXISTS memory_jobs_ready_idx ON memory_jobs(next_at) WHERE status='pending'`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return nil, fmt.Errorf("初始化长期记忆 schema 失败: %w", err)
		}
	}
	return &ManagedStore{db: db, vector: vector}, nil
}

func normalizeTopic(topic string) string {
	return strings.ToLower(strings.Join(strings.Fields(topic), " "))
}

func (s *ManagedStore) Save(ctx context.Context, item model.MemoryItem) error {
	if item.UserID == "" || item.Content == "" || item.SourceSessionID == "" || item.SourceMessageID == "" || !validMemoryKind(item.Kind) {
		return errors.New("无效的长期记忆")
	}
	item.Topic = normalizeTopic(item.Topic)
	if item.Topic == "" {
		item.Topic = normalizeTopic(item.Content)
	}
	if len(item.Topic) > 256 || len([]rune(item.Content)) > 1000 {
		return errors.New("长期记忆内容过长")
	}
	if item.Confidence == 0 {
		item.Confidence = 0.8
	}
	if item.Confidence < 0.7 {
		return nil
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	}
	if item.ValidFrom.IsZero() {
		item.ValidFrom = item.CreatedAt
	}
	if item.ValidTo == nil && (item.Kind == model.MemoryExperience || item.Kind == model.MemorySolution) {
		end := item.ValidFrom.Add(180 * 24 * time.Hour)
		item.ValidTo = &end
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// ponytail: one lock per user makes exact dedupe and first insert atomic; split by topic if a single user's memory throughput becomes material.
	lockKey := item.UserID
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return err
	}
	var duplicateID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM memories WHERE user_id=$1 AND kind=$2 AND lower(content)=lower($3) AND status='active' LIMIT 1`, item.UserID, item.Kind, item.Content).Scan(&duplicateID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var oldID, oldContent, oldStatus string
	var oldVersion int64
	var oldSource time.Time
	err = tx.QueryRowContext(ctx, `SELECT id,content,status,version,source_at FROM memories WHERE user_id=$1 AND kind=$2 AND topic=$3 FOR UPDATE`, item.UserID, item.Kind, item.Topic).Scan(&oldID, &oldContent, &oldStatus, &oldVersion, &oldSource)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if oldStatus == "deleted" || !item.CreatedAt.After(oldSource) || normalizeTopic(oldContent) == normalizeTopic(item.Content) {
			return nil
		}
		item.ID, item.Version = oldID, oldVersion+1
		if _, err = tx.ExecContext(ctx, `UPDATE memory_versions SET status='superseded' WHERE memory_id=$1 AND version=$2`, item.ID, oldVersion); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE memories SET content=$2,importance=$3,confidence=$4,source_session_id=$5,source_message_id=$6,source_at=$7,status='active',version=$8,updated_at=now(),valid_from=$9,valid_to=$10 WHERE id=$1`, item.ID, item.Content, item.Importance, item.Confidence, item.SourceSessionID, item.SourceMessageID, item.CreatedAt, item.Version, item.ValidFrom, item.ValidTo); err != nil {
			return err
		}
	} else {
		item.ID = uuid.NewString()
		item.Version = 1
		if _, err = tx.ExecContext(ctx, `INSERT INTO memories(id,user_id,kind,topic,content,importance,confidence,source_session_id,source_message_id,source_at,status,version,created_at,updated_at,valid_from,valid_to) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'active',1,now(),now(),$11,$12)`, item.ID, item.UserID, item.Kind, item.Topic, item.Content, item.Importance, item.Confidence, item.SourceSessionID, item.SourceMessageID, item.CreatedAt, item.ValidFrom, item.ValidTo); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO memory_versions(memory_id,version,content,status,source_session_id,source_message_id,source_at,created_at,valid_to) VALUES($1,$2,$3,'active',$4,$5,$6,now(),$7)`, item.ID, item.Version, item.Content, item.SourceSessionID, item.SourceMessageID, item.CreatedAt, item.ValidTo); err != nil {
		return err
	}
	if err = enqueueIndex(ctx, tx, item.ID, item.UserID, item.Version); err != nil {
		return err
	}
	return tx.Commit()
}

func enqueueIndex(ctx context.Context, tx *sql.Tx, id, userID string, version int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO memory_jobs(id,kind,user_id,payload,target_version) VALUES($1,'index',$2,$3,$4)`, uuid.NewString(), userID, id, version)
	return err
}

const memoryColumns = `id,user_id,kind,topic,content,importance,confidence,source_session_id,source_message_id,status,version,created_at,updated_at,valid_from,valid_to,use_count,last_used_at`

func scanMemory(scanner interface{ Scan(...any) error }) (model.MemoryItem, error) {
	var item model.MemoryItem
	var validTo, lastUsed sql.NullTime
	err := scanner.Scan(&item.ID, &item.UserID, &item.Kind, &item.Topic, &item.Content, &item.Importance, &item.Confidence, &item.SourceSessionID, &item.SourceMessageID, &item.Status, &item.Version, &item.CreatedAt, &item.UpdatedAt, &item.ValidFrom, &validTo, &item.UseCount, &lastUsed)
	if validTo.Valid {
		item.ValidTo = &validTo.Time
	}
	if lastUsed.Valid {
		item.LastUsedAt = &lastUsed.Time
	}
	if item.Status == "active" && item.ValidTo != nil && !item.ValidTo.After(time.Now()) {
		item.Status = "expired"
	}
	return item, err
}

func (s *ManagedStore) Get(ctx context.Context, userID, id string) (model.MemoryItem, error) {
	item, err := scanMemory(s.db.QueryRowContext(ctx, `SELECT `+memoryColumns+` FROM memories WHERE id=$1 AND user_id=$2 AND status<>'deleted'`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return item, ErrMemoryNotFound
	}
	return item, err
}

func (s *ManagedStore) List(ctx context.Context, userID string, limit, offset int) ([]model.MemoryItem, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+memoryColumns+` FROM memories WHERE user_id=$1 AND status<>'deleted' ORDER BY updated_at DESC,id LIMIT $2 OFFSET $3`, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.MemoryItem, 0)
	for rows.Next() {
		item, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *ManagedStore) Versions(ctx context.Context, userID, id string, limit, offset int) ([]model.MemoryVersion, error) {
	if _, err := s.Get(ctx, userID, id); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx, `SELECT version,content,status,source_session_id,source_message_id,source_at,created_at,valid_to FROM memory_versions WHERE memory_id=$1 ORDER BY version DESC LIMIT $2 OFFSET $3`, id, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	versions := make([]model.MemoryVersion, 0)
	for rows.Next() {
		var item model.MemoryVersion
		var expiry sql.NullTime
		if err = rows.Scan(&item.Version, &item.Content, &item.Status, &item.SourceSessionID, &item.SourceMessageID, &item.SourceAt, &item.CreatedAt, &expiry); err != nil {
			return nil, err
		}
		if expiry.Valid {
			item.ValidTo = &expiry.Time
		}
		versions = append(versions, item)
	}
	return versions, rows.Err()
}

func (s *ManagedStore) Search(ctx context.Context, userID, query string, topK int) ([]model.MemoryItem, error) {
	items, err := s.FindSimilar(ctx, userID, query, topK)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	if len(ids) > 0 {
		_, _ = s.db.ExecContext(ctx, `UPDATE memories SET use_count=use_count+1,last_used_at=now() WHERE user_id=$1 AND id=ANY($2::text[])`, userID, ids)
	}
	return items, nil
}

func (s *ManagedStore) FindSimilar(ctx context.Context, userID, query string, topK int) ([]model.MemoryItem, error) {
	if userID == "" || query == "" || topK <= 0 {
		return nil, nil
	}
	candidates, err := s.vector.Search(ctx, userID, query, topK*3)
	if err != nil {
		return nil, err
	}
	items := make([]model.MemoryItem, 0, topK)
	for _, candidate := range candidates {
		item, err := s.Get(ctx, userID, candidate.ID)
		if errors.Is(err, ErrMemoryNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if item.Status != "active" || item.ValidFrom.After(time.Now()) || (item.ValidTo != nil && !item.ValidTo.After(time.Now())) {
			continue
		}
		decay := 1.0
		if item.Kind == model.MemoryExperience || item.Kind == model.MemorySolution {
			decay = math.Exp(-time.Since(item.ValidFrom).Hours() / (90 * 24))
		}
		boost := 1 + math.Min(float64(item.UseCount), 10)*0.01
		item.Score = candidate.Score * item.Importance * item.Confidence * decay * boost
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Score > items[j].Score })
	if len(items) > topK {
		items = items[:topK]
	}
	return items, nil
}

func (s *ManagedStore) FindTopic(ctx context.Context, userID string, kind model.MemoryKind, topic string) (*model.MemoryItem, error) {
	item, err := scanMemory(s.db.QueryRowContext(ctx, `SELECT `+memoryColumns+` FROM memories WHERE user_id=$1 AND kind=$2 AND topic=$3 AND status='active'`, userID, kind, normalizeTopic(topic)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (s *ManagedStore) Correct(ctx context.Context, userID, id, content string, expected int64) (model.MemoryItem, error) {
	content = strings.TrimSpace(content)
	if content == "" || len([]rune(content)) > 1000 {
		return model.MemoryItem{}, ErrInvalidMemory
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.MemoryItem{}, err
	}
	defer tx.Rollback()
	item, err := scanMemory(tx.QueryRowContext(ctx, `SELECT `+memoryColumns+` FROM memories WHERE id=$1 AND user_id=$2 FOR UPDATE`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return item, ErrMemoryNotFound
	}
	if err != nil {
		return item, err
	}
	if item.Version != expected {
		return item, ErrMemoryVersion
	}
	if _, err = tx.ExecContext(ctx, `UPDATE memory_versions SET status='superseded' WHERE memory_id=$1 AND version=$2`, id, expected); err != nil {
		return item, err
	}
	item.Content = content
	item.Version++
	item.Status = "active"
	item.Confidence = 1
	item.ValidFrom = time.Now()
	item.UpdatedAt = item.ValidFrom
	item.SourceSessionID = ""
	item.SourceMessageID = ""
	item.ValidTo = nil
	if item.Kind == model.MemoryExperience || item.Kind == model.MemorySolution {
		end := item.ValidFrom.Add(180 * 24 * time.Hour)
		item.ValidTo = &end
	}
	if _, err = tx.ExecContext(ctx, `UPDATE memories SET content=$2,confidence=1,status='active',version=$3,updated_at=now(),valid_from=$4,valid_to=$5,source_at=now(),source_session_id='',source_message_id='' WHERE id=$1`, id, content, item.Version, item.ValidFrom, item.ValidTo); err != nil {
		return item, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO memory_versions(memory_id,version,content,status,source_session_id,source_message_id,source_at,created_at,valid_to) VALUES($1,$2,$3,'active',$4,$5,now(),now(),$6)`, id, item.Version, content, item.SourceSessionID, item.SourceMessageID, item.ValidTo); err != nil {
		return item, err
	}
	if err = enqueueIndex(ctx, tx, id, userID, item.Version); err != nil {
		return item, err
	}
	return item, tx.Commit()
}

func (s *ManagedStore) SetExpiry(ctx context.Context, userID, id string, expected int64, expiry *time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	item, err := scanMemory(tx.QueryRowContext(ctx, `SELECT `+memoryColumns+` FROM memories WHERE id=$1 AND user_id=$2 AND status<>'deleted' FOR UPDATE`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrMemoryNotFound
	}
	if err != nil {
		return err
	}
	if item.Version != expected {
		return ErrMemoryVersion
	}
	if _, err = tx.ExecContext(ctx, `UPDATE memory_versions SET status='superseded' WHERE memory_id=$1 AND version=$2`, id, expected); err != nil {
		return err
	}
	status := "active"
	if expiry != nil && !expiry.After(time.Now()) {
		status = "expired"
	}
	version := expected + 1
	if _, err = tx.ExecContext(ctx, `UPDATE memories SET valid_to=$2,status=$3,version=$4,updated_at=now() WHERE id=$1`, id, expiry, status, version); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO memory_versions(memory_id,version,content,status,source_session_id,source_message_id,source_at,created_at,valid_to) VALUES($1,$2,$3,$4,$5,$6,now(),now(),$7)`, id, version, item.Content, status, item.SourceSessionID, item.SourceMessageID, expiry); err != nil {
		return err
	}
	if err = enqueueIndex(ctx, tx, id, userID, version); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *ManagedStore) Delete(ctx context.Context, userID, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int64
	if err = tx.QueryRowContext(ctx, `UPDATE memories SET content='',status='deleted',version=version+1,updated_at=now() WHERE id=$1 AND user_id=$2 AND status<>'deleted' RETURNING version`, id, userID).Scan(&version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMemoryNotFound
		}
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM memory_jobs WHERE kind='extract' AND user_id=$2 AND message_id IN (SELECT source_message_id FROM memory_versions WHERE memory_id=$1)`, id, userID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM memory_versions WHERE memory_id=$1`, id); err != nil {
		return err
	}
	if err = enqueueIndex(ctx, tx, id, userID, version); err != nil {
		return err
	}
	return tx.Commit()
}
