package limiter

import (
	"context"
	"errors"
	"high-performance-api-gateway/internal/observe"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBoundedQueueBurstAndCancellation(t *testing.T) {
	m := observe.New("test", nil, 0)
	tr := NewWithQueue(transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{Body: io.NopCloser(strings.NewReader("held"))}, nil
	}), 1, time.Hour, 8)
	tr.Metrics = m
	req := httptest.NewRequest("GET", "http://backend/", nil)
	held, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Body.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan error, 200)
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := tr.RoundTrip(req.WithContext(ctx)); results <- err }()
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(results) == 192 && m.Snapshot()["gauges"].(map[string]int64)["permit_waiters"] == 8 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(results) != 192 || len(tr.waiters) != 8 || len(tr.permits) != 1 {
		t.Fatalf("queue bound: completed=%d waiters=%d active=%d", len(results), len(tr.waiters), len(tr.permits))
	}
	cancel()
	wg.Wait()
	close(results)
	full, canceled := 0, 0
	for err := range results {
		switch {
		case errors.Is(err, ErrQueueFull):
			full++
		case errors.Is(err, context.Canceled):
			canceled++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if full != 192 || canceled != 8 || len(tr.waiters) != 0 || len(tr.permits) != 1 {
		t.Fatalf("cleanup full=%d canceled=%d queue=%d permits=%d", full, canceled, len(tr.waiters), len(tr.permits))
	}
	if m.Snapshot()["gauges"].(map[string]int64)["permit_waiters"] != 0 {
		t.Fatal("gauge leaked")
	}
	held.Body.Close()
	again, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	again.Body.Close()
}
func TestQueueZeroAndWaitTimeoutReasons(t *testing.T) {
	for _, tc := range []struct {
		queue int
		want  error
	}{{0, ErrQueueFull}, {1, ErrWaitExpired}, {-1, ErrWaitExpired}} {
		tr := NewWithQueue(transportFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{Body: io.NopCloser(strings.NewReader("x"))}, nil
		}), 1, time.Millisecond, tc.queue)
		req := httptest.NewRequest("GET", "http://backend/", nil)
		first, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tr.RoundTrip(req)
		first.Body.Close()
		if !errors.Is(err, tc.want) || !errors.Is(err, ErrOverloaded) {
			t.Errorf("queue %d: %v", tc.queue, err)
		}
	}
}
