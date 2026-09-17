package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/model"
)

func TestSDKStreamAccumulatesNativeToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.UserAgent(); got != "AgentGo/1.0" {
			t.Errorf("User-Agent = %q", got)
		}
		var body struct {
			Model             string `json:"model"`
			Stream            bool   `json:"stream"`
			ParallelToolCalls bool   `json:"parallel_tool_calls"`
			ToolChoice        struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_choice"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "qwen2.5:7b" || !body.Stream || !body.ParallelToolCalls || body.ToolChoice.Function.Name != "calculator" {
			t.Errorf("unexpected SDK request: %+v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"思考","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"calculator","arguments":"{\"operation\":"}}]},"finish_reason":null}]}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"add\"}"}}]},"finish_reason":"tool_calls"}]}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "data: [DONE]")
		fmt.Fprintln(w)
	}))
	defer server.Close()

	client := NewHTTPClient(config.ModelConfig{Name: "qwen-tools", Model: "qwen2.5:7b", BaseURL: server.URL, APIKey: "test"}, 5*time.Second)
	stream, err := client.ChatStream(context.Background(), &model.LLMRequest{
		Model:    "qwen-tools",
		Messages: []model.LLMMessage{{Role: "user", Content: "calculate"}},
		Tools: []model.ToolDef{{Type: "function", Function: model.FunctionDef{
			Name: "calculator", Description: "calculate", Parameters: map[string]interface{}{"type": "object"},
		}}},
		RequiredTool: "calculator",
	})
	if err != nil {
		t.Fatal(err)
	}
	var reasoning string
	var calls []model.LLMToolCall
	for event := range stream {
		if event.Err != nil {
			t.Fatal(event.Err)
		}
		reasoning += event.Reasoning
		if event.Usage != nil {
			t.Fatalf("unexpected usage: %+v", event.Usage)
		}
		if len(event.ToolCalls) > 0 {
			calls = event.ToolCalls
		}
	}
	if reasoning != "思考" || len(calls) != 1 || calls[0].Function.Name != "calculator" || calls[0].Function.Arguments != `{"operation":"add"}` {
		t.Fatalf("reasoning=%q calls=%+v", reasoning, calls)
	}
}

func TestSDKStreamPreservesProviderReportedUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if _, exists := request["stream_options"]; exists {
			t.Error("stream options must remain compatible with existing backends")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"test","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "data: [DONE]")
		fmt.Fprintln(w)
	}))
	defer server.Close()
	client := NewHTTPClient(config.ModelConfig{Name: "test", Model: "test", BaseURL: server.URL, APIKey: "test"}, 5*time.Second)
	stream, err := client.ChatStream(context.Background(), &model.LLMRequest{Messages: []model.LLMMessage{{Role: "user", Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	var usage *model.UsageInfo
	for event := range stream {
		if event.Err != nil {
			t.Fatal(event.Err)
		}
		if event.Usage != nil {
			usage = event.Usage
		}
	}
	if usage == nil || usage.PromptTokens != 12 || usage.CompletionTokens != 3 {
		t.Fatalf("usage=%+v", usage)
	}
}
