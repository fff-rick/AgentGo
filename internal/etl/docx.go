package etl

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

const maxOfficeExpandedBytes = 250 << 20

type officeArchive map[string][]byte

func openOfficeArchive(data []byte) (officeArchive, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("无效的 OOXML 文件: %w", err)
	}
	files := officeArchive{}
	var total uint64
	for _, file := range r.File {
		total += file.UncompressedSize64
		if total > maxOfficeExpandedBytes {
			return nil, fmt.Errorf("OOXML 解压后大小超过 250 MiB")
		}
		if strings.Contains(filepath.ToSlash(file.Name), "../") {
			return nil, fmt.Errorf("OOXML 包含非法路径")
		}
		rc, err := file.Open()
		if err != nil {
			return nil, err
		}
		content, readErr := io.ReadAll(io.LimitReader(rc, maxOfficeExpandedBytes+1))
		closeErr := rc.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		files[filepath.ToSlash(file.Name)] = content
	}
	return files, nil
}

type xmlRaw struct {
	Inner string `xml:",innerxml"`
}

func parseDOCX(source SourceDocument) (*ParsedDocument, error) {
	files, err := openOfficeArchive(source.Data)
	if err != nil {
		return nil, err
	}
	document := files["word/document.xml"]
	if len(document) == 0 {
		return nil, fmt.Errorf("DOCX 缺少 word/document.xml")
	}
	styles := docxStyles(files["word/styles.xml"])
	listFormats := docxNumbering(files["word/numbering.xml"])
	rels := docxRelationships(files["word/_rels/document.xml.rels"])
	doc := &ParsedDocument{Metadata: map[string]any{"parser": "ooxml"}}
	decoder := xml.NewDecoder(bytes.NewReader(document))
	var path []string
	index := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("解析 DOCX XML 失败: %w", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "p":
			var raw xmlRaw
			if err := decoder.DecodeElement(&raw, &start); err != nil {
				return nil, err
			}
			info := parseDocxParagraph(raw.Inner, styles, listFormats)
			if info.text != "" {
				kind := "paragraph"
				if info.level > 0 {
					kind = "heading"
				} else if info.list {
					kind = "list"
				}
				if kind == "heading" {
					if info.level <= len(path) {
						path = path[:info.level-1]
					}
					path = append(path, info.text)
					if doc.Title == "" {
						doc.Title = info.text
					}
				}
				meta := map[string]any{"element_type": kind, "element_index": index}
				if len(path) > 0 {
					meta["section_path"] = append([]string(nil), path...)
				}
				if info.list {
					meta["list_level"] = info.listLevel
					meta["list_format"] = info.listFormat
				}
				doc.Elements = append(doc.Elements, DocumentElement{Type: kind, Content: info.text, Level: info.level, Metadata: meta})
				index++
			}
			for _, relID := range info.images {
				meta := map[string]any{"element_type": "picture", "element_index": index, "relationship_id": relID}
				if target := rels[relID]; target != "" {
					meta["filename"] = filepath.Base(target)
				}
				if len(path) > 0 {
					meta["section_path"] = append([]string(nil), path...)
				}
				pictureContent := info.text
				if pictureContent == "" {
					pictureContent = lastElementText(doc.Elements)
					if pictureContent != "" {
						meta["content_source"] = "preceding_text"
					}
				}
				doc.Elements = append(doc.Elements, DocumentElement{Type: "picture", Content: pictureContent, Metadata: meta})
				index++
			}
		case "tbl":
			var raw xmlRaw
			if err := decoder.DecodeElement(&raw, &start); err != nil {
				return nil, err
			}
			content, imageIDs := parseDocxTable(raw.Inner)
			if content != "" {
				meta := map[string]any{"element_type": "table", "element_index": index}
				if len(path) > 0 {
					meta["section_path"] = append([]string(nil), path...)
				}
				doc.Elements = append(doc.Elements, DocumentElement{Type: "table", Content: content, Metadata: meta})
				index++
			}
			for _, relID := range imageIDs {
				meta := map[string]any{"element_type": "picture", "element_index": index, "relationship_id": relID}
				if target := rels[relID]; target != "" {
					meta["filename"] = filepath.Base(target)
				}
				pictureContent := lastElementText(doc.Elements)
				if pictureContent != "" {
					meta["content_source"] = "preceding_text"
				}
				doc.Elements = append(doc.Elements, DocumentElement{Type: "picture", Content: pictureContent, Metadata: meta})
				index++
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
		return nil, fmt.Errorf("DOCX 未提取到可索引内容")
	}
	return doc, nil
}

type docxParagraph struct {
	text       string
	level      int
	list       bool
	listLevel  int
	listFormat string
	images     []string
}

func parseDocxParagraph(inner string, styles map[string]int, formats map[string]string) docxParagraph {
	decoder := xml.NewDecoder(strings.NewReader("<root>" + inner + "</root>"))
	var out docxParagraph
	var text strings.Builder
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "t":
			var value string
			_ = decoder.DecodeElement(&value, &start)
			text.WriteString(value)
		case "tab":
			text.WriteByte('\t')
		case "br":
			text.WriteByte('\n')
		case "pStyle":
			if value := attr(start, "val"); value != "" {
				out.level = styles[value]
				if out.level == 0 {
					out.level = headingLevel(value)
				}
			}
		case "numId":
			out.list = true
			out.listFormat = formats[attr(start, "val")]
		case "ilvl":
			out.listLevel, _ = strconv.Atoi(attr(start, "val"))
		case "blip":
			if id := attr(start, "embed"); id != "" {
				out.images = append(out.images, id)
			}
		}
	}
	out.text = strings.TrimSpace(text.String())
	return out
}

