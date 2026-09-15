package etl

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xuri/excelize/v2"
)

func TestPDFParserPreservesOrderAndProvenance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if r.FormValue("do_ocr") != "true" || r.FormValue("force_ocr") != "false" || r.FormValue("table_mode") != "accurate" {
			t.Fatalf("unexpected Docling options: %v", r.Form)
		}
		payload := map[string]any{"document": map[string]any{"json_content": map[string]any{
			"body":     map[string]any{"children": []any{map[string]any{"$ref": "#/texts/0"}, map[string]any{"$ref": "#/tables/0"}, map[string]any{"$ref": "#/pictures/0"}}},
			"texts":    []any{map[string]any{"self_ref": "#/texts/0", "label": "section_header", "text": "订单说明", "level": 1, "prov": []any{map[string]any{"page_no": 2, "bbox": map[string]any{"l": 1, "t": 2, "r": 3, "b": 4}}}}},
			"tables":   []any{map[string]any{"self_ref": "#/tables/0", "label": "table", "data": map[string]any{"num_rows": 2, "num_cols": 2, "table_cells": []any{map[string]any{"start_row_offset_idx": 0, "start_col_offset_idx": 0, "text": "ID"}, map[string]any{"start_row_offset_idx": 1, "start_col_offset_idx": 0, "text": "10001"}}}}},
			"pictures": []any{map[string]any{"self_ref": "#/pictures/0", "label": "picture", "text": "架构图", "prov": []any{map[string]any{"page_no": 3}}}},
		}}}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer server.Close()
	parser := NewDocumentParser(server.URL, 0)
	doc, err := parser.Parse(context.Background(), SourceDocument{Filename: "sample.pdf", Type: DocTypePDF, Data: []byte("%PDF-test")})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Elements) != 3 || doc.Elements[0].Type != "heading" || doc.Elements[1].Type != "table" || doc.Elements[2].Type != "picture" {
		t.Fatalf("elements=%+v", doc.Elements)
	}
	if doc.Elements[0].Metadata["page"] != 2 || doc.Elements[2].Metadata["page"] != 3 {
		t.Fatalf("metadata=%+v", doc.Elements)
	}
}

func TestDOCXParserExtractsStructuredElements(t *testing.T) {
	data := zipFixture(t, map[string]string{
		"word/document.xml": `<w:document xmlns:w="w" xmlns:r="r" xmlns:a="a"><w:body>
<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>使用说明</w:t></w:r></w:p>
<w:p><w:pPr><w:numPr><w:ilvl w:val="1"/><w:numId w:val="1"/></w:numPr></w:pPr><w:r><w:t>第一项</w:t></w:r></w:p>
<w:tbl><w:tr><w:tc><w:p><w:r><w:t>ID</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>金额</w:t></w:r></w:p></w:tc></w:tr><w:tr><w:tc><w:p><w:r><w:t>10001</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>9999</w:t></w:r></w:p></w:tc></w:tr></w:tbl>
<w:p><w:r><w:t>架构图</w:t><w:drawing><a:blip r:embed="rId5"/></w:drawing></w:r></w:p></w:body></w:document>`,
		"word/styles.xml":              `<w:styles xmlns:w="w"><w:style w:styleId="Heading1"><w:name w:val="heading 1"/></w:style></w:styles>`,
		"word/numbering.xml":           `<w:numbering xmlns:w="w"><w:abstractNum w:abstractNumId="0"><w:lvl><w:numFmt w:val="decimal"/></w:lvl></w:abstractNum><w:num w:numId="1"><w:abstractNumId w:val="0"/></w:num></w:numbering>`,
		"word/_rels/document.xml.rels": `<Relationships><Relationship Id="rId5" Target="media/image1.png"/></Relationships>`,
	})
	doc, err := parseDOCX(SourceDocument{Filename: "guide.docx", Type: DocTypeDOCX, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"heading", "list", "table", "paragraph", "picture"}
	if len(doc.Elements) != len(want) {
		t.Fatalf("elements=%+v", doc.Elements)
	}
	for i, kind := range want {
		if doc.Elements[i].Type != kind {
			t.Fatalf("element %d=%+v", i, doc.Elements[i])
		}
	}
	if doc.Elements[2].Content != "ID | 金额\n10001 | 9999" || doc.Elements[4].Metadata["filename"] != "image1.png" {
		t.Fatalf("elements=%+v", doc.Elements)
	}
}

func TestXLSXParserProducesExactRowRanges(t *testing.T) {
	book := excelize.NewFile()
	sheet := book.GetSheetName(0)
	_ = book.SetSheetName(sheet, "用户订单")
	sheet = "用户订单"
	rows := [][]any{{"订单ID", "用户", "商品", "金额"}, {10001, "Rick", "MacBook", 9999}, {10002, "Tom", "iPhone", 5999}, {}, {"状态", "数量"}, {"已完成", 2}}
	for r, row := range rows {
		for c, value := range row {
			cell, _ := excelize.CoordinatesToCellName(c+1, r+1)
			_ = book.SetCellValue(sheet, cell, value)
		}
	}
	if err := book.AddTable(sheet, &excelize.Table{Range: "A1:D3", Name: "Orders"}); err != nil {
		t.Fatal(err)
	}
	hidden, _ := book.NewSheet("隐藏")
	_ = hidden
	_ = book.SetCellValue("隐藏", "A1", "秘密")
	_ = book.SetCellValue("隐藏", "A2", "不应索引")
	_ = book.SetSheetVisible("隐藏", false)
	buffer, err := book.WriteToBuffer()
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parseXLSX(SourceDocument{Filename: "orders.xlsx", Type: DocTypeXLSX, Data: buffer.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Elements) != 3 {
		t.Fatalf("elements=%+v", doc.Elements)
	}
	if doc.Elements[0].Metadata["range"] != "用户订单!A2:D2" || doc.Elements[2].Metadata["range"] != "用户订单!A6:B6" {
		t.Fatalf("elements=%+v", doc.Elements)
	}
	headers, ok := doc.Elements[0].Metadata["headers"].([]string)
	if !ok || len(headers) != 4 || headers[3] != "金额" {
		t.Fatalf("headers=%#v", doc.Elements[0].Metadata["headers"])
	}
}

func TestChunkerKeepsUncaptionedPictureWithoutEmbeddingText(t *testing.T) {
	chunks := NewChunker(100, 0).SplitElements([]DocumentElement{{Type: "picture", Metadata: map[string]any{"page": 2}}})
	if len(chunks) != 1 || chunks[0].Content != "" || chunks[0].Metadata["element_type"] != "picture" {
		t.Fatalf("chunks=%+v", chunks)
	}
}

func zipFixture(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range files {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = file.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
