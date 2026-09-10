package rag

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/llm"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/observe"
)

type streamingLLMClient struct {
	events []llm.StreamEvent
}

func (streamingLLMClient) Chat(context.Context, *model.LLMRequest) (*model.LLMResponse, error) {
	panic("synchronous Chat must not be used for answer generation")
}

func (c streamingLLMClient) ChatStream(context.Context, *model.LLMRequest) (<-chan llm.StreamEvent, error) {
	stream := make(chan llm.StreamEvent, len(c.events))
	for _, event := range c.events {
		stream <- event
	}
	close(stream)
	return stream, nil
}

func (streamingLLMClient) Name() string                 { return "streaming" }
func (streamingLLMClient) Healthy(context.Context) bool { return true }

func TestGeneratorEmitsIncrementalAnswer(t *testing.T) {
	client := streamingLLMClient{events: []llm.StreamEvent{
		{Reasoning: "思考"},
		{Content: "流式"},
		{Content: "回答"},
		{Done: true},
	}}
	router := llm.NewRouter(map[string]llm.Client{"streaming": client}, "streaming", config.CBConfig{
		FailureThreshold: 3,
		SuccessThreshold: 1,
	})
	generator := NewGenerator(router, zap.NewNop())

	var emitted []observe.Event
	ctx := observe.WithEmitter(context.Background(), func(event observe.Event) {
		emitted = append(emitted, event)
	})
	answer, err := generator.Generate(ctx, "query", []model.Reference{{DocID: "doc", Content: "content", Score: 0.9}})
	if err != nil {
		t.Fatal(err)
	}
	if answer != "流式回答" {
		t.Fatalf("answer = %q, want 流式回答", answer)
	}
	if len(emitted) != 3 || emitted[0].Type != observe.TypeReasoningDelta ||
		emitted[1].Type != observe.TypeAnswerDelta || emitted[2].Type != observe.TypeAnswerDelta {
		t.Fatalf("unexpected incremental events: %+v", emitted)
	}
}
