package etl

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// ChunkStrategy 分块策略
type ChunkStrategy int

const (
	// StrategyFixedSize 固定大小分块
	StrategyFixedSize ChunkStrategy = iota
	// StrategySentence 按句子分块
	StrategySentence
	// StrategySemantic 按语义段落分块
	StrategySemantic
)

// Chunk 文档分块
type Chunk struct {
	Content    string
	ChunkIndex int
	StartPos   int
	EndPos     int
	Metadata   map[string]any
}

// SplitElements preserves source provenance while grouping only compatible text elements.
func (c *Chunker) SplitElements(elements []DocumentElement) []Chunk {
	var chunks []Chunk
	var pending strings.Builder
	var pendingMeta map[string]any
	pendingKey := ""
	flush := func() {
		content := strings.TrimSpace(pending.String())
		if content != "" {
			chunks = append(chunks, Chunk{Content: content, ChunkIndex: len(chunks), Metadata: cloneMetadata(pendingMeta)})
		}
		pending.Reset()
		pendingMeta = nil
		pendingKey = ""
	}
	for _, item := range elements {
		content := strings.TrimSpace(item.Content)
		if content == "" && item.Type == "picture" {
			flush()
			meta := cloneMetadata(item.Metadata)
			meta["element_type"] = "picture"
			chunks = append(chunks, Chunk{ChunkIndex: len(chunks), Metadata: meta})
			continue
		}
		if content == "" {
			continue
		}
		meta := cloneMetadata(item.Metadata)
		meta["element_type"] = item.Type
		if item.Type == "picture" {
			flush()
			content = withSectionPath(content, meta)
			parts := []Chunk{{Content: content}}
			if utf8.RuneCountInString(content) > c.chunkSize {
				parts = c.splitElementText(content)
			}
			for i, part := range parts {
				partMeta := cloneMetadata(meta)
				if i > 0 {
					partMeta["segment_index"] = i
				}
				chunks = append(chunks, Chunk{Content: part.Content, ChunkIndex: len(chunks), Metadata: partMeta})
			}
			continue
		}
		if item.Type == "table" || item.Type == "row" {
			flush()
			parts := c.splitStructured(content)
			if item.Type == "row" {
				parts = splitSpreadsheetRow(content)
			} else {
				var safe []string
				for _, part := range parts {
					if len(part) > 65000 {
						safe = append(safe, splitUTF8Bytes(part, 65000)...)
					} else {
						safe = append(safe, part)
					}
				}
				parts = safe
			}
			for i, part := range parts {
				partMeta := cloneMetadata(meta)
				if i > 0 {
					partMeta["segment_index"] = i
				}
				chunks = append(chunks, Chunk{Content: part, ChunkIndex: len(chunks), Metadata: partMeta})
			}
			continue
		}
		content = withSectionPath(content, meta)
		keyBytes, _ := json.Marshal(map[string]any{"page": meta["page"], "section_path": meta["section_path"]})
		key := string(keyBytes)
		if pending.Len() > 0 && (key != pendingKey || utf8.RuneCountInString(pending.String())+utf8.RuneCountInString(content)+2 > c.chunkSize) {
			flush()
		}
		if utf8.RuneCountInString(content) > c.chunkSize {
			flush()
			for _, part := range c.splitElementText(content) {
				part.Metadata = cloneMetadata(meta)
				part.ChunkIndex = len(chunks)
				chunks = append(chunks, part)
			}
			continue
		}
		if pending.Len() > 0 {
			pending.WriteString("\n\n")
		}
		pending.WriteString(content)
		pendingMeta = meta
		pendingKey = key
	}
	flush()
	return chunks
}

