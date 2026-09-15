package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/cache"
	"github.com/enterprise/ai-agent-go/internal/model"
)

const (
	ListToolsName      = "list_tools"
	globalUsageKey     = "agentgo:v1:tools:global"
	sessionToolKeyBase = "agentgo:v2:session:"
)

type Scope struct {
	SessionID  string
	Allowed    []string
	Restricted bool
}

func (s Scope) Allows(name string) bool {
	if name == ListToolsName {
		return true
	}
	if !s.Restricted {
		return true
	}
	for _, allowed := range s.Allowed {
		if name == allowed {
			return true
		}
	}
	return false
}

type toolUsage struct {
	SessionUses int64 `json:"session_uses"`
	LastAccess  int64 `json:"last_access"`
}

type sessionToolState struct {
	Tools map[string]toolUsage `json:"tools"`
}

type CatalogEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	GlobalUses  int64  `json:"global_uses"`
	SessionUses int64  `json:"session_uses"`
	Loaded      bool   `json:"loaded"`
}

// Manager owns the request-visible tool catalog and the persisted session hot set.
type Manager struct {
	registry  *Registry
	store     cache.Cache
	threshold int
	ttl       time.Duration
	logger    *zap.Logger
	mu        sync.Mutex
	now       func() time.Time
}

func NewManager(registry *Registry, store cache.Cache, threshold int, ttl time.Duration, logger *zap.Logger) *Manager {
	if threshold <= 0 {
		threshold = 3
	}
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour
	}
	return &Manager{registry: registry, store: store, threshold: threshold, ttl: ttl, logger: logger, now: time.Now}
}

func (m *Manager) Threshold() int { return m.threshold }

func (m *Manager) ValidateAllowed(names []string) error {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == ListToolsName {
			return fmt.Errorf("%s 是保留工具名", ListToolsName)
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		if _, ok := m.registry.Get(name); !ok {
			return fmt.Errorf("工具 %q 未注册", name)
		}
	}
	return nil
}

func (m *Manager) InitialDefinitions(ctx context.Context, scope Scope) []model.ToolDef {
	state, _ := m.readState(ctx, scope.SessionID)
	return append([]model.ToolDef{m.listToolsDefinition()}, m.definitionsForState(state, scope)...)
}

func (m *Manager) Handle(ctx context.Context, scope Scope, input string) (*ToolResult, error) {
	var request struct {
		Action string   `json:"action"`
		Names  []string `json:"names"`
	}
	if err := json.Unmarshal([]byte(input), &request); err != nil {
		return NewErrorResult("参数解析失败: " + err.Error()), nil
	}
	switch request.Action {
	case "catalog":
		entries := m.catalog(ctx, scope)
		encoded, err := json.Marshal(map[string]interface{}{"tools": entries, "max_loaded": m.threshold})
		if err != nil {
			return nil, err
		}
		return NewSuccessResult(string(encoded)), nil
	case "load":
		return m.load(ctx, scope, request.Names)
	default:
		return NewErrorResult("action 只支持 catalog 或 load"), nil
	}
}

func (m *Manager) RecordUse(ctx context.Context, scope Scope, name string) {
	if name == ListToolsName || !scope.Allows(name) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.readStateLocked(ctx, scope.SessionID)
	global, globalErr := m.readGlobalLocked(ctx)
	usage := state.Tools[name]
	usage.SessionUses++
	usage.LastAccess = m.now().UnixNano()
	state.Tools[name] = usage
	global[name]++
	m.trim(state, global)
	m.writeSessionLocked(ctx, scope.SessionID, state)
	if globalErr == nil {
		m.writeGlobalLocked(ctx, global)
	}
}

func (m *Manager) Catalog(ctx context.Context, scope Scope) []CatalogEntry {
	return m.catalog(ctx, scope)
}

func (m *Manager) Load(ctx context.Context, scope Scope, names []string) (*ToolResult, error) {
	return m.load(ctx, scope, names)
}

