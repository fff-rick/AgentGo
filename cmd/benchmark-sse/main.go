// Command benchmark-sse measures the public SSE endpoint event by event.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/enterprise/ai-agent-go/internal/benchmarkmeta"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type sample struct {
	Index               int      `json:"index"`
	HTTPStatus          int      `json:"http_status"`
	TraceID             string   `json:"trace_id,omitempty"`
	TTFTMS              *float64 `json:"answer_ttft_ms,omitempty"`
	DurationMS          float64  `json:"duration_ms"`
	DeltaRunes          int      `json:"delta_runes"`
	DeltaRunesPerSecond *float64 `json:"delta_runes_per_second,omitempty"`
	TokenRate           *float64 `json:"reported_loop_tokens_per_second,omitempty"`
	Done                bool     `json:"done"`
	Disconnected        bool     `json:"disconnected,omitempty"`
	Error               string   `json:"error,omitempty"`
}
type report struct {
	Metadata      benchmarkmeta.Metadata `json:"metadata"`
	StartedAt     time.Time              `json:"started_at"`
	Concurrent    int                    `json:"concurrent"`
	Mode          string                 `json:"mode"`
	P50TTFTMS     *float64               `json:"p50_ttft_ms,omitempty"`
	P95TTFTMS     *float64               `json:"p95_ttft_ms,omitempty"`
	P95DurationMS float64                `json:"p95_duration_ms"`
	Completed     int                    `json:"completed"`
	Failed        int                    `json:"failed"`
	Disconnected  int                    `json:"disconnected"`
	Samples       []sample               `json:"samples"`
	Unavailable   []string               `json:"unavailable,omitempty"`
}

