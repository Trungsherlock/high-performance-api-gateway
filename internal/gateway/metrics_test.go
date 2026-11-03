package gateway

import (
	"encoding/json"
	"high-performance-api-gateway/internal/cache"
	"high-performance-api-gateway/internal/observe"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMetricsReconcileCoalescedBurst(t *testing.T) {
	var backendCalls atomic.Int64
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls.Add(1)
		<-release
		w.Header().Set("Cache-Control", "public, max-age=10")
		io.WriteString(w, "ok")
	}))
	defer backend.Close()
	m := observe.New("test", nil, 0)
	storage := cache.New(1000, 1<<20, nil)
	m.CacheStats = storage.Stats
	p, tr, err := NewWithOptions(backend.URL, 5*time.Second, storage, Options{Metrics: m, Coalesce: true, MaxConcurrent: 1, PermitWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.CloseIdleConnections()
	server := httptest.NewServer(p)
	defer server.Close()
	client := server.Client()
	client.Transport = &http.Transport{DisableCompression: true}
	defer client.CloseIdleConnections()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get(server.URL + "/api/public/item?id=42")
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c := m.Snapshot()["counters"].(map[string]int64)
		if c["local_followers"] == 19 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	resp, err := client.Get(server.URL + "/api/public/item?id=42")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	for time.Now().Before(deadline) {
		if m.Snapshot()["counters"].(map[string]int64)["completed"] == 21 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	snap := m.Snapshot()
	c := snap["counters"].(map[string]int64)
	for k, want := range map[string]int64{"incoming": 21, "completed": 21, "status_2xx": 21, "cache_hits": 1, "cache_misses": 20, "local_followers": 19, "backend_attempts": 1} {
		if c[k] != want {
			t.Errorf("%s=%d want %d", k, c[k], want)
		}
	}
	if backendCalls.Load() != 1 {
		t.Fatal("backend mismatch")
	}
	for k, v := range snap["gauges"].(map[string]int64) {
		if v != 0 {
			t.Errorf("leaked %s=%d", k, v)
		}
	}
	if snap["cache_entries"].(int) != 1 {
		t.Fatal("cache gauge")
	}
	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	var decoded any
	if json.Unmarshal(rr.Body.Bytes(), &decoded) != nil {
		t.Fatal("invalid metrics JSON")
	}
}

func TestMetricsDistinguishRejectedFlightFromRejectedClients(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	started := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release; io.WriteString(w, "ok") }))
	defer backend.Close()
	m := observe.New("test", nil, 0)
	p, tr, err := NewWithOptions(backend.URL, 5*time.Second, cache.New(1000, 1<<20, nil), Options{Metrics: m, Coalesce: true, MaxConcurrent: 1, PermitWait: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.CloseIdleConnections()
	server := httptest.NewServer(p)
	defer server.Close()
	defer free()
	client := server.Client()
	client.Transport = &http.Transport{DisableCompression: true}
	defer client.CloseIdleConnections()
	holdDone := make(chan struct{})
	go func() {
		defer close(holdDone)
		resp, err := client.Get(server.URL + "/hold")
		if err != nil {
			t.Error(err)
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("backend did not acquire permit")
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get(server.URL + "/api/public/item?id=42")
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 503 {
				t.Errorf("status=%d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()
	free()
	<-holdDone
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if m.Snapshot()["counters"].(map[string]int64)["completed"] == 21 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	c := m.Snapshot()["counters"].(map[string]int64)
	for name, want := range map[string]int64{"completed": 21, "status_5xx": 20, "permit_rejections": 1, "backend_attempts": 1, "local_followers": 19} {
		if c[name] != want {
			t.Errorf("%s=%d want %d", name, c[name], want)
		}
	}
}
