// Package etl provides format-aware document extraction.
package etl

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type DocumentType string

const (
	DocTypeText     DocumentType = "text"
	DocTypeMarkdown DocumentType = "markdown"
	DocTypeHTML     DocumentType = "html"
	DocTypePDF      DocumentType = "pdf"
	DocTypeDOCX     DocumentType = "docx"
	DocTypeXLSX     DocumentType = "xlsx"
)

type SourceDocument struct {
	Filename string
	Type     DocumentType
	Data     []byte
}

type ParsedDocument struct {
	Title    string
	Content  string
	Elements []DocumentElement
	Metadata map[string]any
}

type DocumentElement struct {
	Type     string
	Content  string
	Level    int
	Metadata map[string]any
}

type Parser interface {
	Parse(context.Context, SourceDocument) (*ParsedDocument, error)
}

type DefaultParser struct {
	doclingURL string
	client     *http.Client
}

func NewDefaultParser() *DefaultParser { return NewDocumentParser("", 10*time.Minute) }

func NewDocumentParser(doclingURL string, timeout time.Duration) *DefaultParser {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	return &DefaultParser{doclingURL: doclingURL, client: &http.Client{Timeout: timeout}}
}

func (p *DefaultParser) Healthy(ctx context.Context) bool {
	if p.doclingURL == "" {
		return false
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.doclingURL, "/")+"/health", nil)
	resp, err := p.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (p *DefaultParser) Parse(ctx context.Context, source SourceDocument) (*ParsedDocument, error) {
	if len(source.Data) == 0 {
		return nil, fmt.Errorf("文档内容为空")
	}
	switch source.Type {
	case DocTypePDF:
		return p.parsePDF(ctx, source)
	case DocTypeDOCX:
		return parseDOCX(source)
	case DocTypeXLSX:
		return parseXLSX(source)
	case DocTypeMarkdown:
		return parseMarkdown(source.Data), nil
	case DocTypeHTML:
		return parseHTML(source.Data), nil
	case DocTypeText:
		return parseText(source.Data), nil
	default:
		return nil, fmt.Errorf("不支持的文档类型 %q", source.Type)
	}
}

func parseMarkdown(data []byte) *ParsedDocument {
	content := string(data)
	doc := &ParsedDocument{Content: content, Metadata: map[string]any{}}
	var path []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		level := markdownHeadingLevel(trimmed)
		if level > 0 {
			title := strings.TrimSpace(trimmed[level:])
			if doc.Title == "" {
				doc.Title = title
			}
			if level <= len(path) {
				path = path[:level-1]
			}
			path = append(path, title)
			doc.Elements = append(doc.Elements, element("heading", title, level, path))
			continue
		}
		doc.Elements = append(doc.Elements, element("paragraph", trimmed, 0, path))
	}
	return doc
}

func markdownHeadingLevel(line string) int {
	n := 0
	for n < len(line) && n < 6 && line[n] == '#' {
		n++
	}
	if n == 0 || n == len(line) || line[n] != ' ' {
		return 0
	}
	return n
}

func parseHTML(data []byte) *ParsedDocument {
	content := string(data)
	for strings.Contains(content, "<") {
		start, end := strings.Index(content, "<"), strings.Index(content, ">")
		if end <= start {
			break
		}
		content = content[:start] + " " + content[end+1:]
	}
	return parseText([]byte(strings.Join(strings.Fields(content), " ")))
}

func parseText(data []byte) *ParsedDocument {
	content := strings.TrimSpace(string(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	lines := strings.SplitN(content, "\n", 2)
	doc := &ParsedDocument{Title: strings.TrimSpace(lines[0]), Content: content, Metadata: map[string]any{}}
	for _, para := range strings.Split(content, "\n\n") {
		if para = strings.TrimSpace(para); para != "" {
			doc.Elements = append(doc.Elements, element("paragraph", para, 0, nil))
		}
	}
	return doc
}

func element(kind, content string, level int, path []string) DocumentElement {
	metadata := map[string]any{"element_type": kind}
	if len(path) > 0 {
		metadata["section_path"] = append([]string(nil), path...)
	}
	return DocumentElement{Type: kind, Content: strings.TrimSpace(content), Level: level, Metadata: metadata}
}
