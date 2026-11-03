// Package observe provides fixed-name, low-cardinality process-local telemetry.
package observe

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var Bounds = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 3, 10}

type Histogram struct {
	Count      uint64
	SumSeconds float64
	Buckets    []uint64
}
type Metrics struct {
	mu         sync.Mutex
	Counters   map[string]int64
	Gauges     map[string]int64
	Histograms map[string]*Histogram
	Instance   string
	CacheStats func() (int, int64)
	logs       chan map[string]any
	every      uint64
	sequence   atomic.Uint64
}

func New(instance string, out io.Writer, every uint64) *Metrics {
	m := &Metrics{Instance: instance, Counters: map[string]int64{}, Gauges: map[string]int64{}, Histograms: map[string]*Histogram{}, every: every}
	for _, n := range []string{"incoming", "completed", "status_1xx", "status_2xx", "status_3xx", "status_4xx", "status_5xx", "aborted", "cache_hits", "cache_misses", "cache_bypass", "local_followers", "backend_attempts", "backend_errors", "permit_rejections", "permit_queue_full", "permit_wait_timeout", "dependency_errors", "shared_hits", "shared_misses", "shared_read_errors", "shared_write_errors", "shared_write_completed", "logs_dropped"} {
		m.Counters[n] = 0
	}
	for _, n := range []string{"active_backend", "permit_waiters", "flights"} {
		m.Gauges[n] = 0
	}
	for _, n := range []string{"request", "upstream", "cache_dependency", "permit_wait"} {
		m.Histograms[n] = &Histogram{Buckets: make([]uint64, len(Bounds)+1)}
	}
	if out != nil && every > 0 {
		m.logs = make(chan map[string]any, 256)
		go func() {
			enc := json.NewEncoder(out)
			for row := range m.logs {
				_ = enc.Encode(row)
			}
		}()
	}
	return m
}
func (m *Metrics) Add(n string, v int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if _, ok := m.Counters[n]; ok {
		m.Counters[n] += v
	}
	m.mu.Unlock()
}
func (m *Metrics) Gauge(n string, v int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if _, ok := m.Gauges[n]; ok {
		m.Gauges[n] += v
	}
	m.mu.Unlock()
}
func (m *Metrics) Observe(n string, d time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.Histograms[n]
	if h == nil {
		return
	}
	h.Count++
	h.SumSeconds += d.Seconds()
	i := 0
	for i < len(Bounds) && d.Seconds() > Bounds[i] {
		i++
	}
	h.Buckets[i]++
}

// Snapshot copies bounded telemetry before performing any network writes.
func (m *Metrics) Snapshot() map[string]any {
	m.mu.Lock()
	counters := make(map[string]int64, len(m.Counters))
	for k, v := range m.Counters {
		counters[k] = v
	}
	gauges := make(map[string]int64, len(m.Gauges))
	for k, v := range m.Gauges {
		gauges[k] = v
	}
	histograms := make(map[string]Histogram, len(m.Histograms))
	for k, v := range m.Histograms {
		h := *v
		h.Buckets = append([]uint64(nil), v.Buckets...)
		histograms[k] = h
	}
	m.mu.Unlock()
	entries, bytes := 0, int64(0)
	if m.CacheStats != nil {
		entries, bytes = m.CacheStats()
	}
	return map[string]any{"instance": m.Instance, "time": time.Now().UTC(), "counters": counters, "gauges": gauges, "cache_entries": entries, "cache_payload_bytes": bytes, "histogram_bounds_seconds": Bounds, "histograms": histograms}
}
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m.Snapshot())
}

type writer struct {
	http.ResponseWriter
	status int
}

func (w *writer) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *writer) WriteHeader(s int) {
	if s >= 200 && w.status == 0 {
		w.status = s
	}
	w.ResponseWriter.WriteHeader(s)
}
func (w *writer) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(p)
}
func (w *writer) Flush() {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
func (m *Metrics) Wrap(next http.Handler) http.Handler {
	if m == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := m.sequence.Add(1)
		m.Add("incoming", 1)
		ww := &writer{ResponseWriter: w}
		defer func() {
			failure := recover()
			duration := time.Since(start)
			m.Add("completed", 1)
			m.Observe("request", duration)
			outcome := "response"
			if failure != nil {
				m.Add("aborted", 1)
				outcome = "aborted"
			}
			if ww.status != 0 {
				m.Add("status_"+strconv.Itoa(ww.status/100)+"xx", 1)
			}
			if m.logs != nil && id%m.every == 0 {
				route := "other"
				if r.URL.Path == "/api/public/item" {
					route = "public_item"
				}
				reason := ww.Header().Get("X-Gateway-Rejection")
				if reason != "queue_full" && reason != "wait_timeout" {
					reason = ""
				}
				row := map[string]any{"time": time.Now().UTC(), "instance": m.Instance, "request_id": strconv.FormatUint(id, 10), "route": route, "outcome": outcome, "status": ww.status, "duration_ms": float64(duration) / 1e6, "rejection_reason": reason}
				select {
				case m.logs <- row:
				default:
					m.Add("logs_dropped", 1)
				}
			}
			if failure != nil {
				panic(failure)
			}
		}()
		next.ServeHTTP(ww, r)
	})
}

// Transport measures actual backend attempts, including complete body lifetime.
type Transport struct {
	Next    http.RoundTripper
	Metrics *Metrics
}

func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	m := t.Metrics
	start := time.Now()
	m.Add("backend_attempts", 1)
	m.Gauge("active_backend", 1)
	var once sync.Once
	finish := func() { once.Do(func() { m.Gauge("active_backend", -1); m.Observe("upstream", time.Since(start)) }) }
	resp, err := t.Next.RoundTrip(r)
	if err != nil {
		m.Add("backend_errors", 1)
		finish()
		return resp, err
	}
	if resp.Body == nil {
		finish()
	} else {
		resp.Body = &body{ReadCloser: resp.Body, finish: finish, m: m}
	}
	return resp, nil
}

type body struct {
	io.ReadCloser
	finish  func()
	m       *Metrics
	errOnce sync.Once
}

func (b *body) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		if err != io.EOF {
			b.errOnce.Do(func() { b.m.Add("backend_errors", 1) })
		}
		b.finish()
	}
	return n, err
}
func (b *body) Close() error { err := b.ReadCloser.Close(); b.finish(); return err }
