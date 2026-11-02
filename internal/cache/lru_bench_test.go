package cache

import (
	"net/http"
	"testing"
	"time"
)

func BenchmarkCacheGet(b *testing.B) {
	c := New(1000, 16<<20, nil)
	c.Put("hot", Response{Header: http.Header{"Cache-Control": {"public, max-age=10"}}, Body: make([]byte, 1024), ExpiresAt: time.Now().Add(time.Hour)})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := c.Get("hot"); !ok {
			b.Fatal("miss")
		}
	}
}
func BenchmarkCacheGetParallel(b *testing.B) {
	c := New(1000, 16<<20, nil)
	c.Put("hot", Response{Header: http.Header{"Cache-Control": {"public, max-age=10"}}, Body: make([]byte, 1024), ExpiresAt: time.Now().Add(time.Hour)})
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Get("hot")
		}
	})
}
func BenchmarkCachePut(b *testing.B) {
	c := New(1000, 16<<20, nil)
	r := Response{Header: http.Header{"Cache-Control": {"public, max-age=10"}}, Body: make([]byte, 1024), ExpiresAt: time.Now().Add(time.Hour)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Put("hot", r)
	}
}