func (m *Manager) catalog(ctx context.Context, scope Scope) []CatalogEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.readStateLocked(ctx, scope.SessionID)
	global, _ := m.readGlobalLocked(ctx)
	tools := m.registry.ListTools()
	entries := make([]CatalogEntry, 0, len(tools))
	for _, candidate := range tools {
		if !scope.Allows(candidate.Name()) {
			continue
		}
		usage, loaded := state.Tools[candidate.Name()]
		entries = append(entries, CatalogEntry{
			Name: candidate.Name(), Description: candidate.Description(), GlobalUses: global[candidate.Name()],
			SessionUses: usage.SessionUses, Loaded: loaded,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Loaded != entries[j].Loaded {
			return entries[i].Loaded
		}
		if entries[i].SessionUses != entries[j].SessionUses {
			return entries[i].SessionUses > entries[j].SessionUses
		}
		if entries[i].GlobalUses != entries[j].GlobalUses {
			return entries[i].GlobalUses > entries[j].GlobalUses
		}
		return entries[i].Name < entries[j].Name
	})
	return entries
}

func (m *Manager) load(ctx context.Context, scope Scope, names []string) (*ToolResult, error) {
	if len(names) == 0 {
		return NewErrorResult("names 不能为空"), nil
	}
	if len(names) > m.threshold {
		return NewErrorResult(fmt.Sprintf("单次最多加载 %d 个工具", m.threshold)), nil
	}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == ListToolsName || !scope.Allows(name) {
			return NewErrorResult(fmt.Sprintf("工具 %q 不在允许列表中", name)), nil
		}
		if _, ok := m.registry.Get(name); !ok {
			return NewErrorResult(fmt.Sprintf("工具 %q 未注册", name)), nil
		}
		if _, duplicate := seen[name]; duplicate {
			return NewErrorResult(fmt.Sprintf("工具 %q 重复", name)), nil
		}
		seen[name] = struct{}{}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.readStateLocked(ctx, scope.SessionID)
	global, _ := m.readGlobalLocked(ctx)
	before := make(map[string]struct{}, len(state.Tools))
	for name := range state.Tools {
		before[name] = struct{}{}
	}
	now := m.now().UnixNano()
	for _, name := range names {
		usage := state.Tools[name]
		usage.LastAccess = now
		state.Tools[name] = usage
	}
	m.trim(state, global)
	m.writeSessionLocked(ctx, scope.SessionID, state)

	evicted := make([]string, 0)
	for name := range before {
		if _, ok := state.Tools[name]; !ok {
			evicted = append(evicted, name)
		}
	}
	sort.Strings(evicted)
	loaded := make([]string, 0, len(names))
	for _, name := range names {
		if _, ok := state.Tools[name]; ok {
			loaded = append(loaded, name)
		}
	}
	encoded, err := json.Marshal(map[string]interface{}{"loaded": loaded, "evicted": evicted})
	if err != nil {
		return nil, err
	}
	return &ToolResult{Success: true, Output: string(encoded), ToolDefinitions: m.definitionsForState(state, scope)}, nil
}

func (m *Manager) listToolsDefinition() model.ToolDef {
	return model.ToolDef{Type: "function", Function: model.FunctionDef{
		Name:        ListToolsName,
		Description: "发现并加载工具。先用 action=catalog 查看紧凑目录，再用 action=load 和 names 加载所需工具；加载后下一轮才能调用。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"action": map[string]interface{}{"type": "string", "enum": []string{"catalog", "load"}},
				"names":  map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "maxItems": m.threshold},
			},
			"required": []string{"action"},
		},
	}}
}

