package etl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strings"
)

const maxDoclingResponse = 64 << 20

func (p *DefaultParser) parsePDF(ctx context.Context, source SourceDocument) (*ParsedDocument, error) {
	if p.doclingURL == "" {
		return nil, fmt.Errorf("Docling 服务未配置")
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files"; filename="%s"`, strings.ReplaceAll(filepath.Base(source.Filename), `"`, "")))
	header.Set("Content-Type", "application/pdf")
	part, err := w.CreatePart(header)
	if err == nil {
		_, err = part.Write(source.Data)
	}
	fields := map[string]string{
		"from_formats": "pdf", "to_formats": "json", "image_export_mode": "placeholder",
		"do_ocr": "true", "force_ocr": "false", "do_table_structure": "true",
		"table_mode": "accurate", "include_images": "false", "abort_on_error": "true",
	}
	for name, value := range fields {
		if err == nil {
			err = w.WriteField(name, value)
		}
	}
	if closeErr := w.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, fmt.Errorf("构造 Docling 请求失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.doclingURL, "/")+"/v1/convert/file", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("调用 Docling 失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDoclingResponse+1))
	if err != nil {
		return nil, fmt.Errorf("读取 Docling 响应失败: %w", err)
	}
	if len(raw) > maxDoclingResponse {
		return nil, fmt.Errorf("Docling 响应超过 64 MiB")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Docling HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("解析 Docling JSON 失败: %w", err)
	}
	document := object(envelope["document"])
	root := object(document["json_content"])
	if root == nil {
		if encoded := stringValue(document["json_content"]); encoded != "" {
			_ = json.Unmarshal([]byte(encoded), &root)
		}
	}
	if root == nil {
		root = object(envelope["json_content"])
	}
	if root == nil {
		root = document
	}
	if root == nil {
		return nil, fmt.Errorf("Docling 响应缺少 document.json_content")
	}
	return doclingDocument(root)
}

func doclingDocument(root map[string]any) (*ParsedDocument, error) {
	doc := &ParsedDocument{Metadata: map[string]any{"parser": "docling"}}
	items := map[string]map[string]any{}
	for _, collection := range []string{"texts", "tables", "pictures", "groups"} {
		for _, raw := range array(root[collection]) {
			item := object(raw)
			if item == nil {
				continue
			}
			if ref := stringValue(item["self_ref"]); ref != "" {
				items[ref] = item
			}
		}
	}
	seen := map[string]bool{}
	var sectionPath []string
	lastText := ""
	var visit func(map[string]any)
	visit = func(item map[string]any) {
		ref := stringValue(item["self_ref"])
		if ref != "" && seen[ref] {
			return
		}
		seen[ref] = true
		kind := normalizeDoclingLabel(stringValue(item["label"]))
		if kind != "group" {
			content := stringValue(item["text"])
			if kind == "table" {
				content = renderDoclingTable(object(item["data"]))
			}
			if kind == "picture" && content == "" {
				content = pictureCaption(item, items)
			}
			contextFallback := false
			if kind == "picture" && content == "" && lastText != "" {
				content = lastText
				contextFallback = true
			}
			level := intValue(item["level"])
			meta := doclingMetadata(kind, item)
			if contextFallback {
				meta["content_source"] = "preceding_text"
			}
			if kind == "heading" {
				if level <= 0 {
					level = 1
				}
				if level <= len(sectionPath) {
					sectionPath = sectionPath[:level-1]
				}
				sectionPath = append(sectionPath, content)
			}
			if len(sectionPath) > 0 {
				meta["section_path"] = append([]string(nil), sectionPath...)
			}
			if content != "" || kind == "picture" {
				doc.Elements = append(doc.Elements, DocumentElement{Type: kind, Content: content, Level: level, Metadata: meta})
				if doc.Title == "" && kind == "heading" {
					doc.Title = content
				}
				if kind != "picture" && content != "" {
					lastText = content
				}
			}
		}
		for _, child := range array(item["children"]) {
			if childItem := resolveDoclingRef(child, items); childItem != nil {
				visit(childItem)
			}
		}
	}
	if body := object(root["body"]); body != nil {
		for _, child := range array(body["children"]) {
			if item := resolveDoclingRef(child, items); item != nil {
				visit(item)
			}
		}
	}
	if len(doc.Elements) == 0 {
		for _, collection := range []string{"texts", "tables", "pictures"} {
			for _, raw := range array(root[collection]) {
				if item := object(raw); item != nil {
					visit(item)
				}
			}
		}
	}
	var contents []string
	for _, item := range doc.Elements {
		if item.Content != "" {
			contents = append(contents, item.Content)
		}
	}
	doc.Content = strings.Join(contents, "\n\n")
	if len(doc.Elements) == 0 {
		return nil, fmt.Errorf("Docling 未提取到可索引内容")
	}
	return doc, nil
}

func normalizeDoclingLabel(label string) string {
	switch label {
	case "title", "section_header":
		return "heading"
	case "list_item":
		return "list"
	case "table":
		return "table"
	case "picture":
		return "picture"
	case "group", "list", "ordered_list":
		return "group"
	default:
		return "paragraph"
	}
}

func doclingMetadata(kind string, item map[string]any) map[string]any {
	meta := map[string]any{"element_type": kind}
	if provs := array(item["prov"]); len(provs) > 0 {
		prov := object(provs[0])
		if page := intValue(prov["page_no"]); page > 0 {
			meta["page"] = page
		}
		if bbox := object(prov["bbox"]); bbox != nil {
			meta["bbox"] = []any{bbox["l"], bbox["t"], bbox["r"], bbox["b"]}
		}
	}
	return meta
}

func renderDoclingTable(data map[string]any) string {
	if data == nil {
		return ""
	}
	rows, cols := intValue(data["num_rows"]), intValue(data["num_cols"])
	if rows <= 0 || cols <= 0 {
		return ""
	}
	grid := make([][]string, rows)
	for i := range grid {
		grid[i] = make([]string, cols)
	}
	for _, raw := range array(data["table_cells"]) {
		cell := object(raw)
		if cell == nil {
			continue
		}
		r, c := intValue(cell["start_row_offset_idx"]), intValue(cell["start_col_offset_idx"])
		if r >= 0 && r < rows && c >= 0 && c < cols {
			grid[r][c] = stringValue(cell["text"])
		}
	}
	lines := make([]string, 0, rows)
	for _, row := range grid {
		lines = append(lines, strings.Join(row, " | "))
	}
	return strings.Join(lines, "\n")
}

func pictureCaption(item map[string]any, items map[string]map[string]any) string {
	var captions []string
	for _, raw := range array(item["captions"]) {
		if caption := resolveDoclingRef(raw, items); caption != nil {
			if text := stringValue(caption["text"]); text != "" {
				captions = append(captions, text)
			}
		}
	}
	return strings.Join(captions, " ")
}

func resolveDoclingRef(raw any, items map[string]map[string]any) map[string]any {
	item := object(raw)
	if item == nil {
		return nil
	}
	if ref := stringValue(item["$ref"]); ref != "" {
		return items[ref]
	}
	return item
}

func object(value any) map[string]any { result, _ := value.(map[string]any); return result }
func array(value any) []any           { result, _ := value.([]any); return result }
func stringValue(value any) string    { result, _ := value.(string); return strings.TrimSpace(result) }
func intValue(value any) int {
	if number, ok := value.(float64); ok {
		return int(number)
	}
	return 0
}
