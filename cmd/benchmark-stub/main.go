// Command benchmark-stub is a deterministic OpenAI-compatible service for local benchmarks.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type faults struct {
	LLM           string `json:"llm"`
	LLMModel      string `json:"llm_model"`
	Embedding     string `json:"embedding"`
	Search        string `json:"search"`
	DelayMS       int    `json:"delay_ms"`
	StreamDelayMS int    `json:"stream_delay_ms"`
	StreamChunks  int    `json:"stream_chunks"`
}

var state struct {
	sync.RWMutex
	value faults
}
var llmCalls, embeddingCalls, searchCalls atomic.Int64

func main() {
	addr := flag.String("addr", ":8088", "listen address")
	flag.Parse()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/admin/fault", func(w http.ResponseWriter, r *http.Request) {
		state.Lock()
		defer state.Unlock()
		if r.Method == "POST" {
			var v faults
			if json.NewDecoder(r.Body).Decode(&v) != nil {
				http.Error(w, "invalid JSON", 400)
				return
			}
			state.value = v
		}
		writeJSON(w, state.value)
	})
	mux.HandleFunc("/admin/stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]int64{"llm_requests": llmCalls.Load(), "embedding_requests": embeddingCalls.Load(), "search_requests": searchCalls.Load()})
	})
	mux.HandleFunc("/v1/chat/completions", completion)
	mux.HandleFunc("/v1/embeddings", embedding)
	mux.HandleFunc("/search", search)
	fmt.Println(http.ListenAndServe(*addr, mux))
}
func current() faults { state.RLock(); defer state.RUnlock(); return state.value }
func fail(w http.ResponseWriter, r *http.Request, mode string, delay int) bool {
	if delay > 0 {
		select {
		case <-time.After(time.Duration(delay) * time.Millisecond):
		case <-r.Context().Done():
			return true
		}
	}
	switch mode {
	case "429":
		http.Error(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`, 429)
		return true
	case "500":
		http.Error(w, `{"error":{"message":"upstream failed"}}`, 500)
		return true
	case "timeout":
		<-r.Context().Done()
		return true
	}
	return false
}
func completion(w http.ResponseWriter, r *http.Request) {
	llmCalls.Add(1)
	f := current()
	var req struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "bad JSON", 400)
		return
	}
	if f.LLMModel == "" || f.LLMModel == req.Model {
		if fail(w, r, f.LLM, f.DelayMS) {
			return
		}
	}
	prompt := ""
	businessUsed := false
	discoveryCalls := 0
	desired := ""
	for _, m := range req.Messages {
		if m.Role == "user" {
			prompt = m.Content
		}
		for _, call := range m.ToolCalls {
			if call.Function.Name == "list_tools" {
				discoveryCalls++
			}
		}
	}
	switch {
	case strings.Contains(prompt, "计算"):
		desired = "calculator"
	case strings.Contains(prompt, "搜索") || strings.Contains(prompt, "联网"):
		desired = "web_search"
	case strings.Contains(prompt, "文档") || strings.Contains(prompt, "知识库"):
		desired = "knowledge_search"
	}
	for _, m := range req.Messages {
		for _, call := range m.ToolCalls {
			businessUsed = businessUsed || call.Function.Name == desired && desired != ""
		}
	}
	tool := ""
	args := ""
	if f.LLM == "invalid_tool" {
		tool = "unknown_tool"
		args = "{bad"
	} else if desired != "" && !businessUsed {
		hasListTools := false
		for _, candidate := range req.Tools {
			if candidate.Function.Name == desired {
				tool = desired
			}
			if candidate.Function.Name == "list_tools" {
				hasListTools = true
			}
		}
		if tool == "" && hasListTools {
			tool = "list_tools"
			if discoveryCalls == 0 {
				args = `{"action":"catalog"}`
			} else {
				args = fmt.Sprintf(`{"action":"load","names":[%q]}`, desired)
			}
		}
		switch tool {
		case "calculator":
			args = `{"operation":"multiply","a":6,"b":7}`
		case "web_search":
			args = `{"query":"AgentGo benchmark"}`
		case "knowledge_search":
			args = `{"query":"AgentGo Milvus BM25 RRF 检索"}`
		}
	}
	answer := "你好"
	if businessUsed {
		if strings.Contains(prompt, "搜索") {
			answer = "搜索结果已返回。"
		} else if strings.Contains(prompt, "文档") {
			answer = "文档中记录了 AgentGo 的检索流程。"
		} else {
			answer = "结果是 42。"
		}
	} else if strings.Contains(prompt, "Go") {
		answer = "Go 使用 goroutine 运行并发任务。"
	}
	for _, m := range req.Messages {
		if strings.Contains(m.Content, "代号是青鸟") && strings.Contains(prompt, "代号") {
			answer = "你的代号是青鸟。"
		}
	}
	if len(req.Messages) > 0 {
		system := req.Messages[0].Content
		switch {
		case strings.Contains(system, "意图识别引擎"):
			kind := "chat"
			switch {
			case strings.Contains(prompt, "文档") || strings.Contains(prompt, "知识库"):
				kind = "rag_query"
			case strings.Contains(prompt, "搜索") || strings.Contains(prompt, "计算"):
				kind = "tool_use"
			case strings.Contains(prompt, "规划") || strings.Contains(prompt, "步骤"):
				kind = "complex_task"
			}
			answer = fmt.Sprintf(`{"intent":%q,"confidence":1,"entities":{},"required_tools":[]}`, kind)
			tool = ""
		case strings.Contains(system, "任务规划专家"):
			answer = `[{"step":1,"description":"完成任务","tool":"","input":{},"depends_on":[]}]`
			tool = ""
		case strings.Contains(system, "负责为任务选择工具"):
			answer = `{"tools":[]}`
			tool = ""
		}
	}
	choice := map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}
	if tool != "" {
		choice["message"] = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": tool, "arguments": args}}}}
		choice["finish_reason"] = "tool_calls"
	}
	usage := map[string]int{"prompt_tokens": 12, "completion_tokens": 8, "total_tokens": 20}
	if !req.Stream {
		writeJSON(w, map[string]any{"id": "stub-1", "object": "chat.completion", "created": time.Now().Unix(), "model": req.Model, "choices": []any{choice}, "usage": usage})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	delta := map[string]any{"role": "assistant", "content": answer}
	if tool != "" {
		delta = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "call-1", "type": "function", "function": map[string]any{"name": tool, "arguments": args}}}}
	}
	chunks := []any{streamChunk(req.Model, delta, nil)}
	if f.StreamChunks > 1 && tool == "" {
		chunks = chunks[:0]
		for i := 0; i < min(f.StreamChunks, 1000); i++ {
			part := strings.Repeat("慢", 1024)
			if i == 0 {
				part = answer
			}
			chunks = append(chunks, streamChunk(req.Model, map[string]any{"content": part}, nil))
		}
		usage["completion_tokens"] = len([]rune(answer)) + (len(chunks)-1)*1024
		usage["total_tokens"] = usage["prompt_tokens"] + usage["completion_tokens"]
	}
	chunks = append(chunks, streamChunk(req.Model, map[string]any{}, choice["finish_reason"]), map[string]any{"id": "stub-1", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": req.Model, "choices": []any{}, "usage": usage})
	for _, c := range chunks {
		raw, _ := json.Marshal(c)
		fmt.Fprintf(w, "data: %s\n\n", raw)
		if flusher != nil {
			flusher.Flush()
		}
		if f.StreamDelayMS > 0 {
			select {
			case <-time.After(time.Duration(f.StreamDelayMS) * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
func streamChunk(modelName string, delta map[string]any, finish any) map[string]any {
	return map[string]any{
		"id": "stub-1", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": modelName,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
}
func embedding(w http.ResponseWriter, r *http.Request) {
	embeddingCalls.Add(1)
	f := current()
	if fail(w, r, f.Embedding, f.DelayMS) {
		return
	}
	var req struct {
		Input []string `json:"input"`
		Model string   `json:"model"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "bad JSON", 400)
		return
	}
	data := make([]any, len(req.Input))
	for i, input := range req.Input {
		v := make([]float32, 1024)
		h := fnv.New32a()
		_, _ = h.Write([]byte(strings.ToLower(input)))
		index := h.Sum32() % 1024
		v[index] = 1
		v[(index+17)%1024] = float32(1 / math.Sqrt2)
		data[i] = map[string]any{"index": i, "embedding": v, "object": "embedding"}
	}
	writeJSON(w, map[string]any{"data": data, "model": req.Model, "object": "list"})
}
func search(w http.ResponseWriter, r *http.Request) {
	searchCalls.Add(1)
	f := current()
	if fail(w, r, f.Search, f.DelayMS) {
		return
	}
	writeJSON(w, map[string]any{"results": []any{map[string]string{"title": "AgentGo benchmark", "url": "https://example.invalid/benchmark", "content": "固定搜索结果", "engine": "stub"}}})
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
