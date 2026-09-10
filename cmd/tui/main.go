package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/google/uuid"

	"github.com/enterprise/ai-agent-go/internal/observe"
)

type streamEvent struct {
	name string
	data []byte
}
type streamClosed struct{}

type model struct {
	baseURL string
	session string
	input   string
	logs    []string
	width   int
	height  int
	busy    bool
	events  <-chan streamEvent
	cancel  context.CancelFunc
}

var (
	titleStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	userStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("69"))
	statusStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	reasoningStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	toolStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))
	answerStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	errorStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

func main() {
	baseURL := strings.TrimRight(os.Getenv("AGENTGO_API_URL"), "/")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}
	m := &model{baseURL: baseURL, session: uuid.NewString()}
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func (m *model) Init() tea.Cmd { return nil }

func (m *model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "/quit":
			m.stop()
			return m, tea.Quit
		case "esc":
			if m.busy {
				m.stop()
				m.logs = append(m.logs, statusStyle.Render("■ 已取消当前请求"))
			}
		case "enter":
			if !m.busy {
				return m.submit()
			}
		case "backspace":
			if !m.busy && m.input != "" {
				runes := []rune(m.input)
				m.input = string(runes[:len(runes)-1])
			}
		default:
			if !m.busy && msg.Type == tea.KeyRunes {
				m.input += string(msg.Runes)
			}
		}
	case streamEvent:
		m.renderEvent(msg)
		return m, waitEvent(m.events)
	case streamClosed:
		m.busy = false
		m.events = nil
		m.cancel = nil
	}
	return m, nil
}

func (m *model) submit() (tea.Model, tea.Cmd) {
	query := strings.TrimSpace(m.input)
	if query == "" {
		return m, nil
	}
	m.input = ""
	if query == "/clear" {
		m.logs = nil
		m.session = uuid.NewString()
		return m, nil
	}
	if strings.HasPrefix(query, "/import ") {
		path := strings.Trim(strings.TrimSpace(strings.TrimPrefix(query, "/import ")), `"'`)
		if path == "" {
			m.logs = append(m.logs, errorStyle.Render("错误: 请指定 Markdown 文件路径"))
			return m, nil
		}
		m.logs = append(m.logs, userStyle.Render("导入: ")+path)
		ctx, cancel := context.WithCancel(context.Background())
		events := make(chan streamEvent, 8)
		m.cancel, m.events, m.busy = cancel, events, true
		go importMarkdown(ctx, m.baseURL, path, events)
		return m, waitEvent(events)
	}
	m.logs = append(m.logs, userStyle.Render("你: ")+query)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan streamEvent, 32)
	m.cancel, m.events, m.busy = cancel, events, true
	go stream(ctx, m.baseURL, m.session, query, events)
	return m, waitEvent(events)
}

func (m *model) stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.busy = false
	m.cancel = nil
}

func waitEvent(events <-chan streamEvent) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-events
		if !ok {
			return streamClosed{}
		}
		return event
	}
}

func (m *model) renderEvent(raw streamEvent) {
	if raw.name == "error" {
		var payload map[string]string
		_ = json.Unmarshal(raw.data, &payload)
		m.logs = append(m.logs, errorStyle.Render("错误: "+payload["error"]))
		return
	}
	if raw.name == "done" || raw.name == "session" {
		return
	}
	var event observe.Event
	if err := json.Unmarshal(raw.data, &event); err != nil {
		m.logs = append(m.logs, errorStyle.Render("无法解析事件: "+err.Error()))
		return
	}
	switch event.Type {
	case observe.TypeStatus:
		m.logs = append(m.logs, statusStyle.Render("◌ ["+event.Stage+"] "+event.Message))
	case observe.TypeIntent:
		m.logs = append(m.logs, statusStyle.Render(fmt.Sprintf("◆ 意图: %s (%.0f%%)", event.Intent, event.Confidence*100)))
	case observe.TypeReasoning:
		m.logs = append(m.logs, reasoningStyle.Render("推理: ")+event.Message)
	case observe.TypeToolCall:
		if event.Tool != nil {
			m.logs = append(m.logs, toolStyle.Render("工具调用: "+event.Tool.ToolName+"("+event.Tool.Input+")"))
		}
	case observe.TypeToolResult:
		if event.Tool != nil {
			m.logs = append(m.logs, toolStyle.Render(fmt.Sprintf("工具结果: %s (%dms)\n%s", event.Tool.ToolName, event.Tool.Duration, event.Message)))
		}
	case observe.TypeReferences:
		m.logs = append(m.logs, statusStyle.Render(fmt.Sprintf("▣ RAG 命中 %d 条引用", len(event.References))))
		for _, ref := range event.References {
			m.logs = append(m.logs, statusStyle.Render(fmt.Sprintf("  - %s [%s] score=%.4f", ref.Title, ref.DocID, ref.Score)))
		}
	case observe.TypeAnswer:
		m.logs = append(m.logs, answerStyle.Render("回答: ")+event.Message)
	}
}

