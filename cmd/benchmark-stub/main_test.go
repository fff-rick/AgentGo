package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/llm"
	"github.com/enterprise/ai-agent-go/internal/model"
)

func TestLazyToolDiscovery(t *testing.T) {
	state.Lock()
	state.value = faults{}
	state.Unlock()
	server := httptest.NewServer(http.HandlerFunc(completion))
	defer server.Close()
	call := func(prior []string, available []string) string {
		t.Helper()
		messages := []map[string]any{{"role": "user", "content": "请计算 6 乘以 7"}}
		for _, name := range prior {
			messages = append(messages, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"function": map[string]any{"name": name}}}})
			messages = append(messages, map[string]any{"role": "tool", "content": "ok"})
		}
		tools := make([]any, 0, len(available))
		for _, name := range available {
			tools = append(tools, map[string]any{"function": map[string]any{"name": name}})
		}
		body, _ := json.Marshal(map[string]any{"model": "stub", "messages": messages, "tools": tools})
		response, err := http.Post(server.URL, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var result struct {
			Choices []struct {
				Message struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Function struct {
							Name string `json:"name"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if len(result.Choices[0].Message.ToolCalls) > 0 {
			return result.Choices[0].Message.ToolCalls[0].Function.Name
		}
		return result.Choices[0].Message.Content
	}
	if got := call(nil, []string{"list_tools"}); got != "list_tools" {
		t.Fatalf("catalog: %q", got)
	}
	if got := call([]string{"list_tools"}, []string{"list_tools"}); got != "list_tools" {
		t.Fatalf("load: %q", got)
	}
	if got := call([]string{"list_tools", "list_tools"}, []string{"list_tools", "calculator"}); got != "calculator" {
		t.Fatalf("business tool: %q", got)
	}
	if got := call([]string{"list_tools", "list_tools", "calculator"}, []string{"list_tools", "calculator"}); got != "结果是 42。" {
		t.Fatalf("answer: %q", got)
	}
}

func TestOpenAICompatibleChatAndStream(t *testing.T) {
	state.Lock()
	state.value = faults{}
	state.Unlock()
	server := httptest.NewServer(http.HandlerFunc(completion))
	defer server.Close()
	client := llm.NewHTTPClient(config.ModelConfig{Name: "stub", Model: "stub", BaseURL: server.URL, APIKey: "stub"}, time.Second)
	request := &model.LLMRequest{Messages: []model.LLMMessage{{Role: "user", Content: "你好"}}}
	response, err := client.Chat(context.Background(), request)
	if err != nil || response.Content != "你好" || response.Usage == nil {
		t.Fatalf("chat=%+v err=%v", response, err)
	}
	stream, err := client.ChatStream(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	got, usage := "", 0
	for event := range stream {
		if event.Err != nil {
			t.Fatal(event.Err)
		}
		got += event.Content
		if event.Usage != nil {
			usage = event.Usage.CompletionTokens
		}
	}
	if got != "你好" || usage != 8 {
		t.Fatalf("stream content=%q usage=%d", got, usage)
	}
	toolRequest := &model.LLMRequest{Messages: []model.LLMMessage{{Role: "user", Content: "请计算 6 乘以 7"}}, Tools: []model.ToolDef{{Type: "function", Function: model.FunctionDef{Name: "calculator", Parameters: map[string]any{"type": "object"}}}}}
	toolStream, err := client.ChatStream(context.Background(), toolRequest)
	if err != nil {
		t.Fatal(err)
	}
	var calls []model.LLMToolCall
	for event := range toolStream {
		if event.Err != nil {
			t.Fatal(event.Err)
		}
		if len(event.ToolCalls) > 0 {
			calls = event.ToolCalls
		}
	}
	if len(calls) != 1 || calls[0].Function.Name != "calculator" || calls[0].Function.Arguments != `{"operation":"multiply","a":6,"b":7}` {
		t.Fatalf("tool calls=%+v", calls)
	}
}

func TestChunkedStreamProducesMultipleDeltas(t *testing.T) {
	state.Lock()
	state.value = faults{StreamChunks: 3}
	state.Unlock()
	defer func() { state.Lock(); state.value = faults{}; state.Unlock() }()
	server := httptest.NewServer(http.HandlerFunc(completion))
	defer server.Close()
	client := llm.NewHTTPClient(config.ModelConfig{Name: "stub", Model: "stub", BaseURL: server.URL, APIKey: "stub"}, time.Second)
	stream, err := client.ChatStream(context.Background(), &model.LLMRequest{Messages: []model.LLMMessage{{Role: "user", Content: "你好"}}})
	if err != nil {
		t.Fatal(err)
	}
	deltas := 0
	for event := range stream {
		if event.Err != nil {
			t.Fatal(event.Err)
		}
		if event.Content != "" {
			deltas++
		}
	}
	if deltas != 3 {
		t.Fatalf("got %d deltas", deltas)
	}
}
