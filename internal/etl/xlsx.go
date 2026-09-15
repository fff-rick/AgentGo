package etl

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/xuri/excelize/v2"
)

type sheetRow struct {
	number int
	values []string
}
type cellRange struct{ minCol, minRow, maxCol, maxRow int }

func parseXLSX(source SourceDocument) (*ParsedDocument, error) {
	book, err := excelize.OpenReader(bytes.NewReader(source.Data))
	if err != nil {
		return nil, fmt.Errorf("打开 XLSX 失败: %w", err)
	}
	defer book.Close()
	doc := &ParsedDocument{Metadata: map[string]any{"parser": "excelize"}}
	for _, sheet := range book.GetSheetList() {
		visible, err := book.GetSheetVisible(sheet)
		if err != nil {
			return nil, err
		}
		if !visible {
			continue
		}
		rows, err := visibleSheetRows(book, sheet)
		if err != nil {
			return nil, err
		}
		covered := map[int]bool{}
		tables, err := book.GetTables(sheet)
		if err != nil {
			return nil, err
		}
		for _, table := range tables {
			rng, err := parseCellRange(table.Range)
			if err != nil {
				continue
			}
			doc.Elements = append(doc.Elements, xlsxRangeElements(book, sheet, rows, rng)...)
			for row := rng.minRow; row <= rng.maxRow; row++ {
				covered[row] = true
			}
		}
		var block []sheetRow
		flush := func() {
			if len(block) > 1 {
				rng := blockRange(block)
				doc.Elements = append(doc.Elements, xlsxRangeElements(book, sheet, block, rng)...)
			}
			block = nil
		}
		for _, row := range rows {
			if covered[row.number] || emptyRow(row.values) {
				flush()
				continue
			}
			block = append(block, row)
		}
		flush()
	}
	var contents []string
	for _, item := range doc.Elements {
		contents = append(contents, item.Content)
	}
	doc.Content = strings.Join(contents, "\n\n")
	if doc.Content == "" {
		return nil, fmt.Errorf("XLSX 未提取到可索引数据行")
	}
	return doc, nil
}

func visibleSheetRows(book *excelize.File, sheet string) ([]sheetRow, error) {
	iterator, err := book.Rows(sheet)
	if err != nil {
		return nil, err
	}
	defer iterator.Close()
	var result []sheetRow
	rowNumber := 0
	for iterator.Next() {
		rowNumber++
		visible, err := book.GetRowVisible(sheet, rowNumber)
		if err != nil {
			return nil, err
		}
		if !visible {
			continue
		}
		values, err := iterator.Columns()
		if err != nil {
			return nil, err
		}
		for col := range values {
			name, _ := excelize.ColumnNumberToName(col + 1)
			visible, _ := book.GetColVisible(sheet, name)
			if !visible {
				values[col] = ""
				continue
			}
			cell, _ := excelize.CoordinatesToCellName(col+1, rowNumber)
			if values[col] == "" {
				if formula, _ := book.GetCellFormula(sheet, cell); formula != "" {
					values[col] = "=" + formula
				}
			}
		}
		result = append(result, sheetRow{number: rowNumber, values: values})
	}
	return result, iterator.Error()
}

func xlsxRangeElements(book *excelize.File, sheet string, rows []sheetRow, rng cellRange) []DocumentElement {
	byNumber := map[int][]string{}
	for _, row := range rows {
		byNumber[row.number] = row.values
	}
	headers := make([]string, rng.maxCol-rng.minCol+1)
	for col := rng.minCol; col <= rng.maxCol; col++ {
		headers[col-rng.minCol] = rowValue(byNumber[rng.minRow], col)
	}
	expandMergedHeaders(book, sheet, rng, headers)
	seen := map[string]int{}
	for i, header := range headers {
		if header == "" {
			header, _ = excelize.ColumnNumberToName(rng.minCol + i)
		}
		seen[header]++
		if seen[header] > 1 {
			header = fmt.Sprintf("%s_%d", header, seen[header])
		}
		headers[i] = header
	}
	tableRange := excelRange(rng.minCol, rng.minRow, rng.maxCol, rng.maxRow)
	var elements []DocumentElement
	for row := rng.minRow + 1; row <= rng.maxRow; row++ {
		values := make([]string, len(headers))
		nonempty := false
		for col := rng.minCol; col <= rng.maxCol; col++ {
			values[col-rng.minCol] = rowValue(byNumber[row], col)
			nonempty = nonempty || values[col-rng.minCol] != ""
		}
		if !nonempty {
			continue
		}
		rowRange := excelRange(rng.minCol, row, rng.maxCol, row)
		content := "Sheet: " + sheet + "\n" + strings.Join(headers, " | ") + "\n" + strings.Join(values, " | ")
		elements = append(elements, DocumentElement{Type: "row", Content: content, Metadata: map[string]any{
			"element_type": "row", "sheet": sheet, "range": sheet + "!" + rowRange,
			"table_range": sheet + "!" + tableRange, "headers": append([]string(nil), headers...),
		}})
	}
	return elements
}

func expandMergedHeaders(book *excelize.File, sheet string, rng cellRange, headers []string) {
	merges, _ := book.GetMergeCells(sheet)
	for _, merged := range merges {
		startCol, startRow, _ := excelize.CellNameToCoordinates(merged.GetStartAxis())
		endCol, endRow, _ := excelize.CellNameToCoordinates(merged.GetEndAxis())
		if rng.minRow < startRow || rng.minRow > endRow {
			continue
		}
		value, _ := book.GetCellValue(sheet, merged.GetStartAxis())
		for col := maxInt(startCol, rng.minCol); col <= minInt(endCol, rng.maxCol); col++ {
			if headers[col-rng.minCol] == "" {
				headers[col-rng.minCol] = value
			}
		}
	}
}

func blockRange(rows []sheetRow) cellRange {
	rng := cellRange{minCol: 1 << 30, minRow: rows[0].number, maxRow: rows[len(rows)-1].number}
	for _, row := range rows {
		for col, value := range row.values {
			if strings.TrimSpace(value) != "" {
				rng.minCol = minInt(rng.minCol, col+1)
				rng.maxCol = maxInt(rng.maxCol, col+1)
			}
		}
	}
	if rng.minCol == 1<<30 {
		rng.minCol = 1
	}
	return rng
}

func parseCellRange(value string) (cellRange, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return cellRange{}, fmt.Errorf("无效区域 %q", value)
	}
	minCol, minRow, err := excelize.CellNameToCoordinates(parts[0])
	if err != nil {
		return cellRange{}, err
	}
	maxCol, maxRow, err := excelize.CellNameToCoordinates(parts[1])
	return cellRange{minCol, minRow, maxCol, maxRow}, err
}
func excelRange(c1, r1, c2, r2 int) string {
	a, _ := excelize.CoordinatesToCellName(c1, r1)
	b, _ := excelize.CoordinatesToCellName(c2, r2)
	return a + ":" + b
}
func rowValue(row []string, col int) string {
	if col <= 0 || col > len(row) {
		return ""
	}
	return strings.TrimSpace(row[col-1])
}
func emptyRow(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return false
		}
	}
	return true
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
