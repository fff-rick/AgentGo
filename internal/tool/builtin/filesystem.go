package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/pmezard/go-difflib/difflib"

	"github.com/enterprise/ai-agent-go/internal/tool"
)

const (
	maxFileBytes = 1 << 20
	proposalTTL  = 15 * time.Minute
	proposalKey  = "agentgo:v1:file-proposal:"
)

type proposalStore interface {
	Get(context.Context, string) (string, error)
	Set(context.Context, string, string, time.Duration) error
	CompareAndSet(context.Context, string, string, string, time.Duration) (bool, error)
	Delete(context.Context, string) error
}

type FileInspectTool struct{}

func NewFileInspectTool() *FileInspectTool { return &FileInspectTool{} }
func (*FileInspectTool) Name() string      { return "file_inspect" }
func (*FileInspectTool) Description() string {
	return "浏览、搜索或分段读取服务进程可访问的 UTF-8 本地文本文件；路径必须是绝对路径"
}
func (*FileInspectTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"action":      map[string]interface{}{"type": "string", "enum": []string{"list", "search", "read"}},
			"path":        map[string]interface{}{"type": "string", "description": "服务进程可见的绝对路径"},
			"query":       map[string]interface{}{"type": "string", "description": "search 使用的固定文本，不支持正则"},
			"pattern":     map[string]interface{}{"type": "string", "description": "search 文件名 glob，默认 *"},
			"start_line":  map[string]interface{}{"type": "integer", "minimum": 1},
			"end_line":    map[string]interface{}{"type": "integer", "minimum": 1},
			"max_results": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 200},
		},
		"required": []string{"action", "path"},
	}
}

type fileInspectParams struct {
	Action     string `json:"action"`
	Path       string `json:"path"`
	Query      string `json:"query"`
	Pattern    string `json:"pattern"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
	MaxResults int    `json:"max_results"`
}

func (*FileInspectTool) Execute(ctx context.Context, input string) (*tool.ToolResult, error) {
	var params fileInspectParams
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return tool.NewErrorResult("参数解析失败: " + err.Error()), nil
	}
	path, err := canonicalPath(params.Path)
	if err != nil {
		return tool.NewErrorResult(err.Error()), nil
	}
	var output any
	switch params.Action {
	case "list":
		output, err = listDirectory(path)
	case "search":
		output, err = searchFiles(ctx, path, params.Query, params.Pattern, params.MaxResults)
	case "read":
		output, err = readFileLines(path, params.StartLine, params.EndLine)
	default:
		err = fmt.Errorf("action 只支持 list、search 或 read")
	}
	if err != nil {
		return tool.NewErrorResult(err.Error()), nil
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, err
	}
	return tool.NewSuccessResult(string(encoded)), nil
}

func listDirectory(path string) (map[string]any, error) {
	directory, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(501)
	if err != nil && err != io.EOF {
		return nil, err
	}
	truncated := len(entries) > 500
	if truncated {
		entries = entries[:500]
	}
	items := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		kind := "file"
		if entry.IsDir() {
			kind = "directory"
		} else if entry.Type()&os.ModeSymlink != 0 {
			kind = "symlink"
		} else if entry.Type() != 0 {
			kind = "other"
		}
		items = append(items, map[string]any{"name": entry.Name(), "path": filepath.Join(path, entry.Name()), "type": kind})
	}
	return map[string]any{"path": path, "entries": items, "truncated": truncated}, nil
}

func searchFiles(ctx context.Context, root, query, pattern string, limit int) (map[string]any, error) {
	if query == "" {
		return nil, fmt.Errorf("query 不能为空")
	}
	if pattern == "" {
		pattern = "*"
	}
	if _, err := filepath.Match(pattern, "sample"); err != nil {
		return nil, fmt.Errorf("pattern 无效: %w", err)
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 200 {
		limit = 200
	}
	matches := make([]map[string]any, 0)
	stopped := false
	errStop := fmt.Errorf("result limit reached")
	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			if path == root {
				return err
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		matched, _ := filepath.Match(pattern, entry.Name())
		if !matched {
			return nil
		}
		content, _, readErr := readTextFile(path)
		if readErr != nil {
			return nil
		}
		for index, line := range strings.Split(content, "\n") {
			if strings.Contains(line, query) {
				matches = append(matches, map[string]any{"path": path, "line": index + 1, "content": line})
				if len(matches) >= limit {
					stopped = true
					return errStop
				}
			}
		}
		return nil
	})
	if walkErr != nil && walkErr != errStop {
		return nil, walkErr
	}
	return map[string]any{"root": root, "matches": matches, "truncated": stopped}, nil
}

func readFileLines(path string, start, end int) (map[string]any, error) {
	content, hash, err := readTextFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(content, "\n")
	if start <= 0 {
		start = 1
	}
	if end <= 0 || end > start+199 {
		end = start + 199
	}
	if end > len(lines) {
		end = len(lines)
	}
	if start > len(lines) || end < start {
		return nil, fmt.Errorf("行范围超出文件，共 %d 行", len(lines))
	}
	selected := make([]map[string]any, 0, end-start+1)
	for index := start; index <= end; index++ {
		selected = append(selected, map[string]any{"line": index, "content": lines[index-1]})
	}
	return map[string]any{"path": path, "sha256": hash, "total_lines": len(lines), "lines": selected}, nil
}

type FileEditPreviewTool struct{ store proposalStore }

func NewFileEditPreviewTool(store proposalStore) *FileEditPreviewTool {
	return &FileEditPreviewTool{store: store}
}
func (*FileEditPreviewTool) Name() string { return "file_edit_preview" }
func (*FileEditPreviewTool) Description() string {
	return "预览本地文本文件的精确替换，返回 diff 和 proposal_id；不会写入文件，用户须另行批准"
}
func (*FileEditPreviewTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path":            map[string]interface{}{"type": "string", "description": "绝对文件路径"},
			"expected_sha256": map[string]interface{}{"type": "string", "description": "file_inspect read 返回的 SHA-256"},
			"replacements": map[string]interface{}{
				"type": "array", "minItems": 1,
				"items": map[string]interface{}{"type": "object", "properties": map[string]interface{}{
					"old_text": map[string]interface{}{"type": "string"}, "new_text": map[string]interface{}{"type": "string"},
				}, "required": []string{"old_text", "new_text"}},
			},
		},
		"required": []string{"path", "expected_sha256", "replacements"},
	}
}

type textReplacement struct {
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}
type previewParams struct {
	Path           string            `json:"path"`
	ExpectedSHA256 string            `json:"expected_sha256"`
	Replacements   []textReplacement `json:"replacements"`
}
type fileProposal struct {
	ID             string    `json:"id"`
	UserID         string    `json:"user_id"`
	SessionID      string    `json:"session_id"`
	Path           string    `json:"path"`
	OriginalSHA256 string    `json:"original_sha256"`
	NewSHA256      string    `json:"new_sha256"`
	NewContent     string    `json:"new_content"`
	Mode           uint32    `json:"mode"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
}