func (m *model) View() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("AgentGo · 可观察 TUI"))
	b.WriteString("\n")
	b.WriteString(statusStyle.Render("服务: " + m.baseURL + "  会话: " + m.session))
	b.WriteString("\n" + strings.Repeat("─", max(24, min(m.width, 80))) + "\n")
	start := 0
	visible := max(4, m.height-8)
	if len(m.logs) > visible {
		start = len(m.logs) - visible
	}
	b.WriteString(strings.Join(m.logs[start:], "\n\n"))
	b.WriteString("\n\n")
	if m.busy {
		b.WriteString(statusStyle.Render("Agent 运行中… Esc 取消"))
	} else {
		b.WriteString(userStyle.Render("> ") + m.input)
	}
	b.WriteString("\n" + statusStyle.Render("Enter 发送 · /import <file.md> 导入 · /clear 清空 · Ctrl+C 退出"))
	return b.String()
}

func stream(ctx context.Context, baseURL, session, query string, events chan<- streamEvent) {
	defer close(events)
	body, _ := json.Marshal(map[string]any{"session_id": session, "message": query, "stream": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/chat/stream", bytes.NewReader(body))
	if err != nil {
		sendError(events, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			sendError(events, err)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		sendError(events, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(message))))
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	name := "message"
	var data strings.Builder
	flush := func() {
		if data.Len() > 0 {
			events <- streamEvent{name: name, data: []byte(data.String())}
		}
		name = "message"
		data.Reset()
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	flush()
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		sendError(events, err)
	}
}

func sendError(events chan<- streamEvent, err error) {
	data, _ := json.Marshal(map[string]string{"error": err.Error()})
	events <- streamEvent{name: "error", data: data}
}

func importMarkdown(ctx context.Context, baseURL, path string, events chan<- streamEvent) {
	defer close(events)
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".md" && ext != ".markdown" {
		sendError(events, fmt.Errorf("仅支持 .md 或 .markdown 文件"))
		return
	}
	file, err := os.Open(path)
	if err != nil {
		sendError(events, fmt.Errorf("打开文件失败: %w", err))
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		sendError(events, fmt.Errorf("读取文件信息失败: %w", err))
		return
	}
	if info.Size() > 10<<20 {
		sendError(events, fmt.Errorf("Markdown 文件不能超过 10 MiB"))
		return
	}
	sendObserve(events, observe.Event{Type: observe.TypeStatus, Stage: "import", Message: "正在上传并向量化 " + filepath.Base(path)})

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filepath.Base(path))
	if err == nil {
		_, err = io.Copy(part, file)
	}
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		sendError(events, fmt.Errorf("构造上传请求失败: %w", err))
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/documents/import", &body)
	if err != nil {
		sendError(events, err)
		return
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			sendError(events, err)
		}
		return
	}
	defer resp.Body.Close()
	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			DocID      string `json:"doc_id"`
			Title      string `json:"title"`
			Status     string `json:"status"`
			ChunkCount int    `json:"chunk_count"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		sendError(events, fmt.Errorf("解析导入响应失败: %w", err))
		return
	}
	if resp.StatusCode != http.StatusOK || result.Code != 0 {
		sendError(events, fmt.Errorf("导入失败: %s", result.Message))
		return
	}
	sendObserve(events, observe.Event{
		Type: observe.TypeStatus, Stage: "import",
		Message: fmt.Sprintf("导入完成：%s，%d 个分块，doc_id=%s", result.Data.Title, result.Data.ChunkCount, result.Data.DocID),
	})
}

func sendObserve(events chan<- streamEvent, event observe.Event) {
	data, _ := json.Marshal(event)
	events <- streamEvent{name: event.Type, data: data}
}
