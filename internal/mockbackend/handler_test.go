package mockbackend

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestDeterministicItemAndCounter(t *testing.T) {
	b := New(0)
	for range 2 {
		w := httptest.NewRecorder()
		b.ServeHTTP(w, httptest.NewRequest("GET", "/api/public/item?id=42", nil))
		if w.Code != 200 || w.Body.String() != "{\"id\":\"42\",\"name\":\"demo item\"}\n" || w.Header().Get("Cache-Control") != "public, max-age=10" {
			t.Fatalf("unexpected response: %d %s %v", w.Code, w.Body.String(), w.Header())
		}
	}
	w := httptest.NewRecorder()
	b.ServeHTTP(w, httptest.NewRequest("GET", "/stats", nil))
	if w.Body.String() != "{\"calls\":2}\n" {
		t.Fatal(w.Body.String())
	}
}

func TestConcurrentCount(t *testing.T) {
	b := New(0)
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() { b.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/public/item?id=42", nil)) })
	}
	wg.Wait()
	if got := b.calls.Load(); got != 100 {
		t.Fatalf("calls=%d, want 100", got)
	}
}

func TestCanceledRequestStopsDelay(t *testing.T) {
	b := New(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/public/item?id=42", nil).WithContext(ctx))
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled request did not stop")
	}
}