func (t *FileEditPreviewTool) Execute(ctx context.Context, input string) (*tool.ToolResult, error) {
	var params previewParams
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return tool.NewErrorResult("参数解析失败: " + err.Error()), nil
	}
	scope := tool.ScopeFromContext(ctx)
	if scope.UserID == "" || scope.SessionID == "" {
		return tool.NewErrorResult("文件修改缺少用户或会话上下文"), nil
	}
	path, err := canonicalPath(params.Path)
	if err != nil {
		return tool.NewErrorResult(err.Error()), nil
	}
	content, hash, err := readTextFile(path)
	if err != nil {
		return tool.NewErrorResult(err.Error()), nil
	}
	if params.ExpectedSHA256 == "" || !strings.EqualFold(params.ExpectedSHA256, hash) {
		return tool.NewErrorResult("文件已变化或 expected_sha256 不匹配，请重新读取"), nil
	}
	if len(params.Replacements) == 0 {
		return tool.NewErrorResult("replacements 不能为空"), nil
	}
	updated := content
	for index, replacement := range params.Replacements {
		if replacement.OldText == "" {
			return tool.NewErrorResult(fmt.Sprintf("第 %d 个 old_text 不能为空", index+1)), nil
		}
		if !utf8.ValidString(replacement.NewText) {
			return tool.NewErrorResult(fmt.Sprintf("第 %d 个 new_text 不是 UTF-8", index+1)), nil
		}
		if count := strings.Count(updated, replacement.OldText); count != 1 {
			return tool.NewErrorResult(fmt.Sprintf("第 %d 个 old_text 匹配 %d 次，必须恰好一次", index+1, count)), nil
		}
		updated = strings.Replace(updated, replacement.OldText, replacement.NewText, 1)
		if len(updated) > maxFileBytes {
			return tool.NewErrorResult("改写结果超过 1 MiB"), nil
		}
	}
	if updated == content {
		return tool.NewErrorResult("修改前后内容相同"), nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return tool.NewErrorResult(err.Error()), nil
	}
	proposal := fileProposal{ID: uuid.NewString(), UserID: scope.UserID, SessionID: scope.SessionID, Path: path,
		OriginalSHA256: hash, NewSHA256: hashText(updated), NewContent: updated, Mode: uint32(info.Mode().Perm()), Status: "pending", CreatedAt: time.Now().UTC()}
	encoded, err := json.Marshal(proposal)
	if err != nil {
		return nil, err
	}
	if err := t.store.Set(ctx, proposalKey+proposal.ID, string(encoded), proposalTTL); err != nil {
		return nil, err
	}
	diff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{A: strings.SplitAfter(content, "\n"), B: strings.SplitAfter(updated, "\n"), FromFile: path, ToFile: path + " (proposed)", Context: 3})
	if err != nil {
		return nil, err
	}
	output, _ := json.Marshal(struct {
		ProposalID string `json:"proposal_id"`
		Path       string `json:"path"`
		NewSHA256  string `json:"new_sha256"`
		ExpiresIn  int    `json:"expires_in_seconds"`
		Diff       string `json:"diff"`
	}{proposal.ID, path, proposal.NewSHA256, int(proposalTTL.Seconds()), diff})
	return tool.NewSuccessResult(string(output)), nil
}

