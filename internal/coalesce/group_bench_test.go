package coalesce

import (
	"context"
	"testing"
)

func BenchmarkGroupLeader(b *testing.B) {
	var g Group[int]
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.Start("hot", func() (int, error) { return 42, nil }).Wait(ctx)
	}
}
func BenchmarkGroupJoin(b *testing.B) {
	var g Group[int]
	release := make(chan struct{})
	call := g.Start("hot", func() (int, error) { <-release; return 42, nil })
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.Start("hot", func() (int, error) { b.Error("unexpected leader"); return 0, nil })
	}
	b.StopTimer()
	close(release)
	call.Wait(context.Background())
}
