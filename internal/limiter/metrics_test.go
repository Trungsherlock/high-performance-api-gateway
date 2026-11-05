package limiter

import (
	"context"
	"errors"
	"high-performance-api-gateway/internal/observe"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWaiterMetricsReleaseOnCancellationAndRejection(t *testing.T) {
	m := observe.New("test", nil, 0)
	tr := New(transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{Body: io.NopCloser(strings.NewReader("body"))}, nil
	}), 1, 100*time.Millisecond)
	tr.Metrics = m
	req := httptest.NewRequest("GET", "http://backend/", nil)
	first, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := tr.RoundTrip(req.WithContext(ctx)); done <- err }()
	deadline := time.Now().Add(time.Second)
	for m.Snapshot()["gauges"].(map[string]int64)["permit_waiters"] != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.Snapshot()["gauges"].(map[string]int64)["permit_waiters"] != 1 {
		t.Fatal("waiter was not observed")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = tr.RoundTrip(req); !errors.Is(err, ErrOverloaded) {
		t.Fatal(err)
	}
	snap := m.Snapshot()
	if snap["gauges"].(map[string]int64)["permit_waiters"] != 0 {
		t.Fatal("waiter leaked")
	}
	if snap["counters"].(map[string]int64)["permit_rejections"] != 1 {
		t.Fatal("cancellation counted as rejection")
	}
	if snap["histograms"].(map[string]observe.Histogram)["permit_wait"].Count != 3 {
		t.Fatal("wait not observed on each outcome")
	}
}