type FileEditApplyTool struct{ store proposalStore }

func NewFileEditApplyTool(store proposalStore) *FileEditApplyTool {
	return &FileEditApplyTool{store: store}
}
func (*FileEditApplyTool) Name() string { return tool.FileEditApplyName }
func (*FileEditApplyTool) Description() string {
	return "提交用户在当前请求中明确批准的文件修改 proposal；只能使用 approved_proposals 中的 ID"
}
func (*FileEditApplyTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{
		"proposal_id": map[string]interface{}{"type": "string"},
	}, "required": []string{"proposal_id"}}
}

func (t *FileEditApplyTool) Execute(ctx context.Context, input string) (*tool.ToolResult, error) {
	var params struct {
		ProposalID string `json:"proposal_id"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return tool.NewErrorResult("参数解析失败: " + err.Error()), nil
	}
	if _, err := uuid.Parse(params.ProposalID); err != nil {
		return tool.NewErrorResult("proposal_id 无效"), nil
	}
	scope := tool.ScopeFromContext(ctx)
	if !contains(scope.ApprovedProposals, params.ProposalID) {
		return tool.NewErrorResult("该 proposal 未在本次请求中获批"), nil
	}
	key := proposalKey + params.ProposalID
	raw, err := t.store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return tool.NewErrorResult("proposal 不存在、已过期或已使用"), nil
	}
	var proposal fileProposal
	if err := json.Unmarshal([]byte(raw), &proposal); err != nil {
		return nil, err
	}
	if proposal.Status != "pending" || proposal.UserID != scope.UserID || proposal.SessionID != scope.SessionID {
		return tool.NewErrorResult("proposal 不属于当前用户和会话，或已在处理"), nil
	}
	proposal.Status = "applying"
	applying, _ := json.Marshal(proposal)
	claimed, err := t.store.CompareAndSet(ctx, key, raw, string(applying), proposalTTL)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return tool.NewErrorResult("proposal 已被其他请求处理"), nil
	}
	_, hash, err := readTextFile(proposal.Path)
	if err != nil || hash != proposal.OriginalSHA256 {
		_ = t.store.Delete(ctx, key)
		return tool.NewErrorResult("文件在预览后发生变化，请重新读取并预览"), nil
	}
	if err := atomicWriteFile(proposal.Path, []byte(proposal.NewContent), os.FileMode(proposal.Mode)); err != nil {
		_, _ = t.store.CompareAndSet(ctx, key, string(applying), raw, proposalTTL)
		return tool.NewErrorResult("写入失败: " + err.Error()), nil
	}
	_ = t.store.Delete(ctx, key)
	output, _ := json.Marshal(map[string]any{"proposal_id": proposal.ID, "path": proposal.Path, "sha256": proposal.NewSHA256, "status": "applied"})
	return tool.NewSuccessResult(string(output)), nil
}

func canonicalPath(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("path 必须是绝对路径")
	}
	clean := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func readTextFile(path string) (string, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", "", err
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("仅支持普通文件")
	}
	if info.Size() > maxFileBytes {
		return "", "", fmt.Errorf("文件超过 1 MiB")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	if len(content) > maxFileBytes {
		return "", "", fmt.Errorf("文件超过 1 MiB")
	}
	if !utf8.Valid(content) || strings.IndexByte(string(content), 0) >= 0 {
		return "", "", fmt.Errorf("仅支持 UTF-8 文本文件")
	}
	return string(content), hashBytes(content), nil
}

func hashText(value string) string { return hashBytes([]byte(value)) }
func hashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func atomicWriteFile(path string, content []byte, mode os.FileMode) (writeErr error) {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".agentgo-edit-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() {
		_ = temp.Close()
		if writeErr != nil {
			_ = os.Remove(tempName)
		}
	}()
	if err = temp.Chmod(mode.Perm()); err != nil {
		return err
	}
	if _, err = temp.Write(content); err != nil {
		return err
	}
	if err = temp.Sync(); err != nil {
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tempName, path); err != nil {
		return err
	}
	if directory, openErr := os.Open(dir); openErr == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}