func (m *Manager) definitionsForState(state sessionToolState, scope Scope) []model.ToolDef {
	names := make([]string, 0, len(state.Tools))
	for name := range state.Tools {
		if scope.Allows(name) {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		left, right := state.Tools[names[i]], state.Tools[names[j]]
		if left.LastAccess != right.LastAccess {
			return left.LastAccess > right.LastAccess
		}
		if left.SessionUses != right.SessionUses {
			return left.SessionUses > right.SessionUses
		}
		return names[i] < names[j]
	})
	if len(names) > m.threshold {
		names = names[:m.threshold]
	}
	definitions := make([]model.ToolDef, 0, len(names))
	for _, name := range names {
		if candidate, ok := m.registry.Get(name); ok {
			definitions = append(definitions, definition(candidate))
		}
	}
	return definitions
}

func (m *Manager) trim(state sessionToolState, global map[string]int64) {
	for len(state.Tools) > m.threshold {
		names := make([]string, 0, len(state.Tools))
		for name := range state.Tools {
			names = append(names, name)
		}
		sort.Slice(names, func(i, j int) bool {
			left, right := state.Tools[names[i]], state.Tools[names[j]]
			if left.LastAccess != right.LastAccess {
				return left.LastAccess < right.LastAccess
			}
			if left.SessionUses != right.SessionUses {
				return left.SessionUses < right.SessionUses
			}
			if global[names[i]] != global[names[j]] {
				return global[names[i]] < global[names[j]]
			}
			return names[i] < names[j]
		})
		delete(state.Tools, names[0])
	}
}

func (m *Manager) readState(ctx context.Context, sessionID string) (sessionToolState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readStateLocked(ctx, sessionID), nil
}

func (m *Manager) readStateLocked(ctx context.Context, sessionID string) sessionToolState {
	state := sessionToolState{Tools: make(map[string]toolUsage)}
	if m.store == nil || sessionID == "" {
		return state
	}
	raw, err := m.store.Get(ctx, sessionToolKeyBase+sessionID+":tools")
	if err != nil || raw == "" {
		m.warn("读取 Session 工具热集失败", err)
		return state
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		m.warn("解析 Session 工具热集失败", err)
		return sessionToolState{Tools: make(map[string]toolUsage)}
	}
	if state.Tools == nil {
		state.Tools = make(map[string]toolUsage)
	}
	return state
}

func (m *Manager) readGlobalLocked(ctx context.Context) (map[string]int64, error) {
	global := make(map[string]int64)
	if m.store == nil {
		return global, nil
	}
	raw, err := m.store.Get(ctx, globalUsageKey)
	if err != nil {
		m.warn("读取全局工具频率失败", err)
		return global, err
	}
	if raw == "" {
		return global, nil
	}
	if err := json.Unmarshal([]byte(raw), &global); err != nil {
		m.warn("解析全局工具频率失败", err)
		return make(map[string]int64), err
	}
	return global, nil
}

func (m *Manager) writeSessionLocked(ctx context.Context, sessionID string, state sessionToolState) {
	if m.store == nil || sessionID == "" {
		return
	}
	if encoded, err := json.Marshal(state); err != nil {
		m.warn("序列化 Session 工具热集失败", err)
	} else if err := m.store.Set(ctx, sessionToolKeyBase+sessionID+":tools", string(encoded), m.ttl); err != nil {
		m.warn("保存 Session 工具热集失败", err)
	}
}

func (m *Manager) writeGlobalLocked(ctx context.Context, global map[string]int64) {
	if m.store == nil {
		return
	}
	if encoded, err := json.Marshal(global); err != nil {
		m.warn("序列化全局工具频率失败", err)
	} else if err := m.store.Set(ctx, globalUsageKey, string(encoded), 0); err != nil {
		m.warn("保存全局工具频率失败", err)
	}
}

func (m *Manager) warn(message string, err error) {
	if err != nil && m.logger != nil {
		m.logger.Warn(message, zap.Error(err))
	}
}

func definition(candidate Tool) model.ToolDef {
	return model.ToolDef{Type: "function", Function: model.FunctionDef{
		Name: candidate.Name(), Description: candidate.Description(), Parameters: candidate.Parameters(),
	}}
}
