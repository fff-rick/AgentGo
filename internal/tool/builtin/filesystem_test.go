package builtin

import (
	"context"
	"encoding/json"
	"fmt"
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
	workspace, dir := testFileWorkspace(t, "user-1")
	path := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(path, []byte("first\nneedle here\nlast\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspect := NewFileInspectTool(workspace)

	read, err := inspect.Execute(tool.WithScope(context.Background(), tool.Scope{UserID: "user-1"}), mustJSON(t, map[string]any{"action": "read", "path": path, "start_line": 2, "end_line": 2}))
	if err != nil || !read.Success || !strings.Contains(read.Output, `"sha256"`) || !strings.Contains(read.Output, "needle here") {
		t.Fatalf("read=%+v err=%v", read, err)
	}
	search, _ := inspect.Execute(tool.WithScope(context.Background(), tool.Scope{UserID: "user-1"}), mustJSON(t, map[string]any{"action": "search", "path": dir, "query": "needle", "pattern": "*.md"}))
	if !search.Success || !strings.Contains(search.Output, `"line":2`) {
		t.Fatalf("search=%+v", search)
	}
	canceled, cancel := context.WithCancel(tool.WithScope(context.Background(), tool.Scope{UserID: "user-1"}))
	cancel()
	canceledSearch, _ := inspect.Execute(canceled, mustJSON(t, map[string]any{"action": "search", "path": dir, "query": "needle"}))
	if canceledSearch.Success || !strings.Contains(canceledSearch.Error, "context canceled") {
		t.Fatalf("canceled search=%+v", canceledSearch)
	}
	list, _ := inspect.Execute(tool.WithScope(context.Background(), tool.Scope{UserID: "user-1"}), mustJSON(t, map[string]any{"action": "list", "path": dir}))
	if !list.Success || !strings.Contains(list.Output, "notes.md") {
		t.Fatalf("list=%+v", list)
	}
	badPath, _ := inspect.Execute(tool.WithScope(context.Background(), tool.Scope{UserID: "user-1"}), `{"action":"read","path":"../relative.md"}`)
	if badPath.Success || !strings.Contains(badPath.Error, "超出") {
		t.Fatalf("relative path accepted: %+v", badPath)
	}
	notFile, _ := inspect.Execute(tool.WithScope(context.Background(), tool.Scope{UserID: "user-1"}), mustJSON(t, map[string]any{"action": "read", "path": dir}))
	if notFile.Success || !strings.Contains(notFile.Error, "普通文件") {
		t.Fatalf("directory accepted: %+v", notFile)
	}
	binary := filepath.Join(dir, "binary")
	if err := os.WriteFile(binary, []byte{'a', 0, 'b'}, 0o600); err != nil {
		t.Fatal(err)
	}
	badText, _ := inspect.Execute(tool.WithScope(context.Background(), tool.Scope{UserID: "user-1"}), mustJSON(t, map[string]any{"action": "read", "path": binary}))
	if badText.Success || !strings.Contains(badText.Error, "UTF-8") {
		t.Fatalf("binary accepted: %+v", badText)
	}
	large := filepath.Join(dir, "large.txt")
	if err := os.WriteFile(large, make([]byte, maxFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	tooLarge, _ := inspect.Execute(tool.WithScope(context.Background(), tool.Scope{UserID: "user-1"}), mustJSON(t, map[string]any{"action": "read", "path": large}))
	if tooLarge.Success || !strings.Contains(tooLarge.Error, "1 MiB") {
		t.Fatalf("large file accepted: %+v", tooLarge)
	}
}

func TestFileEditRequiresApprovalAndAppliesOnce(t *testing.T) {
	workspace, dir := testFileWorkspace(t, "user-1")
	path := filepath.Join(dir, "code.go")
	if err := os.WriteFile(path, []byte("package demo\n\nconst value = \"old\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	store := newProposalMemoryStore()
	preview := NewFileEditPreviewTool(store, workspace)
	apply := NewFileEditApplyTool(store, workspace)
	baseScope := tool.Scope{UserID: "user-1", SessionID: "session-1"}
	_, hash, err := readTestFile(t, path)
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
	workspace, dir := testFileWorkspace(t, "user")
	path := filepath.Join(dir, "text.md")
	if err := os.WriteFile(path, []byte("same same\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newProposalMemoryStore()
	preview := NewFileEditPreviewTool(store, workspace)
	apply := NewFileEditApplyTool(store, workspace)
	scope := tool.Scope{UserID: "user", SessionID: "session"}
	_, hash, _ := readTestFile(t, path)
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
	workspace, dir := testFileWorkspace(t, "user")
	path := filepath.Join(dir, "text.md")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newProposalMemoryStore()
	preview := NewFileEditPreviewTool(store, workspace)
	apply := NewFileEditApplyTool(store, workspace)
	scope := tool.Scope{UserID: "user", SessionID: "session"}
	_, hash, _ := readTestFile(t, path)
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

func testFileWorkspace(t *testing.T, userID string) (*FileWorkspace, string) {
	t.Helper()
	workspace, err := NewFileWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { workspace.Close() })
	access, err := workspace.open(tool.WithScope(context.Background(), tool.Scope{UserID: userID}))
	if err != nil {
		t.Fatal(err)
	}
	defer access.root.Close()
	path, err := access.resolve(".")
	if err != nil {
		t.Fatal(err)
	}
	return workspace, access.absolute(path)
}

func TestFileWorkspaceRejectsEscapesAndMissingIdentity(t *testing.T) {
	workspace, dir := testFileWorkspace(t, "owner")
	ctx := tool.WithScope(context.Background(), tool.Scope{UserID: "owner", SessionID: "session"})
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("private needle"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(workspace.root, hashText("other"))
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "secret.txt"), []byte("private needle"), 0600); err != nil {
		t.Fatal(err)
	}
	inspect := NewFileInspectTool(workspace)
	preview := NewFileEditPreviewTool(newProposalMemoryStore(), workspace)
	for _, path := range []string{secret, "../" + hashText("other") + "/secret.txt", filepath.Join(dir, "escape", "secret.txt"), filepath.Join(dir, "leak.txt"), other} {
		for _, action := range []string{"read", "list", "search"} {
			result, err := inspect.Execute(ctx, mustJSON(t, map[string]any{"action": action, "path": path, "query": "needle", "workspace_root": outside}))
			if err != nil || result.Success {
				t.Fatalf("%s %s result=%+v err=%v", action, path, result, err)
			}
		}
		result, err := preview.Execute(ctx, mustJSON(t, previewParams{Path: path, ExpectedSHA256: hashText("private needle"), Replacements: []textReplacement{{OldText: "private", NewText: "public"}}}))
		if err != nil || result.Success {
			t.Fatalf("preview %s result=%+v err=%v", path, result, err)
		}
	}
	result, err := inspect.Execute(ctx, `{"action":"search","path":".","query":"needle"}`)
	if err != nil || !result.Success || strings.Contains(result.Output, "private") {
		t.Fatalf("recursive search leaked: %+v %v", result, err)
	}
	result, _ = inspect.Execute(context.Background(), `{"action":"list","path":"."}`)
	if result.Success {
		t.Fatal("missing identity accepted")
	}
}

func TestFileEditRechecksScopeAndSymlinksAtApply(t *testing.T) {
	for _, replaceParent := range []bool{false, true} {
		t.Run(fmt.Sprint(replaceParent), func(t *testing.T) {
			workspace, dir := testFileWorkspace(t, "owner")
			parent := filepath.Join(dir, "docs")
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "note.txt")
			if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			store := newProposalMemoryStore()
			scope := tool.Scope{UserID: "owner", SessionID: "session"}
			id := previewFile(t, store, workspace, scope, path, "new")
			outside := t.TempDir()
			secret := filepath.Join(outside, "note.txt")
			if err := os.WriteFile(secret, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			if replaceParent {
				if err := os.Rename(parent, parent+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, parent); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(secret, path); err != nil {
					t.Fatal(err)
				}
			}
			scope.ApprovedProposals = []string{id}
			result, err := NewFileEditApplyTool(store, workspace).Execute(tool.WithScope(context.Background(), scope), mustJSON(t, map[string]string{"proposal_id": id}))
			if err != nil || result.Success {
				t.Fatalf("symlink apply=%+v err=%v", result, err)
			}
			content, err := os.ReadFile(secret)
			if err != nil || string(content) != "old" {
				t.Fatalf("outside changed: %q %v", content, err)
			}
		})
	}
}

func TestDistinctFileProposalsSerializeHashCheckAndReplace(t *testing.T) {
	workspace, dir := testFileWorkspace(t, "owner")
	path := filepath.Join(dir, "note.txt")
	store := newProposalMemoryStore()
	scope := tool.Scope{UserID: "owner", SessionID: "session"}
	for round := 0; round < 20; round++ {
		if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
		ids := []string{previewFile(t, store, workspace, scope, path, "first"), previewFile(t, store, workspace, scope, path, "second")}
		scope.ApprovedProposals = ids
		ctx := tool.WithScope(context.Background(), scope)
		start := make(chan struct{})
		results := make(chan *tool.ToolResult, 2)
		for _, id := range ids {
			input := mustJSON(t, map[string]string{"proposal_id": id})
			go func() {
				<-start
				// Separate instances must still share the path lock.
				result, err := NewFileEditApplyTool(store, workspace).Execute(ctx, input)
				if err != nil {
					results <- nil
					return
				}
				results <- result
			}()
		}
		close(start)
		successes, stale := 0, 0
		for range 2 {
			result := <-results
			if result == nil {
				t.Fatal("apply failed")
			}
			if result.Success {
				successes++
			} else if strings.Contains(result.Error, "预览后发生变化") {
				stale++
			}
		}
		if successes != 1 || stale != 1 {
			t.Fatalf("success=%d stale=%d", successes, stale)
		}
	}
}

func previewFile(t *testing.T, store proposalStore, workspace *FileWorkspace, scope tool.Scope, path, newText string) string {
	t.Helper()
	result, err := NewFileEditPreviewTool(store, workspace).Execute(tool.WithScope(context.Background(), scope), mustJSON(t, previewParams{Path: path, ExpectedSHA256: hashText("old"), Replacements: []textReplacement{{OldText: "old", NewText: newText}}}))
	if err != nil || !result.Success {
		t.Fatalf("preview=%+v err=%v", result, err)
	}
	var value struct {
		ProposalID string `json:"proposal_id"`
	}
	if err := json.Unmarshal([]byte(result.Output), &value); err != nil {
		t.Fatal(err)
	}
	return value.ProposalID
}

func readTestFile(t *testing.T, path string) (string, string, error) {
	t.Helper()
	content, err := os.ReadFile(path)
	return string(content), hashBytes(content), err
}

func TestFileAccessRejectsSymlinkSwapAfterPathCheck(t *testing.T) {
	workspace, dir := testFileWorkspace(t, "owner")
	parent := filepath.Join(dir, "docs")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "note.txt"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	secret := filepath.Join(outside, "note.txt")
	if err := os.WriteFile(secret, []byte("outside-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	access, err := workspace.open(tool.WithScope(context.Background(), tool.Scope{UserID: "owner"}))
	if err != nil {
		t.Fatal(err)
	}
	defer access.root.Close()
	path, err := access.resolve("docs/note.txt")
	if err != nil {
		t.Fatal(err)
	}
	// Deterministically replace an already checked parent before the actual I/O.
	if err := os.Rename(parent, parent+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatal(err)
	}
	if _, _, err := access.readTextFile(path); err == nil {
		t.Fatal("read escaped after path check")
	}
	if _, err := listDirectory(access, "docs"); err == nil {
		t.Fatal("list escaped after path check")
	}
	if err := access.atomicWriteFile(path, []byte("overwrite"), 0600); err == nil {
		t.Fatal("write escaped after path check")
	}
	output, err := searchFiles(context.Background(), access, ".", "outside-secret", "*", 100)
	if err != nil || len(output["matches"].([]map[string]any)) != 0 {
		t.Fatalf("search escaped: %+v %v", output, err)
	}
	content, err := os.ReadFile(secret)
	if err != nil || string(content) != "outside-secret" {
		t.Fatalf("outside changed: %q %v", content, err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary file escaped: %v %v", entries, err)
	}
}

func TestFileWorkspaceRejectsUserRootSymlink(t *testing.T) {
	workspace, dir := testFileWorkspace(t, "owner")
	other := filepath.Join(workspace.root, hashText("other"))
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, dir); err != nil {
		t.Fatal(err)
	}
	result, err := NewFileInspectTool(workspace).Execute(tool.WithScope(context.Background(), tool.Scope{UserID: "owner"}), `{"action":"list","path":"."}`)
	if err != nil || result.Success {
		t.Fatalf("user root symlink accepted: %+v %v", result, err)
	}
}
