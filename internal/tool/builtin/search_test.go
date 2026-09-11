package builtin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/enterprise/ai-agent-go/internal/config"
)

func TestSearchToolCallsSearXNGAndLimitsResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" || r.URL.Query().Get("q") != "成都天气" ||
			r.URL.Query().Get("format") != "json" || r.URL.Query().Get("language") != "zh-CN" ||
			r.URL.Query().Get("safesearch") != "1" || r.URL.Query().Get("time_range") != "day" {
			t.Fatalf("unexpected search request: %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
            {"title":"成都天气预报","url":"https://weather.example/chengdu","content":"成都今天晴，25°C","engines":["bing","duckduckgo"],"publishedDate":"2026-09-10"},
            {"title":"成都气象","url":"https://meteo.example/chengdu","content":"未来天气趋势","engine":"google"},
            {"title":"第三条","url":"https://example.com/third","content":"不应返回"}
        ]}`))
	}))
	defer server.Close()

	search := NewSearchTool(config.SearchConfig{
		BaseURL: server.URL, Timeout: time.Second, Language: "zh-CN", SafeSearch: 1,
	}, zap.NewNop())
	result, err := search.Execute(context.Background(), `{"query":"成都天气","max_results":2,"time_range":"day"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("search failed: %s", result.Error)
	}
	var output searchOutput
	if err := json.Unmarshal([]byte(result.Output), &output); err != nil {
		t.Fatalf("invalid search output %q: %v", result.Output, err)
	}
	if len(output.Results) != 2 || output.Results[0].Source != "bing,duckduckgo" ||
		output.Results[0].URL != "https://weather.example/chengdu" {
		t.Fatalf("unexpected search output: %+v", output)
	}
}

func TestSearchToolValidatesInput(t *testing.T) {
	search := NewSearchTool(config.SearchConfig{}, zap.NewNop())
	for _, input := range []string{
		`{"query":""}`,
		`{"query":"weather","time_range":"week"}`,
		`not-json`,
	} {
		result, err := search.Execute(context.Background(), input)
		if err != nil {
			t.Fatalf("Execute(%q) error = %v", input, err)
		}
		if result.Success || result.Error == "" {
			t.Fatalf("Execute(%q) = %+v, want validation error", input, result)
		}
	}
}

func TestSearchToolReturnsBackendErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "backend unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	search := NewSearchTool(config.SearchConfig{BaseURL: server.URL, Timeout: time.Second}, zap.NewNop())
	if _, err := search.Execute(context.Background(), `{"query":"AgentGo"}`); err == nil {
		t.Fatal("expected backend error")
	}
}