func main() {
	base := flag.String("base-url", "http://localhost:8080", "AgentGo URL")
	message := flag.String("message", "请简要介绍 Go 的 goroutine", "prompt")
	count := flag.Int("count", 10, "total connections")
	concurrency := flag.Int("concurrency", 2, "parallel connections")
	slow := flag.Duration("slow-read", 0, "pause after each SSE event")
	disconnect := flag.Bool("disconnect-after-first-delta", false, "cancel after first answer delta")
	timeout := flag.Duration("timeout", 2*time.Minute, "connection timeout")
	output := flag.String("output", "", "JSON report path")
	backend := flag.String("backend", "real", "real or stub backend")
	scenario := flag.String("scenario", "sse", "experiment scenario")
	model := flag.String("model", "", "model identifier")
	configVersion := flag.String("config-version", "", "non-secret config version")
	flag.Parse()
	if *count < 1 || *concurrency < 1 {
		fmt.Fprintln(os.Stderr, "count and concurrency must be positive")
		os.Exit(2)
	}
	client := &http.Client{Timeout: *timeout}
	jobs := make(chan int)
	out := make(chan sample, *count)
	var wg sync.WaitGroup
	started := time.Now()
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				out <- run(client, *base, *message, index, *slow, *disconnect)
			}
		}()
	}
	go func() {
		for i := 0; i < *count; i++ {
			jobs <- i + 1
		}
		close(jobs)
		wg.Wait()
		close(out)
	}()
	rep := report{StartedAt: started, Concurrent: *concurrency, Mode: "normal"}
	rep.Metadata = benchmarkmeta.Collect("", *backend, *scenario, *model, "", *configVersion)
	if *disconnect {
		rep.Mode = "disconnect"
	} else if *slow > 0 {
		rep.Mode = "slow_client"
	}
	var ttfts, durations []float64
	for s := range out {
		rep.Samples = append(rep.Samples, s)
		durations = append(durations, s.DurationMS)
		if s.TTFTMS != nil {
			ttfts = append(ttfts, *s.TTFTMS)
		}
		if s.Done {
			rep.Completed++
		} else if s.Disconnected {
			rep.Disconnected++
		} else {
			rep.Failed++
		}
	}
	sort.Slice(rep.Samples, func(i, j int) bool { return rep.Samples[i].Index < rep.Samples[j].Index })
	if len(ttfts) > 0 {
		p50, p95 := percentile(ttfts, .5), percentile(ttfts, .95)
		rep.P50TTFTMS = &p50
		rep.P95TTFTMS = &p95
	}
	rep.P95DurationMS = percentile(durations, .95)
	if len(ttfts) == 0 {
		rep.Unavailable = append(rep.Unavailable, "answer_ttft: no answer_delta event")
	}
	hasTokens := false
	for _, s := range rep.Samples {
		if s.TokenRate != nil {
			hasTokens = true
			break
		}
	}
	if !hasTokens {
		rep.Unavailable = append(rep.Unavailable, "token_rate: provider usage unavailable in SSE done event")
	}
	encoded, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(encoded))
	if *output != "" {
		if err := os.MkdirAll(filepath.Dir(*output), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := os.WriteFile(*output, append(encoded, '\n'), 0644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
}

func run(client *http.Client, base, message string, index int, slow time.Duration, disconnect bool) (s sample) {
	s = sample{Index: index}
	start := time.Now()
	defer func() { s.DurationMS = float64(time.Since(start).Microseconds()) / 1000 }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session, err := createSession(ctx, client, base)
	if err != nil {
		s.Error = err.Error()
		return s
	}
	start = time.Now() // Measure the SSE request, excluding session setup.
	body, _ := json.Marshal(map[string]string{"session_id": session, "message": message})
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(base, "/")+"/api/v1/chat/stream", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if token := os.Getenv("AGENTGO_ACCESS_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		s.Error = err.Error()
		return s
	}
	defer resp.Body.Close()
	s.HTTPStatus = resp.StatusCode
	s.TraceID = resp.Header.Get("X-Trace-ID")
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		s.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, raw)
		return s
	}
	first := time.Time{}
	var reportedTokens int
	err = readEvents(resp.Body, func(name string, data []byte) error {
		switch name {
		case "answer_delta":
			var payload struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(data, &payload) != nil {
				return fmt.Errorf("invalid answer_delta JSON")
			}
			if payload.Message != "" {
				if first.IsZero() {
					first = time.Now()
					v := float64(first.Sub(start).Microseconds()) / 1000
					s.TTFTMS = &v
				}
				s.DeltaRunes += len([]rune(payload.Message))
				if disconnect {
					s.Disconnected = true
					cancel()
					return io.EOF
				}
			}
		case "error":
			s.Error = string(data)
			return fmt.Errorf("SSE error event")
		case "done":
			s.Done = true
			var payload struct {
				Usage *struct {
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(data, &payload) == nil && payload.Usage != nil {
				reportedTokens = payload.Usage.CompletionTokens
			}
		}
		if slow > 0 {
			time.Sleep(slow)
		}
		return nil
	})
	if err != nil && !s.Disconnected && s.Error == "" {
		s.Error = err.Error()
	}
	if s.Done && !first.IsZero() {
		elapsed := time.Since(first).Seconds()
		if elapsed > 0 {
			r := float64(s.DeltaRunes) / elapsed
			s.DeltaRunesPerSecond = &r
		}
		if reportedTokens > 0 {
			t := float64(reportedTokens) / time.Since(start).Seconds()
			s.TokenRate = &t
		}
	}
	if !s.Done && !s.Disconnected && s.Error == "" {
		s.Error = "stream ended without done"
	}
	return s
}

func createSession(ctx context.Context, client *http.Client, base string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(base, "/")+"/api/v1/sessions", strings.NewReader(`{"user":{"display_name":"sse-benchmark"}}`))
	req.Header.Set("Content-Type", "application/json")
	if token := os.Getenv("AGENTGO_ACCESS_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var data struct {
		Code int `json:"code"`
		Data struct {
			SessionID string `json:"session_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data); err != nil {
		return "", err
	}
	if resp.StatusCode != 200 || data.Code != 0 || data.Data.SessionID == "" {
		return "", fmt.Errorf("session HTTP %d code %d", resp.StatusCode, data.Code)
	}
	return data.Data.SessionID, nil
}

func readEvents(r io.Reader, onEvent func(string, []byte) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	var name string
	var data []string
	dispatch := func() error {
		if name == "" && len(data) == 0 {
			return nil
		}
		err := onEvent(name, []byte(strings.Join(data, "\n")))
		name = ""
		data = nil
		return err
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return dispatch()
}
func percentile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
	i := int(float64(len(values)-1)*q + .999999)
	return values[i]
}
