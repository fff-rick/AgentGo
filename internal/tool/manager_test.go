package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/model"
)

type managerTool struct{ name string }

func (t managerTool) Name() string      { return t.name }
func (managerTool) Description() string { return "compact description" }
func (t managerTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object", "schema_marker": t.name}
}
func (t managerTool) Execute(context.Context, string) (*ToolResult, error) {
	return NewSuccessResult(t.name), nil
}

type managerCache struct {
	mu     sync.Mutex
	values map[string]string
	ttls   map[string]time.Duration
	getErr error
	setErr error
}

func newManagerCache() *managerCache {
	return &managerCache{values: make(map[string]string), ttls: make(map[string]time.Duration)}
}
func (c *managerCache) Get(_ context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.getErr != nil {
		return "", c.getErr
	}
	return c.values[key], nil
}
func (c *managerCache) Set(_ context.Context, key, value string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.setErr != nil {
		return c.setErr
	}
	c.values[key], c.ttls[key] = value, ttl
	return nil
}
func (c *managerCache) Delete(context.Context, string) error                { return nil }
func (c *managerCache) Exists(context.Context, string) (bool, error)        { return false, nil }
func (c *managerCache) LPush(context.Context, string, ...interface{}) error { return nil }
func (c *managerCache) LRange(context.Context, string, int64, int64) ([]string, error) {
	return nil, nil
}
func (c *managerCache) LTrim(context.Context, string, int64, int64) error   { return nil }
func (c *managerCache) Expire(context.Context, string, time.Duration) error { return nil }
func (c *managerCache) Incr(context.Context, string) (int64, error)         { return 0, nil }
func (c *managerCache) Close() error                                        { return nil }
func (c *managerCache) Healthy(context.Context) bool                        { return true }

func TestManagerDiscoversWithoutExposingSchemas(t *testing.T) {
	registry := NewRegistry()
	for i := 0; i < 20; i++ {
		registry.MustRegister(managerTool{name: fmt.Sprintf("tool-%02d", i)})
	}
	manager := NewManager(registry, newManagerCache(), 3, time.Hour, zap.NewNop())
	scope := Scope{SessionID: "session"}
	initial := manager.InitialDefinitions(context.Background(), scope)
	if len(initial) != 1 || initial[0].Function.Name != ListToolsName {
		t.Fatalf("initial definitions = %+v", initial)
	}
	result, err := manager.Handle(context.Background(), scope, `{"action":"catalog"}`)
	if err != nil || !result.Success {
		t.Fatalf("catalog result=%+v err=%v", result, err)
	}
	if strings.Contains(result.Output, "schema_marker") || result.ToolDefinitions != nil {
		t.Fatalf("catalog leaked full schemas: %s", result.Output)
	}
	var catalog struct {
		Tools []CatalogEntry `json:"tools"`
	}
	if err := json.Unmarshal([]byte(result.Output), &catalog); err != nil || len(catalog.Tools) != 20 {
		t.Fatalf("catalog=%+v err=%v", catalog, err)
	}
	lazyJSON, _ := json.Marshal(initial)
	eager := make([]model.ToolDef, 0, len(registry.ListTools()))
	for _, candidate := range registry.ListTools() {
		eager = append(eager, definition(candidate))
	}
	eagerJSON, _ := json.Marshal(eager)
	if len(lazyJSON)*3 >= len(eagerJSON) {
		t.Fatalf("initial lazy schemas are not materially smaller: lazy=%d eager=%d", len(lazyJSON), len(eagerJSON))
	}
}

