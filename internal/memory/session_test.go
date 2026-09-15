package memory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/user"
)

type cacheStub struct {
	mu       sync.Mutex
	values   map[string]string
	lists    map[string][]string
	counters map[string]int64
	expires  map[string]time.Duration
}

func newCacheStub() *cacheStub {
	return &cacheStub{values: map[string]string{}, lists: map[string][]string{}, counters: map[string]int64{}, expires: map[string]time.Duration{}}
}
func (c *cacheStub) Get(_ context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[key], nil
}
func (c *cacheStub) Set(_ context.Context, key, value string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[key], c.expires[key] = value, ttl
	return nil
}
func (c *cacheStub) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.values, key)
	delete(c.lists, key)
	return nil
}
func (c *cacheStub) Exists(_ context.Context, key string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, value := c.values[key]
	_, list := c.lists[key]
	return value || list, nil
}
func (c *cacheStub) LPush(_ context.Context, key string, values ...interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, value := range values {
		c.lists[key] = append([]string{value.(string)}, c.lists[key]...)
	}
	return nil
}
func (c *cacheStub) LRange(_ context.Context, key string, start, stop int64) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	values := c.lists[key]
	if stop < 0 || stop >= int64(len(values)) {
		stop = int64(len(values) - 1)
	}
	if start > stop || start >= int64(len(values)) {
		return nil, nil
	}
	return append([]string(nil), values[start:stop+1]...), nil
}
func (*cacheStub) LTrim(context.Context, string, int64, int64) error { return nil }
func (c *cacheStub) Expire(_ context.Context, key string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expires[key] = ttl
	return nil
}
func (c *cacheStub) Incr(_ context.Context, key string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counters[key]++
	return c.counters[key], nil
}
func (*cacheStub) Close() error                 { return nil }
func (*cacheStub) Healthy(context.Context) bool { return true }

func TestSessionKeepsFullHistoryAndRecentWindow(t *testing.T) {
	store := newCacheStub()
	users := user.NewManager(store, 30*24*time.Hour)
	sessions := NewSessionManager(store, users, 30*24*time.Hour)
	session, err := sessions.CreateSession(context.Background(), model.UserInfo{UserID: "user-1", DisplayName: "Xin"})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 25; index++ {
		if err := sessions.AppendMessage(context.Background(), session, model.Message{Role: "user", Content: "message"}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := sessions.GetMessages(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	recent, err := sessions.GetRecentMessages(context.Background(), session, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 25 || len(recent) != 20 || all[0].Sequence != 1 || recent[0].Sequence != 6 {
		t.Fatalf("all=%d recent=%d first=%d recent_first=%d", len(all), len(recent), all[0].Sequence, recent[0].Sequence)
	}
	if store.expires[sessions.messagesKey(session.ID)] != 30*24*time.Hour {
		t.Fatal("真实消息列表没有刷新 TTL")
	}
}

func TestSessionOperationsRejectMissingValidatedSession(t *testing.T) {
	store := newCacheStub()
	sessions := NewSessionManager(store, user.NewManager(store, time.Hour), time.Hour)
	if err := sessions.AppendMessage(context.Background(), nil, model.Message{}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("AppendMessage err=%v", err)
	}
	if _, err := sessions.GetMessages(context.Background(), nil); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("GetMessages err=%v", err)
	}
	if _, err := sessions.GetRecentMessages(context.Background(), nil, 0); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("GetRecentMessages err=%v", err)
	}
	if err := sessions.SaveSummary(context.Background(), nil, SessionSummary{}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("SaveSummary err=%v", err)
	}
}
