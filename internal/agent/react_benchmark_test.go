package agent

import (
	"strings"
	"testing"

	"go.uber.org/zap"
)

func BenchmarkReActParsing(b *testing.B) {
	a := &ReActAgent{logger: zap.NewNop()}
	content := "Thought: 需要计算。\nAction: {\"tool\":\"calculator\",\"input\":\"{\\\"operation\\\":\\\"multiply\\\",\\\"a\\\":42,\\\"b\\\":7}\"}\n" + strings.Repeat("context ", 50)
	b.Run("action", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if action := a.extractAction(content); action == nil {
				b.Fatal("action not parsed")
			}
		}
	})
	b.Run("thought", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = a.extractThought(content)
		}
	})
	b.Run("final-answer", func(b *testing.B) {
		answer := "Thought: 已完成。\nFinal Answer: 294"
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if got := a.extractFinalAnswer(answer); got == "" {
				b.Fatal("answer not parsed")
			}
		}
	})
}