func splitSpreadsheetRow(content string) []string {
	const maxBytes = 65000
	if len(content) <= maxBytes {
		return []string{content}
	}
	lines := strings.SplitN(content, "\n", 3)
	if len(lines) < 3 {
		return splitUTF8Bytes(content, maxBytes)
	}
	prefix := lines[0] + "\n" + lines[1] + "\n"
	limit := maxBytes - len(prefix)
	if limit <= 0 {
		return splitUTF8Bytes(content, maxBytes)
	}
	cells := strings.Split(lines[2], " | ")
	var result []string
	current := ""
	for _, cell := range cells {
		candidate := cell
		if current != "" {
			candidate = current + " | " + cell
		}
		if len(candidate) <= limit {
			current = candidate
			continue
		}
		if current != "" {
			result = append(result, prefix+current)
			current = ""
		}
		if len(cell) > limit {
			for _, part := range splitUTF8Bytes(cell, limit) {
				result = append(result, prefix+part)
			}
		} else {
			current = cell
		}
	}
	if current != "" {
		result = append(result, prefix+current)
	}
	return result
}

func splitUTF8Bytes(content string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	var result []string
	start := 0
	size := 0
	runes := []rune(content)
	for index, value := range runes {
		width := utf8.RuneLen(value)
		if size+width > limit && index > start {
			result = append(result, string(runes[start:index]))
			start = index
			size = 0
		}
		size += width
	}
	if start < len(runes) {
		result = append(result, string(runes[start:]))
	}
	return result
}

func (c *Chunker) splitStructured(content string) []string {
	if utf8.RuneCountInString(content) <= c.chunkSize {
		return []string{content}
	}
	lines := strings.Split(content, "\n")
	if len(lines) < 2 {
		raw := c.splitByFixedSize(content)
		result := make([]string, len(raw))
		for i := range raw {
			result[i] = raw[i].Content
		}
		return result
	}
	prefix := lines[0]
	header := ""
	start := 1
	if len(lines) > 2 {
		header = lines[1]
		start = 2
	}
	var result []string
	current := prefix
	if header != "" {
		current += "\n" + header
	}
	for _, line := range lines[start:] {
		if utf8.RuneCountInString(current)+utf8.RuneCountInString(line)+1 > c.chunkSize && current != prefix {
			result = append(result, current)
			current = prefix
			if header != "" {
				current += "\n" + header
			}
		}
		current += "\n" + line
	}
	if strings.TrimSpace(current) != "" {
		result = append(result, current)
	}
	return result
}

