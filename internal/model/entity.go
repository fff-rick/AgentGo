package model

import "time"

// Session 会话实体
type Session struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// UserInfo 是会话展示上下文；UserID 由 OIDC 身份或本地模式的固定身份确定。
type UserInfo struct {
	UserID      string            `json:"user_id"`
	DisplayName string            `json:"display_name,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// Message 消息实体（持久化存储用）
type Message struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Sequence  int64     `json:"sequence"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// MemoryKind 表示可长期沉淀的语义记忆类型。
type MemoryKind string

const (
	MemoryPreference MemoryKind = "preference"
	MemoryFact       MemoryKind = "fact"
	MemoryExperience MemoryKind = "experience"
	MemorySolution   MemoryKind = "solution"
)

// MemoryItem 是用户隔离的长期语义记忆，而不是原始聊天消息。
type MemoryItem struct {
	ID              string     `json:"id"`
	UserID          string     `json:"user_id"`
	Kind            MemoryKind `json:"kind"`
	Content         string     `json:"content"`
	Importance      float64    `json:"importance"`
	SourceSessionID string     `json:"source_session_id"`
	CreatedAt       time.Time  `json:"created_at"`
	Score           float64    `json:"score,omitempty"`
	Topic           string     `json:"topic,omitempty"`
	Version         int64      `json:"version,omitempty"`
	Confidence      float64    `json:"confidence,omitempty"`
	Status          string     `json:"status,omitempty"`
	SourceMessageID string     `json:"source_message_id,omitempty"`
	ValidFrom       time.Time  `json:"valid_from,omitempty"`
	ValidTo         *time.Time `json:"valid_to,omitempty"`
	UpdatedAt       time.Time  `json:"updated_at,omitempty"`
	LastUsedAt      *time.Time `json:"last_used_at,omitempty"`
	UseCount        int64      `json:"use_count,omitempty"`
}

type MemoryVersion struct {
	Version         int64      `json:"version"`
	Content         string     `json:"content"`
	Status          string     `json:"status"`
	SourceSessionID string     `json:"source_session_id,omitempty"`
	SourceMessageID string     `json:"source_message_id,omitempty"`
	SourceAt        time.Time  `json:"source_at"`
	CreatedAt       time.Time  `json:"created_at"`
	ValidTo         *time.Time `json:"valid_to,omitempty"`
}

// Document 文档实体
type Document struct {
	ID          string         `json:"id"`
	Title       string         `json:"title"`
	Content     string         `json:"content"`
	ContentType string         `json:"content_type"`
	ContentHash string         `json:"-"`
	Tags        []string       `json:"tags"`
	Status      string         `json:"status"`
	ChunkCount  int            `json:"chunk_count"`
	Metadata    map[string]any `json:"metadata"`
	Filename    string         `json:"-"`
	RawContent  []byte         `json:"-"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// DocumentChunk 文档分块
type DocumentChunk struct {
	ID         string         `json:"id"`
	DocID      string         `json:"doc_id"`
	Content    string         `json:"content"`
	Embedding  []float32      `json:"embedding"`
	ChunkIndex int            `json:"chunk_index"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}

// IntentResult 意图识别结果
type IntentResult struct {
	Intent        string            `json:"intent"` // chat / rag_query / tool_use / complex_task
	Confidence    float64           `json:"confidence"`
	Entities      map[string]string `json:"entities"`
	RequiredTools []string          `json:"required_tools,omitempty"`
}

// AgentStep Agent 执行步骤
type AgentStep struct {
	StepIndex  int       `json:"step_index"`
	Type       string    `json:"type"` // thought / action / observation / reflection
	Content    string    `json:"content"`
	ToolName   string    `json:"tool_name,omitempty"`
	ToolInput  string    `json:"tool_input,omitempty"`
	ToolOutput string    `json:"tool_output,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
}
