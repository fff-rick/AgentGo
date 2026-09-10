// Package observe carries request-scoped Agent events to streaming views.
package observe

import (
	"context"
	"time"

	"github.com/enterprise/ai-agent-go/internal/model"
)

const (
	TypeStatus         = "status"
	TypeIntent         = "intent"
	TypeReasoning      = "reasoning"
	TypeReasoningDelta = "reasoning_delta"
	TypeToolCall       = "tool_call"
	TypeToolResult     = "tool_result"
	TypeReferences     = "references"
	TypeAnswer         = "answer"
	TypeAnswerDelta    = "answer_delta"
)

// Event is a safe, user-visible description of Agent execution.
type Event struct {
	Type       string              `json:"type"`
	Stage      string              `json:"stage,omitempty"`
	Message    string              `json:"message,omitempty"`
	Intent     string              `json:"intent,omitempty"`
	Confidence float64             `json:"confidence,omitempty"`
	Tool       *model.ToolCallInfo `json:"tool,omitempty"`
	References []model.Reference   `json:"references,omitempty"`
	Timestamp  time.Time           `json:"timestamp"`
}

type emitterKey struct{}
type Emitter func(Event)

func WithEmitter(ctx context.Context, emitter Emitter) context.Context {
	return context.WithValue(ctx, emitterKey{}, emitter)
}

func Emit(ctx context.Context, event Event) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	if emitter, ok := ctx.Value(emitterKey{}).(Emitter); ok && emitter != nil {
		emitter(event)
	}
}
