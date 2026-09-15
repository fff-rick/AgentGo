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
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/enterprise/ai-agent-go/internal/observe"
)

type streamEvent struct {
	name string
	data []byte
}
type streamClosed struct{}
type spinnerTickMsg time.Time

type model struct {
	baseURL            string
	session            string
	input              string
	logs               []string
	width              int
	height             int
	busy               bool
	events             <-chan streamEvent
	cancel             context.CancelFunc
	scrollOffset       int
	spinnerFrame       int
	startedAt          time.Time
	answerIndex        int
	answerText         string
	answerStreaming    bool
	reasoningIndex     int
	reasoningText      string
	reasoningStreaming bool
}

var (
	titleStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	userStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("69"))
	statusStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	reasoningStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	toolStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))
	answerStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	errorStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	spinnerFrames  = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
)

func main() {
	baseURL := strings.TrimRight(os.Getenv("AGENTGO_API_URL"), "/")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}
	userID := strings.TrimSpace(os.Getenv("AGENTGO_USER_ID"))
	if userID == "" {
		userID = "local-user"
	}
	session, err := createSession(context.Background(), baseURL, userID)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	m := &model{baseURL: baseURL, session: session}
	if _, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func (m *model) Init() tea.Cmd { return nil }

func (m *model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.scrollOffset = min(m.scrollOffset, m.maxScrollOffset())
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
		case "up":
			m.scrollBy(1)
		case "down":
			m.scrollBy(-1)
		case "pgup":
			m.scrollBy(max(1, m.viewportHeight()-1))
		case "pgdown":
			m.scrollBy(-max(1, m.viewportHeight()-1))
		case "home":
			m.scrollOffset = m.maxScrollOffset()
		case "end":
			m.scrollOffset = 0
		case "backspace":
			if !m.busy && m.input != "" {
				runes := []rune(m.input)
				m.input = string(runes[:len(runes)-1])
			}
		default:
			if !m.busy {
				switch msg.Type {
				case tea.KeyRunes:
					m.input += string(msg.Runes)
				case tea.KeySpace:
					m.input += " "
				}
			}
		}
	case tea.MouseMsg:
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			m.scrollBy(3)
		case tea.MouseButtonWheelDown:
			m.scrollBy(-3)
		}
	case spinnerTickMsg:
		if m.busy {
			m.spinnerFrame = (m.spinnerFrame + 1) % len(spinnerFrames)
			return m, tickSpinner()
		}
	case streamEvent:
		oldLineCount := len(m.historyLines())
		m.renderEvent(msg)
		if m.scrollOffset > 0 {
			m.scrollOffset += max(0, len(m.historyLines())-oldLineCount)
			m.scrollOffset = min(m.scrollOffset, m.maxScrollOffset())
		}
		return m, waitEvent(m.events)
	case streamClosed:
		m.busy = false
		m.events = nil
		m.cancel = nil
		m.answerStreaming = false
		m.reasoningStreaming = false
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
		userID := strings.TrimSpace(os.Getenv("AGENTGO_USER_ID"))
		if userID == "" {
			userID = "local-user"
		}
		session, err := createSession(context.Background(), m.baseURL, userID)
		if err != nil {
			m.logs = append(m.logs, errorStyle.Render("创建会话失败: "+err.Error()))
			return m, nil
		}
		m.logs, m.session = nil, session
		return m, nil
	}
	if strings.HasPrefix(query, "/import ") {
		path := strings.Trim(strings.TrimSpace(strings.TrimPrefix(query, "/import ")), `"'`)
		if path == "" {
			m.logs = append(m.logs, errorStyle.Render("错误: 请指定文档路径"))
			return m, nil
		}
		m.logs = append(m.logs, userStyle.Render("导入: ")+path)
		ctx, cancel := context.WithCancel(context.Background())
		events := make(chan streamEvent, 8)
		m.beginRequest(cancel, events)
		go importDocument(ctx, m.baseURL, path, events)
		return m, tea.Batch(waitEvent(events), tickSpinner())
	}
	mode := ""
	if query == "/plan" {
		m.logs = append(m.logs, errorStyle.Render("错误: 请在 /plan 后指定任务"))
		return m, nil
	}
	if strings.HasPrefix(query, "/plan ") {
		query = strings.TrimSpace(strings.TrimPrefix(query, "/plan "))
		if query == "" {
			m.logs = append(m.logs, errorStyle.Render("错误: 请在 /plan 后指定任务"))
			return m, nil
		}
		mode = "planner"
	}
	label := "你: "
	if mode == "planner" {
		label = "规划任务: "
	}
	m.logs = append(m.logs, userStyle.Render(label)+query)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan streamEvent, 32)
	m.beginRequest(cancel, events)
	go stream(ctx, m.baseURL, m.session, query, mode, events)
	return m, tea.Batch(waitEvent(events), tickSpinner())
}

