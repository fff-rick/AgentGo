package rag

import (
	"fmt"
	"testing"

	"github.com/enterprise/ai-agent-go/internal/model"
)

func BenchmarkReciprocalRankFusion(b *testing.B) {
	for _, size := range []int{10, 100, 1000} {
		left := make([]model.Reference, size)
		right := make([]model.Reference, size)
		for i := 0; i < size; i++ {
			left[i] = model.Reference{DocID: fmt.Sprintf("doc-%d", i)}
			right[i] = model.Reference{DocID: fmt.Sprintf("doc-%d", i+size/2)}
		}
		r := &Retriever{}
		b.Run(fmt.Sprintf("candidates-%d", size*2), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got := r.reciprocalRankFusion(left, right)
				if len(got) == 0 {
					b.Fatal("empty fusion result")
				}
			}
		})
	}
}
