package mockbackend

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// Backend counts data requests, excluding health and statistics requests.
type Backend struct {
	delay                         time.Duration
	calls                         atomic.Uint64
	active, peak                  atomic.Int64
	started                       time.Time
	slowAfter, slowFor, slowDelay time.Duration
	maxAge                        int
}

func New(delay time.Duration) *Backend {
	return &Backend{delay: delay, started: time.Now(), maxAge: 10}
}

// SetMaxAge changes the public item lifetime in seconds before serving begins.
// Gateways still cap cached lifetimes at their own 10-second limit.
func (b *Backend) SetMaxAge(seconds int) { b.maxAge = seconds }

// NewWithSlowdown injects one deterministic time window; requests keep their
// selected delay even if the window ends while they are in flight.
func NewWithSlowdown(delay, after, duration, slow time.Duration) *Backend {
	b := New(delay)
	b.slowAfter = after
	b.slowFor = duration
	b.slowDelay = slow
	return b
}
func (b *Backend) currentDelay() time.Duration {
	elapsed := time.Since(b.started)
	if b.slowFor > 0 && elapsed >= b.slowAfter && elapsed < b.slowAfter+b.slowFor {
		return b.slowDelay
	}
	return b.delay
}

func (b *Backend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/healthz":
		_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
		return
	case "/stats":
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(struct {
			Calls         uint64  `json:"calls"`
			Active        int64   `json:"active"`
			Peak          int64   `json:"peak"`
			DelayMS       float64 `json:"delay_ms"`
			UptimeSeconds float64 `json:"uptime_seconds"`
		}{b.calls.Load(), b.active.Load(), b.peak.Load(), float64(b.currentDelay()) / 1e6, time.Since(b.started).Seconds()})
		return
	}
	b.calls.Add(1)
	active := b.active.Add(1)
	defer b.active.Add(-1)
	for old := b.peak.Load(); active > old; old = b.peak.Load() {
		if b.peak.CompareAndSwap(old, active) {
			break
		}
	}
	if r.Method != http.MethodGet || r.URL.Path != "/api/public/item" {
		http.NotFound(w, r)
		return
	}
	if delay := b.currentDelay(); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(b.maxAge))
	_ = json.NewEncoder(w).Encode(struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}{r.URL.Query().Get("id"), "demo item"})
}