func (m *model) beginRequest(cancel context.CancelFunc, events <-chan streamEvent) {
	m.cancel, m.events, m.busy = cancel, events, true
	m.startedAt = time.Now()
	m.spinnerFrame = 0
	m.scrollOffset = 0
	m.answerText, m.reasoningText = "", ""
	m.answerStreaming, m.reasoningStreaming = false, false
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

func tickSpinner() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg { return spinnerTickMsg(t) })
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
	case observe.TypeReasoningDelta:
		m.reasoningText += event.Message
		line := reasoningStyle.Render("推理: ") + m.reasoningText
		if !m.reasoningStreaming {
			m.reasoningIndex = len(m.logs)
			m.logs = append(m.logs, line)
			m.reasoningStreaming = true
		} else {
			m.logs[m.reasoningIndex] = line
		}
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
			locator := ""
			if value := fmt.Sprint(ref.Metadata["range"]); value != "<nil>" && value != "" {
				locator = " · " + value
			} else if value := fmt.Sprint(ref.Metadata["page"]); value != "<nil>" && value != "" {
				locator = " · page " + value
			}
			m.logs = append(m.logs, statusStyle.Render(fmt.Sprintf("  - %s [%s]%s score=%.4f", ref.Title, ref.DocID, locator, ref.Score)))
		}
	case observe.TypeAnswer:
		line := answerStyle.Render("回答: ") + event.Message
		if m.answerStreaming {
			m.logs[m.answerIndex] = line
			m.answerText = event.Message
		} else {
			m.logs = append(m.logs, line)
		}
	case observe.TypeAnswerDelta:
		m.answerText += event.Message
		line := answerStyle.Render("回答: ") + m.answerText
		if !m.answerStreaming {
			m.answerIndex = len(m.logs)
			m.logs = append(m.logs, line)
			m.answerStreaming = true
		} else {
			m.logs[m.answerIndex] = line
		}
	}
}

func (m *model) View() string {
	var b strings.Builder
	width := m.viewWidth()
	b.WriteString(titleStyle.Render("AgentGo · 可观察 TUI"))
	b.WriteString("\n")
	b.WriteString(ansi.Truncate(statusStyle.Render("服务: "+m.baseURL+"  会话: "+m.session), width, "…"))
	b.WriteString("\n" + strings.Repeat("─", min(width, 80)) + "\n")
	lines := m.historyLines()
	visible := m.viewportHeight()
	end := max(0, len(lines)-m.scrollOffset)
	start := max(0, end-visible)
	b.WriteString(strings.Join(lines[start:end], "\n"))
	b.WriteString("\n\n")
	if m.busy {
		elapsed := time.Since(m.startedAt).Round(time.Second)
		b.WriteString(statusStyle.Render(fmt.Sprintf("%s Agent 运行中 · %s · Esc 取消", spinnerFrames[m.spinnerFrame], elapsed)))
	} else {
		b.WriteString(userStyle.Render("> ") + m.input)
	}
	scrollHint := ""
	if m.scrollOffset > 0 {
		scrollHint = fmt.Sprintf(" · 距最新 %d 行", m.scrollOffset)
	}
	help := statusStyle.Render("Enter 发送 · ↑↓/PgUp/PgDn/鼠标滚轮 查看历史" + scrollHint + " · /plan 规划 · /import 导入 · /clear 清空 · Ctrl+C 退出")
	b.WriteString("\n" + ansi.Truncate(help, width, "…"))
	return b.String()
}

func (m *model) viewportHeight() int {
	return max(3, m.height-7)
}

