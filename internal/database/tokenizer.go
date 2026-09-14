package database

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-ego/gse"
)

const maxTermBytes = 128

func newSegmenter() (*gse.Segmenter, error) {
	segmenter, err := gse.NewEmbed("zh_s")
	return &segmenter, err
}

func tokenize(segmenter *gse.Segmenter, text string) map[string]int {
	text = strings.ToLower(text)
	terms := asciiTermFrequency(text)
	for _, raw := range segmenter.CutSearch(text) {
		term := strings.TrimSpace(raw)
		if term == "" || len(term) > maxTermBytes || isASCIIWord(term) || !hasSearchableRune(term) {
			continue
		}
		terms[term]++
	}
	return terms
}

func asciiTermFrequency(text string) map[string]int {
	terms := make(map[string]int)
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return r > unicode.MaxASCII || (!unicode.IsLetter(r) && !unicode.IsNumber(r))
	})
	for _, term := range fields {
		if len(term) <= maxTermBytes {
			terms[term]++
		}
	}
	return terms
}

func isASCIIWord(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r > unicode.MaxASCII || (!unicode.IsLetter(r) && !unicode.IsNumber(r)) {
			return false
		}
	}
	return true
}

func hasSearchableRune(value string) bool {
	for len(value) > 0 {
		r, size := utf8.DecodeRuneInString(value)
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return true
		}
		value = value[size:]
	}
	return false
}
