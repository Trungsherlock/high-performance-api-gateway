package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestScheduledSaturationIsAccounted(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(30 * time.Millisecond); w.WriteHeader(503) }))
	defer s.Close()
	r := run(context.Background(), s.Client(), s.URL, "scheduled", "hot", "measurement", 100*time.Millisecond, 1000, 1, 42)
	if r.Offered != 100 || r.GeneratorDrops == 0 || r.Sent+r.GeneratorDrops != r.Offered || r.Rejected != r.Sent || r.TransportErrors != 0 {
		t.Fatalf("bad accounting: %+v", r)
	}
	if r.ScheduledLatencyMS["p99"] < r.SendLatencyMS["p99"] {
		t.Fatal("scheduled latency omitted lag")
	}
}
func TestClosedLoopAndTransportFailure(t *testing.T) {
	var active, peak atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		active.Add(-1)
	}))
	r := run(context.Background(), s.Client(), s.URL, "closed", "mixed", "measurement", 50*time.Millisecond, 1, 3, 42)
	s.Close()
	if r.Success != r.Sent || r.GeneratorDrops != 0 || peak.Load() > 3 {
		t.Fatalf("bad closed-loop accounting %+v peak=%d", r, peak.Load())
	}
	r = run(context.Background(), &http.Client{Timeout: time.Second}, s.URL, "scheduled", "hot", "measurement", 20*time.Millisecond, 100, 2, 42)
	if r.TransportErrors != r.Sent || r.Rejected != 0 {
		t.Fatalf("transport errors conflated: %+v", r)
	}
}
