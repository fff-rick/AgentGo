package llm

import "testing"

func BenchmarkCircuitBreakerAllowParallel(b *testing.B) {
	cb := NewCircuitBreaker(5, 3, 0)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if !cb.Allow() {
				b.Fatal("closed circuit rejected request")
			}
		}
	})
}
