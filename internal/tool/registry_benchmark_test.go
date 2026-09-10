package tool

import (
	"context"
	"fmt"
	"testing"

	"go.uber.org/zap"
)

type benchmarkTool struct{ name string }

func (t benchmarkTool) Name() string        { return t.name }
func (t benchmarkTool) Description() string { return "benchmark no-op tool" }
func (t benchmarkTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object"}
}
func (t benchmarkTool) Execute(context.Context, string) (*ToolResult, error) {
	return NewSuccessResult("ok"), nil
}

func benchmarkRegistry(size int) *Registry {
	r := NewRegistry()
	for i := 0; i < size; i++ {
		r.MustRegister(benchmarkTool{name: fmt.Sprintf("tool-%05d", i)})
	}
	return r
}

func BenchmarkRegistryGet(b *testing.B) {
	for _, size := range []int{10, 1000, 10000} {
		b.Run(fmt.Sprintf("tools-%d", size), func(b *testing.B) {
			r := benchmarkRegistry(size)
			name := fmt.Sprintf("tool-%05d", size-1)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, ok := r.Get(name); !ok {
					b.Fatal("tool not found")
				}
			}
		})
	}
}

func BenchmarkRegistryGetParallel(b *testing.B) {
	r := benchmarkRegistry(1000)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, ok := r.Get("tool-00999"); !ok {
				b.Fatal("tool not found")
			}
		}
	})
}

func BenchmarkRouterExecute(b *testing.B) {
	r := NewRegistry()
	r.MustRegister(benchmarkTool{name: "noop"})
	router := NewRouter(r, zap.NewNop())
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			result, err := router.Execute(context.Background(), "noop", `{}`)
			if err != nil || !result.Success {
				b.Fatal("execution failed")
			}
		}
	})
}