func TestManagerPersistsFrequencyAndEvictsLeastRecentlyUsed(t *testing.T) {
	registry := NewRegistry()
	for _, name := range []string{"alpha", "beta", "gamma"} {
		registry.MustRegister(managerTool{name: name})
	}
	store := newManagerCache()
	manager := NewManager(registry, store, 2, time.Hour, zap.NewNop())
	now := time.Unix(1, 0)
	manager.now = func() time.Time { now = now.Add(time.Second); return now }
	scope := Scope{SessionID: "session"}

	loaded, _ := manager.Load(context.Background(), scope, []string{"alpha", "beta"})
	if len(loaded.ToolDefinitions) != 2 {
		t.Fatalf("loaded definitions = %+v", loaded.ToolDefinitions)
	}
	manager.RecordUse(context.Background(), scope, "alpha")
	loaded, _ = manager.Load(context.Background(), scope, []string{"gamma"})
	if namesOf(loaded.ToolDefinitions)[0] != "gamma" || !containsName(namesOf(loaded.ToolDefinitions), "alpha") || containsName(namesOf(loaded.ToolDefinitions), "beta") {
		t.Fatalf("LRU definitions = %v", namesOf(loaded.ToolDefinitions))
	}
	if store.ttls[sessionToolKeyBase+"session:tools"] != time.Hour {
		t.Fatalf("session hot-set TTL = %v", store.ttls[sessionToolKeyBase+"session:tools"])
	}

	restarted := NewManager(registry, store, 2, time.Hour, zap.NewNop())
	initial := restarted.InitialDefinitions(context.Background(), scope)
	if len(initial) != 3 || !containsName(namesOf(initial), "alpha") || !containsName(namesOf(initial), "gamma") {
		t.Fatalf("restored definitions = %v", namesOf(initial))
	}
	catalog := restarted.Catalog(context.Background(), scope)
	for _, entry := range catalog {
		if entry.Name == "alpha" && (entry.SessionUses != 1 || entry.GlobalUses != 1 || !entry.Loaded) {
			t.Fatalf("alpha usage = %+v", entry)
		}
	}
}

func TestManagerEnforcesAllowlistAndDegradesOnRedisFailure(t *testing.T) {
	registry := NewRegistry()
	registry.MustRegister(managerTool{name: "alpha"})
	registry.MustRegister(managerTool{name: "beta"})
	store := newManagerCache()
	store.values[globalUsageKey] = `{"alpha":9}`
	store.getErr = errors.New("redis unavailable")
	manager := NewManager(registry, store, 2, time.Hour, zap.NewNop())
	scope := Scope{SessionID: "session", Restricted: true, Allowed: []string{"alpha"}}

	if err := manager.ValidateAllowed([]string{"missing"}); err == nil {
		t.Fatal("unknown allowed tool was accepted")
	}
	if err := manager.ValidateAllowed([]string{ListToolsName}); err == nil {
		t.Fatal("reserved tool name was accepted")
	}
	result, err := manager.Load(context.Background(), scope, []string{"alpha"})
	if err != nil || !result.Success || len(result.ToolDefinitions) != 1 {
		t.Fatalf("degraded load result=%+v err=%v", result, err)
	}
	if result, _ := manager.Load(context.Background(), scope, []string{"beta"}); result.Success {
		t.Fatal("disallowed tool was loaded")
	}
	if store.values[globalUsageKey] != `{"alpha":9}` {
		t.Fatal("failed global read overwrote persisted history")
	}
}

func TestManagerSerializesConcurrentUsageUpdates(t *testing.T) {
	registry := NewRegistry()
	registry.MustRegister(managerTool{name: "alpha"})
	store := newManagerCache()
	manager := NewManager(registry, store, 1, time.Hour, zap.NewNop())
	scope := Scope{SessionID: "session"}
	var group sync.WaitGroup
	for i := 0; i < 25; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			manager.RecordUse(context.Background(), scope, "alpha")
		}()
	}
	group.Wait()
	entry := manager.Catalog(context.Background(), scope)[0]
	if entry.SessionUses != 25 || entry.GlobalUses != 25 {
		t.Fatalf("usage after concurrent updates = %+v", entry)
	}
}

func TestRouterRejectsBusinessToolOutsideAllowlist(t *testing.T) {
	registry := NewRegistry()
	registry.MustRegister(managerTool{name: "alpha"})
	registry.MustRegister(managerTool{name: "beta"})
	manager := NewManager(registry, newManagerCache(), 2, time.Hour, zap.NewNop())
	router := NewRouter(registry, zap.NewNop(), manager)
	scope := Scope{SessionID: "session", Restricted: true, Allowed: []string{"alpha"}}

	if _, err := router.ExecuteScoped(context.Background(), scope, "beta", `{}`); err == nil {
		t.Fatal("execution router accepted a fabricated disallowed tool call")
	}
	entries := manager.Catalog(context.Background(), scope)
	if len(entries) != 1 || entries[0].Name != "alpha" || entries[0].GlobalUses != 0 {
		t.Fatalf("disallowed attempt changed usage or leaked catalog: %+v", entries)
	}
}

func namesOf(definitions []model.ToolDef) []string {
	names := make([]string, len(definitions))
	for i, definition := range definitions {
		names[i] = definition.Function.Name
	}
	return names
}

func containsName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}
