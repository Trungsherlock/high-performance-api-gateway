package front

import (
	"context"
	"high-performance-api-gateway/internal/cache"
	"high-performance-api-gateway/internal/gateway"
	"high-performance-api-gateway/internal/observe"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func client() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 5 * time.Second}
}
func TestIndependentColdFillsDirectAndFront(t *testing.T) {
	for _, via := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "front"}[via], func(t *testing.T) {
			var calls atomic.Int64
			release := make(chan struct{})
			var once sync.Once
			free := func() { once.Do(func() { close(release) }) }
			defer free()
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				<-release
				w.Header().Set("Cache-Control", "public, max-age=10")
				io.WriteString(w, "ok")
			}))
			t.Cleanup(backend.Close)
			var targets []Target
			var metrics []*observe.Metrics
			queue := 16
			for _, name := range []string{"a", "b"} {
				m := observe.New(name, nil, 0)
				metrics = append(metrics, m)
				handler, tr, err := gateway.NewWithOptions(backend.URL, 5*time.Second, cache.New(1000, 1<<20, nil), gateway.Options{Metrics: m, Coalesce: true, MaxConcurrent: 16, MaxWaiters: &queue, PermitWait: time.Second})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(tr.CloseIdleConnections)
				s := httptest.NewServer(handler)
				t.Cleanup(s.Close)
				ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
				t.Cleanup(ready.Close)
				targets = append(targets, Target{Name: name, URL: s.URL, ReadinessURL: ready.URL})

			}
			p, err := New(targets, 5*time.Second, 100*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(p.Close)
			p.Check(context.Background())
			server := httptest.NewServer(p)
			t.Cleanup(server.Close)
			c := client()
			defer c.CloseIdleConnections()
			var wg sync.WaitGroup
			begin := make(chan struct{})
			for i := 0; i < 100; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-begin
					url := targets[i%2].URL
					if via {
						url = server.URL
					}
					resp, err := c.Get(url + "/api/public/item?id=42")
					if err != nil {
						t.Error(err)
						return
					}
					body, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil || resp.StatusCode != 200 || string(body) != "ok" {
						t.Errorf("response: %d %q %v", resp.StatusCode, body, err)
					}
				}(i)
			}
			close(begin)
			deadline := time.Now().Add(3 * time.Second)
			followers := int64(0)
			for time.Now().Before(deadline) {
				followers = 0
				for _, m := range metrics {
					followers += m.Snapshot()["counters"].(map[string]int64)["local_followers"]
				}
				if followers == 98 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			free()
			wg.Wait()
			if followers != 98 || calls.Load() != 2 {
				t.Fatalf("followers=%d backend=%d", followers, calls.Load())
			}
			for _, m := range metrics {
				c := m.Snapshot()["counters"].(map[string]int64)
				if c["incoming"] != 50 || c["backend_attempts"] != 1 {
					t.Fatal(c)
				}
			}
			if via {
				stats := p.Snapshot()
				if stats.Targets[0].Selected != 50 || stats.Targets[1].Selected != 50 {
					t.Fatal(stats)
				}
			}
		})
	}
}
func TestFailureIsNotRetriedAndHealthExclusion(t *testing.T) {
	var otherCalls atomic.Int64
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "a") }))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			otherCalls.Add(1)
		}
		io.WriteString(w, "b")
	}))
	defer b.Close()
	p, err := New([]Target{{Name: "a", URL: a.URL, ReadinessURL: a.URL + "/readyz"}, {Name: "b", URL: b.URL, ReadinessURL: b.URL + "/readyz"}}, time.Second, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != 503 {
		t.Fatal(rr.Code)
	}
	p.Check(context.Background())
	a.Close()
	rr = httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != 502 || otherCalls.Load() != 0 {
		t.Fatalf("failed target rerouted: %d calls=%d", rr.Code, otherCalls.Load())
	}
	p.Check(context.Background())
	rr = httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != 200 || otherCalls.Load() != 1 {
		t.Fatal(rr.Code)
	}
	b.Close()
	p.Check(context.Background())
	rr = httptest.NewRecorder()
	p.Admin().ServeHTTP(rr, httptest.NewRequest("GET", "/readyz", nil))
	if rr.Code != 503 {
		t.Fatal("ready with no healthy target")
	}
	rr = httptest.NewRecorder()
	p.Admin().ServeHTTP(rr, httptest.NewRequest("GET", "/healthz", nil))
	if rr.Code != 200 {
		t.Fatal("liveness coupled to readiness")
	}
}
func TestProbeDeadlineAndValidation(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer slow.Close()
	p, err := New([]Target{{Name: "slow", URL: slow.URL, ReadinessURL: slow.URL}}, time.Second, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	start := time.Now()
	p.Check(context.Background())
	if time.Since(start) > time.Second || p.Snapshot().Targets[0].Healthy {
		t.Fatal("probe did not honor deadline")
	}
	for _, target := range []Target{{Name: "a", URL: "http://user:secret@host", ReadinessURL: slow.URL}, {Name: "a", URL: slow.URL, ReadinessURL: "file:///secret"}} {
		if _, err := New([]Target{target}, time.Second, time.Second); err == nil {
			t.Fatal("invalid target accepted")
		}
	}
}

func TestCallerCancellationDoesNotChangeReadiness(t *testing.T) {
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer ready.Close()
	p, err := New([]Target{{Name: "a", URL: ready.URL, ReadinessURL: ready.URL}}, time.Second, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.Check(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	p.ServeHTTP(httptest.NewRecorder(), req)
	if !p.Snapshot().Targets[0].Healthy {
		t.Fatal("caller cancellation changed independent readiness")
	}
}