func parseDocxTable(inner string) (string, []string) {
	decoder := xml.NewDecoder(strings.NewReader("<root>" + inner + "</root>"))
	var lines []string
	var images []string
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "tr" {
			continue
		}
		var row xmlRaw
		if decoder.DecodeElement(&row, &start) != nil {
			break
		}
		cells, cellImages := docxRow(row.Inner)
		images = append(images, cellImages...)
		lines = append(lines, strings.Join(cells, " | "))
	}
	return strings.TrimSpace(strings.Join(lines, "\n")), images
}

func docxRow(inner string) ([]string, []string) {
	decoder := xml.NewDecoder(strings.NewReader("<root>" + inner + "</root>"))
	var cells, images []string
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "tc" {
			continue
		}
		var cell xmlRaw
		_ = decoder.DecodeElement(&cell, &start)
		p := parseDocxParagraph(cell.Inner, nil, nil)
		cells = append(cells, p.text)
		images = append(images, p.images...)
	}
	return cells, images
}

func docxStyles(data []byte) map[string]int {
	result := map[string]int{}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "style" {
			continue
		}
		id := attr(start, "styleId")
		var raw xmlRaw
		_ = decoder.DecodeElement(&raw, &start)
		level := headingLevel(id)
		nested := xml.NewDecoder(strings.NewReader("<root>" + raw.Inner + "</root>"))
		for {
			t, e := nested.Token()
			if e != nil {
				break
			}
			s, ok := t.(xml.StartElement)
			if !ok {
				continue
			}
			if s.Name.Local == "name" && level == 0 {
				level = headingLevel(attr(s, "val"))
			}
			if s.Name.Local == "outlineLvl" {
				if n, e := strconv.Atoi(attr(s, "val")); e == nil {
					level = n + 1
				}
			}
		}
		if level > 0 {
			result[id] = level
		}
	}
	return result
}

func docxNumbering(data []byte) map[string]string {
	abstract := map[string]string{}
	nums := map[string]string{}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "abstractNum":
			id := attr(start, "abstractNumId")
			var raw xmlRaw
			_ = decoder.DecodeElement(&raw, &start)
			nested := xml.NewDecoder(strings.NewReader("<root>" + raw.Inner + "</root>"))
			for {
				t, e := nested.Token()
				if e != nil {
					break
				}
				if s, ok := t.(xml.StartElement); ok && s.Name.Local == "numFmt" {
					abstract[id] = attr(s, "val")
					break
				}
			}
		case "num":
			id := attr(start, "numId")
			var raw xmlRaw
			_ = decoder.DecodeElement(&raw, &start)
			nested := xml.NewDecoder(strings.NewReader("<root>" + raw.Inner + "</root>"))
			for {
				t, e := nested.Token()
				if e != nil {
					break
				}
				if s, ok := t.(xml.StartElement); ok && s.Name.Local == "abstractNumId" {
					nums[id] = abstract[attr(s, "val")]
					break
				}
			}
		}
	}
	return nums
}

func docxRelationships(data []byte) map[string]string {
	result := map[string]string{}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		if start, ok := token.(xml.StartElement); ok && start.Name.Local == "Relationship" {
			result[attr(start, "Id")] = attr(start, "Target")
		}
	}
	return result
}

func attr(start xml.StartElement, name string) string {
	for _, item := range start.Attr {
		if item.Name.Local == name {
			return item.Value
		}
	}
	return ""
}
func headingLevel(value string) int {
	value = strings.ToLower(strings.ReplaceAll(value, " ", ""))
	if strings.HasPrefix(value, "heading") {
		n, _ := strconv.Atoi(strings.TrimPrefix(value, "heading"))
		if n >= 1 && n <= 9 {
			return n
		}
	}
	return 0
}

func lastElementText(elements []DocumentElement) string {
	for i := len(elements) - 1; i >= 0; i-- {
		if elements[i].Content != "" {
			return elements[i].Content
		}
	}
	return ""
}
