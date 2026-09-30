package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enterprise/ai-agent-go/internal/tool"
)

type proposalMemoryStore struct {
	mu     sync.Mutex
	values map[string]string
}

func newProposalMemoryStore() *proposalMemoryStore {
	return &proposalMemoryStore{values: make(map[string]string)}
}
func (s *proposalMemoryStore) Get(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[key], nil
}
func (s *proposalMemoryStore) Set(_ context.Context, key, value string, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value
	return nil
}
func (s *proposalMemoryStore) CompareAndSet(_ context.Context, key, expected, value string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values[key] != expected {
		return false, nil
	}
	s.values[key] = value
	return true, nil
}
func (s *proposalMemoryStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, key)
	return nil
}

func TestFileInspectReadSearchAndValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(path, []byte("first\nneedle here\nlast\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspect := NewFileInspectTool()

	read, err := inspect.Execute(context.Background(), mustJSON(t, map[string]any{"action": "read", "path": path, "start_line": 2, "end_line": 2}))
	if err != nil || !read.Success || !strings.Contains(read.Output, `"sha256"`) || !strings.Contains(read.Output, "needle here") {
		t.Fatalf("read=%+v err=%v", read, err)
	}
	search, _ := inspect.Execute(context.Background(), mustJSON(t, map[string]any{"action": "search", "path": dir, "query": "needle", "pattern": "*.md"}))
	if !search.Success || !strings.Contains(search.Output, `"line":2`) {
		t.Fatalf("search=%+v", search)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	canceledSearch, _ := inspect.Execute(canceled, mustJSON(t, map[string]any{"action": "search", "path": dir, "query": "needle"}))
	if canceledSearch.Success || !strings.Contains(canceledSearch.Error, "context canceled") {
		t.Fatalf("canceled search=%+v", canceledSearch)
	}
	list, _ := inspect.Execute(context.Background(), mustJSON(t, map[string]any{"action": "list", "path": dir}))
	if !list.Success || !strings.Contains(list.Output, "notes.md") {
		t.Fatalf("list=%+v", list)
	}
	badPath, _ := inspect.Execute(context.Background(), `{"action":"read","path":"relative.md"}`)
	if badPath.Success || !strings.Contains(badPath.Error, "绝对路径") {
		t.Fatalf("relative path accepted: %+v", badPath)
	}
	notFile, _ := inspect.Execute(context.Background(), mustJSON(t, map[string]any{"action": "read", "path": dir}))
	if notFile.Success || !strings.Contains(notFile.Error, "普通文件") {
		t.Fatalf("directory accepted: %+v", notFile)
	}
	binary := filepath.Join(dir, "binary")
	if err := os.WriteFile(binary, []byte{'a', 0, 'b'}, 0o600); err != nil {
		t.Fatal(err)
	}
	badText, _ := inspect.Execute(context.Background(), mustJSON(t, map[string]any{"action": "read", "path": binary}))
	if badText.Success || !strings.Contains(badText.Error, "UTF-8") {
		t.Fatalf("binary accepted: %+v", badText)
	}
	large := filepath.Join(dir, "large.txt")
	if err := os.WriteFile(large, make([]byte, maxFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	tooLarge, _ := inspect.Execute(context.Background(), mustJSON(t, map[string]any{"action": "read", "path": large}))
	if tooLarge.Success || !strings.Contains(tooLarge.Error, "1 MiB") {
		t.Fatalf("large file accepted: %+v", tooLarge)
	}
}

func TestFileEditRequiresApprovalAndAppliesOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "code.go")
	if err := os.WriteFile(path, []byte("package demo\n\nconst value = \"old\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	store := newProposalMemoryStore()
	preview := NewFileEditPreviewTool(store)
	apply := NewFileEditApplyTool(store)
	baseScope := tool.Scope{UserID: "user-1", SessionID: "session-1"}
	_, hash, err := readTextFile(path)
	if err != nil {
		t.Fatal(err)
	}
	previewResult, err := preview.Execute(tool.WithScope(context.Background(), baseScope), mustJSON(t, previewParams{
		Path: path, ExpectedSHA256: hash, Replacements: []textReplacement{
			{OldText: "package demo", NewText: "package updated"},
			{OldText: `"old"`, NewText: `"new"`},
		},
	}))
	if err != nil || !previewResult.Success {
		t.Fatalf("preview=%+v err=%v", previewResult, err)
	}
	var output struct {
		ProposalID string `json:"proposal_id"`
	}
	if err := json.Unmarshal([]byte(previewResult.Output), &output); err != nil {
		t.Fatal(err)
	}
	if content, _ := os.ReadFile(path); strings.Contains(string(content), `"new"`) {
		t.Fatal("preview modified the file")
	}
	input := mustJSON(t, map[string]string{"proposal_id": output.ProposalID})
	unapproved, _ := apply.Execute(tool.WithScope(context.Background(), baseScope), input)
	if unapproved.Success || !strings.Contains(unapproved.Error, "未在本次请求中获批") {
		t.Fatalf("unapproved apply=%+v", unapproved)
	}
	wrongUser := baseScope
	wrongUser.UserID = "user-2"
	wrongUser.ApprovedProposals = []string{output.ProposalID}
	denied, _ := apply.Execute(tool.WithScope(context.Background(), wrongUser), input)
	if denied.Success || !strings.Contains(denied.Error, "不属于") {
		t.Fatalf("cross-user apply=%+v", denied)
	}
	wrongSession := baseScope
	wrongSession.SessionID = "session-2"
	wrongSession.ApprovedProposals = []string{output.ProposalID}
	denied, _ = apply.Execute(tool.WithScope(context.Background(), wrongSession), input)
	if denied.Success || !strings.Contains(denied.Error, "不属于") {
		t.Fatalf("cross-session apply=%+v", denied)
	}
	approved := baseScope
	approved.ApprovedProposals = []string{output.ProposalID}
	applied, err := apply.Execute(tool.WithScope(context.Background(), approved), input)
	if err != nil || !applied.Success {
		t.Fatalf("apply=%+v err=%v", applied, err)
	}
	content, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	if !strings.Contains(string(content), "package updated") || !strings.Contains(string(content), `"new"`) || info.Mode().Perm() != 0o640 {
		t.Fatalf("content=%q mode=%o", content, info.Mode().Perm())
	}
	again, _ := apply.Execute(tool.WithScope(context.Background(), approved), input)
	if again.Success || !strings.Contains(again.Error, "不存在") {
		t.Fatalf("proposal reused: %+v", again)
	}
}

func TestFileEditRejectsAmbiguousAndStaleChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "text.md")
	if err := os.WriteFile(path, []byte("same same\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newProposalMemoryStore()
	preview := NewFileEditPreviewTool(store)
	apply := NewFileEditApplyTool(store)
	scope := tool.Scope{UserID: "user", SessionID: "session"}
	_, hash, _ := readTextFile(path)
	ambiguous, _ := preview.Execute(tool.WithScope(context.Background(), scope), mustJSON(t, previewParams{
		Path: path, ExpectedSHA256: hash, Replacements: []textReplacement{{OldText: "same", NewText: "new"}},
	}))
	if ambiguous.Success || !strings.Contains(ambiguous.Error, "匹配 2 次") {
		t.Fatalf("ambiguous replacement=%+v", ambiguous)
	}
	tooLarge, _ := preview.Execute(tool.WithScope(context.Background(), scope), mustJSON(t, previewParams{
		Path: path, ExpectedSHA256: hash, Replacements: []textReplacement{{OldText: "same same", NewText: strings.Repeat("x", maxFileBytes+1)}},
	}))
	if tooLarge.Success || !strings.Contains(tooLarge.Error, "超过 1 MiB") {
		t.Fatalf("oversized edit=%+v", tooLarge)
	}
	valid, _ := preview.Execute(tool.WithScope(context.Background(), scope), mustJSON(t, previewParams{
		Path: path, ExpectedSHA256: hash, Replacements: []textReplacement{{OldText: "same same", NewText: "new"}},
	}))
	var output struct {
		ProposalID string `json:"proposal_id"`
	}
	_ = json.Unmarshal([]byte(valid.Output), &output)
	if err := os.WriteFile(path, []byte("external change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scope.ApprovedProposals = []string{output.ProposalID}
	stale, _ := apply.Execute(tool.WithScope(context.Background(), scope), mustJSON(t, map[string]string{"proposal_id": output.ProposalID}))
	if stale.Success || !strings.Contains(stale.Error, "预览后发生变化") {
		t.Fatalf("stale apply=%+v", stale)
	}
}

func TestFileEditConcurrentApplyOnlySucceedsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "text.md")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newProposalMemoryStore()
	preview := NewFileEditPreviewTool(store)
	apply := NewFileEditApplyTool(store)
	scope := tool.Scope{UserID: "user", SessionID: "session"}
	_, hash, _ := readTextFile(path)
	result, _ := preview.Execute(tool.WithScope(context.Background(), scope), mustJSON(t, previewParams{
		Path: path, ExpectedSHA256: hash, Replacements: []textReplacement{{OldText: "old", NewText: "new"}},
	}))
	var output struct {
		ProposalID string `json:"proposal_id"`
	}
	_ = json.Unmarshal([]byte(result.Output), &output)
	scope.ApprovedProposals = []string{output.ProposalID}
	input := mustJSON(t, map[string]string{"proposal_id": output.ProposalID})
	results := make(chan bool, 2)
	for range 2 {
		go func() {
			applied, _ := apply.Execute(tool.WithScope(context.Background(), scope), input)
			results <- applied.Success
		}()
	}
	successes := 0
	for range 2 {
		if <-results {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful applies=%d, want 1", successes)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
