package mockbackend

import (
	"context"
	"encoding/json"
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
	var stats struct {
		Calls  int
		Active int
		Peak   int
	}
	if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Calls != 2 || stats.Active != 0 || stats.Peak != 1 {
		t.Fatal(w.Body.String())
	}
}

func TestConfiguredMaxAge(t *testing.T) {
	b := New(0)
	b.SetMaxAge(2)
	w := httptest.NewRecorder()
	b.ServeHTTP(w, httptest.NewRequest("GET", "/api/public/item?id=42", nil))
	if w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=2" {
		t.Fatal(w.Code, w.Header())
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

func TestSlowdownWindowAndActiveCleanup(t *testing.T) {
	b := NewWithSlowdown(time.Millisecond, time.Hour, time.Hour, 10*time.Millisecond)
	if b.currentDelay() != time.Millisecond {
		t.Fatal("healthy delay")
	}
	b.started = time.Now().Add(-time.Hour - time.Minute)
	if b.currentDelay() != 10*time.Millisecond {
		t.Fatal("slow delay")
	}
	b.started = time.Now().Add(-3 * time.Hour)
	if b.currentDelay() != time.Millisecond {
		t.Fatal("did not recover")
	}
	b.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/public/item", nil))
	if b.active.Load() != 0 || b.peak.Load() != 1 {
		t.Fatal("active work leaked")
	}
}
