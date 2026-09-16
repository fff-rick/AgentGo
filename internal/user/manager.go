package user

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/enterprise/ai-agent-go/internal/cache"
	"github.com/enterprise/ai-agent-go/internal/model"
)

const keyPrefix = "agentgo:v2:user:"

var userIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// Manager persists user context; HTTP authentication is enforced before callers create a session.
type Manager struct {
	cache cache.Cache
	ttl   time.Duration
}

func NewManager(store cache.Cache, ttl time.Duration) *Manager {
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour
	}
	return &Manager{cache: store, ttl: ttl}
}

func (m *Manager) Save(ctx context.Context, info model.UserInfo) error {
	if err := Validate(info); err != nil {
		return err
	}
	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("序列化用户信息失败: %w", err)
	}
	return m.cache.Set(ctx, keyPrefix+info.UserID, string(data), m.ttl)
}

func (m *Manager) Get(ctx context.Context, userID string) (*model.UserInfo, error) {
	raw, err := m.cache.Get(ctx, keyPrefix+userID)
	if err != nil || raw == "" {
		return nil, err
	}
	var info model.UserInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		return nil, fmt.Errorf("解析用户信息失败: %w", err)
	}
	return &info, nil
}

func (m *Manager) Touch(ctx context.Context, userID string) error {
	return m.cache.Expire(ctx, keyPrefix+userID, m.ttl)
}

func Validate(info model.UserInfo) error {
	if !userIDPattern.MatchString(info.UserID) {
		return fmt.Errorf("user_id 必须为 1-128 位字母、数字、下划线或连字符")
	}
	if len([]rune(info.DisplayName)) > 128 {
		return fmt.Errorf("display_name 不能超过 128 个字符")
	}
	if len(info.Metadata) > 20 {
		return fmt.Errorf("metadata 不能超过 20 项")
	}
	for key, value := range info.Metadata {
		if len(key) > 64 || len(value) > 512 {
			return fmt.Errorf("metadata 键和值分别不能超过 64 和 512 字节")
		}
	}
	return nil
}
