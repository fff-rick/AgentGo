package skill

import (
	"fmt"
	"html"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	maxNameLength        = 64
	maxDescriptionLength = 1024
	maxContentRunes      = 20000
)

var validName = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

type Skill struct {
	Name        string
	Description string
	Content     string
	Path        string
}

type Diagnostic struct {
	Path    string
	Message string
}

type Registry struct {
	byName map[string]Skill
	names  []string
}

type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

func Load(paths []string) (*Registry, []Diagnostic) {
	registry := &Registry{byName: make(map[string]Skill)}
	var diagnostics []Diagnostic
	for _, root := range paths {
		files, err := skillFiles(root)
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{Path: root, Message: err.Error()})
			continue
		}
		for _, path := range files {
			candidate, err := parse(path)
			if err != nil {
				diagnostics = append(diagnostics, Diagnostic{Path: path, Message: err.Error()})
				continue
			}
			if existing, ok := registry.byName[candidate.Name]; ok {
				diagnostics = append(diagnostics, Diagnostic{Path: path, Message: fmt.Sprintf("Skill 名称 %q 与 %s 重复", candidate.Name, existing.Path)})
				continue
			}
			registry.byName[candidate.Name] = candidate
			registry.names = append(registry.names, candidate.Name)
		}
	}
	sort.Strings(registry.names)
	return registry, diagnostics
}

func skillFiles(root string) ([]string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("Skill 路径不是目录")
	}
	var paths []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && path != root && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if !entry.IsDir() && entry.Name() == "SKILL.md" {
			paths = append(paths, path)
		}
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

func parse(path string) (Skill, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, err
	}
	normalized := strings.ReplaceAll(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\r", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return Skill{}, fmt.Errorf("缺少 YAML frontmatter")
	}
	end := strings.Index(normalized[4:], "\n---")
	if end < 0 {
		return Skill{}, fmt.Errorf("YAML frontmatter 未闭合")
	}
	end += 4
	var metadata frontmatter
	if err := yaml.Unmarshal([]byte(normalized[4:end]), &metadata); err != nil {
		return Skill{}, fmt.Errorf("解析 YAML frontmatter: %w", err)
	}
	metadata.Name = strings.TrimSpace(metadata.Name)
	metadata.Description = strings.TrimSpace(metadata.Description)
	content := strings.TrimSpace(normalized[end+4:])
	switch {
	case metadata.Name == "":
		return Skill{}, fmt.Errorf("name 不能为空")
	case len(metadata.Name) > maxNameLength || !validName.MatchString(metadata.Name):
		return Skill{}, fmt.Errorf("name 必须为不超过 %d 字符的小写字母、数字和单连字符", maxNameLength)
	case metadata.Description == "":
		return Skill{}, fmt.Errorf("description 不能为空")
	case utf8.RuneCountInString(metadata.Description) > maxDescriptionLength:
		return Skill{}, fmt.Errorf("description 超过 %d 字符", maxDescriptionLength)
	case content == "":
		return Skill{}, fmt.Errorf("Skill 正文不能为空")
	case utf8.RuneCountInString(content) > maxContentRunes:
		return Skill{}, fmt.Errorf("Skill 正文超过 %d 字符", maxContentRunes)
	}
	return Skill{Name: metadata.Name, Description: metadata.Description, Content: content, Path: path}, nil
}

func (r *Registry) Count() int {
	if r == nil {
		return 0
	}
	return len(r.names)
}

func (r *Registry) Get(name string) (Skill, bool) {
	if r == nil {
		return Skill{}, false
	}
	candidate, ok := r.byName[name]
	return candidate, ok
}

func (r *Registry) Validate(names []string) error {
	for _, name := range names {
		if _, ok := r.Get(name); !ok {
			return fmt.Errorf("Skill %q 未注册", name)
		}
	}
	return nil
}

func (r *Registry) CatalogPrompt() string {
	if r == nil || len(r.names) == 0 {
		return ""
	}
	var result strings.Builder
	result.WriteString("可用 Skill 提供特定任务的操作指令。任务匹配描述时，先调用 load_skill 加载完整指令。\n<available_skills>")
	for _, name := range r.names {
		candidate := r.byName[name]
		fmt.Fprintf(&result, "\n  <skill><name>%s</name><description>%s</description></skill>", html.EscapeString(candidate.Name), html.EscapeString(candidate.Description))
	}
	result.WriteString("\n</available_skills>")
	return result.String()
}

func (r *Registry) Instructions(names []string) ([]string, error) {
	result := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		candidate, ok := r.Get(name)
		if !ok {
			return nil, fmt.Errorf("Skill %q 未注册", name)
		}
		seen[name] = struct{}{}
		result = append(result, Format(candidate))
	}
	return result, nil
}

func Format(candidate Skill) string {
	return fmt.Sprintf("<skill name=\"%s\">\n%s\n</skill>", html.EscapeString(candidate.Name), candidate.Content)
}