func cloneMetadata(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
func withSectionPath(content string, metadata map[string]any) string {
	path, ok := metadata["section_path"].([]string)
	if !ok || len(path) == 0 {
		return content
	}
	prefix := strings.Join(path, " > ")
	if strings.HasPrefix(content, prefix) {
		return content
	}
	return prefix + "\n" + content
}

// Chunker 文档分块器
type Chunker struct {
	chunkSize int // 每块的最大字符数
	overlap   int // 相邻块的重叠字符数
}

// NewChunker 创建分块器
func NewChunker(chunkSize, overlap int) *Chunker {
	if chunkSize <= 0 {
		chunkSize = 512
	}
	if overlap < 0 || overlap >= chunkSize {
		overlap = chunkSize / 8
	}
	return &Chunker{
		chunkSize: chunkSize,
		overlap:   overlap,
	}
}

// Split 将文档内容分割为多个块
func (c *Chunker) Split(content string, strategy ChunkStrategy) []Chunk {
	switch strategy {
	case StrategySentence:
		return c.splitBySentence(content)
	case StrategySemantic:
		return c.splitBySemantic(content)
	default:
		return c.splitByFixedSize(content)
	}
}

// splitByFixedSize 按固定字符数分块，支持滑动窗口重叠
func (c *Chunker) splitByFixedSize(content string) []Chunk {
	runes := []rune(content)
	totalLen := len(runes)
	if totalLen == 0 {
		return nil
	}

	var chunks []Chunk
	step := c.chunkSize - c.overlap
	if step <= 0 {
		step = c.chunkSize
	}

	for i := 0; i < totalLen; i += step {
		end := i + c.chunkSize
		if end > totalLen {
			end = totalLen
		}

		chunkContent := string(runes[i:end])
		chunks = append(chunks, Chunk{
			Content:    strings.TrimSpace(chunkContent),
			ChunkIndex: len(chunks),
			StartPos:   i,
			EndPos:     end,
		})

		if end >= totalLen {
			break
		}
	}

	return chunks
}

// splitBySentence 按句子分块，尽量在句子边界处切分
func (c *Chunker) splitBySentence(content string) []Chunk {
	// 按句子分隔符切分
	separators := []string{"。", "！", "？", ".", "!", "?", "\n\n"}
	sentences := splitBySeparators(content, separators)

	var chunks []Chunk
	var current strings.Builder
	currentLen := 0

	for _, sentence := range sentences {
		sentLen := utf8.RuneCountInString(sentence)

		if currentLen+sentLen > c.chunkSize && currentLen > 0 {
			// 当前块已满，保存并开始新块
			chunks = append(chunks, Chunk{
				Content:    strings.TrimSpace(current.String()),
				ChunkIndex: len(chunks),
			})

			// 重叠处理：保留最后一部分
			overlapText := getLastNChars(current.String(), c.overlap)
			current.Reset()
			current.WriteString(overlapText)
			currentLen = utf8.RuneCountInString(overlapText)
		}

		current.WriteString(sentence)
		currentLen += sentLen
	}

	if currentLen > 0 {
		chunks = append(chunks, Chunk{
			Content:    strings.TrimSpace(current.String()),
			ChunkIndex: len(chunks),
		})
	}

	return chunks
}

func (c *Chunker) splitElementText(content string) []Chunk {
	var result []Chunk
	for _, chunk := range c.splitBySentence(content) {
		if utf8.RuneCountInString(chunk.Content) <= c.chunkSize {
			chunk.ChunkIndex = len(result)
			result = append(result, chunk)
			continue
		}
		for _, part := range c.splitByFixedSize(chunk.Content) {
			part.ChunkIndex = len(result)
			result = append(result, part)
		}
	}
	return result
}

// splitBySemantic 按语义段落分块（基于段落标题和空行）
func (c *Chunker) splitBySemantic(content string) []Chunk {
	// 按双换行符分割段落
	paragraphs := strings.Split(content, "\n\n")

	var chunks []Chunk
	var current strings.Builder
	currentLen := 0

	for _, para := range paragraphs {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}

		paraLen := utf8.RuneCountInString(para)

		if currentLen+paraLen > c.chunkSize && currentLen > 0 {
			chunks = append(chunks, Chunk{
				Content:    strings.TrimSpace(current.String()),
				ChunkIndex: len(chunks),
			})
			current.Reset()
			currentLen = 0
		}

		current.WriteString(para + "\n\n")
		currentLen += paraLen
	}

	if currentLen > 0 {
		chunks = append(chunks, Chunk{
			Content:    strings.TrimSpace(current.String()),
			ChunkIndex: len(chunks),
		})
	}

	return chunks
}

// splitBySeparators 按多个分隔符切分文本，保留分隔符
func splitBySeparators(text string, separators []string) []string {
	var parts []string
	remaining := text

	for len(remaining) > 0 {
		minIdx := len(remaining)
		minSep := ""

		for _, sep := range separators {
			idx := strings.Index(remaining, sep)
			if idx >= 0 && idx < minIdx {
				minIdx = idx
				minSep = sep
			}
		}

		if minSep == "" {
			parts = append(parts, remaining)
			break
		}

		part := remaining[:minIdx+len(minSep)]
		if strings.TrimSpace(part) != "" {
			parts = append(parts, part)
		}
		remaining = remaining[minIdx+len(minSep):]
	}

	return parts
}

// getLastNChars 获取字符串最后 N 个字符
func getLastNChars(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[len(runes)-n:])
}
