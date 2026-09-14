package database

import "testing"

func TestTokenizeForChineseSearch(t *testing.T) {
	segmenter, err := newSegmenter()
	if err != nil {
		t.Fatal(err)
	}
	terms := tokenize(segmenter, "AgentGo 支持知识库检索，agentgo 可组合工具。")
	if terms["agentgo"] != 2 {
		t.Fatalf("agentgo frequency = %d, want 2; terms = %#v", terms["agentgo"], terms)
	}
	foundChineseWord := false
	for term := range terms {
		if len([]rune(term)) > 1 && term != "agentgo" {
			foundChineseWord = true
			break
		}
	}
	if !foundChineseWord {
		t.Fatalf("expected a multi-rune Chinese search term, got %#v", terms)
	}
	marker := "agentgotest123"
	_ = tokenize(segmenter, "包含标记 "+marker+" "+marker)
	if queryTerms := tokenize(segmenter, marker); queryTerms[marker] != 1 {
		t.Fatalf("query terms = %#v, want %q", queryTerms, marker)
	}
}
