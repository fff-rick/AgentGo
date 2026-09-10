package etl

import (
	"fmt"
	"strings"
	"testing"
)

func BenchmarkChunkerSplit(b *testing.B) {
	strategies := []struct {
		name     string
		strategy ChunkStrategy
	}{
		{"fixed", StrategyFixedSize},
		{"sentence", StrategySentence},
		{"semantic", StrategySemantic},
	}
	for _, size := range []int{10 * 1024, 1024 * 1024} {
		text := benchmarkText(size)
		for _, item := range strategies {
			b.Run(fmt.Sprintf("%s/%dKiB", item.name, size/1024), func(b *testing.B) {
				chunker := NewChunker(512, 64)
				b.ReportAllocs()
				b.SetBytes(int64(len(text)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					chunks := chunker.Split(text, item.strategy)
					if len(chunks) == 0 {
						b.Fatal("no chunks produced")
					}
				}
			})
		}
	}
}

func benchmarkText(size int) string {
	paragraph := "AgentGo 支持模型路由、工具调用和检索增强生成。This sentence keeps the benchmark multilingual.\n\n"
	return strings.Repeat(paragraph, size/len(paragraph)+1)[:size]
}
