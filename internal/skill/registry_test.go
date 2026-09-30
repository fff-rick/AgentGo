package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDiscoversValidSkillsAndKeepsFirstDuplicate(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "b", "SKILL.md"), "review", "second", "second body")
	writeSkill(t, filepath.Join(root, "a", "SKILL.md"), "review", "first", "first body")
	writeSkill(t, filepath.Join(root, "nested", "deploy", "SKILL.md"), "deploy", "deploy app", "deploy body")
	if err := os.WriteFile(filepath.Join(root, "ignored.md"), []byte("---\nname: ignored\ndescription: ignored\n---\nbody"), 0o600); err != nil {
		t.Fatal(err)
	}

	registry, diagnostics := Load([]string{root, filepath.Join(root, "missing")})
	if registry.Count() != 2 || len(diagnostics) != 2 {
		t.Fatalf("count=%d diagnostics=%+v", registry.Count(), diagnostics)
	}
	review, ok := registry.Get("review")
	if !ok || review.Content != "first body" {
		t.Fatalf("review=%+v", review)
	}
	catalog := registry.CatalogPrompt()
	if !strings.Contains(catalog, "deploy app") || strings.Contains(catalog, "deploy body") {
		t.Fatalf("catalog=%s", catalog)
	}
}

func TestLoadRejectsInvalidMetadataAndOversizedContent(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "bad-name", "SKILL.md"), "Bad_Name", "bad", "body")
	writeSkill(t, filepath.Join(root, "missing-description", "SKILL.md"), "missing-description", "", "body")
	writeSkill(t, filepath.Join(root, "too-long", "SKILL.md"), "too-long", "long", strings.Repeat("x", maxContentRunes+1))
	if err := os.MkdirAll(filepath.Join(root, "yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "yaml", "SKILL.md"), []byte("---\nname: [bad\n---\nbody"), 0o600); err != nil {
		t.Fatal(err)
	}

	registry, diagnostics := Load([]string{root})
	if registry.Count() != 0 || len(diagnostics) != 4 {
		t.Fatalf("count=%d diagnostics=%+v", registry.Count(), diagnostics)
	}
}

func writeSkill(t *testing.T, path, name, description, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