func (m *model) historyLines() []string {
	width := m.viewWidth()
	lines := make([]string, 0, len(m.logs)*2)
	for i, log := range m.logs {
		wrapped := ansi.Hardwrap(log, width, false)
		lines = append(lines, strings.Split(wrapped, "\n")...)
		if i < len(m.logs)-1 {
			lines = append(lines, "")
		}
	}
	return lines
}

func (m *model) viewWidth() int {
	if m.width <= 0 {
		return 80
	}
	return max(10, m.width)
}

func (m *model) maxScrollOffset() int {
	return max(0, len(m.historyLines())-m.viewportHeight())
}

func (m *model) scrollBy(delta int) {
	m.scrollOffset = max(0, min(m.maxScrollOffset(), m.scrollOffset+delta))
}

func stream(ctx context.Context, baseURL, session, query, mode string, events chan<- streamEvent) {
	defer close(events)
	payload := map[string]any{"session_id": session, "message": query, "stream": true}
	if mode != "" {
		payload["options"] = map[string]string{"mode": mode}
	}
	body, _ := json.Marshal(payload)
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

func createSession(ctx context.Context, baseURL, userID string) (string, error) {
	body, _ := json.Marshal(map[string]any{"user": map[string]string{"user_id": userID, "display_name": userID}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/sessions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var envelope struct {
		Data struct {
			SessionID string `json:"session_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope); err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK || envelope.Data.SessionID == "" {
		return "", fmt.Errorf("创建会话失败: HTTP %d", resp.StatusCode)
	}
	return envelope.Data.SessionID, nil
}

func sendError(events chan<- streamEvent, err error) {
	data, _ := json.Marshal(map[string]string{"error": err.Error()})
	events <- streamEvent{name: "error", data: data}
}

func importDocument(ctx context.Context, baseURL, path string, events chan<- streamEvent) {
	defer close(events)
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	ext := strings.ToLower(filepath.Ext(path))
	allowed := map[string]bool{".md": true, ".markdown": true, ".pdf": true, ".docx": true, ".xlsx": true}
	if !allowed[ext] {
		sendError(events, fmt.Errorf("仅支持 Markdown、PDF、DOCX 或 XLSX 文件"))
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
	limit := int64(50 << 20)
	if ext == ".md" || ext == ".markdown" {
		limit = 10 << 20
	}
	if info.Size() > limit {
		sendError(events, fmt.Errorf("文件不能超过 %d MiB", limit>>20))
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
	if (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted) || result.Code != 0 {
		sendError(events, fmt.Errorf("导入失败: %s", result.Message))
		return
	}
	if result.Data.Status == "processing" {
		sendObserve(events, observe.Event{Type: observe.TypeStatus, Stage: "import", Message: "文件已接收，正在解析 " + filepath.Base(path)})
		for result.Data.Status == "processing" {
			timer := time.NewTimer(500 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			statusReq, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/documents/"+result.Data.DocID, nil)
			if err != nil {
				sendError(events, err)
				return
			}
			statusResp, err := http.DefaultClient.Do(statusReq)
			if err != nil {
				if ctx.Err() == nil {
					sendError(events, err)
				}
				return
			}
			var statusResult struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				Data    struct {
					DocID      string `json:"doc_id"`
					Title      string `json:"title"`
					Status     string `json:"status"`
					ChunkCount int    `json:"chunk_count"`
					Error      string `json:"error"`
				} `json:"data"`
			}
			err = json.NewDecoder(statusResp.Body).Decode(&statusResult)
			statusResp.Body.Close()
			if err != nil || statusResp.StatusCode != http.StatusOK || statusResult.Code != 0 {
				if err == nil {
					err = fmt.Errorf("查询导入状态失败: %s", statusResult.Message)
				}
				sendError(events, err)
				return
			}
			result.Data.DocID, result.Data.Title, result.Data.Status, result.Data.ChunkCount = statusResult.Data.DocID, statusResult.Data.Title, statusResult.Data.Status, statusResult.Data.ChunkCount
			if statusResult.Data.Status == "failed" {
				sendError(events, fmt.Errorf("导入失败: %s", statusResult.Data.Error))
				return
			}
		}
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
