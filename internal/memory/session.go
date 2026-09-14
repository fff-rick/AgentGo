package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/enterprise/ai-agent-go/internal/cache"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/user"
)

const sessionKeyPrefix = "agentgo:v2:session:"

var ErrSessionNotFound = errors.New("会话不存在或已过期")

type SessionSummary struct {
	Content         string    `json:"content"`
	ThroughSequence int64     `json:"through_sequence"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type SessionManager interface {
	CreateSession(context.Context, model.UserInfo) (*model.Session, error)
	GetSession(context.Context, string) (*model.Session, error)
	GetUser(context.Context, string) (*model.UserInfo, error)
	AppendMessage(context.Context, string, model.Message) error
	GetMessages(context.Context, string) ([]model.Message, error)
	GetRecentMessages(context.Context, string, int) ([]model.Message, error)
	SaveSummary(context.Context, string, SessionSummary) error
	GetSummary(context.Context, string) (*SessionSummary, error)
}

type RedisSessionManager struct {
	cache cache.Cache
	users *user.Manager
	ttl   time.Duration
}

func NewSessionManager(store cache.Cache, users *user.Manager, ttl time.Duration) *RedisSessionManager {
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour
	}
	return &RedisSessionManager{cache: store, users: users, ttl: ttl}
}

func (m *RedisSessionManager) CreateSession(ctx context.Context, info model.UserInfo) (*model.Session, error) {
	if err := m.users.Save(ctx, info); err != nil {
		return nil, err
	}
	now := time.Now()
	session := &model.Session{ID: uuid.NewString(), UserID: info.UserID, CreatedAt: now, UpdatedAt: now}
	data, err := json.Marshal(session)
	if err != nil {
		return nil, fmt.Errorf("序列化会话失败: %w", err)
	}
	if err := m.cache.Set(ctx, m.sessionKey(session.ID), string(data), m.ttl); err != nil {
		return nil, err
	}
	return session, nil
}

func (m *RedisSessionManager) GetSession(ctx context.Context, sessionID string) (*model.Session, error) {
	raw, err := m.cache.Get(ctx, m.sessionKey(sessionID))
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, ErrSessionNotFound
	}
	var session model.Session
	if err := json.Unmarshal([]byte(raw), &session); err != nil {
		return nil, fmt.Errorf("解析会话失败: %w", err)
	}
	if err := m.touch(ctx, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

func (m *RedisSessionManager) GetUser(ctx context.Context, userID string) (*model.UserInfo, error) {
	return m.users.Get(ctx, userID)
}

func (m *RedisSessionManager) AppendMessage(ctx context.Context, sessionID string, message model.Message) error {
	if _, err := m.GetSession(ctx, sessionID); err != nil {
		return err
	}
	sequence, err := m.cache.Incr(ctx, m.sequenceKey(sessionID))
	if err != nil {
		return err
	}
	if message.ID == "" {
		message.ID = uuid.NewString()
	}
	if message.CreatedAt.IsZero() {
		message.CreatedAt = time.Now()
	}
	message.SessionID, message.Sequence = sessionID, sequence
	data, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("序列化消息失败: %w", err)
	}
	if err := m.cache.LPush(ctx, m.messagesKey(sessionID), string(data)); err != nil {
		return err
	}
	if err := m.cache.Expire(ctx, m.messagesKey(sessionID), m.ttl); err != nil {
		return err
	}
	return m.cache.Expire(ctx, m.sequenceKey(sessionID), m.ttl)
}

func (m *RedisSessionManager) GetMessages(ctx context.Context, sessionID string) ([]model.Message, error) {
	return m.loadMessages(ctx, sessionID, -1)
}

func (m *RedisSessionManager) GetRecentMessages(ctx context.Context, sessionID string, limit int) ([]model.Message, error) {
	if limit <= 0 {
		return nil, nil
	}
	return m.loadMessages(ctx, sessionID, int64(limit-1))
}

func (m *RedisSessionManager) SaveSummary(ctx context.Context, sessionID string, summary SessionSummary) error {
	if _, err := m.GetSession(ctx, sessionID); err != nil {
		return err
	}
	if summary.UpdatedAt.IsZero() {
		summary.UpdatedAt = time.Now()
	}
	data, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("序列化会话摘要失败: %w", err)
	}
	return m.cache.Set(ctx, m.summaryKey(sessionID), string(data), m.ttl)
}

func (m *RedisSessionManager) GetSummary(ctx context.Context, sessionID string) (*SessionSummary, error) {
	raw, err := m.cache.Get(ctx, m.summaryKey(sessionID))
	if err != nil || raw == "" {
		return nil, err
	}
	var summary SessionSummary
	if err := json.Unmarshal([]byte(raw), &summary); err != nil {
		return nil, fmt.Errorf("解析会话摘要失败: %w", err)
	}
	return &summary, nil
}

func (m *RedisSessionManager) loadMessages(ctx context.Context, sessionID string, stop int64) ([]model.Message, error) {
	if _, err := m.GetSession(ctx, sessionID); err != nil {
		return nil, err
	}
	items, err := m.cache.LRange(ctx, m.messagesKey(sessionID), 0, stop)
	if err != nil {
		return nil, err
	}
	messages := make([]model.Message, 0, len(items))
	for i := len(items) - 1; i >= 0; i-- {
		var message model.Message
		if err := json.Unmarshal([]byte(items[i]), &message); err != nil {
			return nil, fmt.Errorf("解析会话消息失败: %w", err)
		}
		messages = append(messages, message)
	}
	return messages, nil
}

func (m *RedisSessionManager) touch(ctx context.Context, session *model.Session) error {
	for _, key := range []string{m.sessionKey(session.ID), m.messagesKey(session.ID), m.sequenceKey(session.ID), m.summaryKey(session.ID)} {
		if err := m.cache.Expire(ctx, key, m.ttl); err != nil {
			return err
		}
	}
	return m.users.Touch(ctx, session.UserID)
}

func (*RedisSessionManager) sessionKey(id string) string  { return sessionKeyPrefix + id }
func (*RedisSessionManager) messagesKey(id string) string { return sessionKeyPrefix + id + ":messages" }
func (*RedisSessionManager) sequenceKey(id string) string { return sessionKeyPrefix + id + ":sequence" }
func (*RedisSessionManager) summaryKey(id string) string  { return sessionKeyPrefix + id + ":summary" }
