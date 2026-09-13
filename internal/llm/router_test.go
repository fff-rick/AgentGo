package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/model"
)

type routeClient struct {
	name           string
	err            error
	streamEventErr error
	calls          int
}

func (c *routeClient) Chat(context.Context, *model.LLMRequest) (*model.LLMResponse, error) {
	c.calls++
	return &model.LLMResponse{Content: c.name}, c.err
}

func (c *routeClient) ChatStream(context.Context, *model.LLMRequest) (<-chan StreamEvent, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	stream := make(chan StreamEvent, 1)
	if c.streamEventErr != nil {
		stream <- StreamEvent{Err: c.streamEventErr}
	} else {
		stream <- StreamEvent{Done: true}
	}
	close(stream)
	return stream, nil
}

func TestRouterRecordsStreamingFailure(t *testing.T) {
	gpt := &routeClient{name: "gpt", streamEventErr: errors.New("stream failed")}
	qwen := &routeClient{name: "qwen"}
	router := NewRouter(
		map[string]Client{"gpt": gpt, "qwen": qwen},
		[]config.ModelConfig{{Name: "gpt", Priority: 1}, {Name: "qwen", Priority: 10}},
		config.CBConfig{FailureThreshold: 1, SuccessThreshold: 1, Timeout: time.Hour},
	)

	stream, err := router.ChatStream(context.Background(), &model.LLMRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	gpt.streamEventErr = nil
	stream, err = router.ChatStream(context.Background(), &model.LLMRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if gpt.calls != 1 || qwen.calls != 1 || router.breakers["gpt"].State() != StateOpen {
		t.Fatalf("calls: gpt=%d qwen=%d, states=%v", gpt.calls, qwen.calls, router.ListModels())
	}
}

func (c *routeClient) Name() string                 { return c.name }
func (c *routeClient) Healthy(context.Context) bool { return c.err == nil }

func TestRouterUsesPriorityExplicitModelAndCircuitFallback(t *testing.T) {
	gpt, qwen := &routeClient{name: "gpt"}, &routeClient{name: "qwen"}
	router := NewRouter(
		map[string]Client{"gpt": gpt, "qwen": qwen},
		[]config.ModelConfig{{Name: "qwen", Priority: 10}, {Name: "gpt", Priority: 1}},
		config.CBConfig{FailureThreshold: 1, SuccessThreshold: 1, Timeout: time.Hour},
	)

	if response, err := router.Chat(context.Background(), &model.LLMRequest{}); err != nil || response.Content != "gpt" {
		t.Fatalf("priority route: response=%+v err=%v", response, err)
	}
	if response, err := router.Chat(context.Background(), &model.LLMRequest{Model: "qwen"}); err != nil || response.Content != "qwen" {
		t.Fatalf("explicit route: response=%+v err=%v", response, err)
	}

	gpt.err = errors.New("gpt unavailable")
	if _, err := router.Chat(context.Background(), &model.LLMRequest{}); err == nil {
		t.Fatal("expected first GPT failure to open its circuit")
	}
	gpt.err = nil
	if response, err := router.Chat(context.Background(), &model.LLMRequest{}); err != nil || response.Content != "qwen" {
		t.Fatalf("circuit fallback: response=%+v err=%v", response, err)
	}
}
